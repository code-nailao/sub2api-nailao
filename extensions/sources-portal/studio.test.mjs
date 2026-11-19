import test from 'node:test';
import assert from 'node:assert/strict';
import { createStudioFixture as fixture } from './studio.fixture.mjs';

const flush = () => new Promise((resolve) => setImmediate(resolve));

test('new Studio stays behind one loading state until both connection and shell ACK', async () => {
  const f = await fixture();
  f.host();
  await f.ackConnection();
  assert.equal(f.frame.hidden, true);
  assert.equal(f.document.body.dataset.state, 'loading');
  assert.equal(f.connectionBar.hidden, true);
  assert.notEqual(f.document.body.dataset.integrated, 'true');
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  const shell = f.last('sub2api:image-studio-shell-config');
  assert.equal(shell.shellMode, 'standalone');
  assert.equal(shell.viewer.balance, 0);
  f.send({ type: 'sub2api:image-studio-shell-config-applied', version: 1, requestId: shell.requestId, revision: shell.revision }, { fromOrigin: 'https://evil.example' });
  await flush();
  assert.notEqual(f.document.body.dataset.integrated, 'true');
  await f.ackShell();
  assert.equal(f.document.body.dataset.integrated, 'true');
  assert.equal(f.parentMessages.at(-1).integrated, true);
  assert.equal(f.parentMessages.at(-1).shellMode, 'standalone');
  assert.equal(new URL(f.frame.src).searchParams.get('ui_mode'), 'embedded');
});

test('group changes keep one iframe, fence requests/ACKs, and preserve the selected group on failure', async () => {
  let fail = false;
  const f = await fixture({ connection: (options, config) => fail ? { ok: false, status: 503 } : { ok: true, status: 200, data: config(options.body.group_id) } });
  const shell = await f.integrate();
  const request = { type: 'sub2api:image-studio-select-group', version: 1, requestId: 'selection-request-0001', revision: shell.revision, groupId: 27 };
  f.send(request, { fromOrigin: 'https://evil.example' });
  await flush();
  assert.equal(f.calls.filter((call) => call.path.endsWith('/connection')).length, 1);
  f.send(request);
  f.send(request);
  await flush();
  const config = f.last('sub2api:image-studio-config');
  assert.equal(config.requestId, request.requestId);
  assert.equal(f.calls.filter((call) => call.path.endsWith('/connection')).length, 2);
  f.send({ type: 'sub2api:image-studio-config-applied', payload: { status: 'applied', profileId: 'sub2api-image-studio-group-27', requestId: request.requestId, revision: request.revision + 1 } });
  await flush();
  assert.equal(f.last('sub2api:image-studio-select-group-result'), undefined);
  await f.ackConnection();
  const next = await f.ackShell();
  assert.equal(next.selectedGroupId, 27);
  assert.equal(f.frameSets, 1);
  fail = true;
  f.send({ ...request, requestId: 'selection-request-0002', revision: next.revision, groupId: 26 });
  await flush();
  assert.equal(f.last('sub2api:image-studio-select-group-result').status, 'error');
  assert.equal(f.last('sub2api:image-studio-shell-config').selectedGroupId, 27);
  assert.equal(f.frameSets, 1);
});

test('revoked remembered group is never silently replaced and no group is auto-authorized', async () => {
  const f = await fixture({ remembered: '999' });
  const shell = await f.integrate();
  assert.equal(shell.selectedGroupId, null);
  assert.equal(f.calls.filter((call) => call.path.endsWith('/connection')).length, 0);
  assert.equal(f.frame.hidden, false);
});

test('theme/viewer refresh do not navigate; lock clears the iframe and rejects late messages', async () => {
  const f = await fixture();
  await f.integrate();
  f.host(2, 'embedded');
  const shell = await f.ackShell();
  assert.equal(shell.shellMode, 'embedded');
  assert.equal(f.frameSets, 1);
  f.send({ type: 'sub2api:image-studio-lock', version: 1, revision: shell.revision });
  assert.equal(f.locked, true);
  assert.equal(f.frame.src, 'about:blank');
  assert.equal(f.frame.hidden, true);
  assert.equal(f.parentMessages.at(-1).integrated, false);
  f.send({ type: 'sub2api:image-studio-shell-config-applied', version: 1, requestId: shell.requestId, revision: shell.revision });
  await flush();
  assert.equal(f.frame.hidden, true);
});

test('unconfirmed switch remains uncertain until another explicit successful selection', async () => {
  const f = await fixture();
  const shell = await f.integrate();
  f.send({ type: 'sub2api:image-studio-select-group', version: 1, requestId: 'selection-request-0003', revision: shell.revision, groupId: 27 });
  await flush();
  const timer = [...f.timers.values()].find((entry) => entry.delay === 500);
  assert.ok(timer);
  for (let i = 0; i < 31; i++) timer.callback();
  assert.equal(f.last('sub2api:image-studio-select-group-result').reconnectRequired, true);
  assert.equal(f.frameSets, 1);
});

test('shell timeout restores the native host, while successful theme updates never flash fallback chrome', async () => {
  const f = await fixture();
  await f.integrate();
  const count = f.parentMessages.length;
  f.host(2);
  assert.equal(f.document.body.dataset.integrated, 'true');
  assert.equal(f.parentMessages.length, count);
  await f.ackShell();
  assert.equal(f.document.body.dataset.integrated, 'true');
  assert.ok(f.parentMessages.slice(count).every((item) => item.integrated === true));
  f.host(3);
  const timer = [...f.timers.values()].find((entry) => entry.delay === 500);
  for (let i = 0; i < 31; i++) timer.callback();
  assert.equal(f.document.body.dataset.integrated, 'false');
  assert.equal(f.parentMessages.at(-1).integrated, false);
  assert.equal(f.frameSets, 1);
});

