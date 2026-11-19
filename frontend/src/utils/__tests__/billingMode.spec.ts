import { describe, expect, it } from 'vitest'
import {
  BILLING_MODE_CONTENT_REJECTION,
  getBillingModeLabel,
  BILLING_MODE_IMAGE,
  BILLING_MODE_TOKEN,
  BILLING_MODE_VIDEO,
  getDisplayBillingMode,
  isImageUsage
} from '../billingMode'

describe('billingMode helpers', () => {
  it('prefers explicit video mode over image_count', () => {
    expect(
      getDisplayBillingMode({ image_count: 1, billing_mode: BILLING_MODE_VIDEO })
    ).toBe(BILLING_MODE_VIDEO)
    expect(isImageUsage({ image_count: 1, billing_mode: BILLING_MODE_VIDEO })).toBe(false)
  })

  it('infers image when image_count set and mode missing', () => {
    expect(getDisplayBillingMode({ image_count: 2, billing_mode: null })).toBe(BILLING_MODE_IMAGE)
  })

  it('keeps token mode even with image_count', () => {
    expect(
      getDisplayBillingMode({ image_count: 1, billing_mode: BILLING_MODE_TOKEN })
    ).toBe(BILLING_MODE_TOKEN)
  })
  it.each(['content_rejection', 'upstream_rejection'])('uses the saved %s event instead of token/image heuristics for display', charge_kind => {
    const row = { image_count: 1, billing_mode: 'token', billing_snapshot: { charge_kind } }
    expect(getDisplayBillingMode(row)).toBe(BILLING_MODE_CONTENT_REJECTION)
    expect(isImageUsage(row)).toBe(false)
    expect(getBillingModeLabel(getDisplayBillingMode(row), key => key)).toBe('upstreamRejection.title')
  })

})
