import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';

const source = await readFile(new URL('./internal/portal/web/studio.js', import.meta.url), 'utf8');
const roundSource = await readFile(new URL('./internal/portal/web/request-round.js', import.meta.url), 'utf8');
const flush = () => new Promise((resolve) => setImmediate(resolve));
const initialGroups = [{ id: 26, name: 'Images' }, { id: 27, name: 'Grok' }];

export const CUSTOM_ID = 9000000000000001, SETUP_ID = 9000000000000002, MANAGE_ID = 9000000000000003;

// custom: 'off' | 'available' | 'configured'; admin adds the group settings entry.
export async function createStudioFixture({ remembered, groupList = initialGroups, connection, session, groupsResponse, custom = 'off', admin = false, waitForBootstrap = true,
  origin = 'https://main.example', studioOrigin = 'https://studio.example' } = {}) {
  const window = new EventTarget();
  const parentMessages = [], studioMessages = [], calls = [];
  const parent = { postMessage: (data, target) => { assert.equal(target, origin); parentMessages.push(data); } };
  window.parent = parent;
  let frameSets = 0;
  function makeFrame(src = '') {
    const contentWindow = { postMessage: (data, target) => { assert.equal(target, studioOrigin); studioMessages.push(data); } };
    const element = Object.assign(new EventTarget(), { contentWindow, hidden: true,
      cloneNode: () => makeFrame(src), replaceWith: (next) => { frame = elements['#studio'] = next; frameSets++; } });
    Object.defineProperty(element, 'src', { get: () => src, set: (value) => { src = value; frameSets++; } });
    return element;
  }
  let frame = makeFrame();
  const select = { value: '', disabled: false, children: [], replaceChildren() { this.children = []; }, append(option) { this.children.push(option); } };
  const message = { textContent: '' }, keys = {}, lock = {}, reconnect = { hidden: true, disabled: false };
  const loadingPanel = {}, statusTitle = {}, connectionBar = {}, recoveryActions = {}, retry = {}, login = {};
  const customButton = { hidden: true }, manageButton = { hidden: true }, ownKeyButton = { hidden: true }, manageRecoveryButton = { hidden: true };
  const settings = [], state = { custom, admin, groupList, reloads: 0 };
  const connectionMenu = Object.assign(new EventTarget(), { open: false, querySelector: () => ({ focus() {} }) });
  const elements = { '#studio': frame, '#groups': select, '#message': message, '#keys': keys, '#lock': lock, '#reconnect': reconnect, '#loading-panel': loadingPanel, '#status-title': statusTitle, '#connection-bar': connectionBar, '#recovery-actions': recoveryActions, '#retry': retry, '#login': login, '#connection-menu': connectionMenu,
    '#custom-connection': customButton, '#manage-groups': manageButton, '#use-own-key': ownKeyButton, '#manage-groups-recovery': manageRecoveryButton };
  const document = { body: { dataset: { state: 'loading' }, setAttribute(name, value) { this[name] = value; } }, documentElement: { style: {}, dataset: {} }, visibilityState: 'visible', querySelector: (selector) => elements[selector], createElement: (tag) => { assert.equal(tag, 'option'); return {}; } };
  const values = new Map(remembered ? [['sources:studio:last-group:9', remembered]] : []);
  const timers = new Map();
  let timerID = 0, locked = false;
  const context = vm.createContext({ window, document, URL, URLSearchParams, crypto: webcrypto,
    location: Object.assign(new URL(origin + '/studio?ui_mode=embedded'), { reload: () => { state.reloads++; } }),
    localStorage: { getItem: (key) => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: (key) => values.delete(key) },
    setInterval: (callback, delay) => { timers.set(++timerID, { callback, delay }); return timerID; },
    clearInterval: (id) => timers.delete(id),
    setTimeout: (callback, delay) => { timers.set(++timerID, { callback, delay }); return timerID; },
    clearTimeout: (id) => timers.delete(id),
  });
  const config = (id) => ({ version: 2, surface: 'image_studio', selectedConnectionId: `group-${id}`, authorizationName: `生图 · ${id}` });
  const auth = new vm.SyntheticModule(['authorizedJSON', 'clearSession', 'currentToken'], function () {
    this.setExport('authorizedJSON', async (path, options) => {
      calls.push({ path, options });
      if (path === '/api/studio/groups') return groupsResponse?.() ?? { ok: true, status: 200, data: { user_id: 9, groups: state.groupList, studio_url: studioOrigin, main_origin: origin, viewer: { displayName: 'Alice', balance: 0 },
        custom: { group_id: CUSTOM_ID, setup_id: SETUP_ID, enabled: state.custom !== 'off', configured: state.custom === 'configured' }, can_manage: state.admin, manage_id: MANAGE_ID } };
      if (connection) return connection(options, config);
      return { ok: true, status: 200, data: config(options.body.group_id) };
    });
    this.setExport('clearSession', () => { locked = true; window.dispatchEvent(new Event('sources:locked')); });
    this.setExport('currentToken', async () => { if (locked) throw Error('locked'); await session?.(); return 'never-sent'; });
  }, { context });
  // The dialogs need a real DOM; here they only record that they were opened.
  const dialogs = new vm.SyntheticModule(['openCustomSettings', 'openGroupSettings'], function () {
    this.setExport('openCustomSettings', ({ getGeneration, onSaved }) => {
      const beginSave = () => {
        const generation = getGeneration?.();
        return (configured) => onSaved(configured, generation);
      };
      settings.push({ kind: 'custom', beginSave, onSaved: (configured) => beginSave()(configured) });
    });
    this.setExport('openGroupSettings', (options) => settings.push({ kind: 'manage', ...options }));
  }, { context });
  const module = new vm.SourceTextModule(source, { context });
  const rounds = new vm.SourceTextModule(roundSource, { context });
  await module.link((specifier) => ({ './request-round.js': rounds, './session.js': auth, './studio-settings.js': dialogs })[specifier]);
  const evaluation = module.evaluate();
  if (waitForBootstrap) await evaluation;
  await flush();
  const send = (data, { from = frame.contentWindow, fromOrigin = studioOrigin } = {}) => window.dispatchEvent(Object.assign(new Event('message'), { data, source: from, origin: fromOrigin }));
  const host = (revision = 1, shellMode = 'standalone') => send({ type: 'sources:studio-host-config', version: 1, requestId: 'host-request-' + String(revision).padStart(8, '0'), revision, shellMode, standaloneUrl: origin + '/custom/images?studio_view=standalone', theme: 'dark', lang: 'zh' }, { from: parent, fromOrigin: origin });
  const last = (type) => studioMessages.filter((item) => item.type === type).at(-1);
  const ackConnection = async () => {
    const data = last('sub2api:image-studio-config');
    assert.ok(data);
    send({ type: 'sub2api:image-studio-config-applied', payload: { status: 'applied', profileId: `sub2api-image-studio-${data.payload.selectedConnectionId}`, ...(data.requestId ? { requestId: data.requestId, revision: data.revision } : {}) } });
    await flush();
  };
  const ackShell = async () => {
    const data = last('sub2api:image-studio-shell-config');
    assert.ok(data);
    send({ type: 'sub2api:image-studio-shell-config-applied', version: 1, requestId: data.requestId, revision: data.revision });
    await flush();
    return data;
  };
  const integrate = async () => {
    host();
    if (last('sub2api:image-studio-config')) await ackConnection();
    send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
    return ackShell();
  };
  return { window, parent, get frame() { return frame; }, select, reconnect, message, document, calls, loadingPanel, statusTitle, connectionBar, recoveryActions, retry, login, connectionMenu, studioMessages, parentMessages, send, host, last, ackConnection, ackShell, integrate, timers,
    customButton, manageButton, ownKeyButton, manageRecoveryButton, settings, state, values,
    get frameSets() { return frameSets; }, get locked() { return locked; } };
}
