import { describe, expect, it, vi } from 'vitest'
import type { list, ListResponse } from '@/api/admin/channelMonitor'
import { loadMonitorOrder } from '../order'
import { makeMonitor } from './fixture'

const response = (items: ListResponse['items'], total = items.length, page = 1): ListResponse => ({ items, total, page, page_size: 100, pages: Math.ceil(total / 100) })

describe('loadMonitorOrder', () => {
  it('loads all pages without table filters and retains saved positions', async () => {
    const monitors = Array.from({ length: 101 }, (_, i) => makeMonitor({ id: i + 1, sort_order: 100 - i }))
    const fetch = vi.fn<typeof list>().mockResolvedValueOnce(response(monitors.slice(0, 100), 101)).mockResolvedValueOnce(response(monitors.slice(100), 101, 2))
    const result = await loadMonitorOrder(fetch)
    expect(fetch.mock.calls).toEqual([[{ page: 1, page_size: 100 }], [{ page: 2, page_size: 100 }]])
    expect(result.map(item => item.id)).toEqual(monitors.map(item => item.id).reverse())
  })

  it('uses IDs as a deterministic tiebreaker', async () => {
    const fetch = vi.fn<typeof list>().mockResolvedValue(response([makeMonitor({ id: 5 }), makeMonitor({ id: 2 })]))
    expect((await loadMonitorOrder(fetch)).map(item => item.id)).toEqual([2, 5])
  })

  it('rejects a list that changes between pages', async () => {
    const fetch = vi.fn<typeof list>().mockResolvedValueOnce(response([makeMonitor()], 2)).mockResolvedValueOnce(response([makeMonitor({ id: 43 })], 3, 2))
    await expect(loadMonitorOrder(fetch)).rejects.toThrow('changed')
  })

  it('rejects missing or duplicate rows instead of saving partial order', async () => {
    const missing = vi.fn<typeof list>().mockResolvedValueOnce(response([makeMonitor()], 2)).mockResolvedValueOnce(response([], 2, 2))
    await expect(loadMonitorOrder(missing)).rejects.toThrow('Incomplete')
    const duplicate = vi.fn<typeof list>().mockResolvedValue(response([makeMonitor(), makeMonitor()]))
    await expect(loadMonitorOrder(duplicate)).rejects.toThrow('changed')
  })
})
