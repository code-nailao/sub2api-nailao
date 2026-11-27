/**
 * 模型广场的导出：把当前口径整理成 CSV，或给出可直接对接的公开 JSON 地址。
 *
 * CSV 面向表格/脚本导入，价格一律输出为美元原始数值（每百万 token 或每张/每次），
 * 不输出带货币符号的展示字符串；多档上下文每个档位一行。
 */

import { resolveIntervalPrices } from '@/utils/pricing'
import {
  BILLING_MODE_TOKEN,
  type BillingMode
} from '@/constants/channel'
import type { UserPricingInterval } from '@/api/channels'
import {
  billingMode,
  offeringRate,
  officialIntervals,
  sortByContext,
  tierLabel,
  type PlazaOffering,
  type PlazaRow
} from './plazaDisplay'

const PER_MILLION = 1_000_000

const HEADER = [
  'platform',
  'model',
  'group',
  'group_rate',
  'effective_rate',
  'billing_mode',
  'context_tier',
  'input_paid',
  'output_paid',
  'cache_write_paid',
  'cache_write_1h_paid',
  'cache_write_12h_paid',
  'cache_read_paid',
  'input_official',
  'output_official',
  'cache_write_official',
  'cache_read_official',
  'unit'
] as const

/** 去浮点噪声后输出原始数值，空值输出空串。 */
function num(value: number | null | undefined, scale = PER_MILLION): string {
  if (value == null || !Number.isFinite(value)) return ''
  const scaled = value * scale
  return String(Number(scaled.toPrecision(12)))
}

function modeLabel(mode: BillingMode): string {
  if (mode === BILLING_MODE_TOKEN) return 'token'
  return mode
}

function unitOf(mode: BillingMode): string {
  return mode === BILLING_MODE_TOKEN ? 'usd_per_1m_tokens' : 'usd_per_unit'
}

function escapeCell(value: string): string {
  if (!/[",\n\r]/.test(value)) return value
  return `"${value.replace(/"/g, '""')}"`
}

function tokenRow(offering: PlazaOffering, interval: UserPricingInterval | null): string[] {
  const model = offering.model
  const pricing = model.pricing
  const rate = offeringRate(offering)
  const official = officialIntervals(model)
  const officialAt = interval
    ? official.find((iv) => iv.min_tokens === interval.min_tokens && iv.max_tokens === interval.max_tokens) ?? null
    : null
  const base = interval ?? pricing
  return [
    model.platform,
    model.name,
    offering.group.name,
    String(offering.group.rate_multiplier),
    String(rate),
    'token',
    interval ? tierLabel(interval) : '',
    num(base?.input_price == null ? null : base.input_price * rate),
    num(base?.output_price == null ? null : base.output_price * rate),
    num(base?.cache_write_price == null ? null : base.cache_write_price * rate),
    num(base?.cache_write_1h_price == null ? null : base.cache_write_1h_price * rate),
    num(base?.cache_write_12h_price == null ? null : base.cache_write_12h_price * rate),
    num(base?.cache_read_price == null ? null : base.cache_read_price * rate),
    num(officialAt?.input_price ?? model.official_pricing?.input_price),
    num(officialAt?.output_price ?? model.official_pricing?.output_price),
    num(officialAt?.cache_write_price ?? model.official_pricing?.cache_write_price),
    num(officialAt?.cache_read_price ?? model.official_pricing?.cache_read_price),
    unitOf(BILLING_MODE_TOKEN)
  ]
}

function requestRow(offering: PlazaOffering): string[] {
  const model = offering.model
  const pricing = model.pricing
  const rate = offeringRate(offering)
  const mode = billingMode(model)
  const unitPrice = pricing?.per_request_price ?? pricing?.image_input_price ?? pricing?.input_price
  return [
    model.platform,
    model.name,
    offering.group.name,
    String(offering.group.rate_multiplier),
    String(rate),
    modeLabel(mode),
    '',
    unitPrice == null ? '' : num(unitPrice * rate, 1),
    '',
    '',
    '',
    '',
    '',
    num(model.official_pricing?.input_price),
    num(model.official_pricing?.output_price),
    num(model.official_pricing?.cache_write_price),
    num(model.official_pricing?.cache_read_price),
    unitOf(mode)
  ]
}

function offeringRows(offering: PlazaOffering): string[][] {
  const model = offering.model
  if (billingMode(model) !== BILLING_MODE_TOKEN) return [requestRow(offering)]
  const pricing = model.pricing
  if (!pricing) {
    return [[
      model.platform, model.name, offering.group.name, String(offering.group.rate_multiplier),
      String(offeringRate(offering)), 'token', '', '', '', '', '', '', '', '', '', '', '', 'usd_per_1m_tokens'
    ]]
  }
  const intervals = sortByContext(pricing.intervals ?? []).map((iv) => resolveIntervalPrices(iv, pricing))
  if (intervals.length === 0) return [tokenRow(offering, null)]
  return intervals.map((iv) => tokenRow(offering, iv))
}

/**
 * @param rows 当前视图的行
 * @param mode 'view' 每行只导出口径分组；'all-groups' 导出每个分组各一行
 */
export function buildPlazaCsv(rows: PlazaRow[], mode: 'view' | 'all-groups'): string {
  const lines: string[][] = [[...HEADER]]
  for (const row of rows) {
    const offerings = mode === 'all-groups' ? row.offerings : [row.primary]
    for (const offering of offerings) lines.push(...offeringRows(offering))
  }
  return '﻿' + lines.map((cells) => cells.map(escapeCell).join(',')).join('\r\n')
}

export function downloadCsv(filename: string, content: string) {
  const blob = new Blob([content], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

/** 公开 JSON 接口：匿名可用，带 token 时返回个人专属倍率。 */
export function plazaApiUrl(): string {
  return `${window.location.origin}/api/v1/model-plaza`
}
