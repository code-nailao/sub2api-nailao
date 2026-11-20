/* global afterEach, expect, it, vi */
import { createApp } from 'vue'
import App from './App.vue'

const mainLinks = [
  ['仪表盘', '/dashboard'],
  ['API 密钥', '/keys'],
  ['使用记录', '/usage'],
  ['可用渠道', '/available-channels'],
  ['充值', '/purchase'],
  ['模型广场', '/model-plaza'],
]

let app
let container

function mountPreview(origin) {
  vi.stubEnv('VITE_MONITOR_PREVIEW_MAIN_ORIGIN', origin)
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false })))
  container = document.createElement('div')
  document.body.append(container)
  app = createApp(App)
  app.mount(container)
  return container
}

function findLink(label) {
  const link = [...container.querySelectorAll('a')].find(anchor => anchor.textContent.trim() === label)
  expect(link, label).toBeDefined()
  return link
}

afterEach(() => {
  app?.unmount()
  container?.remove()
  vi.restoreAllMocks()
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

it('keeps every main-site link disabled and inert without a configured origin', () => {
  const page = mountPreview('')
  const startURL = window.location.href
  const open = vi.spyOn(window, 'open')
  expect(page.textContent).toContain('未配置主站地址')

  for (const [label] of mainLinks) {
    const link = findLink(label)
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

it('joins the configured origin to the original paths and enables navigation', () => {
  const origin = 'https://preview-main.example'
  const page = mountPreview(`${origin}/`)
  expect(page.textContent).not.toContain('未配置主站地址')

  for (const [label, path] of mainLinks) {
    const link = findLink(label)
    expect(link.getAttribute('href')).toBe(`${origin}${path}`)
    expect(link.getAttribute('target')).toBe('_blank')
    expect(link.relList.contains('noopener')).toBe(true)
    expect(link.hasAttribute('aria-disabled')).toBe(false)
  }
})

it.each(['//preview-main.example', 'javascript:alert(1)', 'https://preview-main.example/base'])('leaves navigation disabled for an invalid origin (%s)', value => {
  mountPreview(value)
  expect(findLink('仪表盘').getAttribute('aria-disabled')).toBe('true')
  expect(findLink('模型广场').hasAttribute('href')).toBe(false)
})
