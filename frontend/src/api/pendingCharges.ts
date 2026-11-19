import { apiClient } from './client'

export type PendingChargeState = 'reserved' | 'sent' | 'unknown'

export interface PendingCharge {
  request_id: string
  model: string
  created_at: string
  state: PendingChargeState
  held_price: number
}

export interface PendingChargesPage {
  items: PendingCharge[]
  next_cursor: string | null
  page_size: number
}

export interface PendingChargesParams {
  page_size?: number
  cursor?: string
}

/** Read only the authenticated user's pending charges. Cursors are opaque. */
export async function listPendingCharges(
  params: PendingChargesParams = {},
  options: { signal?: AbortSignal } = {},
): Promise<PendingChargesPage> {
  const pageSize = params.page_size ?? 20
  if (!Number.isInteger(pageSize) || pageSize < 1 || pageSize > 50) {
    throw new RangeError('page_size must be an integer between 1 and 50')
  }

  const { data } = await apiClient.get<PendingChargesPage>('/usage/pending-charges', {
    signal: options.signal,
    // Do not forward arbitrary filters, especially user or administrator identifiers.
    params: {
      page_size: pageSize,
      ...(params.cursor ? { cursor: params.cursor } : {}),
    },
  })
  return data
}
