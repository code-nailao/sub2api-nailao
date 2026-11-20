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
  const resent = f.last('sub2api:media-studio-config').payload;
  assert.deepEqual(resent.connections, pending.payload.connections);
  assert.notEqual(resent.requestId, pending.payload.requestId);
  await f.ack({ requestId: pending.payload.requestId });
  assert.equal(f.frame.hidden, true);
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

test('Canvas can rehandshake the previous grant after a failed switch without authorizing again', async () => {
  let fail = false;
  const f = await fixture({ connection: (options, config) => fail
    ? { ok: false, status: 503 }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const original = f.last('sub2api:media-studio-config').payload;
  fail = true;
  f.select.value = String(f.select.children.at(-1).value);
  await f.select.onchange();
  const sent = f.canvasMessages.length;
  f.send({ type: 'sub2api:canvas-ready' });
  assert.equal(f.canvasMessages.length, sent + 1);
  const first = f.last('sub2api:media-studio-config').payload;
  assert.equal(first.selectedConnectionId, original.selectedConnectionId);
  assert.deepEqual(first.connections, original.connections);
  assert.notEqual(first.requestId, original.requestId);
  const deadline = [...f.timers.values()].find(({ delay }) => delay === 20000);
  deadline.callback();
  f.elements['#canvas-retry'].onclick();
  assert.notEqual(f.last('sub2api:media-studio-config').payload.requestId, first.requestId);
  await f.ack({ requestId: first.requestId });
  assert.equal(f.frame.hidden, true);
  await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
  assert.equal(connections(f).length, 2);
});

test('Canvas ignores a late restore conflict after a newer restore of the same grant succeeds', async () => {
  const first = Promise.withResolvers(), second = Promise.withResolvers();
  const restores = [first, second];
  const f = await fixture({ groups: [], custom: 'configured', connection: (options, config) => options.body.restore_only
    ? restores.shift().promise.then((result) => result ?? { ok: true, status: 200, data: config(options.body.group_id) })
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const connectionId = `group-${CUSTOM_ID}`;
  const requestIds = ['00000000-0000-4000-8000-000000000020', '00000000-0000-4000-8000-000000000021'];
  for (const requestId of requestIds) f.send({ type: 'sub2api:canvas-restore-connection', connectionId, requestId });
  second.resolve();
  await flush();
  assert.equal(f.last('sub2api:media-studio-config').payload.requestId, requestIds[1]);
  first.resolve({ ok: false, status: 409, data: { error: 'custom_connection_needs_update' } });
  await flush();
  assert.equal(f.last('sub2api:canvas-restore-error'), undefined);
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
});

test('Canvas keeps the resave action usable before and after a different group authorization times out', async () => {
  const hung = Promise.withResolvers();
  let switching = false;
  const f = await fixture({ custom: 'configured', connection: (options, config) => {
    if (options.body.group_id === CUSTOM_ID) return { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } };
    return switching ? hung.promise : { ok: true, status: 200, data: config(options.body.group_id) };
  } });
  f.loaded(); await f.ack();
  const previous = f.select.value;
  const other = f.select.children.find(({ value }) => String(value) !== previous && value !== CUSTOM_ID);
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  switching = true;
  f.select.value = String(other.value);
  const pending = f.select.onchange();
  const retry = f.elements['#canvas-retry'];
  assert.equal(retry.disabled, false);
  retry.onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  [...f.timers.values()].find(({ delay }) => delay === 20000).callback();
  assert.equal(retry.textContent, '打开自定义连接');
  assert.equal(retry.disabled, false);
  retry.onclick();
  assert.equal(f.settings.length, 2);
  assert.equal(f.select.value, previous);
  hung.resolve({ ok: false, status: 503 });
  await pending;
  assert.match(f.elements['#message'].textContent, /连接用时较长/);
});

test('Canvas can save from the resave dialog while an earlier switch times out', async () => {
  const hung = Promise.withResolvers();
  let saved = false, switching = false;
  const f = await fixture({ custom: 'configured', connection: (options, config) => {
    const id = options.body.group_id;
    if (id === CUSTOM_ID && !saved) return { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } };
    return switching && id !== CUSTOM_ID ? hung.promise : { ok: true, status: 200, data: config(id) };
  } });
  f.loaded(); await f.ack();
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  const other = f.select.children.find(({ value }) => String(value) !== f.select.value && value !== CUSTOM_ID);
  switching = true;
  f.select.value = String(other.value);
  const pending = f.select.onchange();
  f.elements['#canvas-retry'].onclick();
  const finishSave = f.settings.at(-1).beginSave();
  [...f.timers.values()].find(({ delay }) => delay === 20000).callback();

  saved = true;
  finishSave(true);
  await flush();
  assert.equal(f.last('sub2api:media-studio-config').payload.selectedConnectionId, `group-${CUSTOM_ID}`);
  await f.ack();
  hung.resolve({ ok: false, status: 503 });
  await pending;
  assert.equal(f.document.body.dataset.state, 'ready');
});

test('Canvas save keeps its deadline through a poll error and rolls back a hung authorization', async () => {
  const groups = Promise.withResolvers(), authorization = Promise.withResolvers();
  let saved = false, pollFails = false;
  const f = await fixture({ custom: 'configured',
    groupsResponse: () => !saved ? undefined : pollFails ? { ok: false, status: 503 } : groups.promise,
    connection: (options, config) => options.body.group_id === CUSTOM_ID
      ? authorization.promise : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const previous = f.select.value;
  f.elements['#canvas-custom'].onclick();
  saved = true;
  f.settings.at(-1).onSaved(true);
  pollFails = true;
  const poll = [...f.timers.values()].find(({ delay }) => delay === 30000);
  await poll.callback();
  const deadline = [...f.timers.values()].find(({ delay }) => delay === 20000);
  assert.ok(deadline, 'a background poll must not cancel the save deadline');
  groups.resolve({ ok: true, status: 200, data: {
    groups: f.state.groups, custom: { enabled: true, configured: true, group_id: CUSTOM_ID },
  } });
  await flush();
  assert.equal(connections(f).at(-1).options.body.group_id, CUSTOM_ID);
  [...f.timers.values()].find(({ delay }) => delay === 20000).callback();
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.select.value, previous);
  assert.equal(f.frame.hidden, false);
  const retry = f.elements['#canvas-retry'];
  assert.equal(retry.disabled, false);
  retry.onclick();
  await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  authorization.resolve({ ok: false, status: 503 });
  await flush();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(connections(f).length, 2);
});

test('a stale custom Canvas connection offers settings and reconnects after saving', async () => {
  let stale = true;
  const f = await fixture({ groups: [], custom: 'configured', connection: (options, config) => stale
    ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  assert.equal(f.settings.length, 0);
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  stale = false;
  f.settings.at(-1).onSaved(true);
  await flush(); await flush();
  assert.equal(connections(f).at(-1).options.body.group_id, CUSTOM_ID);
  f.loaded(); await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
});

test('switching Canvas to a stale connection offers settings and preserves the acknowledged frame', async () => {
  const f = await fixture({ custom: 'configured', connection: (options, config) => options.body.group_id === CUSTOM_ID
    ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const previous = f.select.value;
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  assert.equal(f.select.value, previous);
  assert.equal(f.frame.hidden, false);
  f.state.custom = 'available';
  f.settings.at(-1).onSaved(false);
  await flush();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.select.value, previous);
});

test('a failed Canvas group poll after a stale switch keeps the settings action and prior canvas', async () => {
  let pollFails = false;
  const f = await fixture({ custom: 'configured',
    groupsResponse: () => pollFails ? { ok: false, status: 503 } : undefined,
    connection: (options, config) => options.body.group_id === CUSTOM_ID
      ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
      : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const previous = f.select.value;
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();
  pollFails = true;
  [...f.timers.values()].find(({ delay }) => delay === 30000).callback();
  await flush();
  assert.equal(f.calls.filter(({ path }) => path === '/api/canvas/groups').length, 2);

  const retry = f.elements['#canvas-retry'];
  retry.onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  assert.equal(retry.textContent, '打开自定义连接');
  assert.equal(f.elements['#canvas-status'].hidden, false);
  assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
  assert.equal(f.select.value, previous);
  assert.equal(f.frame.hidden, false);
  assert.equal(connections(f).length, 2);
});

test('Canvas keeps stale connection recovery through another failed switch until a connection succeeds', async () => {
  let networkFailure = false;
  const f = await fixture({ custom: 'configured', connection: (options, config) => {
    const id = options.body.group_id;
    if (id === CUSTOM_ID) return { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } };
    if (networkFailure) throw Error('连接网络中断');
    return { ok: true, status: 200, data: config(id) };
  } });
  f.loaded(); await f.ack();
  const previous = f.select.value;
  const alternate = f.select.children.map(({ value }) => String(value)).find((id) => id !== previous && id !== String(CUSTOM_ID));
  f.select.value = String(CUSTOM_ID);
  await f.select.onchange();

  networkFailure = true;
  f.select.value = alternate;
  await f.select.onchange();
  assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
  assert.match(f.elements['#message'].textContent, /连接网络中断/);
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');

  networkFailure = false;
  f.select.value = alternate;
  await f.select.onchange();
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
  assert.equal(f.frame.hidden, true);
  await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
});

test('saving Canvas settings clears the resave action even if the subsequent group refresh fails', async () => {
  let saved = false;
  const f = await fixture({ groups: [], custom: 'configured',
    groupsResponse: () => saved ? { ok: false, status: 503 } : undefined,
    connection: () => ({ ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }) });
  const retry = f.elements['#canvas-retry'];
  assert.equal(retry.textContent, '打开自定义连接');
  retry.onclick();
  saved = true;
  f.settings.at(-1).onSaved(true);
  await flush();
  assert.equal(retry.textContent, '重试连接');
  assert.equal(f.elements['#message'].textContent, '授权暂不可用，请稍后重试；不会自动换组或创建新任务。');
  assert.equal(f.frame.hidden, true);
});

test('a late Canvas ACK cannot reveal a connection after a restore conflict', async () => {
  for (const validateBeforeConflict of [false, true]) {
    const validation = Promise.withResolvers();
    const f = await fixture({ groups: [], custom: 'configured', session: () => validation.promise,
      connection: (options, config) => options.body.restore_only
        ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
        : { ok: true, status: 200, data: config(options.body.group_id) } });
    f.host();
    f.loaded();
    if (validateBeforeConflict) await f.ack();
    f.send({ type: 'sub2api:canvas-restore-connection', connectionId: `group-${CUSTOM_ID}`,
      requestId: '00000000-0000-4000-8000-000000000010' });
    await flush();
    assert.equal(f.frame.hidden, true);
    assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');

    validation.resolve();
    if (validateBeforeConflict) await flush();
    else await f.ack();
    assert.equal(f.frame.hidden, true);
    assert.equal(f.document.body.dataset.state, 'recovery');
    assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
    assert.equal(f.parentMessages.some(({ integrated }) => integrated === true), false);
  }
});

test('a hung Canvas group refresh after saving exposes a usable retry', async () => {
  const hungGroups = Promise.withResolvers();
  let saved = false;
  let waitForGroups = false;
  const f = await fixture({ groups: [], custom: 'configured',
    groupsResponse: () => waitForGroups ? hungGroups.promise : undefined,
    connection: (options, config) => saved
      ? { ok: true, status: 200, data: config(options.body.group_id) }
      : { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } } });
  const retry = f.elements['#canvas-retry'];
  retry.onclick();
  saved = waitForGroups = true;
  f.settings.at(-1).onSaved(true);
  assert.equal(f.frame.hidden, true);
  assert.equal(f.elements['#canvas-recovery'].hidden, false);
  const timeout = [...f.timers.values()].find(({ delay }) => delay === 20000);
  assert.ok(timeout, 'saving should arm the startup timeout during the group refresh');
  timeout.callback();
  assert.match(f.elements['#message'].textContent, /连接用时较长/);
  assert.equal(retry.textContent, '重试连接');
  assert.equal(retry.disabled, false);

  waitForGroups = false;
  retry.onclick();
  await flush();
  assert.equal(f.calls.filter(({ path }) => path === '/api/canvas/groups').length, 3);
  f.loaded(); await f.ack();
  assert.equal(f.document.body.dataset.state, 'ready');
  hungGroups.resolve({ ok: false, status: 503 });
  await flush();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
});

test('Canvas discards a group poll failure from an earlier connection generation', async () => {
  const oldPoll = Promise.withResolvers();
  let pollPending = false;
  const f = await fixture({ groupsResponse: () => pollPending ? oldPoll.promise : undefined });
  f.loaded(); await f.ack();
  pollPending = true;
  [...f.timers.values()].find(({ delay }) => delay === 30000).callback();
  f.select.value = String(f.select.children.at(-1).value);
  await f.select.onchange();
  await f.ack();
  const message = f.elements['#message'].textContent;

  oldPoll.resolve({ ok: false, status: 503 });
  await flush();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.elements['#message'].textContent, message);
  assert.equal(f.frame.hidden, false);
});

test('Canvas ignores a settings save result after a newer group connection', async () => {
  const f = await fixture({ custom: 'configured' });
  f.loaded(); await f.ack();
  f.elements['#canvas-custom'].onclick();
  const finishSave = f.settings.at(-1).beginSave();

  const alternate = String(f.select.children.find(({ value }) => String(value) !== f.select.value).value);
  f.select.value = alternate;
  await f.select.onchange();
  await f.ack();
  const before = f.calls.length;

  finishSave(true);
  await flush();
  assert.equal(f.calls.length, before);
  assert.equal(f.select.value, alternate);
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frame.hidden, false);
});

test('restoring a stale Canvas connection reports the failure and offers settings without opening a dialog', async () => {
  let stale = true;
  const f = await fixture({ custom: 'configured', connection: (options, config) => options.body.restore_only && stale
    ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  f.loaded(); await f.ack();
  const connectionId = `group-${CUSTOM_ID}`;
  const requestId = '00000000-0000-4000-8000-000000000000';
  f.send({ type: 'sub2api:canvas-restore-connection', connectionId, requestId });
  await flush();
  assert.equal(f.last('sub2api:canvas-restore-error').requestId, requestId);
  assert.match(f.elements['#message'].textContent, /重新保存自定义连接/);
  assert.equal(f.elements['#canvas-retry'].textContent, '打开自定义连接');
  assert.equal(f.frame.hidden, false);
  assert.equal(f.settings.length, 0);
  f.elements['#canvas-retry'].onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');

  stale = false;
  const nextRequestId = '00000000-0000-4000-8000-000000000001';
  f.send({ type: 'sub2api:canvas-restore-connection', connectionId, requestId: nextRequestId });
  await flush();
  const config = f.last('sub2api:media-studio-config').payload;
  assert.equal(config.requestId, nextRequestId);
  assert.equal(config.selectActive, false);
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
  assert.equal(f.frame.hidden, false);
});

test('other Canvas connection errors keep their existing hints and do not open settings', async () => {
  const generic = '授权暂不可用，请稍后重试；不会自动换组或创建新任务。';
  for (const [status, error, message] of [
    [409, 'custom_connection_unavailable', generic],
    [409, undefined, generic],
    [503, 'custom_connection_needs_update', generic],
    [403, 'custom_connection_needs_update', '画布授权已撤销或过期，请在主站 API 密钥页面核对。'],
    [401, 'custom_connection_needs_update', '登录已失效，请重新登录主站后打开画布。'],
  ]) {
    const f = await fixture({ custom: 'configured', connection: () => ({ ok: false, status, data: { error } }) });
    assert.equal(f.elements['#message'].textContent, message);
    assert.equal(f.elements['#canvas-retry'].textContent, '重试连接');
    assert.equal(f.settings.length, 0);
  }
});

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
