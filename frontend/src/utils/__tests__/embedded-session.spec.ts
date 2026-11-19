import { afterEach, describe, expect, it, vi } from 'vitest'
import { createEmbeddedSessionCheckHandler, isEmbeddedSessionPage } from '../embedded-session'

afterEach(() => {
  document.body.replaceChildren()
  vi.restoreAllMocks()
})

function fixture() {
  const frame = document.createElement('iframe')
  document.body.append(frame)
  const reply = vi.spyOn(frame.contentWindow!, 'postMessage')
  const refreshSession = vi.fn().mockResolvedValue(undefined)
  const isEnabled = vi.fn(() => true)
  const getFrame = vi.fn<() => HTMLIFrameElement | null>(() => frame)
  const handle = createEmbeddedSessionCheckHandler({ getFrame, isEnabled, refreshSession })
  const message = (overrides: MessageEventInit = {}) => new MessageEvent('message', {
    origin: window.location.origin,
    source: frame.contentWindow,
    data: { type: 'sub2api:session-check', requestId: 'fixture-request-12345' },
    ...overrides,
  })
  return { frame, reply, refreshSession, isEnabled, getFrame, handle, message }
}

describe('embedded native session check', () => {
  it('only enables reviewed same-origin extension pages', () => {
    const origin = 'https://main.example'
    for (const path of ['/admin/home', '/admin/balances', '/studio', '/canvas']) {
      expect(isEmbeddedSessionPage(origin + path + '?theme=dark', origin)).toBe(true)
    }
    for (const raw of ['https://other.example/studio', origin + '/studio/other', origin + '/admin/settings', 'not-a-url']) {
      expect(isEmbeddedSessionPage(raw, origin)).toBe(false)
    }
  })

  it('rejects other frames, origins, protocols, and invalid request identifiers', async () => {
    const f = fixture()
    for (const overrides of [
      { origin: 'https://other.example' },
      { source: window },
      { data: { type: 'other', requestId: 'fixture-request-12345' } },
      { data: { type: 'sub2api:session-check', requestId: '' } },
      { data: { type: 'sub2api:session-check', requestId: 'x'.repeat(81) } },
    ]) await f.handle(f.message(overrides))
    f.isEnabled.mockReturnValue(false)
    await f.handle(f.message())
    expect(f.refreshSession).not.toHaveBeenCalled()
    expect(f.reply).not.toHaveBeenCalled()
  })

  it('uses the existing Main refresh path and returns no credentials or user data', async () => {
    const f = fixture()
    f.refreshSession.mockResolvedValue({ id: 9, access_token: 'never-message-this' })
    await f.handle(f.message())
    expect(f.refreshSession).toHaveBeenCalledTimes(1)
    expect(f.reply).toHaveBeenCalledTimes(1)
    expect(f.reply).toHaveBeenCalledWith({
      type: 'sub2api:session-checked', requestId: 'fixture-request-12345', ok: true,
    }, window.location.origin)
  })

  it('coalesces concurrent checks while replying to each request identifier', async () => {
    const f = fixture()
    let finish!: () => void
    f.refreshSession.mockReturnValue(new Promise<void>((resolve) => { finish = resolve }))
    const first = f.handle(f.message())
    const second = f.handle(f.message({ data: { type: 'sub2api:session-check', requestId: 'fixture-request-67890' } }))
    expect(f.refreshSession).toHaveBeenCalledTimes(1)
    finish()
    await Promise.all([first, second])
    expect(f.reply).toHaveBeenCalledTimes(2)
    expect(f.reply.mock.calls.map(([data]) => data.requestId)).toEqual(['fixture-request-12345', 'fixture-request-67890'])
  })

  it('does not acknowledge an iframe removed during refresh', async () => {
    const f = fixture()
    let finish!: () => void
    f.refreshSession.mockReturnValue(new Promise<void>((resolve) => { finish = resolve }))
    const pending = f.handle(f.message())
    f.getFrame.mockReturnValue(null)
    finish()
    await pending
    expect(f.reply).not.toHaveBeenCalled()
  })

  it('reports failure without forwarding private backend errors', async () => {
    const f = fixture()
    f.refreshSession.mockRejectedValue(Error('private backend context'))
    await f.handle(f.message())
    expect(f.reply).toHaveBeenCalledTimes(1)
    expect(f.reply).toHaveBeenCalledWith({
      type: 'sub2api:session-checked', requestId: 'fixture-request-12345', ok: false,
    }, window.location.origin)
  })
})
