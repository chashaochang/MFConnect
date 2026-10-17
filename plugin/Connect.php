<?php
declare(strict_types=1);
header('Content-Type: application/json; charset=utf-8');
header('Cache-Control: no-store');
header('X-Content-Type-Options: nosniff');
require_once __DIR__.'/include/Settings.php';
function mf_connect_reply(int $code,array $data): never {
    http_response_code($code);echo json_encode($data,JSON_UNESCAPED_UNICODE|JSON_INVALID_UTF8_SUBSTITUTE);exit;
}
if (session_status()!==PHP_SESSION_ACTIVE) session_start();
$loggedIn=!empty($_SESSION['unraid_login']) && ($_SESSION['unraid_user']??'')==='root';
session_write_close();
if (!$loggedIn) mf_connect_reply(401,['error'=>'nas_login_required']);
$method=$_SERVER['REQUEST_METHOD']??'';
$vars=@parse_ini_file('/var/local/emhttp/var.ini');$csrf=$vars['csrf_token']??'';
if ($method==='POST') {
    if ((int)($_SERVER['CONTENT_LENGTH']??0)>8192) mf_connect_reply(413,['error'=>'request_too_large']);
    $provided=$_POST['mf_csrf']??null;
    if ($csrf==='' || !is_string($provided) || !hash_equals($csrf,$provided)) mf_connect_reply(403,['error'=>'invalid_csrf']);
}
try {
    $settings=new MFSettings();
    $requested=($method==='POST' ? ($_POST['endpoint_contract']??'1') : ($_GET['endpoint_contract']??'1'));
    if (!is_string($requested) || !in_array($requested,['1','2'],true)) mf_connect_reply(400,['error'=>'unsupported_endpoint_contract']);
    $contract=(int)$requested;
    if ($method==='GET') mf_connect_reply(200,$settings->connectState($contract)+['csrf_token'=>$csrf]);
    if ($method!=='POST') mf_connect_reply(405,['error'=>'method_not_allowed']);
    if (($_POST['action']??'')!=='session') mf_connect_reply(400,['error'=>'invalid_action']);
    mf_connect_reply(200,$settings->connectSession($contract));
} catch (MFSettingsError $e) {mf_connect_reply($e->getCode()?:400,['error'=>$e->getMessage()]);}
catch (Throwable $e) {mf_connect_reply(500,['error'=>'connect_unavailable']);}
