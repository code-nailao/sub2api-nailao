const style = document.createElement('style')
style.textContent = `#sources-local-controls{position:fixed;z-index:999999;bottom:10px;left:50%;transform:translateX(-50%);display:flex;align-items:center;gap:12px;background:#17202e;color:#f0d69a;border:1px solid #6e5d32;box-shadow:0 8px 30px #0006;border-radius:12px;padding:10px 14px;font:12px/1.4 system-ui;max-width:calc(100vw - 24px)}#sources-local-controls strong{white-space:nowrap}#sources-local-controls small{color:#b7c2d4}#sources-local-controls label{display:flex;align-items:center;gap:5px;white-space:nowrap}#sources-local-controls select,#sources-local-controls button{font:inherit;color:#e9edf5;background:#263449;border:1px solid #4b5b72;padding:5px 8px;border-radius:6px}#sources-local-controls button{cursor:pointer}#sources-local-controls select:focus-visible,#sources-local-controls button:focus-visible{outline:2px solid #f4c568;outline-offset:2px}body{padding-bottom:70px!important}@media(max-width:760px){#sources-local-controls{gap:7px;padding:9px 10px;flex-wrap:wrap;justify-content:center;width:max-content}#sources-local-controls small{display:none}body{padding-bottom:100px!important}}`
document.head.append(style)
const bar = document.createElement('aside')
bar.id = 'sources-local-controls'
bar.setAttribute('aria-label', '本地演示控制台')
bar.innerHTML = '<div><strong>本地演示数据</strong><br><small>实际 Sub 前端 · 合成指标 · 排序仅存本机</small></div><label>版本 <select aria-label="本地监控版本"><option value="v2">V2 卡片</option><option value="v1">V1 探测</option></select></label><label>身份 <select aria-label="本地预览身份"><option value="admin">管理员</option><option value="user">普通用户</option></select></label><button type="button">应用切换</button>'
document.body.append(bar)
const mode = bar.querySelector('[aria-label="本地监控版本"]')
const role = bar.querySelector('[aria-label="本地预览身份"]')
role.value = localStorage.getItem('monitor-preview-role') === 'user' ? 'user' : 'admin'
try { const response = await fetch('/__fixture/state'); mode.value = (await response.json()).data.mode } catch { mode.value = 'v2' }
bar.querySelector('button').addEventListener('click', async event => {
  event.currentTarget.disabled = true
  try {
    const response = await fetch('/__fixture/state', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ mode: mode.value }) })
    if (!response.ok) throw new Error('本地模式切换失败')
    localStorage.setItem('monitor-preview-role', role.value)
    location.assign('/monitor?preview_role=' + role.value)
  } catch (error) { event.currentTarget.disabled = false; alert(error.message) }
})
