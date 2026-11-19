import { describe, expect, it } from 'vitest'
import { deepSeekToolsURL, defaultDeepSeekTools, deepSeekToolAccountForm } from '../deepseekTools'
describe('DeepSeek tools configuration', () => {
  it('keeps custom prefixes and version paths', () => {
    for (const path of ['', '/v1', '/v1/', '/v1/tools']) expect(deepSeekToolsURL(`https://relay.test${path}`)).toBe('https://relay.test/v1/tools')
    expect(deepSeekToolsURL('https://relay.test/custom/v4')).toBe('https://relay.test/custom/v4/tools')
    expect(deepSeekToolsURL('https://user:key@relay.test')).toBe('')
  })
  it('defaults groups off and keeps explicit zero supplier costs', () => {
    expect(defaultDeepSeekTools().enabled).toBe(false)
    expect(defaultDeepSeekTools().fetch_price_per_1k).toBe(6)
    expect(deepSeekToolAccountForm({ deepseek_search_cost_per_call: 0 }).deepseek_search_cost_per_call).toBe(0)
    expect(deepSeekToolAccountForm().deepseek_search_cost_per_call).toBe(null)
  })
})
