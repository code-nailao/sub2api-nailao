// This module creates synthetic metrics. Production metadata, when provided,
// contributes display names and multipliers only, never real performance claims.
export const categoryNames = ['openai', 'anthropic', 'grok', 'gemini', 'domestic', 'image', 'other']
export const fixtureUser = role => ({
  id: role === 'admin' ? 900001 : 900002,
  username: role === 'admin' ? '本地管理员' : '本地普通用户',
  email: `${role}@preview.invalid`, role, balance: 120, frozen_balance: 0,
  concurrency: 5, status: 'active', allowed_groups: null, run_mode: 'standard',
  balance_notify_enabled: false, balance_notify_threshold: null,
  balance_notify_extra_emails: [], created_at: '2026-11-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z',
})

export function publicSettings(mode) {
  return {
    registration_enabled: false, email_verify_enabled: false,
    force_email_on_third_party_signup: false, registration_email_suffix_whitelist: [],
    promo_code_enabled: false, password_reset_enabled: false, invitation_code_enabled: false,
    login_agreement_enabled: false, turnstile_enabled: false, turnstile_site_key: '',
    site_name: 'Sources · 本地演示', site_logo: '/logo.svg', site_subtitle: '渠道状态开发预览',
    api_base_url: '', contact_info: '', doc_url: '', home_content: '', compact_home_enabled: true,
    hide_ccs_import_button: false, payment_enabled: false, risk_control_enabled: false,
    table_default_page_size: 20, table_page_size_options: [20, 50, 100],
    custom_menu_items: [], custom_endpoints: [], linuxdo_oauth_enabled: false,
    wechat_oauth_enabled: false, oidc_oauth_enabled: false, oidc_oauth_provider_name: '',
    github_oauth_enabled: false, google_oauth_enabled: false, backend_mode_enabled: false,
    version: 'local-monitor-preview', server_timezone: 'Asia/Shanghai', server_utc_offset: '+08:00',
    balance_low_notify_enabled: false, account_quota_notify_enabled: false, balance_low_notify_threshold: 0,
    channel_monitor_enabled: true, channel_monitor_mode: mode,
    channel_monitor_default_interval_seconds: 60, channel_monitor_hide_throughput: true,
    channel_monitor_show_quota: false, channel_monitor_hide_user_ranking: true,
    available_channels_enabled: true, subscription_enabled: false, payment_balance_disabled: true,
    model_plaza_enabled: false, model_plaza_require_auth: true, plugin_management_enabled: false,
    service_quota_enabled: false, affiliate_enabled: false, allow_user_view_error_requests: true,
    mcp_enabled: false, ops_monitoring_enabled: false,
  }
}

const providers = ['openai', 'anthropic', 'grok', 'gemini', 'deepseek', 'kimi', 'zhipu']
const fallbackGroups = Array.from({ length: 28 }, (_, i) => ({
  id: 7001 + i,
  name: `${['Codex', 'Claude', 'Grok', 'Gemini', 'DeepSeek', 'Kimi', 'GLM'][i % 7]} 样例渠道 ${Math.floor(i / 7) + 1}`,
  platform: providers[i % 7], rate_multiplier: [.065, .08, .12, .2, .3, .15, .1][i % 7],
}))
// Keep the portable fallback useful even when no private metadata is present.
fallbackGroups[26] = { id: 7027, name: '样例 · GPT Image 生图', platform: 'openai', rate_multiplier: 1 }
fallbackGroups[27] = { id: 7028, name: '样例 · Gemini Image 生图', platform: 'gemini', rate_multiplier: 1 }
const edgeGroups = [
  [91001, '样例 · 无请求', 'openai', .065, 'no-requests'],
  [91002, '样例 · 无缓存输入', 'anthropic', .08, 'no-cache-input'],
  [91003, '样例 · 缓存命中为零', 'openai', .1, 'zero-cache'],
  [91004, '样例 · 非流式无首 Token', 'grok', .12, 'no-ttft'],
  [91005, '样例 · 样本不足', 'gemini', .2, 'low-sample'],
  [91006, '样例 · Kimi 标准', 'kimi', .12, 'healthy'],
  [91007, '样例 · 长名称展示与多行标题边界验证渠道', 'deepseek', .3, 'warning'],
  [91008, '样例 · 管理员可见渠道', 'zhipu', .15, 'critical'],
].map(([id, name, platform, rate_multiplier, scenario]) => ({ id, name, platform, rate_multiplier, scenario }))

