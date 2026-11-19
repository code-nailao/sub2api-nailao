import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import vm from 'node:vm';

// The dialogs themselves need a browser; this covers the rules they apply
// before asking Portal, which checks every value again.
const source = await readFile(new URL('./internal/portal/web/studio-settings.js', import.meta.url), 'utf8');
const context = vm.createContext({});
const session = new vm.SyntheticModule(['authorizedJSON'], function () { this.setExport('authorizedJSON', async () => { throw Error('not called'); }); }, { context });
const module = new vm.SourceTextModule(source, { context });
await module.link(() => session);
await module.evaluate();
const { sortCandidates, managedGroups, customPrefix, customRequest } = module.namespace;
const plain = (value) => JSON.parse(JSON.stringify(value));

test('model candidates list image models first and keep the given order otherwise', () => {
  assert.deepEqual(plain(sortCandidates(['gpt-5', 'gpt-image-2', 'o4', 'grok-imagine-image', 'gemini-3-pro-image-preview'])),
    ['gpt-image-2', 'grok-imagine-image', 'gemini-3-pro-image-preview', 'gpt-5', 'o4']);
});

test('only what the administrator changed becomes a managed group', () => {
  const group = (source, extra = {}) => ({ group_id: 1, name: 'G', source, locked: false, enabled: source !== 'none', models: source === 'none' ? [] : ['a'], blockers: [], ...extra });
  const entry = (group, changes = {}) => ({ group, enabled: group.enabled, models: group.models, dirty: false, reset: false, ...changes });
  const saved = (...entries) => plain(managedGroups(entries));
  // Untouched rows: file and unregistered groups are left to their source.
  assert.deepEqual(saved(entry(group('file')), entry(group('none')), entry(group('file', { locked: true }))), { groups: [] });
  // Managed and overridden rows are kept as they are even when untouched.
  assert.deepEqual(saved(entry(group('managed')), entry(group('override', { group_id: 2, file_models: ['f'] }))),
    { groups: [{ group_id: 1, models: ['a'], enabled: true }, { group_id: 2, models: ['a'], enabled: true }] });
  // Adding a group needs a model; unchecking an empty one saves nothing.
  assert.match(managedGroups([entry(group('none'), { enabled: true, dirty: true })]).error, /至少选择一个模型/);
  assert.deepEqual(saved(entry(group('none'), { enabled: false, dirty: true })), { groups: [] });
  assert.deepEqual(saved(entry(group('none'), { enabled: true, models: ['m'], dirty: true })), { groups: [{ group_id: 1, models: ['m'], enabled: true }] });
  // A file group can be hidden or given other models; put back unchanged, it stays a file group.
  assert.deepEqual(saved(entry(group('file'), { enabled: false, dirty: true })), { groups: [{ group_id: 1, models: ['a'], enabled: false }] });
  assert.deepEqual(saved(entry(group('file'), { models: ['a', 'b'], dirty: true })), { groups: [{ group_id: 1, models: ['a', 'b'], enabled: true }] });
  assert.deepEqual(saved(entry(group('file'), { dirty: true })), { groups: [] });
  // Undo drops the managed entry; a media engine row is never sent.
  assert.deepEqual(saved(entry(group('override', { file_models: ['f'] }), { reset: true }), entry(group('managed'), { reset: true }), entry(group('file', { locked: true }), { dirty: true, enabled: false })), { groups: [] });
});

test('a custom connection is a key, an interface, a prefix and models; paths are never typed', () => {
  const base = { keyId: 11, protocol: 'openai_images', prefix: '', models: ['gpt-image-2'] };
  // An empty prefix is the interface's usual one.
  assert.deepEqual(plain(customRequest(base)), { body: { key_id: 11, protocol: 'openai_images', prefix: '/v1', models: ['gpt-image-2'] } });
  assert.equal(customRequest({ ...base, protocol: 'gemini_native' }).body.prefix, '/v1beta');
  assert.equal(customRequest({ ...base, protocol: 'gemini_native', prefix: ' /antigravity/v1beta ' }).body.prefix, '/antigravity/v1beta');
  assert.equal(customRequest({ ...base, prefix: '/alt/v1' }).body.prefix, '/alt/v1');
  assert.deepEqual(Object.keys(customRequest(base).body).sort(), ['key_id', 'models', 'prefix', 'protocol']);
  for (const prefix of ['//elsewhere.example/v1', 'https://elsewhere.example/v1', '/a/../v1', '/v1?b=1', '/a b/v1', 'v1', '/%2e%2e/v1', '/a\\b/v1', '/{model}/v1', '/v1/', '/a/b/c/d/v1']) {
    assert.ok(customRequest({ ...base, prefix }).error, prefix);
    assert.ok(customPrefix('gemini_native', prefix).error, prefix);
  }
  // The Studio client always addresses the OpenAI and Grok interfaces below a /v1 segment.
  assert.match(customRequest({ ...base, prefix: '/v1beta' }).error, /以 \/v1 结尾/);
  assert.match(customRequest({ ...base, protocol: 'grok_image', prefix: '/grok' }).error, /以 \/v1 结尾/);
  assert.match(customRequest({ ...base, protocol: 'grok_video' }).error, /接口类型/);
  assert.match(customRequest({ ...base, keyId: 0 }).error, /密钥/);
  assert.match(customRequest({ ...base, models: [] }).error, /模型/);
});
