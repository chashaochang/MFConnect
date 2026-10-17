<?php
declare(strict_types=1);

final class MFSettingsError extends RuntimeException {}

// All paths are fixed by the caller. The HTTP endpoint always uses the real root;
// an alternate root is only passed by the CLI integration test.
final class MFSettings {
    private string $config;
    private string $run;
    private string $base;
    private string $control;
    private string $tokenFile;

    public function __construct(private string $root = '') {
        $this->config = $root.'/boot/config/plugins/mfconnect/config.json';
        $this->run = $root.'/var/run/mfconnect';
        $this->base = $root.'/usr/local/lib/mfconnect/current';
        $this->control = $root.'/etc/rc.d/rc.mfconnect';
        $this->tokenFile = dirname($this->config).'/management-token';
    }

    private function config(): ?array {
        if (!is_file($this->config)) return null;
        $c = json_decode((string)file_get_contents($this->config), true);
        if (!is_array($c) || !isset($c['server'], $c['private_key']) || !is_array($c['targets'] ?? null)) {
            throw new MFSettingsError('现有配置无法读取，请先恢复有效配置。', 409);
        }
        return $c;
    }

    private function revision(): string {
        return is_file($this->config) ? hash_file('sha256', $this->config) : 'new';
    }

    private function text(array $data, string $key, int $max, bool $required = true): string {
        $value = $data[$key] ?? '';
        if (!is_string($value) || strlen($value) > $max || preg_match('/[\x00-\x1f\x7f]/', $value)) {
            throw new MFSettingsError('输入格式不正确：'.$key, 400);
        }
        $value = trim($value);
        if ($required && $value === '') throw new MFSettingsError('请填写完整信息。', 400);
        return $value;
    }

    private function directory(string $path): void {
        if (!is_dir($path) && !mkdir($path, 0700, true) && !is_dir($path)) {
            throw new MFSettingsError('无法创建配置目录。', 500);
        }
    }

    private function write(string $path, string $bytes): void {
        $this->directory(dirname($path));
        $temp = tempnam(dirname($path), '.mf-ui-');
        if ($temp === false) throw new MFSettingsError('无法保存配置。', 500);
        try {
            $file = fopen($temp, 'wb');
            if (!$file) throw new MFSettingsError('无法保存配置。', 500);
            try {
                if (fwrite($file, $bytes) !== strlen($bytes) || !fflush($file) || !fsync($file)) {
                    throw new MFSettingsError('无法保存配置。', 500);
                }
            } finally { fclose($file); }
            if (!rename($temp, $path)) throw new MFSettingsError('无法替换配置。', 500);
        } finally { if (is_file($temp)) unlink($temp); }
    }

    private function command(array $args, int $seconds = 25): bool {
        $process = proc_open($args, [0 => ['file', '/dev/null', 'r'],
            1 => ['file', '/dev/null', 'w'], 2 => ['file', '/dev/null', 'w']], $pipes);
        if (!is_resource($process)) throw new MFSettingsError('无法启动连接程序。', 500);
        $deadline = microtime(true) + $seconds;
        do {
            $status = proc_get_status($process);
            if (!$status['running']) {
                proc_close($process);
                return $status['exitcode'] === 0;
            }
            usleep(50000);
        } while (microtime(true) < $deadline);
        proc_terminate($process, 9);
        proc_close($process);
        throw new MFSettingsError('操作超时，请检查连接状态后重试。', 504);
    }

    private function validate(array $config): void {
        $file = $this->run.'/check-'.bin2hex(random_bytes(8)).'.json';
        $this->write($file, json_encode($config, JSON_THROW_ON_ERROR));
        try {
            if (!$this->command([$this->base.'/mfconnectd', 'check', '--config', $file, '--require-transport', 'frp'])) {
                throw new MFSettingsError('配置无效。应用地址应为 http://IP:端口 或 https://域名，不含路径、用户名或密码。', 400);
            }
        } finally { if (is_file($file)) unlink($file); }
    }

    private function ca(): string {
        foreach (['/etc/ssl/certs/ca-certificates.crt', '/etc/ssl/cert.pem', '/etc/pki/tls/certs/ca-bundle.crt'] as $path) {
            if (is_readable($this->root.$path)) return $this->root.$path;
        }
        throw new MFSettingsError('未找到系统可信证书，请检查 Unraid 系统。', 500);
    }

