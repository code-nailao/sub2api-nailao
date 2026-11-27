import { describe, expect, it } from 'vitest'
import type { ModelPlazaGroup, PlazaModel } from '@/api/modelPlaza'
import {
  buildPlazaRows,
  comparablePrice,
  offeringRate,
  paidPrice,
  sortPlatforms,
  tierLabel,
  type PlazaOffering
} from '../plazaDisplay'

const PER_MILLION = 1_000_000

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
      intervals: []
    },
    official_pricing: {
      input_price: 5e-6,
      output_price: 3e-5,
      cache_write_price: null,
      cache_read_price: 5e-7
    },
    ...overrides
  }
}

function group(overrides: Partial<ModelPlazaGroup> = {}): ModelPlazaGroup {
  return {
    id: 1,
    name: 'Group A',
    description: '',
    platform: 'openai',
    subscription_type: 'standard',
    rate_multiplier: 0.1,
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
    models: [],
    ...overrides
  }
}

describe('buildPlazaRows', () => {
  it('lists a model once and picks the lowest paid price across groups', () => {
    const expensive = group({ id: 1, name: 'Expensive', rate_multiplier: 0.5, models: [model()] })
    const cheap = group({ id: 2, name: 'Cheap', rate_multiplier: 0.05, models: [model()] })

    const rows = buildPlazaRows([expensive, cheap], 'all')

    expect(rows).toHaveLength(1)
    expect(rows[0].offerings.map((o) => o.group.name)).toEqual(['Cheap', 'Expensive'])
    expect(rows[0].primary.group.name).toBe('Cheap')
  })

  it('keeps the personal rate as the primary effective rate', () => {
    const personal = group({
      id: 3,
      name: 'Personal',
      rate_multiplier: 0.3,
      user_rate_multiplier: 0.2,
      models: [model()]
    })

    const rows = buildPlazaRows([personal], 'all')

    expect(rows[0].primary.rate).toBe(0.2)
    expect(rows[0].primary.personal).toBe(true)
  })

  it('keeps only the selected group under the group lens', () => {
    const a = group({ id: 1, name: 'A', rate_multiplier: 0.1, models: [model({ name: 'a' })] })
    const b = group({ id: 2, name: 'B', rate_multiplier: 0.2, models: [model({ name: 'b' })] })

    const rows = buildPlazaRows([a, b], 2)

    expect(rows.map((row) => row.name)).toEqual(['b'])
    expect(rows[0].primary.group.id).toBe(2)
  })

  it('compares image billed models by their unit price', () => {
    const image = model({
      name: 'image-model',
      pricing: {
        billing_mode: 'image',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: 0.04,
        image_output_price: null,
        per_request_price: null,
        intervals: []
      }
    })
    const offering: PlazaOffering = { group: group(), model: image, rate: 1, personal: false }

    expect(comparablePrice(offering)).toBeCloseTo(0.04)
  })
})

describe('paidPrice / offeringRate', () => {
  it('multiplies the token price by the group rate', () => {
    const offering: PlazaOffering = { group: group({ rate_multiplier: 0.035 }), model: model(), rate: 0.035, personal: false }

    expect(paidPrice(5e-6, offering)).toBe('$0.175')
  })

  it('uses the image rate only when the group enables an independent image rate', () => {
    const image = model({
      pricing: {
        billing_mode: 'image',
        input_price: null,
        output_price: null,
        cache_write_price: null,
        cache_read_price: null,
        image_input_price: 0.04,
        image_output_price: null,
        per_request_price: null,
        intervals: []
      }
    })
    const shared: PlazaOffering = {
      group: group({ rate_multiplier: 0.5, image_rate_independent: false, image_rate_multiplier: 2 }),
      model: image,
      rate: 0.5,
      personal: false
    }
    const independent: PlazaOffering = {
      ...shared,
      group: group({ rate_multiplier: 0.5, image_rate_independent: true, image_rate_multiplier: 2 })
    }

    expect(offeringRate(shared)).toBe(0.5)
    expect(offeringRate(independent)).toBe(2)
  })

  it('formats long official decimals without noise', () => {
    const offering: PlazaOffering = { group: group({ rate_multiplier: 1 }), model: model(), rate: 1, personal: false }

    expect(paidPrice(2.948069751e-6, offering)).toBe('$2.94807')
    expect(paidPrice(5e-6 * PER_MILLION, { ...offering, rate: 1 / PER_MILLION })).toBe('$5')
  })
})

describe('tierLabel / sortPlatforms', () => {
  it('prefers the backend tier label and falls back to token ranges', () => {
    expect(tierLabel({ min_tokens: 0, max_tokens: 272000, tier_label: '≤272K' } as never)).toBe('≤272K')
    expect(tierLabel({ min_tokens: 0, max_tokens: 272000 } as never)).toBe('≤272K')
    expect(tierLabel({ min_tokens: 272000, max_tokens: null } as never)).toBe('>272K')
  })

  it('keeps the known platform order and appends unknown ones', () => {
    expect(sortPlatforms(['zhipu', 'openai', 'custom', 'anthropic'])).toEqual([
      'openai',
      'anthropic',
      'zhipu',
      'custom'
    ])
  })
})
