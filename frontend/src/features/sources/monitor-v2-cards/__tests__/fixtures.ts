import type { MonitorHealth, MonitorMetric, MonitorMatrixRow } from '@/api/channelMonitorV2'

export function metric(overrides: Partial<MonitorMetric> = {}): MonitorMetric {
  return {
    has_requests: true, has_cache_input: true,
    success_requests: 99, error_requests: 1, request_count: 100, token_count: 1000,
    rpm: 2, tpm: 20, error_rate: .01, cache_rate: .8, cache_rate_numerator: 80,
    cache_rate_denominator: 100,
    ttft: { sample_count: 99, p50_ms: 1200, p95_ms: 2400, avg_ms: 1400 },
    duration: { sample_count: 99, p50_ms: 3000, p95_ms: 7000, avg_ms: 3400 },
    ...overrides,
  }
}
export function health(overrides: Partial<MonitorHealth> = {}): MonitorHealth {
  return { overall: 'healthy', error_rate: 'healthy', ttft: 'healthy', cache: 'healthy', minimum_sample: 50, ...overrides }
}
export function row(id: number, name = `Channel ${id}`, platform = 'openai'): MonitorMatrixRow {
  return { platform, group_id: id, group_name: name, metrics: metric(), health: health(), buckets: [] }
}
