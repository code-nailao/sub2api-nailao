import { beforeEach, describe, expect, it, vi } from 'vitest'
import { listPendingCharges } from '../pendingCharges'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { get } }))

beforeEach(() => get.mockReset())

describe('authenticated user pending charges API', () => {
  it('uses the user-only read endpoint and never forwards user/admin filters', async () => {
    const page = { items: [], next_cursor: null, page_size: 20 }
    get.mockResolvedValueOnce({ data: page })
    const params = { user_id: 999, account_id: 12, admin: true, page_size: 20 }
    expect(await listPendingCharges(params)).toBe(page)
    expect(get).toHaveBeenCalledOnce()
    expect(get).toHaveBeenCalledWith('/usage/pending-charges', {
      signal: undefined,
      params: { page_size: 20 },
    })
  })

  it('defaults to 20 and passes an opaque cursor and cancellation signal unchanged', async () => {
    get.mockResolvedValue({ data: { items: [], next_cursor: null, page_size: 20 } })
    await listPendingCharges()
    expect(get.mock.calls[0][1].params).toEqual({ page_size: 20 })

    const controller = new AbortController()
    const cursor = 'opaque+/token==&not_a_filter'
    await listPendingCharges({ cursor, page_size: 50 }, { signal: controller.signal })
    expect(get).toHaveBeenLastCalledWith('/usage/pending-charges', {
      signal: controller.signal,
      params: { page_size: 50, cursor },
    })
  })

  it.each([0, -1, 51, 1.5, NaN, Infinity])('rejects invalid page size %s before a request', async page_size => {
    await expect(listPendingCharges({ page_size })).rejects.toBeInstanceOf(RangeError)
    expect(get).not.toHaveBeenCalled()
  })

  it.each([401, 403, 404, 500])('preserves status %s for the caller instead of fabricating an empty list', async status => {
    const failure = { status, message: 'Unavailable' }
    get.mockRejectedValueOnce(failure)
    await expect(listPendingCharges()).rejects.toBe(failure)
    expect(get).toHaveBeenCalledOnce()
  })
})
