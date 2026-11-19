import { describe, expect, it } from 'vitest'
import { alignedBuckets, availability, cacheRate, cardHealth, categoryOf, categoryOrder, orderedGroups, formatMultiplier, replaceVisibleOrder } from '../display'
import { metric, health } from './fixtures'

describe('monitor card data semantics', () => {
  it('distinguishes zero cache hits from no cache input after user counter redaction', () => {
    const hidden = metric({ request_count: 0, cache_rate_denominator: 0, cache_rate_numerator: 0, cache_rate: 0, rpm: 0, tpm: 0 })
    expect(cacheRate(hidden, health())).toBe(0)
    expect(availability(hidden, health())).toBe(.99)
    expect(cacheRate({ ...hidden, has_cache_input: false }, health())).toBeNull()
  })

  it('never presents no traffic as 100% availability or a healthy bar', () => {
    const noTraffic = metric({ has_requests: false, has_cache_input: false, request_count: 0, error_rate: 0 })
    expect(availability(noTraffic, health())).toBeNull()
    expect(cardHealth(noTraffic, health())).toBe('unknown')
    expect(cardHealth(metric(), health({ overall: 'unknown' }))).toBe('unknown')
  })

  it('handles older redacted payloads conservatively without treating all zeros as traffic', () => {
    const empty = metric({ has_requests: undefined, has_cache_input: undefined, request_count: 0, rpm: 0, tpm: 0, error_rate: 0, cache_rate: 0, cache_rate_denominator: 0, ttft: { sample_count: 0, p50_ms: null, p95_ms: null, avg_ms: null } })
    const unknown = health({ overall: 'unknown', error_rate: 'unknown', ttft: 'unknown', cache: 'unknown' })
    expect(availability(empty, unknown)).toBeNull()
    expect(cacheRate(empty, unknown)).toBeNull()
    expect(availability(empty, health())).toBe(1)
  })

  it('keeps three-decimal user rates and a legitimate free multiplier', () => {
    expect(formatMultiplier(.065)).toBe('0.065×')
    expect(formatMultiplier(.075)).toBe('0.075×')
    expect(formatMultiplier(0)).toBe('0×')
    expect(formatMultiplier(undefined)).toBe('—')
    expect(formatMultiplier(NaN)).toBe('—')
  })

  it('keeps missing leading and trailing time periods visible as unknown', () => {
    const buckets = [{ bucket_start: '2026-11-17T00:05:00Z', metrics: metric(), health: health() }]
    const aligned = alignedBuckets(buckets, {
      requested_start: '2026-11-17T00:00:00Z', requested_end: '2026-11-17T00:15:00Z',
      coverage_start: '2026-11-17T00:05:00Z', data_through: '2026-11-17T00:10:00Z',
      computed_at: '2026-11-17T00:10:00Z', aggregation_lag_seconds: 0, coverage_complete: false, bucket_seconds: 300,
    })
    expect(aligned.map(b => cardHealth(b.metrics, b.health))).toEqual(['unknown', 'healthy', 'unknown'])
    expect(availability(aligned[0].metrics, aligned[0].health)).toBeNull()
    expect(aligned[1]).toBe(buckets[0])
  })
})

