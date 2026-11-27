/**
 * 模型广场的展示模型：把「分组 × 模型」的原始响应整理成「一行一个模型」。
 *
 * 后端返回的是以分组为中心的结构（同一模型会出现在多个分组里）。广场按模型去重后，
 * 每个模型保留一份「提供它、且当前可见」的分组列表，价格口径由所选分组决定：
 * - 未选分组（全部）：取各分组里最低的实付价，并记录达成分组；
 * - 选了分组：只保留该分组的模型，按其倍率计算实付价。
 */

import { resolveIntervalPrices } from '@/utils/pricing'
import {
  BILLING_MODE_IMAGE,
  BILLING_MODE_TOKEN,
  BILLING_MODE_VIDEO,
  type BillingMode
} from '@/constants/channel'
import type { UserPricingInterval } from '@/api/channels'
import type { ModelPlazaGroup, PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'

export const PER_MILLION = 1_000_000

/**
 * 「国模集合」：国产模型常挂在 openai 平台的分组下（广场按分组平台归类），
 * 单看平台会把 deepseek / GLM 之类混进 OpenAI。这里按模型名识别，统一收进一个集合。
 */
export const CN_SEGMENT = 'cn'

// 结尾允许数字：qwen3.8、hy3、k3、minimax-m3 这类不带分隔符的写法也要认出来。
const DOMESTIC_MODEL_PREFIX = /^(deepseek|glm|chatglm|kimi|moonshot|qwen|qwq|minimax|abab|ernie|hunyuan|doubao|seedance|seedream|step|yi|baichuan|spark|internlm|hailuo|mimo|hy|k3)([-._0-9]|$)/i

export function isDomesticModel(name: string): boolean {
  return DOMESTIC_MODEL_PREFIX.test(name)
}

/** 平台在标签栏中的固定顺序；未列出的平台排在后面。 */
const PLATFORM_ORDER = [
  'openai',
  'anthropic',
  'gemini',
  'grok',
  CN_SEGMENT,
  'deepseek',
  'zhipu',
  'kimi',
  'minimax',
  'composite'
]

export interface PlazaOffering {
  group: ModelPlazaGroup
  model: PlazaModel
  /** 该分组对该模型的生效倍率（用户专属优先）。 */
  rate: number
  /** 该分组是否给用户配了专属倍率（展示时划线原倍率）。 */
  personal: boolean
}

export interface PlazaRow {
  key: string
  name: string
  platform: string
  /** 提供该模型的分组，按倍率升序。 */
  offerings: PlazaOffering[]
  /** 当前口径下用于展示价格的分组：最低价分组或选定分组。 */
  primary: PlazaOffering
  /** 官方参考价（同模型各分组相同，取先出现的）。 */
  official: PlazaModel['official_pricing']
}

export function effectiveRate(group: ModelPlazaGroup): number {
  return group.user_rate_multiplier ?? group.rate_multiplier
}

/**
 * 模型在广场里的归类：
 * 只有挂在 OpenAI 接口下的国产模型（如 openai 分组里的 deepseek / GLM）才拆进「国模集合」；
 * DeepSeek、GLM 等自己的接口分组保持独立，不合并。
 */
export function rowSegment(row: PlazaRow): string {
  return row.platform === 'openai' && isDomesticModel(row.name) ? CN_SEGMENT : row.platform
}

/** 分组所属的归集：OpenAI 接口下整组都是国产模型时归入「国模集合」。 */
export function groupSegment(group: ModelPlazaGroup): string {
  if (
    group.platform === 'openai' &&
    group.models.length > 0 &&
    group.models.every((m) => isDomesticModel(m.name))
  ) {
    return CN_SEGMENT
  }
  return group.platform
}

export function billingMode(model: PlazaModel): BillingMode {
  return (model.pricing?.billing_mode || BILLING_MODE_TOKEN) as BillingMode
}

/** 生图 / 视频模型可走独立倍率，其余沿用分组生效倍率。 */
export function offeringRate(offering: PlazaOffering): number {
  const { group, model, rate } = offering
  if (billingMode(model) === BILLING_MODE_IMAGE && group.image_rate_independent) {
    return group.image_rate_multiplier ?? 1
  }
  if (billingMode(model) === BILLING_MODE_VIDEO && group.video_rate_independent) {
    return group.video_rate_multiplier ?? 1
  }
  return rate
}

/** token 模式的阶梯（绝对价解析后的实收单价，不含倍率）。 */
export function tokenIntervals(model: PlazaModel): UserPricingInterval[] {
  const pricing = model.pricing
  if (!pricing) return []
  return sortByContext(pricing.intervals ?? []).map((iv) => resolveIntervalPrices(iv, pricing))
}

export function officialIntervals(model: PlazaModel): UserPricingInterval[] {
  return sortByContext(model.official_pricing?.intervals ?? [])
}

export function sortByContext(intervals: UserPricingInterval[]): UserPricingInterval[] {
  return [...intervals].sort((a, b) => a.min_tokens - b.min_tokens)
}

export function hasCachePricing(model: PlazaModel): boolean {
  const p = model.pricing
  return (
    p?.cache_write_price != null ||
    p?.cache_write_1h_price != null ||
    p?.cache_write_12h_price != null ||
    p?.cache_read_price != null
  )
}

export function hasOfficialCache(official: PlazaModel['official_pricing']): boolean {
  return (
    official?.cache_write_price != null ||
    official?.cache_read_price != null ||
    official?.cache_write_1h_price != null ||
    official?.cache_write_12h_price != null
  )
}

/** 档位标签：优先后端给出的 tier_label，否则按区间生成。 */
export function tierLabel(iv: UserPricingInterval): string {
  if (iv.tier_label) return iv.tier_label
  const { min_tokens: min, max_tokens: max } = iv
  return max == null ? `>${formatTokenCount(min)}` : `≤${formatTokenCount(max)}`
}

export function formatTokenCount(n: number): string {
  if (n >= 1_000_000) return `${trimZero(n / 1_000_000)}M`
  if (n >= 1_000) return `${trimZero(n / 1_000)}K`
  return String(n)
}

function trimZero(n: number): string {
  return String(Math.round(n * 100) / 100)
}

/**
 * 美元价格统一格式：保留 6 位有效数字并去掉尾零。
 * 目录价常带长尾小数（如 2.948069751），直接展示会让表格显得杂乱。
 */
function formatUsd(scaled: number): string {
  if (!Number.isFinite(scaled)) return '-'
  if (scaled === 0) return '$0'
  return `$${Number(scaled.toPrecision(6))}`
}

/** 实付单价：单价 × 该分组对该模型的生效倍率。 */
export function paidPrice(value: number | null | undefined, offering: PlazaOffering): string {
  if (value == null) return '-'
  return formatUsd(value * offeringRate(offering) * PER_MILLION)
}

/** 实付按次/按张单价（不换算为百万 token）。 */
export function paidRequestPrice(value: number | null | undefined, offering: PlazaOffering): string {
  if (value == null) return '-'
  return formatUsd(value * offeringRate(offering))
}

export function officialPrice(value: number | null | undefined): string {
  if (value == null) return '-'
  return formatUsd(value * PER_MILLION)
}

/** 用于选出「最低价分组」的可比数值：token 取首档输入价，按次/按张取请求价。 */
export function comparablePrice(offering: PlazaOffering): number {
  const p = offering.model.pricing
  if (!p) return Number.POSITIVE_INFINITY
  const rate = offeringRate(offering)
  if (billingMode(offering.model) === BILLING_MODE_TOKEN) {
    const first = sortByContext(p.intervals ?? [])[0]
    const value = first ? resolveIntervalPrices(first, p).input_price : p.input_price
    return value == null ? Number.POSITIVE_INFINITY : value * rate
  }
  const value = p.per_request_price ?? p.image_input_price ?? p.input_price
  return value == null ? Number.POSITIVE_INFINITY : value * rate
}

/**
 * 组成表格行。
 * @param groups 后端返回的可见分组
 * @param groupId 当前选中的分组；'all' 表示按最低价展示
 */
export function buildPlazaRows(
  groups: ModelPlazaGroup[],
  groupId: number | 'all'
): PlazaRow[] {
  const picked = groupId === 'all' ? groups : groups.filter((g) => g.id === groupId)
  const rows = new Map<string, PlazaRow>()

  for (const group of picked) {
    const rate = effectiveRate(group)
    const personal = group.user_rate_multiplier != null && group.user_rate_multiplier !== group.rate_multiplier
    for (const model of group.models) {
      const key = `${model.platform}:${model.name}`
      const offering: PlazaOffering = { group, model, rate, personal }
      const existing = rows.get(key)
      if (!existing) {
        rows.set(key, {
          key,
          name: model.name,
          platform: model.platform,
          offerings: [offering],
          primary: offering,
          official: model.official_pricing
        })
        continue
      }
      existing.offerings.push(offering)
      if (!existing.official && model.official_pricing) existing.official = model.official_pricing
      if (comparablePrice(offering) < comparablePrice(existing.primary)) {
        existing.primary = offering
      }
    }
  }

  const list = [...rows.values()]
  for (const row of list) {
    row.offerings.sort((a, b) => offeringRate(a) - offeringRate(b) || a.group.name.localeCompare(b.group.name))
    // 未配置价格的行排在最后比较用的 Infinity 之上没有意义，保持其原分组顺序。
  }
  return list
}

/** 分组行内的模型排序：token 计费在前，按官方输出价从高到低，同价按名称降序。 */
export function sortPlazaRows(rows: PlazaRow[]): PlazaRow[] {
  return [...rows].sort((a, b) => {
    const ta = billingMode(a.primary.model) === BILLING_MODE_TOKEN
    const tb = billingMode(b.primary.model) === BILLING_MODE_TOKEN
    if (ta !== tb) return ta ? -1 : 1
    const pa = a.official?.output_price ?? null
    const pb = b.official?.output_price ?? null
    if (pa != null && pb != null && pa !== pb) return pb - pa
    if (pa != null && pb == null) return -1
    if (pa == null && pb != null) return 1
    return b.name.localeCompare(a.name)
  })
}

/** 平台顺序：固定顺序优先，其余按字母序排在后面。 */
export function sortPlatforms(platforms: string[]): string[] {
  const rank = (p: string) => {
    const i = PLATFORM_ORDER.indexOf(p)
    return i === -1 ? PLATFORM_ORDER.length : i
  }
  return [...platforms].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b))
}

/** "00:30–08:30"；整分钟的 HH:mm:ss 省略秒。 */
export function formatTimeWindow(period: PlazaTimePricingPeriod): string {
  const clock = (v: string) => v.replace(/^(\d{2}:\d{2}):00$/, '$1')
  return `${clock(period.start_time)}–${clock(period.end_time)}`
}
