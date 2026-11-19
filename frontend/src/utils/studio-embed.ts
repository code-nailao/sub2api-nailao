export type StudioShellMode = 'embedded' | 'standalone'

function isWorkspacePage(raw: string, origin: string, path: '/studio' | '/canvas'): boolean {
  try {
    const url = new URL(raw)
    return !url.username && !url.password && url.origin === origin && url.pathname === path
  } catch { return false }
}

export function isStudioPage(raw: string, origin: string): boolean {
  return isWorkspacePage(raw, origin, '/studio')
}

export function isCanvasPage(raw: string, origin: string): boolean {
  return isWorkspacePage(raw, origin, '/canvas')
}

/** Studio uses the existing same-origin session; credentials never belong in its URL. */
export function buildStudioEmbedURL(raw: string): string {
  const url = new URL(raw)
  for (const key of ['token', 'user_id', 'src_url', 'src_host', 'studio_view']) url.searchParams.delete(key)
  url.searchParams.set('ui_mode', 'embedded')
  return url.toString()
}

export function buildStudioStandaloneURL(path: string, origin: string): string {
  if (!/^\/custom\/[^/?#]+$/.test(path)) return ''
  const url = new URL(path, origin)
  url.searchParams.set('studio_view', 'standalone')
  return url.toString()
}

export function createStudioHostBridge(options: {
  getFrame: () => HTMLIFrameElement | null
  isEnabled: () => boolean
  getState: () => { shellMode: StudioShellMode; standaloneUrl: string; theme: 'light' | 'dark'; lang: string }
  onApplied: (ready: boolean) => void
  onRecovery?: (needed: boolean) => void
}) {
  let revision = 0
  let pending: { requestId: string; revision: number; shellMode: StudioShellMode } | undefined
  let timeout: ReturnType<typeof setTimeout> | undefined
  const reset = () => {
    pending = undefined
    clearTimeout(timeout)
    options.onApplied(false)
    options.onRecovery?.(false)
  }
  const send = () => {
    const target = options.getFrame()?.contentWindow
    if (!options.isEnabled() || !target) { reset(); return }
    const state = options.getState()
    pending = { requestId: crypto.randomUUID(), revision: ++revision, shellMode: state.shellMode }
    clearTimeout(timeout)
    // Recovery controls remain available without switching the page shell.
    timeout = setTimeout(() => { options.onApplied(false); options.onRecovery?.(true) }, 20000)
    target.postMessage({ type: 'sources:studio-host-config', version: 1, ...pending, ...state }, window.location.origin)
  }
  return {
    send,
    reset,
    dispose: reset,
    receive(event: MessageEvent) {
      if (!options.isEnabled() || event.origin !== window.location.origin || event.source !== options.getFrame()?.contentWindow || event.data?.version !== 1) return
      if (event.data.type === 'sources:studio-host-ready') { send(); return }
      if (event.data.type !== 'sources:studio-host-config-applied' || !pending) return
      if (event.data.requestId !== pending.requestId || event.data.revision !== pending.revision || event.data.shellMode !== pending.shellMode) return
      clearTimeout(timeout)
      options.onApplied(event.data.integrated === true)
      options.onRecovery?.(event.data.integrated !== true)
    },
  }
}
