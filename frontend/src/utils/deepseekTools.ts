import type { DeepSeekToolsConfig } from '@/types'

export const defaultDeepSeekTools = (): DeepSeekToolsConfig => ({
  enabled: false, auto_inject: true, account_id: null, fetch_price_per_1k: 6,
  ocr_price_per_1k_pages: null, asr_price_per_hour: null, tts_price_per_10k_chars: null
})
/** The standalone document and speech tools, priced in their own units. */
export const deepSeekMediaTools = [
  { tool: 'ocr', price: 'ocr_price_per_1k_pages', units: 'ocr_pages', per: 1000, cost: 'deepseek_ocr_cost_per_1k_pages' },
  { tool: 'asr', price: 'asr_price_per_hour', units: 'asr_seconds', per: 3600, cost: 'deepseek_asr_cost_per_hour' },
  { tool: 'tts', price: 'tts_price_per_10k_chars', units: 'tts_characters', per: 10000, cost: 'deepseek_tts_cost_per_10k_chars' }
] as const
const optionalPrice = (value: unknown): number | null => typeof value === 'number' ? value : null
export type DeepSeekToolMode = 'inherit' | 'enabled' | 'disabled'
export interface DeepSeekToolAccountForm {
  deepseek_tools_enabled: boolean
  deepseek_tools_base_url: string
  deepseek_search_cost_per_call: number | null
  deepseek_fetch_cost_per_call: number | null
  deepseek_ocr_cost_per_1k_pages: number | null
  deepseek_asr_cost_per_hour: number | null
  deepseek_tts_cost_per_10k_chars: number | null
}
export const deepSeekToolAccountForm = (extra: Record<string, unknown> = {}): DeepSeekToolAccountForm => ({
  deepseek_tools_enabled: extra.deepseek_tools_enabled === true,
  deepseek_tools_base_url: typeof extra.deepseek_tools_base_url === 'string' ? extra.deepseek_tools_base_url : '',
  deepseek_search_cost_per_call: optionalPrice(extra.deepseek_search_cost_per_call),
  deepseek_fetch_cost_per_call: optionalPrice(extra.deepseek_fetch_cost_per_call),
  deepseek_ocr_cost_per_1k_pages: optionalPrice(extra.deepseek_ocr_cost_per_1k_pages),
  deepseek_asr_cost_per_hour: optionalPrice(extra.deepseek_asr_cost_per_hour),
  deepseek_tts_cost_per_10k_chars: optionalPrice(extra.deepseek_tts_cost_per_10k_chars)
})
export function deepSeekToolsURL(base: string): string {
  try {
    const u = new URL(base.trim())
    if (!['http:', 'https:'].includes(u.protocol) || u.username || u.password || u.search || u.hash) return ''
    let path = u.pathname.replace(/\/+$/, '')
    if (!path.endsWith('/tools')) path += /\/v\d+(?:[a-z0-9.-]*)$/i.test(path) ? '/tools' : '/v1/tools'
    u.pathname = path
    return u.toString().replace(/\/$/, '')
  } catch { return '' }
}
