export type State = 'healthy' | 'warning' | 'critical' | 'unknown'
export type Brand = 'openai' | 'anthropic' | 'grok' | 'gemini' | 'domestic' | 'other'
export type Range = '90m' | '24h' | '7d' | '30d'
export type Provider = 'openai' | 'anthropic' | 'grok' | 'gemini' | 'deepseek' | 'kimi' | 'zhipu'
export interface Group { id: number; name: string; platform: Provider; rate_multiplier: number }
export interface Monitor {
  id: number; name: string; provider: Provider; group_id: number | null;
  availability_7d: number; primary_latency_ms: number | null; primary_status: string;
  timeline: [number, string][];
}
export interface Snapshot {
  captured_at: string; source: string; monitor_mode: 'v1'; v2_status: number;
  groups: Group[]; monitors: Monitor[];
}
export interface Point { at: number; state: State; availability: number | null; cache: number | null; ttft: number | null }
export interface Card {
  id: string; title: string; subtitle: string; fullName: string; brand: Brand; provider: Provider;
  multiplier: number | null; state: State; availability: number | null; cache: number | null;
  ttft: number | null; latency: number | null; points: Point[]; source: 'demo' | 'snapshot';
}
export const labels: Record<State, string> = { healthy: '健康', warning: '需关注', critical: '异常', unknown: '样本不足' }
export const brands: { id: Brand; label: string; caption: string; provider: Provider }[] = [
  { id: 'openai', label: 'OpenAI', caption: 'GPT · Codex', provider: 'openai' },
  { id: 'anthropic', label: 'Anthropic', caption: 'Claude · Kiro', provider: 'anthropic' },
  { id: 'grok', label: 'Grok', caption: 'xAI', provider: 'grok' },
  { id: 'gemini', label: 'Gemini', caption: 'Google', provider: 'gemini' },
  { id: 'domestic', label: '国模', caption: 'DeepSeek · Kimi · GLM', provider: 'deepseek' },
  { id: 'other', label: '其他', caption: '生图 · 其他服务', provider: 'openai' },
]
export const providerLabels: Record<Provider, string> = { openai: 'OpenAI', anthropic: 'Anthropic', grok: 'Grok', gemini: 'Gemini', deepseek: 'DeepSeek', kimi: 'Kimi', zhipu: 'GLM' }
const fallbackGroups: Group[] = [
  { id: 1, name: 'Codex Plus', platform: 'openai', rate_multiplier: .065 },
  { id: 2, name: 'Codex Pro', platform: 'openai', rate_multiplier: .1 },
  { id: 3, name: 'Codex Pro Max', platform: 'openai', rate_multiplier: .15 },
  { id: 4, name: 'Kiro 稳定', platform: 'anthropic', rate_multiplier: .08 },
  { id: 5, name: 'Claude Max', platform: 'anthropic', rate_multiplier: .9 },
  { id: 6, name: 'Grok Ultra', platform: 'grok', rate_multiplier: .12 },
  { id: 7, name: 'Gemini Ultra', platform: 'gemini', rate_multiplier: .35 },
  { id: 8, name: 'DeepSeek', platform: 'deepseek', rate_multiplier: .3 },
  { id: 9, name: 'Kimi', platform: 'kimi', rate_multiplier: .12 },
  { id: 10, name: 'GLM', platform: 'zhipu', rate_multiplier: .15 },
]

export function brandOf(platform: Provider, name: string): Brand {
  if (['deepseek', 'kimi', 'zhipu'].includes(platform) || /国模/.test(name)) return 'domestic'
  return platform as Brand
}
function titleOf(name: string) {
  const short = name.replace(/【[^】]*】/g, '').replace(/[（(][^）)]*[）)]/g, '').replace(/「[^」]*」/g, '').trim()
  return short.length < 4 ? name.replace(/[【】]/g, ' ').trim() : short
}
export const percent = (value: number | null) => value == null ? '—' : `${value.toFixed(1)}%`
export const seconds = (value: number | null) => value == null ? '—' : `${(value / 1000).toFixed(1)}s`
export const rate = (value: number | null) => value == null ? '—' : `${Number(value.toFixed(3))}×`
export function time(value: number | string, date = false) {
  return new Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', ...(date ? { month: '2-digit', day: '2-digit' } : {}), hour: '2-digit', minute: '2-digit', hour12: false }).format(new Date(value))
}
export function v1State(status: string): State {
  return status === 'operational' ? 'healthy' : status === 'degraded' ? 'warning' : ['failed', 'error'].includes(status) ? 'critical' : 'unknown'
}

