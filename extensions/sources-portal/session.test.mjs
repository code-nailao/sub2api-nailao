import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';
import { webcrypto } from 'node:crypto';

const source = await readFile(new URL('./internal/portal/web/session.js', import.meta.url), 'utf8');
const origin = 'https://main.example';
const token = (user = 9, sid = 'session-a', version = 1) =>
  `fixture.${Buffer.from(JSON.stringify({ user_id: user, sid, version })).toString('base64url')}.fixture`;
const oldToken = token();
const newToken = token(9, 'session-a', 2);
const json = (status, data) => new Response(JSON.stringify(data), { status });
const deferred = () => {
  let resolve;
  const promise = new Promise((r) => { resolve = r; });
  return { promise, resolve };
};

async function loadSession({ api, refresh, runtime, embedded = true, schedule = setTimeout, cancel = clearTimeout } = {}) {
  const window = new EventTarget();
  const values = new Map([['auth_token', oldToken]]);
  const calls = [];
  let checks = 0;
  let locks = 0;
  const localStorage = {
    getItem: (key) => values.get(key) ?? null,
    setItem: (key, value) => values.set(key, value),
    removeItem: (key) => values.delete(key),
  };
  const message = (data, options = {}) => window.dispatchEvent(Object.assign(new Event('message'), {
    data, origin: options.origin ?? origin, source: options.source ?? parent,
  }));
  const setToken = (value) => {
    if (value === null) localStorage.removeItem('auth_token');
    else localStorage.setItem('auth_token', value);
    window.dispatchEvent(Object.assign(new Event('storage'), { key: 'auth_token' }));
  };
  const parent = {
    postMessage: (data, targetOrigin) => {
      assert.equal(targetOrigin, origin);
      assert.equal(data.type, 'sub2api:session-check');
      assert.match(data.requestId, /^[\w-]{16,80}$/);
      assert.deepEqual(Object.keys(data).sort(), ['requestId', 'type']);
      checks++;
      void refresh?.({ data, message, setToken, localStorage });
    },
  };
  window.parent = embedded ? parent : window;
  window.addEventListener('sources:locked', () => locks++);
  const context = vm.createContext({
    window, localStorage, URL, URLSearchParams, Event, crypto: webcrypto,
    location: new URL(origin + '/admin/balances?token=removed&user_id=9'),
    history: { replaceState() {} }, atob, setTimeout: schedule, clearTimeout: cancel,
    fetch: async (url, options = {}) => {
      if (url === '/public/runtime') {
        await runtime?.promise;
        return json(200, { main_origin: origin });
      }
      calls.push({ url: String(url), options });
      return api({ url: String(url), options, count: calls.length, localStorage });
    },
  });
  const module = new vm.SourceTextModule(source, { context });
  await module.link(() => { throw Error('session transport has no dependencies'); });
  await module.evaluate();
  return { session: module.namespace, localStorage, setToken, message, calls,
    get checks() { return checks; }, get locks() { return locks; } };
}

test('a late authorization 401 adopts the parent token already rotated for the same user/session', async () => {
  const received = deferred(), release = deferred();
  const fixture = await loadSession({ api: async ({ count, options }) => {
    if (count === 1) { received.resolve(); await release.promise; return json(401, { error: 'admin_login_required' }); }
    assert.equal(options.headers.Authorization, 'Bearer ' + newToken);
    return json(200, { config: { site_name: 'Recovered' } });
  } });
  const request = fixture.session.authorizedJSON('/api/home');
  await received.promise;
  fixture.setToken(newToken);
  release.resolve();
  assert.equal((await request).status, 200);
  assert.equal(fixture.calls.length, 2);
  assert.equal(fixture.checks, 0);
  assert.equal(fixture.locks, 0);
});

