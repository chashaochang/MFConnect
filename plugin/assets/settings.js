(() => {
  'use strict';
  const root = document.getElementById('mf-settings');
  if (!root) return;
  const $ = id => document.getElementById(id);
  let state = null, dirty = false, busy = false;
  const states = {configuration_required:'尚未绑定',starting:'正在连接',supervising_tunnel:'连接程序运行中',controller_unreachable:'暂时无法连接云端',authorization_required:'设备授权已失效',tunnel_start_failed:'连接程序启动失败',stopped:'已停止'};
  function message(text, error = false) { $('mf-message').textContent = text; $('mf-message').classList.toggle('mf-error', error); }
  function pending(value) { busy = value; root.querySelectorAll('button').forEach(b => b.disabled = value); }
  async function request(action, data) {
    const options = {credentials:'same-origin',cache:'no-store',redirect:'error',headers:{Accept:'application/json'}};
    if (action) {
      options.method = 'POST';
      options.body = new URLSearchParams({csrf_token:root.dataset.csrf,mf_csrf:root.dataset.csrf,action,data:JSON.stringify({...data,revision:state.revision})});
    }
    const controller = new AbortController(); options.signal = controller.signal;
    const timer = setTimeout(() => controller.abort(), 55000);
    try {
      const response = await fetch('/plugins/mfconnect/Settings.php', options);
      if (!(response.headers.get('Content-Type') || '').includes('application/json')) throw Error('登录已过期，请刷新页面重新登录。');
      const result = await response.json();
      if (!response.ok) throw Error(result.error || '操作失败，请刷新状态后重试。');
      return result;
    } finally { clearTimeout(timer); }
  }
  function node(tag, text) { const el = document.createElement(tag); if (text !== undefined) el.textContent = text; return el; }
  function field(parent, title, name, value, options = {}) {
    const label = node('label', title), input = node('input');
    input.name = name; input.value = value || ''; input.maxLength = options.max || 128;
    input.type = options.type || 'text'; input.required = !!options.required;
    input.addEventListener('input', () => { dirty = true; });
    label.append(input); parent.append(label); return input;
  }
  function targetRow(target) {
    const row = node('div'); row.className = 'mf-target'; row.dataset.key = target.key || '';
    field(row,'应用名称','name',target.name,{required:true}).readOnly = !!target.docker_managed;
    field(row,'应用地址','url',target.url,{type:'url',required:true,max:1024}).readOnly = !!target.docker_managed;
    if (target.docker_managed) row.append(node('p','已与 Docker 自动关联。这里保留原始配置，当前入口及远程访问状态见上方自动发现列表。'));
    const details = node('details'); details.append(node('summary','HTTPS 证书指纹（可选）'));
    field(details,'自签名证书的 SHA-256 指纹','certificate_sha256',target.certificate_sha256,{max:64}); row.append(details);
    row.append(node('p',target.registered ? (target.published ? '远程访问：已启用' : '远程访问：未启用') : '等待保存并同步到云端'));
    if (target.origin) { const origin = node('p',target.origin); origin.className = 'mf-origin'; row.append(origin); row.append(node('p','请从 RaidMate 打开应用；直接访问地址需要有效的中继会话。')); }
    if (target.registered && !target.docker_managed) {
      const publish = node('button',target.published ? '停用远程访问' : '启用远程访问'); publish.type = 'button';
      publish.addEventListener('click', () => {
        if (dirty) return message('请先保存应用修改，再操作远程访问。',true);
        if (target.published && !confirm('停用后，当前应用的远程会话将失效。确定停用？')) return;
        act('publish',{key:target.key,published:!target.published});
      }); row.append(publish);
    }
    if (!target.docker_managed) { const remove = node('button','移除应用'); remove.type = 'button';
    remove.addEventListener('click', () => { if (confirm('移除并保存后，此应用将停止远程访问。确定移除？')) { row.remove(); dirty = true; } }); row.append(remove);
    }
    $('mf-targets').append(row);
  }
  function render(next) {
    state = next; dirty = false;
    const status = $('mf-status'); status.replaceChildren();
    for (const [label,value] of [['插件版本',next.version],['连接状态',states[next.state] || '状态未知'],['云端地址',next.server],['设备',next.device_id || '尚未绑定'],['最近同步',next.last_sync ? new Date(next.last_sync * 1000).toLocaleString() : '尚未同步']]) status.append(node('dt',label),node('dd',value));
    $('mf-binding').hidden = next.bound; $('mf-apps').hidden = !next.bound; $('mf-connect-details').hidden = !next.bound;
    const connect=next.connect, details=$('mf-connect-status'); details.replaceChildren();
    for (const [label,value] of [['连接 ID',connect?.id || '请在 RaidMate 中设置'],['远程访问',connect?.enabled ? '已开启' : '未开启'],['浏览器公网直连',connect?.browser_public_direct ? '优先尝试，失败使用中继' : '仅使用中继']]) details.append(node('dt',label),node('dd',value));
    const entry=$('mf-entry'); entry.replaceChildren();
    if (connect?.entry_url) {const link=node('a',connect.entry_url); link.href=connect.entry_url; link.target='_blank'; link.rel='noopener noreferrer'; entry.append(link);}
    $('mf-targets').replaceChildren(); next.targets.forEach(targetRow);
    $('mf-public-direct').checked=!!connect?.browser_public_direct;
    $('mf-public-direct').disabled=!connect;
    $('mf-public-hint').textContent=connect?.web_access?.public_ingress_configured ? '访问时验证公网网关；不可达时自动使用中继。' : '尚未配置可验证的公网网关，开启后仍将使用中继。';
    const docker=next.docker;
    $('mf-discovery-status').textContent = docker?.fresh ? '发现结果已同步' : '发现结果暂不可用，等待插件同步';
    const discovered=$('mf-docker-apps'); discovered.replaceChildren();
    const appStates={unreachable:'NAS 无法访问 WebUI',ready:'可用',stopped:'容器已停止',no_webui:'未声明 WebUI',invalid_webui:'WebUI 地址无效',network_unresolved:'无法确定访问地址',identity_conflict:'应用身份或地址冲突'};
    for (const app of docker?.applications || []) {
      const row=node('div'); row.className='mf-target';
      row.append(node('strong',app.name),node('p',(appStates[app.state] || app.state)+' · '+(app.relay_enabled ? '自动远程访问已就绪' : '等待 MF Connect 开启或应用恢复')));
      if (app.origin) row.append(node('p',app.origin+app.entry_path));
      discovered.append(row);
    }
    $('mf-host-access').textContent = next.host_access === null ? '无法读取设置' : (next.host_access ? '已启用' : '未启用');
    if (next.cloud_error) message(next.cloud_error,true);
    $('mf-start').hidden = !next.bound || next.running;
    $('mf-restart').hidden = !next.bound || !next.running;
    $('mf-stop').hidden = !next.bound || !next.running;
  }
  async function act(action, data = {}) {
    if (busy || !state) return; pending(true);
    try { const result = await request(action,data); render(result.state); message(result.message); }
    catch (e) { message(e.name === 'AbortError' ? '请求超时。操作可能已经完成，请刷新状态确认后再试。' : e.message,true); }
    finally { pending(false); }
  }
  async function refresh() {
    if (busy || (dirty && !confirm('刷新将丢弃尚未保存的应用修改，是否继续？'))) return;
    pending(true);
    try { const result = await request(); render(result.state); message('设置已读取。'); }
    catch (e) { message(e.message,true); } finally { pending(false); }
  }
  $('mf-refresh').addEventListener('click',refresh);
  $('mf-public-save').addEventListener('click',()=>act('save_web_access',{browser_public_direct:$('mf-public-direct').checked}));
  $('mf-add').addEventListener('click',() => { if ($('mf-targets').children.length >= 64) return message('最多配置 64 个应用。',true); targetRow({}); dirty = true; });
  $('mf-save').addEventListener('click',() => {
    const targets = [];
    for (const row of $('mf-targets').children) {
      const target = {key:row.dataset.key};
      for (const input of row.querySelectorAll('input')) { if (!input.reportValidity()) return; target[input.name] = input.value.trim(); }
      targets.push(target);
    }
    act('save_targets',{targets});
  });
  for (const action of ['start','restart','stop']) $('mf-'+action).addEventListener('click',() => {
    if (dirty) return message('请先保存应用修改。',true);
    if (action === 'stop' && !confirm('停止后所有中继应用将暂时不可用，重启 NAS 仍保持停止，直到再次启动。确定停止？')) return;
    act(action);
  });
  window.addEventListener('beforeunload',event => { if (dirty) { event.preventDefault(); event.returnValue = ''; } });
  refresh();
})();