    private function cloud(array $c, string $token, string $method, string $path, ?array $body = null): array {
        $url = parse_url($c['server']);
        if (!$url || ($url['scheme'] ?? '') !== 'https' || empty($url['host']) ||
            isset($url['user']) || isset($url['pass']) || isset($url['query']) || isset($url['fragment']) ||
            !in_array($url['path'] ?? '', ['', '/'], true)) {
            throw new MFSettingsError('云端地址必须是 HTTPS 地址。', 400);
        }
        $base = $c['api_base_url'] ?? rtrim($c['server'], '/');
        if (!in_array($base, [rtrim($c['server'], '/'), rtrim($c['server'], '/').'/api'], true)) throw new MFSettingsError('API 地址与已配置入口不一致。', 409);
        $handle = curl_init($base.$path);
        $raw = '';
        $headers = ['Authorization: Bearer '.$token, 'Accept: application/json'];
        if ($body !== null) $headers[] = 'Content-Type: application/json';
        curl_setopt_array($handle, [CURLOPT_CUSTOMREQUEST => $method, CURLOPT_HTTPHEADER => $headers,
            CURLOPT_CONNECTTIMEOUT => 5, CURLOPT_TIMEOUT => 12, CURLOPT_FOLLOWLOCATION => false,
            CURLOPT_SSL_VERIFYPEER => true, CURLOPT_SSL_VERIFYHOST => 2,
            CURLOPT_CAINFO => $c['ca_file'], CURLOPT_PROTOCOLS => CURLPROTO_HTTPS,
            CURLOPT_WRITEFUNCTION => static function ($ch, string $chunk) use (&$raw): int {
                if (strlen($raw) + strlen($chunk) > 1048576) return 0;
                $raw .= $chunk; return strlen($chunk);
            }]);
        if ($body !== null) curl_setopt($handle, CURLOPT_POSTFIELDS, json_encode($body, JSON_THROW_ON_ERROR));
        $ok = curl_exec($handle);
        $status = curl_getinfo($handle, CURLINFO_RESPONSE_CODE);
        curl_close($handle);
        if ($ok === false) throw new MFSettingsError('无法连接云端，请检查网络、系统时间和证书。', 502);
        if ($status === 401 || $status === 403) throw new MFSettingsError('云端管理授权无效或已失效，请重新设置管理令牌。', 403);
        if ($status < 200 || $status >= 300) throw new MFSettingsError('云端暂时无法完成此操作，请稍后重试。', 502);
        $data = json_decode($raw, true);
        if (!is_array($data)) throw new MFSettingsError('云端响应无效。', 502);
        return $data;
    }

    private function token(): string {
        $value = is_file($this->tokenFile) ? trim((string)file_get_contents($this->tokenFile)) : '';
        if (!preg_match('/^[A-Za-z0-9_-]{32,128}$/D', $value)) {
            throw new MFSettingsError('请先设置设备所属用户的云端管理令牌。', 409);
        }
        return $value;
    }

