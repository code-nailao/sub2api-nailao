import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createCanvasFixture as fixture, CUSTOM_ID } from './canvas.fixture.mjs';
const flush = () => new Promise((resolve) => setImmediate(resolve));

test('Canvas hides repeated title/helper chrome from first paint and reveals one compact connection row after ACK', async () => {
  const f = await fixture(); f.host();
  assert.equal(f.elements['#canvas-controls'].hidden, true);
  assert.equal(f.elements['#canvas-title'].hidden, true);
  assert.equal(f.parentMessages.some(m => m.integrated === true), false);
  f.loaded(); await f.ack();
  assert.equal(f.frame.hidden, false);
  assert.equal(f.elements['#canvas-controls'].hidden, false);
  assert.equal(f.elements['#canvas-status'].hidden, true);
  assert.equal(f.elements['#canvas-title'].hidden, true);
  assert.equal(f.parentMessages.at(-1).integrated, true);
  assert.equal(f.elements['#canvas-open'].href, 'https://main.example/custom/canvas?studio_view=standalone');
  assert.equal(f.elements['#keys'].href, 'https://main.example/keys');
  assert.equal(JSON.stringify(f.parentMessages).includes('fixture-key'), false);
});
test('Main standalone keeps minimal title/back navigation, while managed child URLs carry no Main credential', async () => {
  const f = await fixture(); f.host(1, 'standalone'); f.loaded(); await f.ack();
  assert.equal(f.elements['#canvas-title'].hidden, false);
  assert.equal(f.elements['#canvas-home'].hidden, false);
  assert.equal(f.elements['#canvas-open'].hidden, true);
  assert.equal(new URL(f.frame.src).searchParams.get('ui_mode'), 'embedded');
  assert.equal(f.frame.src.includes('token'), false);
});
test('direct Canvas shell still offers navigation without a Main parent', async () => {
  const f = await fixture({ standalone: true }); f.loaded(); await f.ack();
  assert.equal(f.elements['#canvas-title'].hidden, false);
  assert.equal(f.elements['#canvas-home'].hidden, false);
  assert.equal(f.parentMessages.length, 0);
});
test('same-account group change keeps the actual iframe and already granted connections', async () => {
  const f = await fixture(); f.host(); f.loaded(); await f.ack();
  f.select.value = '27'; await f.select.onchange();
  assert.equal(f.frames.length, 1); assert.equal(f.frame.frameSets, 1);
  const sent = f.last('sub2api:media-studio-config');
  assert.equal(sent.payload.selectedConnectionId, 'group-27');
  assert.equal(sent.payload.connections.map(c => c.id).join(','), 'group-26,group-27');
  await f.ack(); assert.equal(f.frame.hidden, false);
  assert.equal(f.calls.filter(c => c.path.endsWith('/connection')).length, 2);
});
test('failed group selection returns to the acknowledged group without destroying the frame', async () => {
  let fail = false;
  const f = await fixture({ connection: (options, config) => fail ? { ok: false, status: 503 } : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack(); fail = true;
  f.select.value = '27'; await f.select.onchange();
  assert.equal(f.select.value, '26'); assert.equal(f.frame.hidden, false); assert.equal(f.frame.frameSets, 1);
  assert.equal(f.document.body.dataset.state, 'recovery');
});
test('forged/stale host configs cannot alter the shell or the independent link', async () => {
  const f = await fixture(); f.host(2);
  f.host(1, 'standalone'); assert.equal(f.elements['#canvas-title'].hidden, true);
  f.host(3, 'standalone', { standaloneUrl: 'https://evil.example/custom/canvas?studio_view=standalone' });
  f.host(3, 'standalone', { standaloneUrl: 'https://main.example/custom/canvas?studio_view=standalone&token=private' });
  assert.equal(f.elements['#canvas-title'].hidden, true);
  f.send({ type: 'sources:studio-host-config', version: 1, requestId: 'forged-host-request', revision: 8, shellMode: 'standalone', theme: 'light', standaloneUrl: 'https://main.example/custom/canvas?studio_view=standalone' });
  assert.equal(f.elements['#canvas-title'].hidden, true);
});
test('connection ACK rejects foreign origin, foreign window and stale request IDs', async () => {
  const f = await fixture(); f.host(); f.loaded();
  await f.ack({}, { fromOrigin: 'https://evil.example' }); await f.ack({}, { from: f.parent }); await f.ack({ requestId: 'stale-request' });
  assert.equal(f.frame.hidden, true); assert.equal(f.parentMessages.some(m => m.integrated), false);
  await f.ack(); assert.equal(f.frame.hidden, false);
});
test('theme/locale changes use a trusted appearance message and never navigate or reauthorize', async () => {
  const f = await fixture(); f.host(); f.loaded(); await f.ack();
  const calls = f.calls.length; f.host(2, 'embedded', { theme: 'light', lang: 'en' });
  assert.equal(f.frame.frameSets, 1); assert.equal(f.calls.length, calls);
  assert.equal(f.last('sources:canvas-appearance').theme, 'light');
  assert.equal(f.last('sources:canvas-appearance').lang, 'en-US');
  assert.equal(f.document.documentElement.dataset.theme, 'light');
});
test('lost connection ACK retries the same configuration without a second authorization POST', async () => {
  const f = await fixture(); f.host(); f.loaded();
  const pending = f.last('sub2api:media-studio-config'); const calls = f.calls.length;
  const timer = [...f.timers.values()].find(t => t.delay === 500);
  for (let i = 0; i < 31; i++) timer.callback();
  assert.equal(f.document.body.dataset.state, 'recovery');
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.last('sub2api:media-studio-config').payload.requestId, pending.payload.requestId);
  assert.equal(f.calls.length, calls); assert.equal(f.frame.frameSets, 1);
  await f.ack(); assert.equal(f.document.body.dataset.state, 'ready');
});
test('never-loaded Canvas exposes recovery rather than leaving a permanent empty frame', async () => {
  const f = await fixture();
  [...f.timers.values()].find(t => t.delay === 20000).callback();
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#canvas-recovery'].hidden, false);
  assert.equal(f.frame.hidden, true);
});
test('lock clears credentials/frame and cannot be reversed by late ACK or a host request', async () => {
  const f = await fixture(); f.host(); f.loaded(); await f.ack();
  f.elements['#lock'].onclick();
  assert.equal(f.frame.src, 'about:blank'); assert.equal(f.frame.hidden, true);
  assert.equal(f.parentMessages.at(-1).integrated, false);
  await f.ack(); f.host(3); assert.equal(f.frame.hidden, true);
  assert.equal(f.elements['#canvas-login'].hidden, false);
  assert.equal(f.elements['#canvas-retry'].hidden, true);
});
test('session failure at ACK does not reveal Canvas', async () => {
  const f = await fixture({ session: async () => { throw Error('expired'); } }); f.host(); f.loaded(); await f.ack();
  assert.equal(f.frame.hidden, true); assert.equal(f.parentMessages.some(m => m.integrated), false);
});
test('no groups and failed bootstrap make no authorization POST and remain recoverable', async () => {
  for (const settings of [{ groups: [] }, { groupsResponse: () => ({ ok: false, status: 503 }) }]) {
    const f = await fixture(settings);
    assert.equal(f.document.body.dataset.state, 'recovery'); assert.equal(f.frame.hidden, true);
    assert.equal(f.calls.filter(c => c.path.endsWith('/connection')).length, 0);
  }
});
test('authorization details retain keyboard Escape and restore focus to the summary', async () => {
  const f = await fixture(); const menu = f.elements['#canvas-connection-menu']; menu.open = true;
  menu.dispatchEvent(Object.assign(new Event('keydown'), { key: 'Escape' }));
  assert.equal(menu.open, false); assert.equal(menu.focused, true);
});
test('Canvas layout is isolated, responsive and keeps native controls instead of a permanent helper banner', async () => {
  const html = await readFile(new URL('./internal/portal/web/canvas.html', import.meta.url), 'utf8');
  const css = await readFile(new URL('./internal/portal/web/studio.css', import.meta.url), 'utf8');
  assert.match(html, /data-surface="canvas" data-state="loading"/);
  assert.match(html, /id="canvas-title" hidden/); assert.match(html, /<details id="canvas-connection-menu">/);
  assert.match(html, /id="canvas-open"[^>]+hidden/); assert.match(css, /body\[data-surface="canvas"\]>iframe\{flex:1/);
  assert.match(css, /@media\(max-width:560px\)/); assert.match(css, /prefers-reduced-motion:reduce/);
  assert.equal(html.includes('普通、可见、可撤销'), false);
});

const connections = (f) => f.calls.filter((call) => call.path.endsWith('/connection'));

test('a saved own-key connection is one more entry of the group menu', async () => {
  const f = await fixture({ custom: 'configured' }); f.host(); f.loaded(); await f.ack();
  assert.deepEqual(f.select.children.map((option) => option.value), [26, 27, CUSTOM_ID]);
  assert.equal(f.elements['#canvas-custom'].hidden, false);
  assert.equal(f.elements['#canvas-manage'].hidden, true);
  f.select.value = String(CUSTOM_ID); await f.select.onchange();
  assert.equal(connections(f).at(-1).options.body.group_id, CUSTOM_ID);
  assert.equal(f.last('sub2api:media-studio-config').payload.selectedConnectionId, `group-${CUSTOM_ID}`);
  await f.ack();
  assert.equal(f.frame.hidden, false);
  assert.equal(f.document.body.dataset.state, 'ready');
});

test('saving an own-key connection from the empty state connects to it', async () => {
  const f = await fixture({ groups: [], custom: 'available' });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.match(f.elements['#message'].textContent, /使用我的密钥/);
  assert.equal(f.elements['#canvas-own-key'].hidden, false);
  assert.equal(connections(f).length, 0);
  f.elements['#canvas-own-key'].onclick();
  assert.equal(f.settings.at(-1).kind, 'custom');
  f.state.custom = 'configured';
  f.settings.at(-1).onSaved(true);
  await flush(); await flush();
  assert.equal(connections(f).length, 1);
  assert.equal(connections(f)[0].options.body.group_id, CUSTOM_ID);
  f.loaded(); await f.ack();
  assert.equal(f.frame.hidden, false);
  // Removing it leaves nothing to connect to, and no second authorization.
  f.state.custom = 'available';
  f.settings.at(-1).onSaved(false);
  await flush(); await flush();
  assert.equal(f.select.children.length, 0);
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(connections(f).length, 1);
});

test('group settings refresh the menu without reconnecting the selected group', async () => {
  const f = await fixture({ admin: true }); f.host(); f.loaded(); await f.ack();
  assert.equal(f.elements['#canvas-manage'].hidden, false);
  f.elements['#canvas-connection-menu'].open = true;
  f.elements['#canvas-manage'].onclick();
  assert.equal(f.settings.at(-1).kind, 'manage');
  assert.equal(f.elements['#canvas-connection-menu'].open, false);
  const before = connections(f).length;
  f.state.groups = [...f.state.groups, { id: 28, name: 'Added' }];
  f.settings.at(-1).onSaved();
  await flush(); await flush();
  assert.deepEqual(f.select.children.map((option) => option.value), [26, 27, 28]);
  assert.equal(f.select.value, '26');
  assert.equal(connections(f).length, before);
  assert.equal(f.frame.hidden, false);
  // Removing the selected group moves to the first one that is left.
  f.state.groups = [{ id: 27, name: 'Grok' }];
  f.settings.at(-1).onSaved();
  await flush(); await flush();
  assert.equal(f.select.value, '27');
  assert.equal(connections(f).at(-1).options.body.group_id, 27);

  const user = await fixture();
  assert.equal(user.elements['#canvas-manage'].hidden, true);
  assert.equal(user.elements['#canvas-custom'].hidden, true);
  user.window.dispatchEvent(new Event('sources:locked'));
  user.elements['#canvas-custom'].onclick();
  user.elements['#canvas-manage'].onclick();
  assert.equal(user.settings.length, 0);
});

const staleConnection = { ok: false, status: 409,
  data: { error: 'custom_connection_needs_update', message: '这把密钥所在的分组已经变化，请重新打开“自定义连接”保存一次。' } };
const staleOnly = (options, config) => options.body.group_id === CUSTOM_ID
  ? staleConnection
  : { ok: true, status: 200, data: config(options.body.group_id) };

test('a stale own-key connection asks to resave and opens its dialog', async () => {
  const f = await fixture({ custom: 'configured', connection: staleOnly });
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#message'].textContent, staleConnection.data.message);
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.settings.at(-1).kind, 'custom');
});

test('a stale connection hint stays after the page acknowledgement', async () => {
  const f = await fixture({ custom: 'configured', connection: staleOnly });
  f.host(); f.loaded(); await f.ack();
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.equal(f.document.body.dataset.state, 'recovery');
  await f.ack();
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
});

test('a stale restore request asks to resave instead of only failing', async () => {
  const f = await fixture({ custom: 'configured', connection: (options, config) => options.body.restore_only === true
    ? staleConnection
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.host(); f.loaded(); await f.ack();
  f.send({ type: 'sub2api:canvas-restore-connection', connectionId: `group-${CUSTOM_ID}`, requestId: crypto.randomUUID() });
  await flush(); await flush();
  assert.equal(f.last('sub2api:canvas-restore-error')?.connectionId, `group-${CUSTOM_ID}`);
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
});

test('an unrelated 409 keeps the released wording', async () => {
  const f = await fixture({ connection: () => ({ ok: false, status: 409, data: { error: 'settings_changed' } }) });
  assert.equal(f.elements['#message'].textContent, '授权暂不可用，请稍后重试；不会自动换组或创建新任务。');
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
  assert.equal(f.settings.length, 0);
});

test('a normal failure after a stale connection keeps the resave entry', async () => {
  let failNormal = false;
  const f = await fixture({ custom: 'configured', connection: (options, config) => options.body.group_id === CUSTOM_ID
    ? staleConnection
    : failNormal ? { ok: false, status: 503 } : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  failNormal = true;
  f.select.value = '26';
  await f.select.onchange();
  assert.equal(f.elements['#message'].textContent, '授权暂不可用，请稍后重试；不会自动换组或创建新任务。');
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.settings.at(-1).kind, 'custom');
});

test('a new acknowledged configuration clears the resave entry', async () => {
  const f = await fixture({ custom: 'configured', connection: staleOnly });
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  f.select.value = '26';
  await f.select.onchange();
  await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
});

test('removing a stale custom connection clears the resave entry at once', async () => {
  const f = await fixture({ custom: 'configured', connection: staleOnly });
  f.host(); f.loaded(); await f.ack();
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  f.elements['#canvas-retry'].onclick();
  const dialog = f.settings.at(-1);
  assert.equal(dialog.kind, 'custom');
  f.state.custom = 'available';
  dialog.onSaved(false);
  await flush(); await flush();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
  assert.equal(f.elements['#canvas-recovery'].hidden, true);
  assert.equal(f.frame.hidden, false);
});

test('non-stale connection failures keep the released wording', async () => {
  const f = await fixture({ connection: () => ({ ok: false, status: 503 }) });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#message'].textContent, '授权暂不可用，请稍后重试；不会自动换组或创建新任务。');
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
  f.elements['#canvas-retry'].onclick();
  await flush();
  assert.equal(f.settings.length, 0);
  assert.equal(connections(f).length, 2);
});
