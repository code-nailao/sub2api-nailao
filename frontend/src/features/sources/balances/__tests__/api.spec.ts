import { beforeEach, describe, expect, it, vi } from 'vitest'
import { balanceAPI } from '../api'

const client = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), delete: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: client }))
describe('same-origin balance transport', () => {
  beforeEach(() => Object.values(client).forEach(mock => mock.mockReset()))
  it('uses the shared auth client outside /api/v1 and keeps cancellation', async () => {
    client.get.mockResolvedValue({ data: { accounts: [] } })
    const controller = new AbortController()
    await balanceAPI.summaries([1, 2], controller.signal)
    expect(client.get).toHaveBeenCalledWith('/api/balance/accounts', { baseURL: '', params: { ids: '1,2' }, signal: controller.signal })
  })
  it('sends only the binding revision when unlinking', async () => {
    client.delete.mockResolvedValue({ data: { account_id: 5, status: 'unbound' } })
    await balanceAPI.unbind(5, 9)
    expect(client.delete).toHaveBeenCalledWith('/api/balance/accounts/5/binding', { baseURL: '', data: { binding_revision: 9 } })
  })
})

describe('automatic wallet transport', () => {
  it('uses account ID only and keeps a real zero wallet', async () => {
    client.get.mockResolvedValue({ data: { status: 'ready', remaining: '0', unit: 'USD', checked_at: '2026-10-15T00:00:00Z', stale: false } })
    const result = await balanceAPI.automatic(7, true)
    expect(client.get).toHaveBeenLastCalledWith('/admin/accounts/balance-auto', { params: { id: 7, refresh: '1', request_id: expect.any(String) }, signal: undefined, timeout: 20000 })
    expect(result.automatic).toBe(true)
    expect(result.balance?.remaining).toBe('0')
    expect(result.source_id).toBeUndefined()
  })
  it('does not invent a wallet from unsupported quota data', async () => {
    client.get.mockResolvedValue({ data: { status: 'wallet_unavailable', stale: false } })
    expect((await balanceAPI.automatic(7)).balance).toBeNull()
  })
})


describe('six-minute wallet polling', () => {
  beforeEach(() => { Object.values(client).forEach(mock => mock.mockReset()); vi.useFakeTimers() })
  it('reuses one operation ID and waits with short HTTP requests', async () => {
    client.get.mockResolvedValueOnce({ data: { status: 'pending', querying: true } })
      .mockResolvedValueOnce({ data: { status: 'ready', remaining: '12', unit: 'USD', checked_at: '2026-10-25T00:00:00Z', stale: false } })
    const operation = balanceAPI.automatic(7, true)
    await vi.advanceTimersByTimeAsync(2000)
    const result = await operation
    expect(result.balance?.remaining).toBe('12')
    expect(client.get).toHaveBeenCalledTimes(2)
    expect(client.get.mock.calls[0][1].params.request_id).toBe(client.get.mock.calls[1][1].params.request_id)
    expect(client.get.mock.calls[1][1].timeout).toBe(20000)
    vi.useRealTimers()
  })
  it('cancels polling when the drawer closes', async () => {
    client.get.mockResolvedValue({ data: { status: 'pending', querying: true } })
    const controller = new AbortController()
    const operation = balanceAPI.automatic(7, true, controller.signal)
    const rejected = expect(operation).rejects.toMatchObject({ name: 'AbortError' })
    await vi.advanceTimersByTimeAsync(0)
    controller.abort()
    await rejected
    expect(client.get).toHaveBeenCalledTimes(1)
    expect(vi.getTimerCount()).toBe(0)
    vi.useRealTimers()
  })
  it('polls custom URL verification without losing its proof', async () => {
    client.post.mockResolvedValueOnce({ data: { url: 'https://wallet.example', wallet: { querying: true } } })
      .mockResolvedValueOnce({ data: { url: 'https://wallet.example', wallet: { status: 'ready' }, verification_token: 'proof' } })
    const operation = balanceAPI.testWallet(7, 'https://wallet.example', 'revision')
    await vi.advanceTimersByTimeAsync(2000)
    expect((await operation).verification_token).toBe('proof')
    expect(client.post.mock.calls[0][2].params.request_id).toBe(client.post.mock.calls[1][2].params.request_id)
    vi.useRealTimers()
  })
})