export function createFixtureGroups(metadata) {
  const groups = Array.isArray(metadata?.groups) && metadata.groups.length ? metadata.groups : fallbackGroups
  return [...groups.map(g => ({ id: g.id, name: g.name, platform: g.platform, rate_multiplier: g.rate_multiplier })), ...edgeGroups]
    .map((g, i) => ({ ...g, scenario: g.scenario || (/生图|image/i.test(g.name) ? 'image' : i % 13 === 8 ? 'critical' : i % 9 === 6 ? 'warning' : 'healthy'),
      status: 'active', description: '本地演示数据；指标为合成样例', subscription_type: 'standard',
      is_exclusive: g.id === 91008, peak_rate_enabled: false, peak_rate_multiplier: 1,
      peak_start: '', peak_end: '', sort_order: i, service_tier_policy: 'inherit',
    }))
}

export function defaultConfig(groups) {
  return {
    version: 1, enabled: true, refresh_interval_seconds: 60,
    platforms: providers.map(platform => ({ platform, enabled: true, models: [...new Set(groups.filter(g => g.platform === platform).map(modelForGroup))] })),
    group_ids: groups.map(g => g.id), ignored_error_categories: ['client_cancelled'],
    health_thresholds: { minimum_sample: 5, warning_error_rate: .05, critical_error_rate: .2,
      target_ttft_ms: 2000, warning_ttft_ms: 10000, critical_ttft_ms: 30000,
      warning_cache_rate: .2, critical_cache_rate: .05, error_weight: .6, ttft_weight: .4, cache_weight: 0 },
    display_order: { categories: [...categoryNames], image_grouping: 'separate', groups: {}, group_categories: Object.fromEntries(groups.filter(g => /国模/.test(g.name)).map(g => [g.id, 'domestic'])) },
  }
}
const modelFor = platform => ({ openai: 'gpt-6.1-sol', anthropic: 'claude-sonnet-5-5', grok: 'grok-5', gemini: 'gemini-3.5-pro', deepseek: 'deepseek-v4-pro', kimi: 'kimi-k3', zhipu: 'glm-5.1' })[platform] || 'fixture-model'
const modelForGroup = group => /生图|image/i.test(group.name) ? group.platform === 'gemini' ? 'gemini-image' : 'gpt-image-2.6' : modelFor(group.platform)
const iso = time => new Date(time).toISOString()
const latency = (value, count) => ({ sample_count: value == null ? 0 : count, p50_ms: value, p90_ms: value == null ? null : value * 1.5, p95_ms: value == null ? null : value * 1.8, avg_ms: value == null ? null : value * 1.1 })

