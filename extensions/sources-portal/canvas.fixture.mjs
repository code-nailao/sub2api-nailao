import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';
import { Element } from './dialog.fixture.mjs';

const source = await readFile(new URL('./internal/portal/web/canvas.js', import.meta.url), 'utf8');
const roundSource = await readFile(new URL('./internal/portal/web/request-round.js', import.meta.url), 'utf8');
const settingsSource = await readFile(new URL('./internal/portal/web/studio-settings.js', import.meta.url), 'utf8');
const sessionSource = await readFile(new URL('./internal/portal/web/session.js', import.meta.url), 'utf8');
const fixtureToken = (version) => `fixture.${Buffer.from(JSON.stringify({ user_id: 9, sid: 'fixture-session', version })).toString('base64url')}.fixture`;
const flush = () => new Promise((resolve) => setImmediate(resolve));
export const CUSTOM_ID = 9000000000000001;

// custom: 'off' | 'available' | 'configured'; admin shows the group settings button.
export async function createCanvasFixture({ groups = [{ id: 26, name: 'Images' }, { id: 27, name: 'Grok' }], connection, session, groupsResponse, standalone = false, custom = 'off', admin = false, settingsRequest, useSessionTransport = Boolean(settingsRequest) } = {}) {
  const origin = 'https://main.example', canvasOrigin = 'https://canvas.example';
  const parentMessages = [], canvasMessages = [], calls = [], frames = [];
  const settings = [], state = { custom, admin, groups };
  const window = new EventTarget();
  let token = fixtureToken(1);
  const parent = standalone ? window : { postMessage: (data, target) => {
    assert.equal(target, origin);
    parentMessages.push(data);
    if (useSessionTransport && data.type === 'sub2api:session-check') {
      token = fixtureToken(2);
      message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: true }, parent, origin);
    }
  } };
  window.parent = parent;
  function makeFrame() {
    const target = { postMessage: (data, targetOrigin) => { assert.equal(targetOrigin, canvasOrigin); canvasMessages.push(data); } };
    const f = Object.assign(new EventTarget(), { contentWindow: target, hidden: true, frameSets: 0, source: '', title: '', referrerPolicy: '',
      getAttribute(name) { return name === 'src' ? this.source || null : null; },
      replaceWith(next) { elements['#studio'] = next; } });
    Object.defineProperty(f, 'src', { get() { return f.source; }, set(value) { f.source = value; f.frameSets++; } });
    frames.push(f); return f;
  }
  const frame = makeFrame();
  const select = { value: '', disabled: false, children: [], replaceChildren() { this.children = []; this.value = ''; }, append(option) { this.children.push(option); if (!this.value) this.value = String(option.value); } };
  const elements = { '#studio': frame, '#groups': select };
  for (const name of ['message','canvas-controls','canvas-status','canvas-status-title','canvas-recovery','canvas-retry','canvas-login','canvas-open','canvas-title','canvas-home','keys','lock','canvas-custom','canvas-manage','canvas-own-key']) elements['#'+name] = { hidden: true, disabled: false, textContent: '' };
  const menu = Object.assign(new EventTarget(), { open: false, focused: false, querySelector() { return { focus: () => { menu.focused = true; } }; } });
  elements['#canvas-connection-menu'] = menu;
  const document = { body: Object.assign(new Element('body'), { dataset: { surface: 'canvas', state: 'loading', shellMode: 'embedded' } }),
    documentElement: { dataset: {}, style: {} }, visibilityState: 'visible', querySelector: (selector) => elements[selector],
    createElement: (tag) => tag === 'iframe' ? makeFrame() : new Element(tag) };
  const timers = new Map(); let timerID = 0, locked = false;
  const context = vm.createContext({ window, document, URL, URLSearchParams, crypto: webcrypto, location: new URL(origin + '/canvas?ui_mode=embedded'),
    Event, atob, history: { replaceState() {} }, localStorage: { getItem: () => token },
    fetch: async (url, options) => {
      if (url === '/public/runtime') return Response.json({ main_origin: origin });
      const request = { ...options, body: options.body && JSON.parse(options.body) };
      const result = await respond(url.pathname, request);
      return Response.json(result.data ?? null, { status: result.status });
    },
    setInterval: (callback, delay) => { timers.set(++timerID, { callback, delay }); return timerID; }, clearInterval: (id) => timers.delete(id),
    setTimeout: (callback, delay) => { timers.set(++timerID, { callback, delay }); return timerID; }, clearTimeout: (id) => timers.delete(id),
  });
  const config = (id) => ({ version: 2, surface: 'media', user_id: 9, main_origin: origin, selectedConnectionId: `group-${id}`, connections: [{ id: `group-${id}`, apiKey: 'fixture-key' }], capabilities: [{ id: `group-${id}`, groupId: id }] });
  async function respond(path, options) {
    calls.push({ path, options });
    if (path === '/api/canvas/groups') return groupsResponse?.() ?? { ok: true, status: 200, data: { user_id: 9, canvas_url: canvasOrigin + '/infinite/canvas', main_origin: origin, groups: state.groups,
      custom: { group_id: CUSTOM_ID, enabled: state.custom !== 'off', configured: state.custom === 'configured' }, can_manage: state.admin } };
    if (path.startsWith('/api/studio/custom')) return settingsRequest(path, options);
    return connection ? connection(options, config) : { ok: true, status: 200, data: config(options.body.group_id) };
  }
  window.addEventListener('sources:locked', () => { locked = true; });
  const auth = useSessionTransport ? new vm.SourceTextModule(sessionSource, { context })
    : new vm.SyntheticModule(['authorizedJSON', 'clearSession', 'currentToken'], function () {
      this.setExport('authorizedJSON', respond);
      this.setExport('clearSession', () => window.dispatchEvent(new Event('sources:locked')));
      this.setExport('currentToken', async () => { if (locked) throw Error('locked'); await session?.(); return 'must-not-send-main-token'; });
    }, { context });
  // Use real settings and session code for tests that provide dialog HTTP responses.
  const dialogs = settingsRequest ? new vm.SourceTextModule(settingsSource, { context })
    : new vm.SyntheticModule(['openCustomSettings', 'openGroupSettings'], function () {
      this.setExport('openCustomSettings', ({ captureConnection, onSaved }) => {
        const beginSave = () => {
          const current = captureConnection();
          return (configured) => { if (current()) onSaved(configured); };
        };
        settings.push({ kind: 'custom', beginSave, onSaved: (configured) => beginSave()(configured) });
      });
      this.setExport('openGroupSettings', (options) => settings.push({ kind: 'manage', ...options }));
    }, { context });
  const module = new vm.SourceTextModule(source, { context });
  const rounds = new vm.SourceTextModule(roundSource, { context });
  await module.link((specifier) => ({ './request-round.js': rounds, './session.js': auth, './studio-settings.js': dialogs })[specifier]);
  await module.evaluate();
  await flush();
  function message(data, source, origin) {
    window.dispatchEvent(Object.assign(new Event('message'), { data, source, origin }));
  }
  const send = (data, { from = elements['#studio'].contentWindow, fromOrigin = canvasOrigin } = {}) => message(data, from, fromOrigin);
  const host = (revision = 1, shellMode = 'embedded', changes = {}) => send({ type: 'sources:studio-host-config', version: 1, requestId: 'host-request-' + String(revision).padStart(8, '0'), revision, shellMode, standaloneUrl: origin + '/custom/canvas?studio_view=standalone', theme: 'dark', lang: 'zh-CN', ...changes }, { from: parent, fromOrigin: origin });
  const last = (type) => canvasMessages.filter((m) => m.type === type).at(-1);
  const loaded = () => elements['#studio'].dispatchEvent(new Event('load'));
  const ack = async (extra = {}, sender) => {
    const data = last('sub2api:media-studio-config'); assert.ok(data);
    send({ type: 'sub2api:media-studio-config-applied', payload: { status: 'applied', profileId: `sub2api-media-studio-${data.payload.selectedConnectionId}`, requestId: data.payload.requestId, ...extra } }, sender); await flush();
  };
  return { window, parent, document, elements, frame, select, frames, calls, timers, parentMessages, canvasMessages, host, last, loaded, ack, send, settings, state,
    get locked() { return locked; } };
}
