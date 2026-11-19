import type {
  HealthState,
  MonitorCategory,
  MonitorCoverage,
  MonitorDisplayOrder,
  MonitorHealth,
  MonitorMatrixBucket,
  MonitorMatrixRow,
  MonitorMetric,
} from '@/api/channelMonitorV2'

export type DisplayCategory = MonitorCategory
export interface DisplayGroup { id: number; name: string; platform?: string }
export const DEFAULT_CATEGORIES: MonitorCategory[] = ['openai', 'anthropic', 'grok', 'gemini', 'domestic', 'image', 'other']
export const DOMESTIC_PROVIDERS = new Set(['deepseek', 'kimi', 'zhipu', 'glm', 'minimax', 'qwen', 'doubao'])

export function categoryOf(group: DisplayGroup, order?: MonitorDisplayOrder | null): DisplayCategory {
  const configured = order?.group_categories?.[String(group.id)]
  if (configured && DEFAULT_CATEGORIES.includes(configured)) return configured
  if (order?.image_grouping !== 'provider' && isImageGroupName(group.name)) return 'image'
  const platform = (group.platform || '').toLowerCase()
  // An explicit domestic label is a product grouping, even on the OpenAI protocol.
  if (DOMESTIC_PROVIDERS.has(platform) || /【国模】|^国模(?:\s|\d|$)/u.test(group.name)) return 'domestic'
  if (platform === 'anthropic' || platform === 'kiro') return 'anthropic'
  if (platform === 'gemini' || platform === 'antigravity') return 'gemini'
  if (platform === 'openai' || platform === 'grok') return platform
  return 'other'
}

export function isImageGroupName(name: string): boolean {
  return /生图|(?:^|[\s【（(·_-])(?:gpt[\s_-]*)?images?(?=[\s\d.()（）【】·_-]|$)/i.test(name)
}

export function categoryOrder(order?: MonitorDisplayOrder | null): DisplayCategory[] {
  return [...new Set([...(order?.categories || []).filter(c => DEFAULT_CATEGORIES.includes(c)), ...DEFAULT_CATEGORIES])]
}

export function orderedGroups<T extends DisplayGroup>(groups: T[], category: DisplayCategory, order?: MonitorDisplayOrder | null): T[] {
  const ids = order?.groups?.[category] || []
  const rank = new Map(ids.map((id, index) => [id, index]))
  return groups.filter(g => categoryOf(g, order) === category)
    .sort((a, b) => {
      const configured = (rank.get(a.id) ?? Number.MAX_SAFE_INTEGER) - (rank.get(b.id) ?? Number.MAX_SAFE_INTEGER)
      if (configured !== 0) return configured
      // Explicit manual ordering wins. New/unlisted channels keep text before image,
      // with stable order inside each kind and no change to their brand/protocol.
      return Number(isImageGroupName(a.name)) - Number(isImageGroupName(b.name))
    })
}

export function rowGroup(row: MonitorMatrixRow): DisplayGroup {
  return { id: row.group_id || 0, name: row.group_name || `#${row.group_id}`, platform: row.platform }
}

/** Never infer lack of traffic from counters that the public API deliberately redacts. */
export function hasRequests(metrics: MonitorMetric, health: MonitorHealth): boolean {
  if (typeof metrics.has_requests === 'boolean') return metrics.has_requests
  return metrics.request_count > 0 || metrics.rpm > 0 || health.overall !== 'unknown' || health.error_rate !== 'unknown' || metrics.ttft.p50_ms != null
}

export function availability(metrics: MonitorMetric, health: MonitorHealth): number | null {
  return hasRequests(metrics, health) && Number.isFinite(metrics.error_rate)
    ? Math.min(1, Math.max(0, 1 - metrics.error_rate)) : null
}

export function cacheRate(metrics: MonitorMetric, health: MonitorHealth): number | null {
  if (metrics.has_cache_input === false) return null
  const present = metrics.has_cache_input === true || metrics.cache_rate_denominator > 0 || metrics.cache_rate > 0 || (health.cache != null && health.cache !== 'unknown')
  return present && Number.isFinite(metrics.cache_rate) ? Math.min(1, Math.max(0, metrics.cache_rate)) : null
}

export function cardHealth(metrics: MonitorMetric, health: MonitorHealth): HealthState {
  if (!hasRequests(metrics, health)) return 'unknown'
  return health.overall || 'unknown'
}

export function formatMultiplier(value: number | undefined | null): string {
  return value != null && Number.isFinite(value) && value >= 0 ? `${Number(value.toFixed(3))}×` : '—'
}

/** Replace visible entries in place while retaining IDs outside the current viewer/catalog. */
export function replaceVisibleOrder(existing: number[], reordered: number[]): number[] {
  const visible = new Set(reordered)
  const next = [...reordered]
  const merged = existing.map(id => visible.has(id) ? next.shift()! : id)
  return [...new Set([...merged, ...next])]
}

export function cloneDisplayOrder(order?: MonitorDisplayOrder | null): MonitorDisplayOrder {
  return JSON.parse(JSON.stringify(order || {}))
}

/** Keep partial coverage and missing periods gray instead of compressing time around them. */
export function alignedBuckets(buckets: MonitorMatrixBucket[], coverage: MonitorCoverage | null): MonitorMatrixBucket[] {
  const sorted = [...buckets].sort((a, b) => Date.parse(a.bucket_start) - Date.parse(b.bucket_start))
  if (!coverage) return sorted
  const start = Date.parse(coverage.requested_start)
  const end = Date.parse(coverage.requested_end || coverage.data_through)
  const step = Math.max(60, coverage.bucket_seconds) * 1000
  if (![start, end, step].every(Number.isFinite) || end <= start) return sorted
  const byStart = new Map(sorted.map(bucket => [Date.parse(bucket.bucket_start), bucket]))
  const result: MonitorMatrixBucket[] = []
  for (let at = Math.floor(start / step) * step; at < end; at += step) {
    result.push(byStart.get(at) || {
      bucket_start: new Date(at).toISOString(),
      metrics: {
        has_requests: false, has_cache_input: false,
        success_requests: 0, error_requests: 0, request_count: 0, token_count: 0,
        rpm: 0, tpm: 0, error_rate: 0, cache_rate: 0, cache_rate_numerator: 0, cache_rate_denominator: 0,
        ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
        duration: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null },
      },
      health: { overall: 'unknown', error_rate: 'unknown', cache: 'unknown', ttft: 'unknown', minimum_sample: 0 },
    })
  }
  return result
}
