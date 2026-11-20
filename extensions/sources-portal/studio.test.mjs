import test from 'node:test';
import assert from 'node:assert/strict';
import { createStudioFixture as fixture, CUSTOM_ID, SETUP_ID, MANAGE_ID } from './studio.fixture.mjs';

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

const select = (groupId, revision, requestId) => ({ type: 'sub2api:image-studio-select-group', version: 1, requestId, revision, groupId });
const connections = (f) => f.calls.filter((call) => call.path.endsWith('/connection')).length;
// Messages are built in the page's realm; copy the IDs before comparing them.
const ids = (groups) => Array.from(groups, (group) => group.id);

test('a stale custom connection offers to open settings on startup', async () => {
  const f = await fixture({ custom: 'configured', remembered: String(CUSTOM_ID),
    connection: () => ({ ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }) });
  assert.match(f.message.textContent, /重新保存自定义连接/);
  assert.equal(f.document.body.dataset.state, 'recovery');
  f.send({ type: 'sub2api:image-studio-shell-ready', version: 1 });
  await f.ackShell();
  assert.equal(f.frame.hidden, true);
  assert.equal(f.loadingPanel.hidden, false);
  assert.match(f.message.textContent, /重新保存自定义连接/);
  assert.equal(f.retry.textContent, '打开自定义连接');
  assert.equal(f.settings.length, 0);
  f.retry.onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  const before = connections(f);
  f.settings.at(-1).onSaved(true);
  assert.equal(f.values.get('sources:studio:last-group:9'), String(CUSTOM_ID));
  assert.equal(f.state.reloads, 1);
  assert.equal(connections(f), before);
});

test('a failed Studio group poll and the next shell ACK keep stale connection recovery visible', async () => {
  let pollFails = false;
  const f = await fixture({ custom: 'configured', remembered: String(CUSTOM_ID),
    groupsResponse: () => pollFails ? { ok: false, status: 503 } : undefined,
    connection: () => ({ ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }) });
  await f.integrate();
  pollFails = true;
  await [...f.timers.values()].find(({ delay }) => delay === 30000).callback();
  assert.equal(f.calls.filter(({ path }) => path === '/api/studio/groups').length, 2);
  await f.ackShell();

  assert.equal(f.loadingPanel.hidden, false);
  assert.equal(f.frame.hidden, true);
  assert.match(f.message.textContent, /重新保存自定义连接/);
  assert.equal(f.retry.textContent, '打开自定义连接');
  f.retry.onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  assert.equal(connections(f), 1);
});

test('Studio keeps stale connection recovery through a network error until a new connection succeeds', async () => {
  let networkFailure = true;
  const f = await fixture({ custom: 'configured', remembered: String(CUSTOM_ID),
    connection: (options, config) => {
      const id = options.body.group_id;
      if (id === CUSTOM_ID) return { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } };
      if (networkFailure) throw Error('连接网络中断');
      return { ok: true, status: 200, data: config(id) };
    } });
  const shell = await f.integrate();
  const alternate = shell.groups.find(({ id }) => id !== CUSTOM_ID).id;
  f.send(select(alternate, shell.revision, 'failed-recovery-request'));
  await flush();
  assert.match(f.message.textContent, /重新保存自定义连接/);
  assert.match(f.message.textContent, /连接网络中断/);
  assert.equal(f.retry.textContent, '打开自定义连接');
  const refreshedShell = await f.ackShell();
  assert.equal(f.frame.hidden, true);

  networkFailure = false;
  f.send(select(alternate, refreshedShell.revision, 'successful-recovery-request'));
  await flush();
  assert.equal(f.retry.textContent, '重试连接');
  await f.ackConnection();
  await f.ackShell();
  assert.equal(f.document.body.dataset.state, 'ready');
  assert.equal(f.loadingPanel.hidden, true);
  assert.equal(f.frame.hidden, false);
});

test('an integrated Studio keeps the resave action visible after a failed switch and shell ACK', async () => {
  const f = await fixture({ custom: 'configured', connection: (options, config) => options.body.group_id === CUSTOM_ID
    ? { ok: false, status: 409, data: { error: 'custom_connection_needs_update' } }
    : { ok: true, status: 200, data: config(options.body.group_id) } });
  const shell = await f.integrate();
  f.send(select(CUSTOM_ID, shell.revision, 'resave-connection-request'));
  await flush();
  const result = f.last('sub2api:image-studio-select-group-result');
  assert.equal(result.status, 'error');
  assert.match(result.message, /重新保存自定义连接/);
  await f.ackShell();
  assert.equal(f.loadingPanel.hidden, false);
  assert.equal(f.retry.textContent, '打开自定义连接');
  assert.equal(f.settings.length, 0);
  f.retry.onclick();
  assert.equal(f.settings.at(-1)?.kind, 'custom');
  assert.equal(f.document.body.dataset.integrated, 'true');
  assert.equal(f.frame.hidden, false);
});

