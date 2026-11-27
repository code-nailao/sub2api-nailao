import { describe, expect, it } from 'vitest'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'
import { buildPlazaRows } from '../plazaDisplay'
import { buildPlazaCsv } from '../plazaExport'

function model(overrides: Partial<PlazaModel> = {}): PlazaModel {
  return {
    name: 'gpt-test',
    platform: 'openai',
    pricing: {
      billing_mode: 'token',
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: 5e-7,
      image_input_price: null,
      image_output_price: null,
      per_request_price: null,
      intervals: [
        {
          min_tokens: 0,
          max_tokens: 272000,
          tier_label: '≤272K',
          input_price: 5e-6,
          output_price: 3e-5,
          cache_write_price: null,
          cache_read_price: 5e-7,
          per_request_price: null
        },
        {
          min_tokens: 272000,
          max_tokens: null,
          tier_label: '>272K',
          input_price: 1e-5,
          output_price: 4.5e-5,
          cache_write_price: null,
          cache_read_price: 1e-6,
          per_request_price: null
        }
      ]
    },
    official_pricing: null,
    ...overrides
  }
}

function group(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1,
    name: 'Group, A',
    description: '',
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 0.035,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    is_exclusive: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    video_rate_independent: false,
    video_rate_multiplier: 1,
    long_context_pricing_enabled: true,
    models: [model()],
    ...overrides
  }
}

/** 最小 CSV 解析：支持双引号包裹和 "" 转义，够验证导出格式。 */
function parseRow(line: string): string[] {
  const cells: string[] = []
  let cell = ''
  let quoted = false
  for (let i = 0; i < line.length; i += 1) {
    const ch = line[i]
    if (quoted) {
      if (ch === '"' && line[i + 1] === '"') {
        cell += '"'
        i += 1
      } else if (ch === '"') {
        quoted = false
      } else {
        cell += ch
      }
    } else if (ch === '"') {
      quoted = true
    } else if (ch === ',') {
      cells.push(cell)
      cell = ''
    } else {
      cell += ch
    }
  }
  cells.push(cell)
  return cells
}

function linesOf(csv: string): string[][] {
  return csv.replace(/^\uFEFF/, '').split('\r\n').map(parseRow)
}

describe('buildPlazaCsv', () => {
  it('writes one row per context tier for the current price lens', () => {
    const rows = buildPlazaRows([group()], 'all')
    const lines = linesOf(buildPlazaCsv(rows, 'view'))

    expect(lines).toHaveLength(3) // header + two tiers
    expect(lines[0][0]).toBe('platform')
    expect(lines[0][1]).toBe('model')

    const paid = lines[1]
    expect(paid[1]).toBe('gpt-test')
    expect(paid[6]).toBe('≤272K')
    expect(Number(paid[7])).toBeCloseTo(0.175) // 5e-6 × 0.035 × 1M
    expect(Number(paid[8])).toBeCloseTo(1.05)
    expect(lines[2][6]).toBe('>272K')
    expect(Number(lines[2][7])).toBeCloseTo(0.35)
  })

  it('exports every group when asked for the full matrix', () => {
    const rows = buildPlazaRows(
      [
        group({ id: 1, name: 'Group, A', rate_multiplier: 0.035 }),
        group({ id: 2, name: 'Group B', rate_multiplier: 0.07 })
      ],
      'all'
    )
    const csv = buildPlazaCsv(rows, 'all-groups')
    const lines = linesOf(csv)

    expect(lines).toHaveLength(5) // header + 2 tiers × 2 groups
    const groups = lines.slice(1).map((line) => line[2])
    expect(new Set(groups)).toEqual(new Set(['Group, A', 'Group B']))
  })

  it('starts with a BOM so Excel reads UTF-8', () => {
    expect(buildPlazaCsv([], 'view').startsWith('\uFEFF')).toBe(true)
  })
})
