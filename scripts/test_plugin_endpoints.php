<?php
declare(strict_types=1);
// CLI test in an isolated /tmp root. No Unraid login or live NAS is contacted.
require $argv[1].'/plugin/include/Settings.php';
$root=$argv[2];
if (!str_starts_with($root,'/tmp/mfconnect-plugin-endpoints-')) throw new RuntimeException('isolated root required');
function check(bool $v,string $m): void {if (!$v) throw new RuntimeException($m);}
function fails(callable $f,string $message): void {try {$f();} catch(MFSettingsError $e) {check($e->getMessage()===$message,'wrong error');return;}throw new RuntimeException('expected rejection');}
mkdir($root.'/boot/config/plugins/mfconnect',0700,true);
mkdir($root.'/usr/local/lib/mfconnect/current',0700,true);
$config=$root.'/boot/config/plugins/mfconnect/config.json';
$c=['server'=>'https://api.old.test','private_key'=>'secret-private-key','tunnel_token'=>'secret-tunnel-token','device_id'=>'dev_test','targets'=>[]];
file_put_contents($config,json_encode($c));
$binary=$root.'/usr/local/lib/mfconnect/current/mfconnectd';
file_put_contents($binary, <<<'PY'
#!/usr/bin/python3
import sys,json
args=sys.argv; contract=int(args[args.index('--endpoint-contract')+1]); c=json.load(open(args[args.index('--config')+1])); action=json.load(sys.stdin)['action']
if c.get('legacy_controller') and action=='endpoints':
 print(json.dumps({'status':400,'error':'invalid_action','data':{}}));sys.exit(0)
connect={'id':'home','origin':'https://home.example.test','entry_url':'https://example.test/home','enabled':True}
if action=='status': data=connect
else:
 assert contract==2
 data={'schema_version':2,'endpoint_contract':2,'api_origin':c['server'],'api_base_url':c.get('api_base_url',c['server']),'entry_origin':'https://example.test','canonical_api_base_url':'https://example.test/api','connect':connect,'capabilities':{'unified_entry':True}}
 if action=='session': data['access_token']='test-session-token'
print(json.dumps({'status':200,'data':data}))
PY);
chmod($binary,0700);
$s=new MFSettings($root);
$v1=$s->connectState();check($v1['schema_version']===1 && $v1['api_origin']===$c['server'],'legacy response changed');
$v2=$s->connectState(2);check($v2['schema_version']===2 && $v2['endpoint_verified'] && $v2['api_base_url']===$c['server'],'configured legacy API not retained');
check($v2['canonical_api_base_url']==='https://example.test/api','canonical endpoint missing');
check(!str_contains(json_encode($v2),'secret-'),'secret leaked');
$c['legacy_controller']=true;file_put_contents($config,json_encode($c));
fails(fn()=>$s->connectState(2),'endpoint_contract_2_unavailable');
check($s->connectState()['schema_version']===1,'legacy controller fallback failed');
unset($c['legacy_controller']);
$c['server']='https://example.test';$c['entry_origin']=$c['server'];$c['api_base_url']=$c['server'].'/api';
file_put_contents($config,json_encode($c));
fails(fn()=>$s->connectState(),'endpoint_contract_2_required');
fails(fn()=>$s->connectSession(),'endpoint_contract_2_required');
$new=$s->connectState(2);check($new['api_origin']==='https://example.test' && $new['api_base_url']==='https://example.test/api','base/origin conflated');
$session=$s->connectSession(2);check($session['access_token']==='test-session-token','session negotiation failed');
fails(fn()=>$s->connectState(3),'unsupported_endpoint_contract');
echo "PASS: PHP local contract negotiation, legacy configuration preference, v2 session, pure-unified v1 rejection, no config secrets\n";