test('session failure while waiting for ACK never unlocks the workspace', async () => {
  let rejectSession = false;
  const f = await fixture({ session: async () => { if (rejectSession) throw Error('session unavailable'); } });
  await f.ackConnection();
  f.host();
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  rejectSession = true;
  await f.ackShell();
  assert.notEqual(f.document.body.dataset.integrated, 'true');
  assert.ok(f.parentMessages.every((item) => item.integrated !== true));
});

test('lost shell ACK recovers through a new handshake before another group switch', async () => {
  const f = await fixture();
  f.host();
  await f.ackConnection();
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  const firstShell = f.last('sub2api:image-studio-shell-config');
  // The new Studio has applied this shell, but its ACK did not arrive.
  const timer = [...f.timers.values()].find((entry) => entry.delay === 500);
  for (let i = 0; i < 31; i++) timer.callback();
  assert.equal(f.document.body.dataset.integrated, 'false');
  assert.equal(f.select.disabled, true);
  assert.equal(f.reconnect.hidden, false);
  assert.equal(f.reconnect.disabled, false);
  const connections = f.studioMessages.filter((item) => item.type === 'sub2api:image-studio-config').length;
  f.select.value = '27';
  f.select.onchange();
  await flush();
  assert.equal(f.studioMessages.filter((item) => item.type === 'sub2api:image-studio-config').length, connections);
  f.reconnect.onclick();
  const recovered = f.last('sub2api:image-studio-shell-config');
  assert.ok(recovered.revision > firstShell.revision);
  assert.equal(recovered.selectedGroupId, 26);
  await f.ackShell();
  assert.equal(f.reconnect.hidden, true);
  f.send({ type: 'sub2api:image-studio-select-group', version: 1, requestId: 'selection-after-recovery', revision: recovered.revision, groupId: 27 });
  await flush();
  await f.ackConnection();
  const switched = await f.ackShell();
  assert.equal(switched.selectedGroupId, 27);
  assert.equal(f.frameSets, 1);
});

test('initial connection timeout remains retryable before any new shell was published', async () => {
  const f = await fixture();
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  assert.equal(f.last('sub2api:image-studio-shell-config'), undefined);
  const timer = [...f.timers.values()].find((entry) => entry.delay === 500);
  for (let i = 0; i < 31; i++) timer.callback();
  assert.equal(f.select.disabled, false);
  assert.equal(f.reconnect.hidden, true);
  f.select.value = '26';
  f.select.onchange();
  await flush();
  await f.ackConnection();
  await f.ackShell();
  assert.equal(f.document.body.dataset.integrated, 'true');
  assert.equal(f.frameSets, 1);
});


test('older peers get recovery controls after the startup deadline without iframe navigation', async () => {
  const f = await fixture();
  await f.ackConnection();
  assert.equal(f.frame.hidden, true);
  const deadline = [...f.timers.values()].find((entry) => entry.delay === 20000);
  deadline.callback();
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.document.body['aria-busy'], 'false');
  assert.equal(f.connectionBar.hidden, false);
  assert.equal(f.frame.hidden, false);
  assert.equal(f.frameSets, 1);
});

test('initial connection failure offers retry without discarding the iframe', async () => {
  let failed = true;
  const f = await fixture({ connection: (options, config) => failed
    ? { ok: false, status: 503 }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.retry.disabled, false);
  failed = false;
  f.retry.onclick();
  await flush();
  await f.ackConnection();
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  await f.ackShell();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.loadingPanel.hidden, true);
  assert.equal(f.connectionBar.hidden, true);
  assert.equal(f.frameSets, 1);
});

test('no groups exposes a helpful recovery instead of a never-ending spinner', async () => {
  const f = await fixture({ groupList: [] });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.match(f.message.textContent, /没有可用/);
  assert.equal(f.select.disabled, true);
  assert.equal(f.calls.filter((item) => item.path.endsWith('/connection')).length, 0);
});

test('locked sessions expose login and never retry or reveal an old frame', async () => {
  const f = await fixture();
  await f.integrate();
  f.window.dispatchEvent(new Event('sources:locked'));
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.retry.hidden, true);
  assert.equal(f.login.hidden, false);
  assert.equal(f.frame.hidden, true);
  assert.equal(f.frame.src, 'about:blank');
  const calls = f.calls.length;
  f.retry.onclick();
  await flush();
  assert.equal(f.calls.length, calls);
});


test('transient metadata failure retries the same bootstrap without a page reload', async () => {
  let failed = true;
  const f = await fixture({ groupsResponse: () => failed
    ? { ok: false, status: 503 }
    : { ok: true, status: 200, data: { user_id: 9, groups: [{ id: 26, name: 'Images' }], studio_url: 'https://studio.example', main_origin: 'https://main.example' } } });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.equal(f.frameSets, 0);
  failed = false;
  f.retry.onclick();
  await flush();
  await f.integrate();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.frameSets, 1);
});

test('theme changes preserve the integrated view and update recovery colors', async () => {
  const f = await fixture();
  await f.integrate();
  f.host(2);
  assert.equal(f.document.documentElement.dataset.theme, 'dark');
  assert.equal(f.loadingPanel.hidden, true);
  assert.equal(f.connectionBar.hidden, true);
  assert.equal(f.frameSets, 1);
});
