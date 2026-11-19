import { afterEach, describe, expect, it, vi } from 'vitest'
import { apiClient } from '../client'
import { runNow } from '../admin/channelMonitor'

afterEach(() => vi.restoreAllMocks())

describe('manual channel monitor timing', () => {
  it('allows the whole server probe budget without changing the shared API timeout', async () => {
    const originalTimeout = apiClient.defaults.timeout
    const post = vi.spyOn(apiClient, 'post').mockResolvedValue({ data: { results: [] } })
    await runNow(7)
    expect(post).toHaveBeenCalledWith('/admin/channel-monitors/7/run', undefined, { timeout: 375_000 })
    expect(apiClient.defaults.timeout).toBe(originalTimeout)
  })
})