describe('monitor category and order rules', () => {
  it('respects explicit overrides and clear domestic labels without guessing from a model name', () => {
    expect(categoryOf({ id: 1, platform: 'openai', name: '【国模】1' })).toBe('domestic')
    expect(categoryOf({ id: 2, platform: 'openai', name: 'DeepSeek reseller' })).toBe('openai')
    expect(categoryOf({ id: 2, platform: 'openai', name: 'DeepSeek reseller' }, { group_categories: { '2': 'domestic' } })).toBe('domestic')
    expect(categoryOf({ id: 3, platform: 'deepseek', name: 'Direct' })).toBe('domestic')
    expect(categoryOf({ id: 4, platform: 'future-provider', name: 'Future' })).toBe('other')
    expect(categoryOf({ id: 5, platform: 'kiro', name: 'Kiro' })).toBe('anthropic')
    expect(categoryOf({ id: 6, platform: 'antigravity', name: 'Google' })).toBe('gemini')
  })

  it('appends new categories and channels stably without mutating API rows', () => {
    expect(categoryOrder({ categories: ['gemini', 'openai'] })).toEqual(['gemini', 'openai', 'anthropic', 'grok', 'domestic', 'image', 'other'])
    const groups = [{ id: 1, name: 'A', platform: 'openai' }, { id: 2, name: 'B', platform: 'openai' }, { id: 3, name: 'New', platform: 'openai' }]
    expect(orderedGroups(groups, 'openai', { groups: { openai: [2, 1] } }).map(g => g.id)).toEqual([2, 1, 3])
    expect(groups.map(g => g.id)).toEqual([1, 2, 3])
  })

  it('keeps image groups in their original brand while honoring explicit overrides', () => {
    for (const name of ['【生图-原生】GPT Image 2.6（Adobe）', '【生图】备用', 'Gemini Image（Adobe）', 'gpt-image-2', 'Image 2']) {
      expect(categoryOf({ id: 1, name, platform: 'openai' }, { image_grouping: 'provider' })).toBe('openai')
    }
    expect(categoryOf({ id: 1, name: 'Gemini Image（Adobe）', platform: 'gemini' }, { image_grouping: 'provider' })).toBe('gemini')
    expect(categoryOf({ id: 1, name: 'Imagery study', platform: 'openai' })).toBe('openai')
    expect(categoryOf({ id: 1, name: 'Gemini Image', platform: 'gemini' }, { group_categories: { '1': 'gemini' } })).toBe('gemini')
    expect(categoryOf({ id: 1, name: 'Codex', platform: 'openai' }, { group_categories: { '1': 'other' } })).toBe('other')
  })

  it('groups image channels separately by default and allows manual categories to override either mode', () => {
    const image = { id: 1, name: 'GPT Image 2', platform: 'openai' }
    expect(categoryOf(image)).toBe('image')
    expect(categoryOf(image, { image_grouping: 'separate' })).toBe('image')
    expect(categoryOf(image, { image_grouping: 'separate', group_categories: { '1': 'openai' } })).toBe('openai')
    expect(categoryOf(image, { image_grouping: 'provider', group_categories: { '1': 'image' } })).toBe('image')
    expect(categoryOrder()).toEqual(['openai', 'anthropic', 'grok', 'gemini', 'domestic', 'image', 'other'])
  })

  it('supports other as a regular sortable category and reorders its channels', () => {
    expect(categoryOrder({ categories: ['other', 'gemini'] })).toEqual(['other', 'gemini', 'openai', 'anthropic', 'grok', 'domestic', 'image'])
    const groups = [{ id: 1, name: 'Future A', platform: 'future-a' }, { id: 2, name: 'Future B', platform: 'future-b' }]
    expect(orderedGroups(groups, 'other', { groups: { other: [2, 1] } }).map(g => g.id)).toEqual([2, 1])
  })

  it('places unlisted image channels at the end of each brand while preserving stable order', () => {
    const groups = [
      { id: 1, name: 'GPT Image 2', platform: 'openai' }, { id: 2, name: 'Codex Pro', platform: 'openai' },
      { id: 3, name: '【生图】备用', platform: 'openai' }, { id: 4, name: 'Codex Plus', platform: 'openai' },
      { id: 5, name: 'Gemini Image', platform: 'gemini' }, { id: 6, name: 'Gemini Ultra', platform: 'gemini' },
    ]
    expect(orderedGroups(groups, 'openai', { image_grouping: 'provider' }).map(g => g.id)).toEqual([2, 4, 1, 3])
    expect(orderedGroups(groups, 'gemini', { image_grouping: 'provider' }).map(g => g.id)).toEqual([6, 5])
    expect(groups.map(g => g.id)).toEqual([1, 2, 3, 4, 5, 6])
  })

  it('keeps a manually promoted image channel ahead of text and appends new channels stably', () => {
    const groups = [
      { id: 1, name: 'GPT Image 2', platform: 'openai' }, { id: 2, name: 'Codex Pro', platform: 'openai' },
      { id: 3, name: '【生图】备用', platform: 'openai' }, { id: 4, name: 'Codex Plus', platform: 'openai' },
    ]
    expect(orderedGroups(groups, 'openai', { image_grouping: 'provider', groups: { openai: [1, 2] } }).map(g => g.id)).toEqual([1, 2, 4, 3])
  })

  it('preserves IDs outside the current catalog when reordering visible channels', () => {
    expect(replaceVisibleOrder([1, 90, 2, 91], [2, 1, 3])).toEqual([2, 90, 1, 91, 3])
  })
})
