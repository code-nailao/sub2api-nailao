import http from 'node:http'
import { readFile, writeFile, mkdir } from 'node:fs/promises'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createServer } from '../../frontend/node_modules/vite/dist/node/index.js'
import { categoryNames, createFixtureGroups, defaultConfig, fixtureUser, makeV1, makeV2, publicSettings } from './fixtures.mjs'

const root = dirname(fileURLToPath(import.meta.url))
const repo = resolve(root, '../..')
const frontend = resolve(repo, 'frontend')
const statePath = resolve(repo, '.dev/channel-monitor-local/state.json')
const metadataPath = resolve(repo, '.dev/channel-monitor-design/source-snapshot.json')
const apiPort = Number(process.env.MONITOR_FIXTURE_API_PORT || 4181)
const uiPort = Number(process.env.MONITOR_FIXTURE_UI_PORT || 4182)
let metadata = null
try { metadata = JSON.parse(await readFile(metadataPath, 'utf8')) } catch { /* Use portable synthetic names. */ }
const groups = createFixtureGroups(metadata)
let state = { mode: 'v2', config: defaultConfig(groups) }
try {
  const saved = JSON.parse(await readFile(statePath, 'utf8'))
  if (['v1', 'v2'].includes(saved.mode) && saved.config?.version) state = saved
} catch { /* Fresh local run. */ }
const persist = async () => { await mkdir(dirname(statePath), { recursive: true }); await writeFile(statePath, JSON.stringify(state, null, 2), { mode: 0o600 }) }
// Import new fixture metadata without discarding locally reviewed arrangements.
const currentDefaults = defaultConfig(groups)
const previousConfig = JSON.stringify(state.config)
const existingOrder = state.config.display_order || { categories: [], groups: {}, group_categories: {} }
state.config.group_ids = [...new Set([...state.config.group_ids, ...groups.map(g => g.id)])]
state.config.platforms = currentDefaults.platforms
state.config.display_order = {
  categories: [...new Set([...existingOrder.categories, ...categoryNames])],
  image_grouping: existingOrder.image_grouping ?? 'separate',
  groups: existingOrder.groups,
  group_categories: { ...currentDefaults.display_order.group_categories, ...existingOrder.group_categories },
}
if (JSON.stringify(state.config) !== previousConfig) { state.config.version += 1; await persist() }
const json = (res, data, status = 200) => { res.writeHead(status, { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' }); res.end(JSON.stringify({ code: status === 200 ? 0 : status, message: status === 200 ? 'success' : data.message, data: status === 200 ? data : undefined })) }
async function body(req) {
  let result = ''
  for await (const chunk of req) { result += chunk; if (result.length > 1024 * 1024) throw new Error('body too large') }
  return result ? JSON.parse(result) : {}
}
function normalizeOrder(value) {
  const order = { categories: [], groups: {}, group_categories: {} }
  if (!value || typeof value !== 'object') throw new Error('display_order must be an object')
  if (value.image_grouping !== undefined) {
    if (!['separate', 'provider'].includes(value.image_grouping)) throw new Error('invalid image grouping')
    order.image_grouping = value.image_grouping
  }
  for (const category of value.categories || []) { if (!categoryNames.includes(category)) throw new Error('invalid category'); if (!order.categories.includes(category)) order.categories.push(category) }
  for (const [category, ids] of Object.entries(value.groups || {})) {
    if (!categoryNames.includes(category) || !Array.isArray(ids) || ids.some(id => !Number.isSafeInteger(id) || id <= 0)) throw new Error('invalid display groups')
    order.groups[category] = [...new Set(ids)]
  }
  for (const [id, category] of Object.entries(value.group_categories || {})) {
    if (!/^[1-9]\d*$/.test(id) || !categoryNames.includes(category)) throw new Error('invalid category override')
    order.group_categories[id] = category
  }
  return order
}

const api = http.createServer(async (req, res) => {
  const url = new URL(req.url || '/', `http://127.0.0.1:${apiPort}`)
  const path = url.pathname
  const role = req.headers.authorization === 'Bearer local-fixture-user' ? 'user' : req.headers.authorization === 'Bearer local-fixture-admin' ? 'admin' : null
  const admin = role === 'admin'
  try {
    if (req.headers.origin && ![`http://127.0.0.1:${uiPort}`, `http://localhost:${uiPort}`].includes(req.headers.origin)) return json(res, { message: 'Cross-origin writes are disabled in local fixtures' }, 403)
    if (path === '/__fixture/state' && req.method === 'GET') return json(res, { mode: state.mode, groups: groups.length, metadata: Boolean(metadata), config_version: state.config.version })
    if (path === '/__fixture/state' && req.method === 'POST') {
      const input = await body(req)
      if (!['v1', 'v2'].includes(input.mode)) return json(res, { message: 'mode must be v1 or v2' }, 400)
      state.mode = input.mode; await persist(); return json(res, { mode: state.mode })
    }
    if (path === '/health') return json(res, { status: 'ok', fixture: true })
    if (path === '/setup/status') return json(res, { needs_setup: false, step: 'complete' })
    if (path === '/api/v1/settings/public') return json(res, publicSettings(state.mode))
    if (path === '/api/v1/auth/login' && req.method === 'POST') {
      const input = await body(req), selected = input.email?.startsWith('user') ? 'user' : 'admin'
      return json(res, { access_token: `local-fixture-${selected}`, token_type: 'Bearer', expires_in: 86400, user: fixtureUser(selected) })
    }
    if (!role) return json(res, { message: 'Local fixture identity required' }, 401)
    if (path.startsWith('/api/v1/admin/') && !admin) return json(res, { message: 'Administrator role required' }, 403)
    if (path === '/api/v1/auth/me' || path === '/api/v1/user/profile') return json(res, fixtureUser(role))
    if (path === '/api/v1/announcements') return json(res, [])
    if (path === '/api/v1/admin/compliance') return json(res, { required: false, version: 'local-fixture', document_path_zh: '', document_path_en: '', document_url_zh: '', document_url_en: '', ack_phrase_zh: '', ack_phrase_en: '' })
    if (path === '/api/v1/admin/system/check-updates') return json(res, { current_version: 'local-preview', latest_version: 'local-preview', has_update: false, cached: true, build_type: 'source' })
    if (path === '/api/v1/admin/system/version') return json(res, { version: 'local-preview' })
    if (path === '/api/v1/admin/settings' && req.method === 'GET') return json(res, { ...publicSettings(state.mode), ops_monitoring_enabled: false, ops_realtime_monitoring_enabled: false, ops_query_mode_default: 'auto', custom_menu_items: [], custom_endpoints: [] })
    if (path === '/api/v1/admin/payment/config' && req.method === 'GET') return json(res, { enabled: false, min_amount: 1, max_amount: 100, daily_limit: 0, order_timeout_minutes: 10, max_pending_orders: 1, enabled_payment_types: [], balance_disabled: true, balance_recharge_multiplier: 1, subscription_usd_to_cny_rate: 1, recharge_fee_rate: 0, load_balance_strategy: 'round_robin', product_name_prefix: '', product_name_suffix: '', help_image_url: '', help_text: '本地演示环境未启用支付' })
    if (path === '/api/v1/subscriptions/active') return json(res, [])
    if (path === '/api/v1/groups/available') return json(res, admin ? groups : groups.filter(g => !g.is_exclusive))
    if (path === '/api/v1/groups/rates') return json(res, { 91001: 0, 91002: .038 })
    if (path === '/api/v1/admin/groups/all') return json(res, groups)
    if (path === '/api/v1/admin/groups') return json(res, { items: groups, total: groups.length, page: 1, page_size: 100, pages: 1 })
    if (path === '/api/v1/channel-monitors' && req.method === 'GET') return json(res, { items: makeV1(groups, admin) })
    const detail = path.match(/^\/api\/v1\/channel-monitors\/(\d+)\/status$/)
    if (detail && req.method === 'GET') {
      const item = makeV1(groups, admin).find(g => g.id === Number(detail[1]))
      return item ? json(res, { id: item.id, name: item.name, provider: item.provider, group_name: item.group_name, models: [{ model: item.primary_model, latest_status: item.primary_status, latest_latency_ms: item.primary_latency_ms, availability_7d: item.availability_7d, availability_15d: Math.max(0, item.availability_7d - .2), availability_30d: Math.max(0, item.availability_7d - .5), avg_latency_7d_ms: item.primary_latency_ms }] }) : json(res, { message: 'Unknown fixture monitor' }, 404)
    }
    const monitor = path.match(/^\/api\/v1\/(admin\/)?channel-monitor-v2\/(config|snapshot|matrix|dimensions|models|errors|users)$/)
    if (monitor) {
      if (state.mode !== 'v2') return json(res, { message: 'channel monitor mode does not allow this operation' }, 403)
      const action = monitor[2]
      if (action === 'config') {
        if (!admin) return json(res, { message: 'Administrator role required' }, 403)
        if (req.method === 'GET') return json(res, state.config)
        if (req.method !== 'PUT') return json(res, { message: 'Method not allowed' }, 405)
        const input = await body(req)
        if (input.version !== state.config.version) return json(res, { message: 'channel monitor v2 config version conflict' }, 409)
        const displayOrder = input.display_order == null ? state.config.display_order : normalizeOrder(input.display_order)
        state.config = { ...state.config, ...input, display_order: displayOrder, version: state.config.version + 1 }
        await persist(); return json(res, state.config)
      }
      if (req.method !== 'GET') return json(res, { message: 'Method not allowed' }, 405)
      const data = makeV2(groups, state.config, url.searchParams, admin)
      if (action === 'snapshot') return json(res, data.snapshot)
      if (action === 'dimensions') return json(res, data.dimensions)
      if (action === 'matrix') return json(res, { coverage: data.coverage, group_by: data.group_by, items: data.items })
      if (action === 'models') return json(res, { coverage: data.coverage, items: data.items.map(({ platform, model, metrics, health }) => ({ platform, model, metrics, health })) })
      if (action === 'errors') return json(res, { coverage: data.coverage, items: [{ category: 'timeout', count: admin ? 38 : 0, rate: .012, ignored: false }, { category: 'client_cancelled', count: admin ? 17 : 0, rate: .004, ignored: true }] })
      if (action === 'users') return json(res, { coverage: data.coverage, items: [] })
    }
    return json(res, { message: `Local preview does not implement ${req.method} ${path}; no upstream request was made` }, 404)
  } catch (error) { return json(res, { message: error.message || 'Invalid fixture request' }, 400) }
})
await new Promise((resolveReady, reject) => { api.once('error', reject); api.listen(apiPort, '127.0.0.1', resolveReady) })

const boot = `(function(){const q=new URLSearchParams(location.search);const role=q.get('preview_role')||localStorage.getItem('monitor-preview-role')||'admin';const allowed=role==='user'?'user':'admin';localStorage.setItem('monitor-preview-role',allowed);localStorage.setItem('auth_token','local-fixture-'+allowed);localStorage.setItem('auth_user',JSON.stringify(allowed==='admin'?${JSON.stringify(fixtureUser('admin'))}:${JSON.stringify(fixtureUser('user'))}));localStorage.removeItem('refresh_token');localStorage.setItem('token_expires_at',String(Date.now()+86400000));localStorage.setItem('locale','zh');})();`
const runtimeScript = await readFile(resolve(root, 'runtime.js'), 'utf8')
process.env.VITE_DEV_PROXY_TARGET = `http://127.0.0.1:${apiPort}`
process.env.VITE_DEV_PORT = String(uiPort)
process.chdir(frontend)
let vite
try {
  vite = await createServer({
    configFile: resolve(frontend, 'vite.config.ts'), root: frontend,
    server: { host: '127.0.0.1', port: uiPort, strictPort: true,
      proxy: { '/__fixture': { target: `http://127.0.0.1:${apiPort}` } },
      headers: { 'Content-Security-Policy': "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self' ws://127.0.0.1:* ws://localhost:*; frame-src 'none'; object-src 'none'; base-uri 'self'" },
    },
    plugins: [{ name: 'sources-local-fixture-banner', apply: 'serve', transformIndexHtml: { order: 'post', handler(html) { return html.replace('</head>', `<script>${boot}</script></head>`).replace('</body>', '<script type="module" src="/__local-preview-runtime.js"></script></body>') } } },
      { name: 'sources-local-fixture-script', configureServer(server) { server.middlewares.use('/__local-preview-runtime.js', (_req, res) => { res.setHeader('Content-Type', 'text/javascript'); res.end(runtimeScript) }) } }],
  })
  await vite.listen()
} catch (error) { api.close(); throw error }
console.log(`Local synthetic API: http://127.0.0.1:${apiPort}`)
console.log(`Actual Sub frontend: http://127.0.0.1:${uiPort}/monitor`)
console.log(`${groups.length} fixture groups; ${metadata ? 'local display metadata loaded' : 'portable sample names'}; all performance values are synthetic.`)
const close = async () => { await vite.close(); api.close(); process.exit(0) }
process.once('SIGINT', close); process.once('SIGTERM', close)
