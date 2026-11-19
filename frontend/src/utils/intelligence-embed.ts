import type { AppearanceState } from '../../../extensions/sources-brand/appearance'

export const INTELLIGENCE_APPEARANCE = 'sources:intelligence-appearance'
export const INTELLIGENCE_APPEARANCE_READY = 'sources:intelligence-appearance-ready'

/** This reviewed same-origin page has its own authenticated data boundary. */
export function isIntelligencePage(raw: string, origin: string): boolean {
  try {
    const url = new URL(raw)
    return !url.username && !url.password && url.origin === origin && url.pathname === '/intelligence-tests'
  } catch { return false }
}

export function buildIntelligenceEmbedURL(raw: string, lang: string): string {
  const url = new URL(raw)
  for (const key of ['token', 'user_id', 'src_url', 'src_host']) url.searchParams.delete(key)
  url.searchParams.set('ui_mode', 'embedded')
  url.searchParams.set('lang', lang)
  return url.toString()
}

export function createIntelligenceAppearanceBridge(options: {
  getFrame: () => HTMLIFrameElement | null
  isEnabled: () => boolean
  getState: () => AppearanceState
}) {
  const send = () => {
    const target = options.getFrame()?.contentWindow
    if (!options.isEnabled() || !target) return
    const { color, mode } = options.getState()
    // No identity or credentials; the frame URL never changes on theme updates.
    target.postMessage({ type: INTELLIGENCE_APPEARANCE, color, mode }, window.location.origin)
  }
  return {
    send,
    receive(event: MessageEvent) {
      if (
        options.isEnabled() &&
        event.origin === window.location.origin &&
        event.source === options.getFrame()?.contentWindow &&
        event.data?.type === INTELLIGENCE_APPEARANCE_READY
      ) send()
    },
  }
}
