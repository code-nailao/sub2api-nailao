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
const { sortCandidates, managedGroups, customPathPrefix, customAddressChoice, customPreview, customAPILabelRequest, customRequest } = module.namespace;
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

test('custom requests select a label and derive paths from the platform and interface', () => {
  const base = { keyId: 11, protocol: 'openai_images', apiLabel: 'api', models: ['gpt-image-2'] };
  assert.deepEqual(plain(customRequest(base)), { body: { key_id: 11, protocol: 'openai_images', api_label: 'api', models: ['gpt-image-2'] } });
  assert.equal(customPathPrefix('gemini', 'openai_images'), '/v1');
  assert.equal(customPathPrefix('openai', 'gemini_native'), '/v1beta');
  assert.equal(customPathPrefix('antigravity', 'gemini_native'), '/antigravity/v1beta');
  assert.equal(customPathPrefix('antigravity', 'openai_images'), null);
  assert.equal(customRequest({ ...base, platform: 'antigravity', protocol: 'gemini_native' }).body.api_label, '');
  assert.ok(customRequest({ ...base, platform: 'antigravity' }).error);
  assert.ok(customRequest({ ...base, apiLabel: 'API' }).error);
  assert.ok(customRequest({ ...base, keyId: 0 }).error);
  assert.ok(customRequest({ ...base, protocol: 'unknown' }).error);
  assert.ok(customRequest({ ...base, models: [] }).error);
});

test('saved allowed labels stay selected; removed or old labels use the default', () => {
  assert.deepEqual(plain(customAddressChoice(['api', 'api2'], 'api', 'api2')), { label: 'api2', removed: false });
  assert.deepEqual(plain(customAddressChoice(['api'], 'api', 'api2')), { label: 'api', removed: true });
  assert.deepEqual(plain(customAddressChoice(['api'], 'api', undefined)), { label: 'api', removed: false });
  assert.deepEqual(plain(customAddressChoice([], '', 'api')), { label: '', removed: true });
});

test('previews show the full derived address and editing path', () => {
  assert.equal(customPreview('https://api.main.example', 'openai', 'openai_images'), '路径由系统自动拼接，不需要填写：https://api.main.example/v1/images/generations（编辑：https://api.main.example/v1/images/edits）');
  assert.match(customPreview('https://main.example', 'antigravity', 'gemini_native'), /https:\/\/main.example\/antigravity\/v1beta\/models\/<模型>:generateContent/);
  assert.match(customPreview('https://api.main.example', 'gemini', 'gemini_native'), /\/v1beta\/models\/<模型>:generateContent/);
});

test('admin address validation requires unique lowercase labels and a listed default', () => {
  assert.deepEqual(plain(customAPILabelRequest([], '')), { custom_api_labels: [], custom_api_default: '' });
  assert.equal(customAPILabelRequest(['api', 'api2'], 'api2').custom_api_default, 'api2');
  for (const [labels, chosen] of [[['api', 'api'], 'api'], [['API'], 'API'], [['api'], ''], [[], 'api'], [Array(17).fill('api'), 'api']]) {
    assert.ok(customAPILabelRequest(labels, chosen).error);
  }
});
