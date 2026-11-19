import { afterEach, describe, expect, it, vi } from 'vitest'
import { buildStudioEmbedURL, buildStudioStandaloneURL, createStudioHostBridge, isStudioPage, isCanvasPage } from '../studio-embed'

afterEach(() => { vi.useRealTimers(); document.body.replaceChildren() })

describe('native Studio host', () => {
  it('only recognizes the exact trusted path and keeps credentials out of both URLs', () => {
    const origin = 'https://main.example'
    expect(isStudioPage(origin + '/studio?lang=zh', origin)).toBe(true)
    for (const url of ['https://other.example/studio', origin + '/studio/other', 'https://user:password@main.example/studio', 'bad']) expect(isStudioPage(url, origin)).toBe(false)
    const embed = new URL(buildStudioEmbedURL(origin + '/studio?token=secret&user_id=7&src_url=private&src_host=private&studio_view=standalone'))
    expect([...embed.searchParams]).toEqual([['ui_mode', 'embedded']])
    expect(buildStudioStandaloneURL('/custom/images', origin)).toBe(origin + '/custom/images?studio_view=standalone')
    expect(buildStudioStandaloneURL('//other.example', origin)).toBe('')
    expect(buildStudioStandaloneURL('/admin/settings', origin)).toBe('')
  })

  it('recognizes only the exact same-origin Canvas and strips legacy credential query fields', () => {
    const origin = 'https://main.example'
    expect(isCanvasPage(origin + '/canvas', origin)).toBe(true)
    expect(isCanvasPage(origin + '/canvas?theme=dark', origin)).toBe(true)
    for (const raw of ['https://other.example/canvas', origin + '/canvas/other', origin + '/canvas/', 'https://user:password@main.example/canvas', 'bad', origin + '/studio']) expect(isCanvasPage(raw, origin)).toBe(false)
    expect(new URL(buildStudioEmbedURL(origin + '/canvas?token=old&user_id=9&src_url=private&src_host=private')).search).toBe('?ui_mode=embedded')
  })

  it('hides chrome only after an exact end-to-end ACK and rejects stale and forged replies', () => {
    vi.useFakeTimers()
    const frame = document.createElement('iframe')
    document.body.append(frame)
    const send = vi.spyOn(frame.contentWindow!, 'postMessage')
    const applied = vi.fn()
    const bridge = createStudioHostBridge({ getFrame: () => frame, isEnabled: () => true,
      getState: () => ({ shellMode: 'standalone', standaloneUrl: window.location.origin + '/custom/images?studio_view=standalone', theme: 'dark', lang: 'zh' }), onApplied: applied })
    bridge.send()
    const first = send.mock.calls[0][0]
    const event = (data: unknown, extra: MessageEventInit = {}) => new MessageEvent('message', { origin: window.location.origin, source: frame.contentWindow, data, ...extra })
    const ack = { ...first, type: 'sources:studio-host-config-applied', integrated: true }
    bridge.receive(event(ack, { source: window }))
    bridge.receive(event(ack, { origin: 'https://other.example' }))
    bridge.receive(event({ ...ack, revision: first.revision + 1 }))
    expect(applied).not.toHaveBeenCalled()
    bridge.receive(event(ack))
    expect(applied).toHaveBeenLastCalledWith(true)
    bridge.send()
    applied.mockClear()
    bridge.receive(event(ack))
    expect(applied).not.toHaveBeenCalled()
    vi.advanceTimersByTime(20000)
    expect(applied).toHaveBeenLastCalledWith(false)
    bridge.dispose()
  })

  it('sends appearance as messages without changing the frame URL or sending credentials', () => {
    vi.useFakeTimers()
    const target = { postMessage: vi.fn() }
    const frame = { contentWindow: target, src: 'https://main.example/studio?ui_mode=embedded' } as unknown as HTMLIFrameElement
    let theme: 'light' | 'dark' = 'light'
    const bridge = createStudioHostBridge({ getFrame: () => frame, isEnabled: () => true,
      getState: () => ({ shellMode: 'embedded', standaloneUrl: window.location.origin + '/custom/images?studio_view=standalone', theme, lang: 'zh' }), onApplied: vi.fn() })
    bridge.send()
    theme = 'dark'
    bridge.send()
    expect(frame.src).toBe('https://main.example/studio?ui_mode=embedded')
    expect(target.postMessage.mock.calls[1][0].theme).toBe('dark')
    expect(JSON.stringify(target.postMessage.mock.calls)).not.toMatch(/auth_token|refresh_token|Bearer/)
    bridge.dispose()
  })
})
