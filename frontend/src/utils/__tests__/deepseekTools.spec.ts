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
  it('leaves the standalone tools unoffered and their supplier costs optional', () => {
    const tools = defaultDeepSeekTools()
    expect([tools.ocr_price_per_1k_pages, tools.asr_price_per_hour, tools.tts_price_per_10k_chars]).toEqual([null, null, null])
    const form = deepSeekToolAccountForm({ deepseek_tools_enabled: true, deepseek_asr_cost_per_hour: 0, deepseek_tts_cost_per_10k_chars: '1' })
    expect(form.deepseek_asr_cost_per_hour).toBe(0)
    expect(form.deepseek_tts_cost_per_10k_chars).toBe(null)
    expect(form.deepseek_ocr_cost_per_1k_pages).toBe(null)
  })
})
