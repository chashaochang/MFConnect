<?php
declare(strict_types=1);

header('Content-Type: application/json; charset=utf-8');
header('Cache-Control: no-store');
header('X-Content-Type-Options: nosniff');
require_once __DIR__.'/include/Settings.php';

function mf_reply(int $code, array $data): never {
    http_response_code($code);
    echo json_encode($data, JSON_UNESCAPED_UNICODE | JSON_INVALID_UTF8_SUBSTITUTE);
    exit;
}

// Unraid's Nginx auth_request and PHP auto_prepend_file protect this endpoint.
// Check the same session here too, and verify our additional CSRF field even if
// auto_prepend_file has already consumed Unraid's csrf_token POST field.
if (session_status() !== PHP_SESSION_ACTIVE) session_start();
$loggedIn = !empty($_SESSION['unraid_login']) && ($_SESSION['unraid_user'] ?? '') === 'root';
session_write_close();
if (!$loggedIn) mf_reply(401, ['error' => '请先登录 Unraid 管理员账号。']);

$method = $_SERVER['REQUEST_METHOD'] ?? '';
if (!in_array($method, ['GET', 'POST'], true)) mf_reply(405, ['error' => '不支持的请求方法。']);
if ($method === 'POST') {
    $vars = @parse_ini_file('/var/local/emhttp/var.ini');
    $expected = $vars['csrf_token'] ?? '';
    $provided = $_POST['mf_csrf'] ?? null;
    if (!is_string($expected) || $expected === '' || !is_string($provided) || !hash_equals($expected, $provided)) {
        mf_reply(403, ['error' => '页面已过期，请刷新后重试。']);
    }
    if ((int)($_SERVER['CONTENT_LENGTH'] ?? 0) > 131072) mf_reply(413, ['error' => '配置内容过大。']);
}
try {
    $settings = new MFSettings();
    if ($method === 'GET') mf_reply(200, ['state' => $settings->state()]);
    $action = $_POST['action'] ?? null;
    $raw = $_POST['data'] ?? null;
    if (!is_string($action) || !is_string($raw)) throw new MFSettingsError('请求格式错误。', 400);
    $data = json_decode($raw, true, 32, JSON_THROW_ON_ERROR);
    if (!is_array($data)) throw new MFSettingsError('请求格式错误。', 400);
    $message = $settings->action($action, $data);
    mf_reply(200, ['message' => $message, 'state' => $settings->state()]);
} catch (MFSettingsError $e) {
    mf_reply($e->getCode() ?: 400, ['error' => $e->getMessage()]);
} catch (JsonException $e) {
    mf_reply(400, ['error' => '配置格式错误。']);
} catch (Throwable $e) {
    // Do not serialize exception details, tokens, private configuration or commands.
    mf_reply(500, ['error' => '操作失败，请检查插件是否安装完整及存储是否可写。']);
}