function metricsFor(group, index, tick = 0) {
  const scenario = group.scenario
  const count = scenario === 'no-requests' ? 0 : scenario === 'low-sample' ? 2 : 620 + index * 71 + tick * 7
  const variation = Math.sin((index + tick) * .7)
  const errorRate = !count ? 0 : scenario === 'critical' ? .25 + Math.abs(variation) * .1 : scenario === 'warning' ? .07 + Math.abs(variation) * .04 : .002 + Math.abs(variation) * .01
  const errorCount = Math.round(count * errorRate)
  const denominator = ['no-cache-input', 'no-requests', 'image'].includes(scenario) ? 0 : count * 2300
  const cacheRate = scenario === 'zero-cache' || !denominator ? 0 : .78 + variation * .12
  const ttft = ['no-requests', 'no-ttft', 'image'].includes(scenario) ? null : scenario === 'critical' ? 34000 : scenario === 'warning' ? 12400 : 1600 + (index % 6) * 330 + Math.round(variation * 100)
  return {
    success_requests: count - errorCount, error_requests: errorCount, request_count: count,
    has_requests: count > 0, has_cache_input: denominator > 0,
    token_count: count * 2800, rpm: count / 90, tpm: count * 2800 / 90,
    error_rate: errorCount / Math.max(count, 1), cache_rate: cacheRate,
    cache_rate_numerator: Math.round(denominator * cacheRate), cache_rate_denominator: denominator,
    ttft: latency(ttft, count), duration: latency(count ? 8000 + index * 150 : null, count),
    upstream_affected_requests: errorCount, upstream_attempt_count: count,
  }
}
function healthFor(metrics, thresholds) {
  const enough = metrics.request_count >= thresholds.minimum_sample
  const error = !enough ? 'unknown' : metrics.error_rate >= thresholds.critical_error_rate ? 'critical' : metrics.error_rate >= thresholds.warning_error_rate ? 'warning' : 'healthy'
  const ttft = !enough || metrics.ttft.p50_ms == null ? 'unknown' : metrics.ttft.p50_ms >= thresholds.critical_ttft_ms ? 'critical' : metrics.ttft.p50_ms >= thresholds.warning_ttft_ms ? 'warning' : 'healthy'
  const cache = !enough || !metrics.has_cache_input ? 'unknown' : metrics.cache_rate < .05 ? 'critical' : metrics.cache_rate < .2 ? 'warning' : 'healthy'
  const overall = !enough ? 'unknown' : error === 'critical' || ttft === 'critical' ? 'critical' : error === 'warning' || ttft === 'warning' ? 'warning' : 'healthy'
  const score = state => state === 'unknown' ? null : state === 'healthy' ? 96 : state === 'warning' ? 68 : 28
  return { overall, error_rate: error, ttft, cache, score: score(overall), error_rate_score: score(error), ttft_score: score(ttft), cache_score: score(cache), minimum_sample: thresholds.minimum_sample, thresholds }
}
export function redactMetric(metric) {
  return { ...metric, success_requests: 0, error_requests: 0, request_count: 0, token_count: 0, rpm: 0, tpm: 0,
    cache_rate_numerator: 0, cache_rate_denominator: 0, upstream_affected_requests: 0, upstream_attempt_count: 0,
    ttft: { ...metric.ttft, sample_count: 0 }, duration: { ...metric.duration, sample_count: 0 } }
}
export function visibleConfig(config, groups, admin) {
  if (admin) return structuredClone(config)
  const ids = new Set(groups.map(g => g.id))
  return { ...structuredClone(config), group_ids: config.group_ids.filter(id => ids.has(id)),
    display_order: config.display_order ? {
      categories: [...config.display_order.categories],
      image_grouping: config.display_order.image_grouping,
      groups: Object.fromEntries(Object.entries(config.display_order.groups).map(([key, value]) => [key, value.filter(id => ids.has(id))])),
      group_categories: Object.fromEntries(Object.entries(config.display_order.group_categories).filter(([id]) => ids.has(Number(id)))),
    } : undefined }
}
function queryList(query, key) { return query.getAll(key).flatMap(v => v.split(',')).filter(Boolean) }
export function makeV2(groups, config, query, admin) {
  const range = query.get('range') || '90m'
  const [spanSeconds, bucketSeconds] = ({ '90m': [5400, 300], '24h': [86400, 3600], '7d': [604800, 21600], '30d': [2592000, 86400] })[range] || [5400, 300]
  const count = spanSeconds / bucketSeconds
  const end = Math.floor(Date.now() / (bucketSeconds * 1000)) * bucketSeconds * 1000
  const coverage = { requested_start: iso(end - spanSeconds * 1000), requested_end: iso(end), coverage_start: iso(end - spanSeconds * 1000), data_through: iso(end), computed_at: iso(Date.now()), aggregation_lag_seconds: 20, coverage_complete: true, bucket_seconds: bucketSeconds }
  const platformFilters = queryList(query, 'platform'), groupFilters = queryList(query, 'group_id').map(Number), modelFilters = queryList(query, 'model')
  const accessible = admin ? groups : groups.filter(g => !g.is_exclusive)
  const selected = accessible.filter(g => (!platformFilters.length || platformFilters.includes(g.platform)) && (!groupFilters.length || groupFilters.includes(g.id)) && (!modelFilters.length || modelFilters.includes(modelForGroup(g))))
  const rows = selected.map(g => {
    const index = groups.findIndex(candidate => candidate.id === g.id)
    const metrics = metricsFor(g, index)
    const buckets = Array.from({ length: count }, (_, tick) => {
      const bucketMetrics = metricsFor(g, index, tick)
      // Deliberately vary healthy histories and include missing early buckets.
      if (tick === 0 && index % 7 === 0) { bucketMetrics.request_count = 0; bucketMetrics.has_requests = false; bucketMetrics.has_cache_input = false; bucketMetrics.ttft = latency(null, 0) }
      return { bucket_start: iso(end - spanSeconds * 1000 + tick * bucketSeconds * 1000), metrics: admin ? bucketMetrics : redactMetric(bucketMetrics), health: healthFor(bucketMetrics, config.health_thresholds) }
    })
    return { platform: g.platform, group_id: g.id, group_name: g.name, model: modelForGroup(g), metrics: admin ? metrics : redactMetric(metrics), health: healthFor(metrics, config.health_thresholds), buckets }
  })
  const summaryMetrics = metricsFor({ scenario: 'healthy' }, 12)
  return {
    coverage, items: rows, group_by: query.get('group_by') || 'platform_group',
    snapshot: { coverage, config: visibleConfig(config, accessible, admin), metrics: admin ? summaryMetrics : redactMetric(summaryMetrics), health: healthFor(summaryMetrics, config.health_thresholds), trend: rows[0]?.buckets || [] },
    dimensions: {
      platforms: providers.filter(p => accessible.some(g => g.platform === p)).map(value => ({ value, label: value, request_count: admin ? 3000 : 0 })),
      groups: accessible.map(g => ({ id: g.id, name: g.name, platform: g.platform, request_count: admin ? 600 : 0 })),
      models: [...new Map(accessible.map(g => [modelForGroup(g), { value: modelForGroup(g), label: modelForGroup(g), platform: g.platform, request_count: admin ? 3000 : 0 }])).values()],
    },
  }
}

