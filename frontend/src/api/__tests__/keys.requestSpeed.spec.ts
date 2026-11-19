import { beforeEach, describe, expect, it, vi } from 'vitest'
import { create, update } from '../keys'

const { post, put } = vi.hoisted(() => ({ post: vi.fn(), put: vi.fn() }))
vi.mock('../client', () => ({ apiClient: { post, put } }))

describe('native Key request-tier transport', () => {
  beforeEach(() => {
    post.mockReset().mockResolvedValue({ data: { id: 7 } })
    put.mockReset().mockResolvedValue({ data: { id: 7 } })
  })

  it.each(['fast', 'ultrafast'] as const)('sends %s alongside the other create fields in one POST', async tier => {
    await create('Project key', 42, undefined, undefined, undefined, 12, undefined,
      { rate_limit_5h: 3 }, 'inherit', tier)
    expect(post).toHaveBeenCalledOnce()
    expect(post).toHaveBeenCalledWith('/keys', {
      name: 'Project key', group_id: 42, quota: 12, rate_limit_5h: 3,
      deepseek_tool_mode: 'inherit', request_speed_tier: tier,
    })
    expect(put).not.toHaveBeenCalled()
  })

  it('keeps omission distinct from an explicit request to use the new group default', async () => {
    await create('Ordinary key', 43)
    expect(post.mock.calls[0][1]).not.toHaveProperty('request_speed_tier')
    await create('Use current default', 42, undefined, undefined, undefined, undefined,
      undefined, undefined, undefined, null)
    expect(post.mock.calls[1][1]).toMatchObject({ group_id: 42, request_speed_tier: null })
  })

  it('updates group and requested tier together without a second metadata mutation', async () => {
    await update(7, { name: 'Moved key', group_id: 42, request_speed_tier: 'fast' })
    expect(put).toHaveBeenCalledOnce()
    expect(put).toHaveBeenCalledWith('/keys/7', {
      name: 'Moved key', group_id: 42, request_speed_tier: 'fast',
    })
    expect(post).not.toHaveBeenCalled()
  })
})
