const SESSION_CHECK = 'sub2api:session-check'
const SESSION_CHECKED = 'sub2api:session-checked'
const SESSION_PAGES = new Set(['/admin/home', '/admin/balances', '/studio', '/canvas', '/intelligence-tests'])

/** Only the reviewed same-origin extension pages participate in native session renewal. */
export function isEmbeddedSessionPage(rawURL: string, origin: string): boolean {
  try {
    const url = new URL(rawURL)
    return url.origin === origin && SESSION_PAGES.has(url.pathname)
  } catch {
    return false
  }
}

export function createEmbeddedSessionCheckHandler(options: {
  getFrame: () => HTMLIFrameElement | null
  isEnabled: () => boolean
  refreshSession: () => Promise<unknown>
}) {
  let pending: Promise<unknown> | undefined
  return async (event: MessageEvent): Promise<void> => {
    const target = options.getFrame()?.contentWindow
    const requestId = event.data?.requestId
    if (
      !options.isEnabled() || !target || event.source !== target ||
      event.origin !== window.location.origin ||
      event.data?.type !== SESSION_CHECK ||
      typeof requestId !== 'string' || !/^[\w-]{16,80}$/.test(requestId)
    ) return

    let ok = false
    try {
      // Existing apiClient handles refresh locking and one-time refresh tokens.
      pending ??= options.refreshSession()
      const current = pending
      try {
        await current
        ok = true
      } finally {
        if (pending === current) pending = undefined
      }
    } catch {
      // The child receives only completion, never Main credentials or user data.
    }
    if (!options.isEnabled() || options.getFrame()?.contentWindow !== target) return
    target.postMessage({ type: SESSION_CHECKED, requestId, ok }, window.location.origin)
  }
}
