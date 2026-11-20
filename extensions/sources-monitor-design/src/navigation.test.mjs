// @vitest-environment node
/* global afterEach, expect, it, vi */
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { build } from '../../../frontend/node_modules/vite/dist/node/index.js'

const requireFrontend = createRequire(new URL('../../../frontend/package.json', import.meta.url))
const { JSDOM } = requireFrontend('jsdom')
const configFile = fileURLToPath(new URL('../vite.config.mjs', import.meta.url))

const mainLinks = [
  ['仪表盘', '/dashboard'],
  ['API 密钥', '/keys'],
  ['使用记录', '/usage'],
  ['可用渠道', '/available-channels'],
  ['充值', '/purchase'],
  ['模型广场', '/model-plaza'],
]

let dom

async function buildPreview(origin) {
  vi.stubEnv('VITE_MONITOR_PREVIEW_MAIN_ORIGIN', origin)
  const result = await build({ configFile, logLevel: 'silent', build: { write: false } })
  return result.output.filter(file => file.type === 'chunk').map(chunk => chunk.code)
}

function mountPreview(scripts) {
  dom = new JSDOM('<div id="app"></div>', { url: 'https://local-preview.example/', runScripts: 'outside-only' })
  dom.window.fetch = async () => ({ ok: false })
  for (const script of scripts) dom.window.eval(script)
  return dom.window
}

function findLink(document, label) {
  const link = [...document.querySelectorAll('a')].find(anchor => anchor.textContent.trim() === label)
  expect(link, label).toBeDefined()
  return link
}

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllEnvs()
  dom?.window.close()
  dom = undefined
})

it('keeps every main-site link disabled and inert without a configured origin', async () => {
  const window = mountPreview(await buildPreview(''))
  const { document } = window
  const startURL = window.location.href
  const open = vi.spyOn(window, 'open')
  expect(document.body.textContent).toContain('未配置主站地址')

  for (const [label] of mainLinks) {
    const link = findLink(document, label)
    expect(link.hasAttribute('href')).toBe(false)
    expect(link.hasAttribute('target')).toBe(false)
    expect(link.getAttribute('role')).toBe('link')
    expect(link.getAttribute('aria-disabled')).toBe('true')
    const hint = document.getElementById(link.getAttribute('aria-describedby'))
    expect(hint?.textContent).toContain('未配置主站地址')
    link.click()
  }

  expect(open).not.toHaveBeenCalled()
  expect(window.location.href).toBe(startURL)
})

it('joins the configured origin to the original paths and enables navigation', async () => {
  const origin = 'https://preview-main.example'
  const { document } = mountPreview(await buildPreview(`${origin}/`))
  expect(document.body.textContent).not.toContain('未配置主站地址')

  for (const [label, path] of mainLinks) {
    const link = findLink(document, label)
    expect(link.getAttribute('href')).toBe(`${origin}${path}`)
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.relList.contains('noopener')).toBe(true)
    expect(link.hasAttribute('aria-disabled')).toBe(false)
  }
})

it.each([
  '//preview-main.example',
  'javascript:alert(1)',
  'https://preview-main.example/base',
  'https://preview-main.example/app/..',
])('leaves navigation disabled for an invalid origin (%s)', async value => {
  const { document } = mountPreview(await buildPreview(value))
  expect(findLink(document, '仪表盘').getAttribute('aria-disabled')).toBe('true')
  expect(findLink(document, '模型广场').hasAttribute('href')).toBe(false)
})

it('keeps the username and password of a rejected URL out of the built scripts', async () => {
  const user = 'preview-fixture-user'
  const password = 'preview-fixture-password'
  const scripts = await buildPreview(`https://${user}:${password}@preview-main.example/`)
  expect(scripts.some(script => script.includes(user) || script.includes(password))).toBe(false)
  const { document } = mountPreview(scripts)
  expect(findLink(document, '仪表盘').getAttribute('aria-disabled')).toBe('true')
}, 15_000)
