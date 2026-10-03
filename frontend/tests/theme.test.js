import test from 'node:test'
import assert from 'node:assert/strict'
import { getTheme, applyTheme, themes } from '../src/theme.js'

function environment() {
  const data = new Map()
  const events = []
  const classes = new Set()
  globalThis.localStorage = { getItem: key => data.get(key) ?? null, setItem: (key, value) => data.set(key, value) }
  globalThis.document = { documentElement: { dataset: {}, classList: { toggle: (name, enabled) => enabled ? classes.add(name) : classes.delete(name) } } }
  globalThis.CustomEvent = class { constructor(type, options) { this.type = type; this.detail = options.detail } }
  globalThis.window = { dispatchEvent: event => events.push(event) }
  return { data, events, classes }
}

test('theme starts in night mode and only exposes the chosen day and night designs', () => {
  environment()
  assert.equal(getTheme(), 'dark')
  assert.deepEqual(themes.map(({ value, label }) => [value, label]), [['light', '留白'], ['dark', '夜航']])
})

test('legacy light palettes migrate to Linen while an explicit night preference survives reload', () => {
  const { data, classes, events } = environment()
  for (const old of ['ocean', 'mint']) {
    data.set('theme', JSON.stringify({ version: 1, value: old }))
    assert.equal(getTheme(), 'light')
    applyTheme()
    assert.deepEqual(JSON.parse(data.get('theme')), { version: 1, value: 'light' })
    assert.equal(document.documentElement.dataset.theme, 'light')
    assert.equal(classes.has('dark'), false)
  }
  applyTheme('dark')
  assert.equal(getTheme(), 'dark')
  assert.equal(classes.has('dark'), true)
  assert.equal(events.at(-1).detail, 'dark')
  applyTheme('light')
  assert.equal(getTheme(), 'light')
  assert.equal(classes.has('dark'), false)
})

test('invalid or unavailable preference storage cannot prevent applying the default night appearance', () => {
  const { data, classes } = environment()
  for (const corrupt of ['{broken', '{"version":2,"value":"light"}', '{"version":1,"value":"unknown"}']) {
    data.set('theme', corrupt)
    assert.equal(getTheme(), 'dark')
  }
  globalThis.localStorage = { getItem() { throw Error('disabled') }, setItem() { throw Error('disabled') } }
  assert.equal(applyTheme(), 'dark')
  assert.equal(classes.has('dark'), true)
})
