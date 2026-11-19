import type { DeepSeekToolsConfig } from '@/types'

export const defaultDeepSeekTools = (): DeepSeekToolsConfig => ({
  enabled: false, auto_inject: true, account_id: null, fetch_price_per_1k: 6
})
export type DeepSeekToolMode = 'inherit' | 'enabled' | 'disabled'
export interface DeepSeekToolAccountForm {
  deepseek_tools_enabled: boolean
  deepseek_native_tools_enabled: boolean
  deepseek_tools_base_url: string
  deepseek_search_cost_per_call: number | null
  deepseek_fetch_cost_per_call: number | null
}
export const deepSeekToolAccountForm = (extra: Record<string, unknown> = {}): DeepSeekToolAccountForm => ({
  deepseek_tools_enabled: extra.deepseek_tools_enabled === true,
  deepseek_native_tools_enabled: extra.deepseek_native_tools_enabled === true,
  deepseek_tools_base_url: typeof extra.deepseek_tools_base_url === 'string' ? extra.deepseek_tools_base_url : '',
  deepseek_search_cost_per_call: typeof extra.deepseek_search_cost_per_call === 'number' ? extra.deepseek_search_cost_per_call : null,
  deepseek_fetch_cost_per_call: typeof extra.deepseek_fetch_cost_per_call === 'number' ? extra.deepseek_fetch_cost_per_call : null
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