test('an expired embedded session asks the exact parent to check native auth before one retry', async () => {
  const fixture = await loadSession({
    api: ({ count }) => count === 1 ? json(401, { error: 'admin_login_required' }) : json(200, { sources: [] }),
    refresh: ({ data, message, setToken }) => {
      setToken(newToken);
      message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: true });
    },
  });
  assert.equal((await fixture.session.authorizedJSON('/api/balance/sources')).status, 200);
  assert.equal(fixture.checks, 1);
  assert.equal(fixture.calls.length, 2);
  assert.equal(fixture.locks, 0);
});

test('a retired request does not ask the parent to check native auth', async () => {
  const received = deferred(), release = deferred();
  let current = true;
  const fixture = await loadSession({
    api: async () => {
      received.resolve();
      await release.promise;
      return json(401, { error: 'admin_login_required' });
    },
    refresh: ({ data, message }) => message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: false }),
  });
  const request = fixture.session.authorizedJSON('/api/studio/groups', { isCurrent: () => current });
  await received.promise;
  current = false;
  release.resolve();
  assert.equal((await request).status, 401);
  assert.equal(fixture.checks, 0);
  assert.equal(fixture.locks, 0);
});

test('a parent check cannot replay or lock a request retired while the check was pending', async () => {
  for (const rotated of [false, true]) {
    const checked = deferred();
    let current = true;
    const fixture = await loadSession({
      api: ({ count }) => count === 1 ? json(401, { error: 'admin_login_required' }) : json(200, {}),
      refresh: (details) => checked.resolve(details),
    });
    const request = fixture.session.authorizedJSON('/api/studio/connection', {
      method: 'POST', body: { group_id: 26 }, isCurrent: () => current,
    });
    const { data, message, setToken } = await checked.promise;
    current = false;
    if (rotated) setToken(newToken);
    message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: rotated });
    assert.equal(fixture.checks, 1);
    assert.equal((await request).status, 401);
    assert.equal(fixture.calls.length, 1);
    assert.equal(fixture.locks, 0);
  }
});

test('the same user with another session, logout, and account switch reject a late response', async () => {
  for (const replacement of [token(9, 'session-b'), token(10), null]) {
    const received = deferred(), release = deferred();
    const fixture = await loadSession({ api: async () => {
      received.resolve(); await release.promise; return json(200, { secret: 'old account data' });
    } });
    const request = fixture.session.authorizedJSON('/api/home');
    await received.promise;
    fixture.setToken(replacement);
    release.resolve();
    await assert.rejects(request, /主站已退出或切换账号/);
    assert.equal(fixture.calls.length, 1);
    assert.equal(fixture.checks, 0);
    assert.ok(fixture.locks > 0);
  }
});

test('network errors, 5xx, 403, and unknown 401 never replay a possibly accepted write', async () => {
  for (const status of [0, 500, 503, 403, 401]) {
    const fixture = await loadSession({ api: () => {
      if (!status) throw Error('network result unknown');
      return json(status, { error: 'business_result_unknown' });
    } });
    const request = fixture.session.authorizedJSON('/api/home/draft', { method: 'POST', body: { revision: 7 } });
    if (!status) await assert.rejects(request, /network result unknown/);
    else assert.equal((await request).status, status);
    assert.equal(fixture.calls.length, 1);
    assert.equal(fixture.checks, 0);
  }
});

test('authorization rejection happens before a write and retries its unchanged body at most once', async () => {
  let accepted = 0;
  const fixture = await loadSession({
    api: ({ count, options }) => {
      assert.equal(options.method, 'POST');
      assert.equal(options.body, '{"group_id":26}');
      if (count === 1) return json(401, { error: 'admin_login_required' });
      accepted++;
      return json(200, { connection: 'local fixture only' });
    },
    refresh: ({ data, message, setToken }) => {
      setToken(newToken);
      message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: true });
    },
  });
  assert.equal((await fixture.session.authorizedJSON('/api/studio/connection', { method: 'POST', body: { group_id: 26 } })).status, 200);
  assert.equal(accepted, 1);
  assert.equal(fixture.calls.length, 2);
});

