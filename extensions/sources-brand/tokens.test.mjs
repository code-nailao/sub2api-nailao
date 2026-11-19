import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import tokens, { palettes, variablesFor, contrast, resolvePalette } from './tokens.mjs'

test('the eight legacy choices and all color values remain byte-equivalent', () => {
  assert.deepEqual(palettes.map(p => p.key), ['sources-blue', 'teal', 'warm-natural', 'forest-green', 'orange', 'yellow', 'red', 'slate'])
  assert.equal(createHash('sha256').update(JSON.stringify(palettes, null, 2)).digest('hex'), '6f02f95f7b06e21a370e87b89feb35a307994df613b2f6edc27aa3ada8d3db95')
})
test('invalid values use the default palette; Tailwind colors remain opacity-aware', () => {
  assert.equal(resolvePalette('url(https://example.invalid)').key, 'yellow')
  for (const value of Object.values(tokens.primary)) assert.match(value, /rgb\(var\(--color-primary-\d+-rgb, [\d ]+\) \/ <alpha-value>\)/)
})
for (const palette of palettes) test(`${palette.key}: shared CSS variables and readable action foregrounds`, () => {
  const vars = variablesFor(palette.key)
  for (const [shade, hex] of Object.entries(palette.colors)) {
    assert.equal(vars[`--color-primary-${shade}`], hex)
    assert.ok(contrast(vars[`--color-primary-${shade}-contrast`], hex) >= 4.5)
    assert.match(vars[`--color-primary-${shade}-rgb`], /^\d+ \d+ \d+$/)
  }
  for (const state of ['', '-hover']) {
    const fg = vars[`--sources-button${state}-foreground`]
    assert.ok(contrast(fg, vars[`--sources-button${state}-from`]) >= 4.5)
    assert.ok(contrast(fg, vars[`--sources-button${state}-to`]) >= 4.5)
  }
  // Accent text/icons must retain the legacy shade rather than a darker replacement.
  assert.equal(vars['--color-primary-600'], palette.colors[600])
  assert.ok(Object.values(vars).every(v => typeof v === 'string' && v.length))
})
