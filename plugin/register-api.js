// Local installation helper. Changes only this module's package/config entries.
// No npm network access, credential creation, nginx edits or added listeners.
const fs = require('node:fs');
const path = require('node:path');
const root = process.env.MFCONNECT_TEST_ROOT || '';
const prefix = (p) => root + p;
const api = prefix('/usr/local/unraid-api');
const config = prefix('/boot/config/plugins/dynamix.my.servers/configs/api.json');
const name = 'unraid-api-plugin-mfconnect';
const packagePath = path.join(api, 'package.json');
const installed = path.join(api, 'node_modules', name);
const source = prefix('/usr/local/lib/mfconnect/current/api');
const statePath = prefix('/boot/config/plugins/mfconnect/api-integration.json');
function read(p) { return JSON.parse(fs.readFileSync(p, 'utf8')); }
function atomic(p, data) {
  fs.mkdirSync(path.dirname(p), { recursive: true });
  const tmp = `${p}.mfconnect-${process.pid}`;
  const mode = fs.existsSync(p) ? fs.statSync(p).mode & 0o777 : 0o600;
  try { fs.writeFileSync(tmp, `${JSON.stringify(data, null, 2)}\n`, { mode, flag: 'wx' }); fs.renameSync(tmp, p); }
  finally { if (fs.existsSync(tmp)) fs.unlinkSync(tmp); }
}
const op = process.argv[2];
if (!['install', 'remove', 'check'].includes(op)) throw new Error('Expected install|remove|check');
if (!fs.existsSync(packagePath) || !fs.existsSync(config)) {
  if (op === 'remove') process.exit(0);
  throw new Error('Unraid API package/config not available; existing-origin transport not registered');
}
const pkg = read(packagePath), cfg = read(config);
if (!Array.isArray(cfg.plugins)) throw new Error('Unsupported Unraid API plugin configuration');
if (op === 'check') {
  if (!cfg.plugins.includes(name) || !pkg.dependencies?.[name] || !fs.existsSync(path.join(installed, 'index.js'))) process.exit(1);
  process.exit(0);
}
if (op === 'install') {
  if (!fs.existsSync(path.join(api, 'node_modules', '@nestjs', 'core'))) throw new Error('Unraid API runtime missing');
  const expected = `file:/usr/local/lib/mfconnect/current/api`;
  if (pkg.dependencies?.[name] && pkg.dependencies[name] !== expected) throw new Error('Existing third-party registration conflicts with MF Connect');
  if (!fs.existsSync(statePath)) atomic(statePath, { schema: 1, dependency: pkg.dependencies?.[name] ?? null, listed: cfg.plugins.includes(name) });
  // A real copy is required: ESM resolves peer dependencies from this directory.
  fs.mkdirSync(installed, { recursive: true });
  for (const file of ['package.json', 'index.js']) fs.copyFileSync(path.join(source, file), path.join(installed, file));
  pkg.dependencies ??= {}; pkg.dependencies[name] = expected;
  if (!cfg.plugins.includes(name)) cfg.plugins.push(name);
} else {
  if (!fs.existsSync(statePath)) process.exit(0);
  const old = read(statePath);
  if (old.dependency === null) delete pkg.dependencies?.[name]; else pkg.dependencies[name] = old.dependency;
  if (!old.listed) cfg.plugins = cfg.plugins.filter((x) => x !== name);
  if (fs.existsSync(path.join(installed, 'package.json')) && read(path.join(installed, 'package.json')).name === name) fs.rmSync(installed, { recursive: true });
}
atomic(packagePath, pkg); atomic(config, cfg);
if (op === 'remove') fs.unlinkSync(statePath);