    public function deviceCall(array $input, int $contract = 1): array {
        if (!in_array($contract, [1,2], true)) throw new MFSettingsError('unsupported_endpoint_contract',400);
        $c=$this->config();
        if (!$c || empty($c['device_id'])) throw new MFSettingsError('请先接入自己的云服务器。',409);
        $process=proc_open([$this->base.'/mfconnectd','connect','--config',$this->config,'--endpoint-contract',(string)$contract],
            [0=>['pipe','r'],1=>['pipe','w'],2=>['file','/dev/null','w']],$pipes);
        if (!is_resource($process)) throw new MFSettingsError('无法启动连接程序。',500);
        $raw='';$deadline=microtime(true)+20;
        try {
            fwrite($pipes[0],json_encode($input,JSON_THROW_ON_ERROR));fclose($pipes[0]);
            stream_set_blocking($pipes[1],false);
            do {
                $raw.=stream_get_contents($pipes[1]);
                if (strlen($raw)>1048576) throw new MFSettingsError('云端响应过大。',502);
                $status=proc_get_status($process);
                if (!$status['running']) {$raw.=stream_get_contents($pipes[1]);break;}
                if (microtime(true)>$deadline) throw new MFSettingsError('连接云端超时，请稍后重试。',504);
                usleep(20000);
            } while (true);
        } finally {
            if (is_resource($pipes[0])) fclose($pipes[0]);
            fclose($pipes[1]);
            if (proc_get_status($process)['running']) proc_terminate($process,9);
            proc_close($process);
        }
        $out=json_decode($raw,true);
        if (!is_array($out) || !isset($out['status']) || !is_array($out['data']??null)) throw new MFSettingsError('无法读取云端响应，请检查插件和云端版本。',502);
        if ($out['status']<200 || $out['status']>=300) {
            if ($contract===2 && ($input['action']??'')==='endpoints' && ($out['error']??'')==='invalid_action') {
                throw new MFSettingsError('endpoint_contract_2_unavailable',409);
            }
            $messages=['connect_id_taken'=>'这个连接 ID 已被使用，请换一个。','invalid_connect_id'=>'连接 ID 应为 1–32 位字母、数字或中间连字符。',
                'connector_upgrade_required'=>'请先升级云端和插件。','invalid_device_signature'=>'设备接入身份已失效。','docker_automatically_managed'=>'Docker 应用随 MF Connect 自动管理，无需单独发布。'];
            throw new MFSettingsError($messages[$out['error']]??'暂时无法连接云端，请检查连接状态。',(int)$out['status']);
        }
        return $out['data'];
    }

    public function connectState(int $contract = 1): array {
        $c=$this->config();
        if (!$c || empty($c['device_id'])) throw new MFSettingsError('device_not_provisioned',409);
        if ($contract === 2) return $this->deviceCall(['action'=>'endpoints'],2)+['configuration_source'=>'plugin','endpoint_verified'=>true];
        if ($contract !== 1) throw new MFSettingsError('unsupported_endpoint_contract',400);
        if (!empty($c['api_base_url']) && $c['api_base_url'] !== $c['server']) throw new MFSettingsError('endpoint_contract_2_required',409);
        return ['schema_version'=>1,'api_origin'=>$c['server'],'device_id'=>$c['device_id'],'connect'=>$this->deviceCall(['action'=>'status']),
            'capabilities'=>['docker_discovery'=>1,'self_install'=>1,'endpoint_contracts'=>[1,2]]];
    }

    public function connectSession(int $contract = 1): array {
        $this->connectState($contract);
        return $this->deviceCall(['action'=>'session'],$contract);
    }

    public function state(): array {
        $c = $this->config();
        $runtime = is_file($this->run.'/status.json') ? json_decode((string)file_get_contents($this->run.'/status.json'), true) : [];
        if (!is_array($runtime)) $runtime = [];
        $running = $this->command([$this->control, 'status'], 3);
        $disabled = is_file(dirname($this->config).'/disabled');
        $state = $disabled || !$running ? 'stopped' : ($runtime['state'] ?? 'starting');
        if (!$c || empty($c['device_id'])) $state = 'configuration_required';
        $cloudServices = [];
        $cloudError = null;
        $hasToken = !empty($c['device_id']);
        $connect = null;
        $discovery = null;
        if (!empty($c['device_id'])) {
            try {
                $overview=$this->deviceCall(['action'=>'overview']);
                $connect=$overview['connect']??null;
                $discovery=$overview['docker']??null;
                foreach ($overview['services']??[] as $service) {
                    if (($service['device_id'] ?? '') === $c['device_id']) $cloudServices[$service['key']] = $service;
                }
            } catch (MFSettingsError $e) { $cloudError = $e->getMessage(); }
        }
        $services = [];
        foreach (($runtime['services'] ?? []) as $service) $services[$service['key']] = $service;
        $targets = [];
        foreach (($c['targets'] ?? []) as $target) {
            $service = $cloudServices[$target['key']] ?? $services[$target['key']] ?? null;
            $targets[] = ['key' => $target['key'], 'name' => $target['name'] ?? '', 'url' => $target['url'],
                'certificate_sha256' => $target['certificate_sha256'] ?? '',
                'registered' => $service !== null, 'published' => $service['published'] ?? false,
                'docker_managed' => $cloudServices[$target['key']]['docker_managed'] ?? false,
                'origin' => $cloudServices[$target['key']]['origin'] ?? null];
        }
        $docker = @file_get_contents($this->root.'/boot/config/docker.cfg');
        $hostAccess = $docker === false ? null : (bool)preg_match('/^\s*DOCKER_ALLOW_ACCESS=["\x27]?yes["\x27]?\s*$/m', $docker);
        return ['version' => trim((string)@file_get_contents($this->base.'/VERSION')),
            'revision' => $this->revision(), 'server' => $c['entry_origin'] ?? $c['server'] ?? '',
            'name' => $c['name'] ?? 'Unraid', 'device_id' => $c['device_id'] ?? '',
            'bound' => !empty($c['device_id']), 'running' => $running, 'disabled' => $disabled,
            'state' => $state, 'last_sync' => $runtime['last_sync'] ?? 0,
            'management_configured' => $hasToken, 'cloud_error' => $cloudError,
            'connect' => $connect, 'docker' => $discovery,
            'host_access' => $hostAccess, 'targets' => $targets];
    }

