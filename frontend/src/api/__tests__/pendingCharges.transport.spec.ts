import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { AxiosInstance, InternalAxiosRequestConfig } from 'axios'

vi.mock('@/i18n', () => ({ getLocale: () => 'zh' }))

describe('pending charges through the real API request interceptor', () => {
  let client: AxiosInstance
  let listPendingCharges: typeof import('../pendingCharges')['listPendingCharges']
  const page = { items: [], next_cursor: null, page_size: 20 }
  const adapter = vi.fn(async (config: InternalAxiosRequestConfig) => ({
    config, status: 200, statusText: 'OK', headers: {},
    data: { code: 0, message: 'success', data: page },
  }))

  beforeEach(async () => {
    localStorage.clear()
    sessionStorage.clear()
    window.history.replaceState({}, '', '/usage')
    vi.resetModules()
    adapter.mockClear()
    client = (await import('../client')).apiClient
    listPendingCharges = (await import('../pendingCharges')).listPendingCharges
    client.defaults.adapter = adapter
  })
  afterEach(() => { localStorage.clear(); vi.restoreAllMocks() })

  it('sends the standard timezone and auth headers on the user read endpoint', async () => {
    localStorage.setItem('auth_token', 'synthetic-ui-transport-only')
    await expect(listPendingCharges()).resolves.toEqual(page)
    const config = adapter.mock.calls[0][0]
    const uri = new URL(client.getUri(config), 'http://fixture.invalid')
    expect(config.method).toBe('get')
    expect(uri.pathname).toBe('/api/v1/usage/pending-charges')
    expect(uri.searchParams.get('page_size')).toBe('20')
    expect(uri.searchParams.get('timezone')).toBe(Intl.DateTimeFormat().resolvedOptions().timeZone)
    expect(config.headers.get('Authorization')).toBe('Bearer synthetic-ui-transport-only')
    expect(config.headers.get('Accept-Language')).toBe('zh')
    expect([...uri.searchParams.keys()].sort()).toEqual(['page_size', 'timezone'])
  })

  it('keeps the cursor opaque on the wire and never adds identity filters', async () => {
    const cursor = 'opaque+/cursor==&user_id=999'
    const params = { page_size: 50, cursor, user_id: 999, account_id: 2, admin: true }
    await listPendingCharges(params)
    const uri = new URL(client.getUri(adapter.mock.calls[0][0]), 'http://fixture.invalid')
    expect(uri.searchParams.get('cursor')).toBe(cursor)
    expect(uri.searchParams.get('page_size')).toBe('50')
    expect([...uri.searchParams.keys()].sort()).toEqual(['cursor', 'page_size', 'timezone'])
    expect(uri.searchParams.has('user_id')).toBe(false)
    expect(uri.searchParams.has('account_id')).toBe(false)
    expect(uri.searchParams.has('admin')).toBe(false)
  })
})
