import type { ChannelMonitor, list } from '@/api/admin/channelMonitor'

// Sorting always covers the complete list, independently of table filters and
// pagination. Refuse an incomplete/changing snapshot instead of saving a subset.
export async function loadMonitorOrder(fetchPage: typeof list): Promise<ChannelMonitor[]> {
  const items: ChannelMonitor[] = []
  let total: number | undefined
  for (let page = 1; ; page++) {
    const result = await fetchPage({ page, page_size: 100 })
    if (total === undefined) total = result.total
    if (result.total !== total || total > 10000 || total < 0) throw new Error('Monitor list changed; refresh and try again')
    items.push(...result.items)
    if (items.length === total) break
    if (items.length > total || result.items.length === 0) throw new Error('Incomplete monitor list; refresh and try again')
  }
  if (new Set(items.map(item => item.id)).size !== items.length) throw new Error('Monitor list changed; refresh and try again')
  return items.sort((a, b) => a.sort_order - b.sort_order || a.id - b.id)
}