test('a repeated authorization rejection locks after one native session check and one retry', async () => {
  const fixture = await loadSession({
    api: () => json(401, { error: 'admin_login_required' }),
    refresh: ({ data, message, setToken }) => {
      setToken(newToken);
      message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: true });
    },
  });
  assert.equal((await fixture.session.authorizedJSON('/api/home')).status, 401);
  assert.equal(fixture.calls.length, 2);
  assert.equal(fixture.checks, 1);
  assert.equal(fixture.session.isCurrentSession(), false);
});

test('a same-session refresh before runtime configuration arrives does not lock the page', async () => {
  const runtime = deferred();
  const fixture = await loadSession({ runtime, api: () => json(200, { sources: [] }) });
  fixture.setToken(newToken);
  runtime.resolve();
  assert.equal((await fixture.session.authorizedJSON('/api/balance/sources')).status, 200);
  assert.equal(fixture.locks, 0);
});

test('a new account before runtime configuration arrives is not silently adopted', async () => {
  const runtime = deferred();
  const fixture = await loadSession({ runtime, api: () => json(200, { sources: [] }) });
  fixture.setToken(token(10));
  runtime.resolve();
  await assert.rejects(fixture.session.authorizedJSON('/api/balance/sources'), /主站已退出或切换账号/);
  assert.equal(fixture.calls.length, 0);
});

test('forged parent replies cannot unlock the retry and concurrent checks share one handshake', async () => {
  const checked = deferred();
  const fixture = await loadSession({
    api: ({ options }) => options.headers.Authorization === 'Bearer ' + oldToken
      ? json(401, { error: 'admin_login_required' }) : json(200, { sources: [] }),
    refresh: (details) => checked.resolve(details),
  });
  const first = fixture.session.authorizedJSON('/api/home');
  const second = fixture.session.authorizedJSON('/api/balance/sources');
  const { data, message, setToken } = await checked.promise;
  const reply = { type: 'sub2api:session-checked', requestId: data.requestId, ok: true };
  message(reply, { origin: 'https://untrusted.example' });
  message(reply, { source: {} });
  message({ ...reply, requestId: 'not-the-active-request' });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(fixture.calls.length, 2);
  assert.equal(fixture.checks, 1);
  setToken(newToken);
  message(reply);
  assert.deepEqual((await Promise.all([first, second])).map((r) => r.status), [200, 200]);
  assert.equal(fixture.calls.length, 4);
  assert.equal(fixture.locks, 0);
});

test('logout cancels an outstanding parent check and a late success cannot revive it', async () => {
  const checked = deferred();
  const fixture = await loadSession({
    api: () => json(401, { error: 'admin_login_required' }),
    refresh: (details) => checked.resolve(details),
  });
  const request = fixture.session.authorizedJSON('/api/home');
  const { data, message } = await checked.promise;
  fixture.setToken(null);
  await assert.rejects(request, /主站已退出或切换账号/);
  fixture.setToken(newToken);
  message({ type: 'sub2api:session-checked', requestId: data.requestId, ok: true });
  assert.equal(fixture.calls.length, 1);
  assert.equal(fixture.session.isCurrentSession(), false);
});

test('a missing parent response has a bounded timeout and never replays the request', async () => {
  const checked = deferred();
  let timeout;
  const fixture = await loadSession({
    api: () => json(401, { error: 'admin_login_required' }),
    refresh: () => checked.resolve(),
    schedule: (callback, delay) => { assert.ok(delay > 0 && delay <= 60000); timeout = callback; return 1; },
    cancel: () => {},
  });
  const request = fixture.session.authorizedJSON('/api/home');
  await checked.promise;
  timeout();
  assert.equal((await request).status, 401);
  assert.equal(fixture.calls.length, 1);
  assert.equal(fixture.session.isCurrentSession(), false);
});

test('a standalone page cannot ask itself to renew native credentials', async () => {
  const fixture = await loadSession({ embedded: false, api: () => json(401, { error: 'admin_login_required' }) });
  assert.equal((await fixture.session.authorizedJSON('/api/home')).status, 401);
  assert.equal(fixture.checks, 0);
  assert.equal(fixture.calls.length, 1);
});