export function makeV1(groups, admin) {
  const end = Math.floor(Date.now() / 60000) * 60000
  return (admin ? groups : groups.filter(g => !g.is_exclusive)).map((g, i) => {
    const operational = g.scenario === 'healthy' || ['no-cache-input', 'zero-cache', 'no-ttft', 'image'].includes(g.scenario)
    const status = g.scenario === 'no-requests' ? 'unknown' : operational ? 'operational' : g.scenario === 'critical' ? 'failed' : 'degraded'
    return { id: g.id, name: g.name, provider: g.platform, group_name: g.name, primary_model: modelForGroup(g),
      primary_status: status, primary_latency_ms: operational ? 2100 + i * 47 : status === 'failed' ? 45000 : null,
      primary_ping_latency_ms: status === 'unknown' ? null : 120 + i,
      availability_7d: status === 'unknown' ? 0 : operational ? 99.5 : 84.3, extra_models: [],
      timeline: Array.from({ length: 60 }, (_, tick) => ({ checked_at: iso(end - tick * 60000), status: status === 'unknown' ? 'unknown' : (tick + i) % 23 === 0 ? 'failed' : (tick + i) % 11 === 0 ? 'degraded' : status, latency_ms: 2100 + i * 47, ping_latency_ms: 120 + i })),
    }
  })
}