    public function action(string $action, array $data): string {
        umask(0077);
        $this->directory($this->run);
        $locks = [];
        try {
            foreach (['install.lock', 'setup.lock'] as $name) {
                $lock = fopen($this->run.'/'.$name, 'c+e');
                if (!$lock) throw new MFSettingsError('无法锁定配置。', 500);
                $locks[] = $lock;
                if (!flock($lock, LOCK_EX | LOCK_NB)) throw new MFSettingsError('另一个安装或配置操作正在进行，请稍后重试。', 409);
            }
            if (($data['revision'] ?? null) !== $this->revision()) {
                throw new MFSettingsError('配置已被其他操作更新，请刷新页面后重试。', 409);
            }
            return $this->apply($action, $data);
        } finally { foreach (array_reverse($locks) as $lock) fclose($lock); }
    }

    private function apply(string $action, array $data): string {
        $c = $this->config();
        if ($action === 'bind') {
            if (!empty($c['device_id'])) throw new MFSettingsError('此 NAS 已绑定，请直接管理下方应用。', 409);
            $server = rtrim($this->text($data, 'server', 512), '/');
            $name = $this->text($data, 'name', 128);
            $code = $this->text($data, 'enrollment_code', 128);
            if (!preg_match('/^[A-Za-z0-9_-]{32,128}$/D', $code)) throw new MFSettingsError('绑定码格式不正确。', 400);
            if (!$c) {
                if (!$this->command([$this->base.'/mfconnectd', 'init', '--config', $this->config,
                    '--server', $server, '--name', $name, '--ca', $this->ca()])) {
                    throw new MFSettingsError('初始化失败，请检查 HTTPS 云端地址。', 400);
                }
                $c = $this->config();
            } else {
                $c['server'] = $server; $c['name'] = $name;
                $this->validate($c);
                $this->write($this->config, json_encode($c, JSON_THROW_ON_ERROR | JSON_PRETTY_PRINT)."\n");
            }
            $invite = $this->run.'/enrollment-'.bin2hex(random_bytes(8));
            $this->write($invite, $code."\n");
            try {
                if (!$this->command([$this->base.'/mfconnectd', 'enroll', '--config', $this->config, '--enrollment-file', $invite])) {
                    throw new MFSettingsError('绑定失败：请检查绑定码是否过期或已使用，以及 NAS 到云端的网络和系统时间。', 400);
                }
            } finally { if (is_file($invite)) unlink($invite); }
            $this->enable();
            return '设备绑定成功，连接已启动。';
        }
        if (!$c || empty($c['device_id'])) throw new MFSettingsError('请先绑定此 NAS。', 409);
        if ($action === 'save_web_access') {
            if (!is_bool($data['browser_public_direct']??null)) throw new MFSettingsError('请选择有效的访问方式。',400);
            $current=$this->deviceCall(['action'=>'status']);
            $saved=$this->deviceCall(['action'=>'configure','id'=>$current['id'],'enabled'=>$current['enabled'],'browser_public_direct'=>$data['browser_public_direct']]);
            if (($saved['browser_public_direct']??null)!==$data['browser_public_direct']) throw new MFSettingsError('访问方式未确认保存，请刷新重试。',502);
            return '浏览器访问方式已保存。';
        }
        if ($action === 'save_targets') {
            $input = $data['targets'] ?? null;
            if (!is_array($input) || !array_is_list($input) || count($input) > 64) throw new MFSettingsError('最多配置 64 个应用。', 400);
            $targets = []; $seen = [];
            foreach ($input as $item) {
                if (!is_array($item)) throw new MFSettingsError('应用配置格式错误。', 400);
                $key = $this->text($item, 'key', 64, false);
                if ($key === '') $key = 'app_'.bin2hex(random_bytes(8));
                if (isset($seen[$key])) throw new MFSettingsError('应用标识重复。', 400);
                $seen[$key] = true;
                $target = ['key' => $key, 'name' => $this->text($item, 'name', 128), 'url' => $this->text($item, 'url', 1024)];
                $pin = $this->text($item, 'certificate_sha256', 64, false);
                if ($pin !== '') $target['certificate_sha256'] = strtolower($pin);
                $targets[] = $target;
            }
            $c['targets'] = $targets;
            $this->validate($c);
            $this->write($this->config, json_encode($c, JSON_THROW_ON_ERROR | JSON_PRETTY_PRINT | JSON_UNESCAPED_UNICODE)."\n");
            return '应用已保存。连接运行时会自动同步；新增或修改的应用需要重新启用远程访问。';
        }
        if ($action === 'save_management') {
            $token = $this->text($data, 'management_token', 128);
            if (!preg_match('/^[A-Za-z0-9_-]{32,128}$/D', $token)) throw new MFSettingsError('管理令牌格式不正确。', 400);
            $devices = $this->cloud($c, $token, 'GET', '/v1/devices');
            $found = false;
            foreach ($devices as $device) {
                if (($device['id'] ?? '') === $c['device_id'] && empty($device['revoked'])) $found = true;
            }
            if (!$found) throw new MFSettingsError('此管理令牌不能管理当前 NAS。', 403);
            $this->write($this->tokenFile, $token."\n");
            return '云端管理授权已保存，可以启用或停用应用的远程访问。';
        }
        if ($action === 'forget_management') {
            if (is_file($this->tokenFile) && !unlink($this->tokenFile)) throw new MFSettingsError('无法清除管理授权。', 500);
            return '本机管理令牌已清除，现有应用发布状态保持不变。';
        }
        if ($action === 'publish') {
            $key = $this->text($data, 'key', 64);
            if (!is_bool($data['published'] ?? null)) throw new MFSettingsError('请选择有效的发布状态。', 400);
            foreach ($this->deviceCall(['action'=>'services']) as $service) {
                if (($service['device_id'] ?? '') === $c['device_id'] && ($service['key'] ?? '') === $key) {
                    if (!in_array($key, array_column($c['targets'], 'key'), true) && empty($service['docker_managed'])) throw new MFSettingsError('应用不存在，请刷新页面。', 404);
                    $this->deviceCall(['action'=>'publish','service_id'=>$service['id'],'published'=>$data['published']]);
                    return $data['published'] ? '远程访问已启用，连接同步可能需要几秒钟。' : '远程访问已停用，原访问会话已失效。';
                }
            }
            throw new MFSettingsError('应用尚未同步到云端，请确认连接已启动，稍后刷新。', 409);
        }
        if ($action === 'start' || $action === 'restart') {
            $this->enable($action);
            return '连接已启动，请稍后刷新状态。';
        }
        if ($action === 'stop') {
            $this->write(dirname($this->config).'/disabled', "disabled from settings\n");
            if (!$this->command([$this->control, 'stop'], 15)) throw new MFSettingsError('停止失败，请检查连接程序。', 500);
            return '连接已停止，重启 NAS 后也会保持停用，直到你再次启动。';
        }
        throw new MFSettingsError('不支持的操作。', 400);
    }

    private function enable(string $action = 'start'): void {
        $flag = dirname($this->config).'/disabled';
        if (is_file($flag) && !unlink($flag)) throw new MFSettingsError('无法启用连接。', 500);
        if (!$this->command([$this->control, $action], 15)) throw new MFSettingsError('配置已保存，但连接启动失败，请检查连接程序。', 500);
    }
}