// Explicit fixture: health metrics and timelines below are synthetic, never production claims.
export function demoCards(snapshot: Snapshot | null, range: Range): Card[] {
  const groups = snapshot?.groups.length ? snapshot.groups : fallbackGroups
  const span = { '90m': 90 * 60_000, '24h': 86_400_000, '7d': 7 * 86_400_000, '30d': 30 * 86_400_000 }[range]
  const count = { '90m': 18, '24h': 24, '7d': 28, '30d': 30 }[range]
  const end = Date.parse(snapshot?.captured_at || '2026-11-16T16:25:00Z')
  return groups.map((g, index) => {
    const brand = brandOf(g.platform, g.name)
    const sampleMissing = /官 key/i.test(g.name) && g.platform === 'gemini'
    const state: State = sampleMissing ? 'unknown' : /Luna/.test(g.name) ? 'critical' : /福利|反重力/.test(g.name) ? 'warning' : 'healthy'
    const availability = sampleMissing ? null : state === 'critical' ? 62.8 : state === 'warning' ? 94.6 : [99.3, 99.8, 99.7, 99.9, 99.5, 99.6][index % 6]
    const cache = sampleMissing || /生图|\bimage\b/i.test(g.name) ? null : brand === 'gemini' ? 32.6 : [82.4, 91.2, 93.6, 96.8, 97.1, 84.5, 88.2, 76.4][index % 8]
    const ttft = sampleMissing || /生图|\bimage\b/i.test(g.name) ? null : state === 'critical' ? 24800 : state === 'warning' ? 10600 : [2300, 1800, 2100, 1600, 1200, 3100][index % 6]
    const points: Point[] = Array.from({ length: count }, (_, i) => {
      const pointState: State = sampleMissing ? 'unknown' : state === 'critical' && i >= count - 5 ? 'critical' : state === 'warning' && i % 5 >= 3 ? 'warning' : index % 4 === 0 && i === 7 ? 'warning' : 'healthy'
      return { at: end - (count - 1 - i) * span / count, state: pointState, availability: sampleMissing ? null : pointState === 'critical' ? 62.8 : pointState === 'warning' ? 93.2 : 99.7, cache, ttft: sampleMissing ? null : pointState === 'warning' ? 10600 : ttft }
    })
    return { id: `group-${g.id}`, title: titleOf(g.name), subtitle: g.name.match(/【([^】]*)】/)?.[1] || providerLabels[g.platform], fullName: g.name, brand, provider: brand === 'domestic' && g.platform === 'openai' ? 'deepseek' : g.platform, multiplier: g.rate_multiplier, state, availability, cache, ttft, latency: null, points, source: 'demo' }
  })
}

export function realCards(snapshot: Snapshot | null): Card[] {
  if (!snapshot) return []
  return snapshot.monitors.map(m => {
    const group = snapshot.groups.find(g => g.id === m.group_id)
    const brand = brandOf(m.provider, m.name)
    return {
      id: `monitor-${m.id}`, title: titleOf(m.name), subtitle: '主站 V1 · 主动探测', fullName: m.name,
      brand, provider: brand === 'domestic' && m.provider === 'openai' ? 'deepseek' : m.provider,
      multiplier: group?.rate_multiplier ?? null, state: v1State(m.primary_status), availability: m.availability_7d,
      cache: null, ttft: null, latency: m.primary_latency_ms, source: 'snapshot',
      points: [...m.timeline].sort((a, b) => a[0] - b[0]).map(([at, status]) => ({ at: at * 1000, state: v1State(status), availability: null, cache: null, ttft: null })),
    }
  })
}
