<?php
declare(strict_types=1);
// 在临时目录测试真实配置校验；只用测试进程，不操作 NAS 正式服务。
require $argv[1].'/plugin/include/Settings.php';
$bundle = $argv[1];
$root = $argv[2];
if (!str_starts_with($root, '/tmp/mfconnect-settings-')) throw new RuntimeException('isolated root required');
function check(bool $value, string $message): void { if (!$value) throw new RuntimeException($message); }
function fails(callable $fn, int $code): void { try {$fn();} catch (MFSettingsError $e) {check($e->getCode() === $code, $e->getMessage()); return;} throw new RuntimeException('expected rejection'); }
mkdir($root.'/boot/config/plugins/mfconnect',0700,true);
mkdir($root.'/usr/local/lib/mfconnect',0700,true);
symlink($bundle.'/payload',$root.'/usr/local/lib/mfconnect/current');
mkdir($root.'/etc/rc.d',0700,true);
mkdir($root.'/etc/ssl/certs',0700,true);
symlink('/etc/ssl/certs/ca-certificates.crt',$root.'/etc/ssl/certs/ca-certificates.crt');
$control = $root.'/etc/rc.d/rc.mfconnect';
file_put_contents($control, '#!/bin/sh'."\n".'flag="$MFCONNECT_TEST_ROOT/running"'."\n".'case "$1" in status) test -f "$flag";; stop) rm -f "$flag";; start|restart) touch "$flag";; *) exit 1;; esac'."\n");
chmod($control,0700); putenv('MFCONNECT_TEST_ROOT='.$root);
$s = new MFSettings($root);
$initial = $s->state();check(!$initial['bound'] && $initial['revision']==='new','initial state');
fails(fn()=>$s->action('bind',['revision'=>'new','server'=>'http://invalid','name'=>'Test','enrollment_code'=>str_repeat('a',43)]),400);
// init is separately verified by the real binary, with an existing system CA path.
$config = $root.'/boot/config/plugins/mfconnect/config.json';
$args = [$bundle.'/payload/mfconnectd','init','--config',$config,'--server','https://api.connect.example.com','--ca','/etc/ssl/certs/ca-certificates.crt'];
$p = proc_open($args,[0=>['file','/dev/null','r'],1=>['file','/dev/null','w'],2=>['file','/dev/null','w']],$pipes);check(proc_close($p)===0,'init');
$c=json_decode(file_get_contents($config),true);$c['device_id']='dev_test';$c['tunnel_token']=str_repeat('t',43);file_put_contents($config,json_encode($c));
$before=$s->state();$raw=file_get_contents($config);
check(!str_contains(json_encode($before),$c['private_key'])&&!str_contains(json_encode($before),$c['tunnel_token']),'secret disclosure');
fails(fn()=>$s->action('save_targets',['revision'=>$before['revision'],'targets'=>[['key'=>'web','name'=>'Web','url'=>'http://host/path']]]),400);
check(file_get_contents($config)===$raw,'invalid target changed configuration');
$s->action('save_targets',['revision'=>$before['revision'],'targets'=>[['key'=>'jellyfin','name'=>'Jellyfin','url'=>'http://192.168.50.96:8096']]]);
$new=json_decode(file_get_contents($config),true);check($new['private_key']===$c['private_key']&&$new['device_id']===$c['device_id'],'binding changed');
check(count($s->state()['targets'])===1,'target absent');
fails(fn()=>$s->action('save_targets',['revision'=>$before['revision'],'targets'=>[]]),409);
$current=$s->state();$lock=fopen($root.'/var/run/mfconnect/install.lock','c+');flock($lock,LOCK_EX);
fails(fn()=>$s->action('start',['revision'=>$current['revision']]),409);fclose($lock);
$s->action('start',['revision'=>$current['revision']]);check($s->state()['running'],'not started');
$s->action('stop',['revision'=>$current['revision']]);check($s->state()['disabled']&&!$s->state()['running'],'stop not persistent');
$s->action('start',['revision'=>$current['revision']]);check(!$s->state()['disabled'],'start not enabled');
fails(fn()=>$s->action('save_management',['revision'=>$current['revision'],'management_token'=>"invalid\nheader"]),400);
$s->action('save_targets',['revision'=>$current['revision'],'targets'=>[]]);check($s->state()['targets']===[],'delete target failed');
echo "PASS: setup state, invalid/stale/locked changes rejected, real config validation, secrets redacted, binding preserved, persistent stop/start, target removal\n";