test('other Studio connection errors keep their existing hints and do not open settings', async () => {
  const generic = '暂时无法连接此分组，请重试。已有作品和输入内容已保留。';
  for (const [status, error, message] of [
    [409, 'custom_connection_unavailable', generic],
    [409, undefined, generic],
    [503, 'custom_connection_needs_update', generic],
    [403, 'custom_connection_needs_update', '此分组的授权已撤销或过期，请在主站 API 密钥页面核对。'],
    [401, 'custom_connection_needs_update', '登录已失效，请重新登录主站后打开。'],
  ]) {
    const f = await fixture({ custom: 'configured', connection: () => ({ ok: false, status, data: { error } }) });
    assert.equal(f.message.textContent, message);
    assert.equal(f.retry.textContent, '重试连接');
    assert.equal(f.settings.length, 0);
  }
});

test('settings entries follow the groups and open a dialog instead of connecting', async () => {
  const f = await fixture({ custom: 'available', admin: true });
  const shell = await f.integrate();
  assert.deepEqual(ids(shell.groups), [26, 27, SETUP_ID, MANAGE_ID]);
  assert.equal(shell.groups.at(-2).name, '＋ 自定义连接（用我的密钥）…');
  const before = connections(f);
  f.send(select(SETUP_ID, shell.revision, 'settings-request-0001'));
  await flush();
  const result = f.last('sub2api:image-studio-select-group-result');
  assert.equal(result.requestId, 'settings-request-0001');
  assert.equal(result.status, 'error');
  assert.equal(result.message, '');
  assert.equal(result.reconnectRequired, false);
  assert.equal(f.settings.length, 1);
  assert.equal(f.settings[0].kind, 'custom');
  assert.equal(connections(f), before);
  assert.equal(f.last('sub2api:image-studio-config-applied'), undefined);
  // A repeated message is one request, not a second dialog.
  f.send(select(SETUP_ID, shell.revision, 'settings-request-0001'));
  await flush();
  assert.equal(f.settings.length, 1);
  // Saving starts the page again on the saved connection; removing forgets it.
  f.settings[0].onSaved(true);
  assert.equal(f.values.get('sources:studio:last-group:9'), String(CUSTOM_ID));
  assert.equal(f.state.reloads, 1);
  f.settings[0].onSaved(false);
  assert.equal(f.values.has('sources:studio:last-group:9'), false);
  assert.equal(f.state.reloads, 2);
});

test('group settings are an administrator entry and saving republishes the groups', async () => {
  const f = await fixture({ admin: true });
  const shell = await f.integrate();
  assert.deepEqual(ids(shell.groups), [26, 27, MANAGE_ID]);
  f.send(select(MANAGE_ID, shell.revision, 'manage-request-00001'));
  await flush();
  assert.equal(f.settings.at(-1).kind, 'manage');
  f.state.groupList = [...f.state.groupList, { id: 28, name: 'Added' }];
  f.settings.at(-1).onSaved();
  await flush();
  assert.deepEqual(ids(f.last('sub2api:image-studio-shell-config').groups), [26, 27, 28, MANAGE_ID]);
  assert.equal(f.state.reloads, 0);

  const user = await fixture();
  const plain = await user.integrate();
  assert.deepEqual(ids(plain.groups), [26, 27]);
  for (const id of [MANAGE_ID, SETUP_ID, CUSTOM_ID]) {
    user.send(select(id, plain.revision, `forged-request-${id}`));
    await flush();
    assert.equal(user.last('sub2api:image-studio-select-group-result').message, '分组信息已更新，请重新选择。');
  }
  assert.equal(user.settings.length, 0);
  assert.equal(connections(user), 1);
});

test('a saved own-key connection is offered and connected like a group', async () => {
  const f = await fixture({ custom: 'configured', remembered: String(CUSTOM_ID) });
  assert.equal(f.calls.find((call) => call.path.endsWith('/connection')).options.body.group_id, CUSTOM_ID);
  const shell = await f.integrate();
  assert.deepEqual(ids(shell.groups), [26, 27, CUSTOM_ID, SETUP_ID]);
  assert.equal(shell.groups.at(-1).name, '修改自定义连接…');
  assert.equal(shell.selectedGroupId, CUSTOM_ID);
  assert.equal(f.document.body.dataset.integrated, 'true');
  f.send(select(26, shell.revision, 'switch-back-request-01'));
  await flush();
  assert.equal(f.calls.at(-1).options.body.group_id, 26);
});

test('an account without registered groups is offered its own key, and a lock withdraws the offer', async () => {
  const f = await fixture({ groupList: [], custom: 'available' });
  assert.equal(f.document.body.dataset.state, 'recovery');
  assert.match(f.message.textContent, /使用我的密钥/);
  assert.equal(f.ownKeyButton.hidden, false);
  assert.equal(f.customButton.hidden, false);
  assert.equal(f.manageButton.hidden, true);
  assert.equal(f.manageRecoveryButton.hidden, true);
  f.connectionMenu.open = true;
  f.ownKeyButton.onclick();
  assert.equal(f.settings.at(-1).kind, 'custom');
  assert.equal(f.connectionMenu.open, false);
  f.window.dispatchEvent(new Event('sources:locked'));
  assert.equal(f.ownKeyButton.hidden, true);
  f.ownKeyButton.onclick();
  assert.equal(f.settings.length, 1);

  const off = await fixture({ groupList: [] });
  assert.match(off.message.textContent, /请联系管理员/);
  assert.equal(off.ownKeyButton.hidden, true);
});
