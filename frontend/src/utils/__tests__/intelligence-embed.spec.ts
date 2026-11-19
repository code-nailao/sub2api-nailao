import { describe, expect, it, vi } from 'vitest'
import { buildIntelligenceEmbedURL, createIntelligenceAppearanceBridge, isIntelligencePage } from '../intelligence-embed'

describe('intelligence embed boundary', () => {
  it('only identifies the reviewed same-origin page', () => {
    expect(isIntelligencePage('https://main.example/intelligence-tests?manage=1', 'https://main.example')).toBe(true)
    expect(isIntelligencePage('https://other.example/intelligence-tests', 'https://main.example')).toBe(false)
    expect(isIntelligencePage('https://user:secret@main.example/intelligence-tests', 'https://main.example')).toBe(false)
    expect(isIntelligencePage('https://main.example/studio', 'https://main.example')).toBe(false)
    expect(isIntelligencePage('bad-url', 'https://main.example')).toBe(false)
  })
  it('keeps appearance options but removes credentials and source tracking', () => {
    const url = new URL(buildIntelligenceEmbedURL('https://main.example/intelligence-tests?token=private&user_id=42&src_url=private&src_host=private&theme=dark&theme_color=teal&manage=1', 'zh-CN'))
    for (const key of ['token', 'user_id', 'src_url', 'src_host']) expect(url.searchParams.has(key)).toBe(false)
    expect(url.searchParams.get('theme')).toBe('dark')
    expect(url.searchParams.get('theme_color')).toBe('teal')
    expect(url.searchParams.get('ui_mode')).toBe('embedded')
    expect(url.searchParams.get('manage')).toBe('1')
  })
  it('only sends appearance to its exact frame and never auth state', () => {
    const target = { postMessage: vi.fn() }
    const bridge = createIntelligenceAppearanceBridge({
      getFrame: () => ({ contentWindow: target }) as unknown as HTMLIFrameElement,
      isEnabled: () => true,
      getState: () => ({ color: 'yellow', mode: 'dark', isDark: true, storageAvailable: false }),
    })
    bridge.receive({ origin: 'https://evil.example', source: target, data: { type: 'sources:intelligence-appearance-ready' } } as unknown as MessageEvent)
    bridge.receive({ origin: window.location.origin, source: {}, data: { type: 'sources:intelligence-appearance-ready' } } as MessageEvent)
    expect(target.postMessage).not.toHaveBeenCalled()
    bridge.send()
    expect(target.postMessage).toHaveBeenCalledWith({ type: 'sources:intelligence-appearance', color: 'yellow', mode: 'dark' }, window.location.origin)
  })
})
