import { expect, test } from 'claude-code/testing'

import { bashCandidates, isArtifact } from '../hooks/register'

const PANE = {
  plugin: 'artifacts',
  component: 'Pane',
  requestId: 'artifacts',
  props: { title: 'Artifacts', isFocused: false, bodyColumns: 60, placement: 'dock', scroll: { offset: 0, bodyRows: 30 }, view: {} },
} as const

// Nothing sits beneath the plugin in a test: these stand for the engine's clock, store, env and session.
const engine = (on: Parameters<Parameters<typeof test>[1]>[1]) => {
  const store = new Map<string, unknown>()
  on('clock.now', () => ({ value: 1_000_000 }))
  on('store.get', (_, e) => ({ value: store.get((e as { key: string }).key) }))
  on('store.set', (_, e) => { const x = e as { key: string; value: unknown }; store.set(x.key, x.value); return { value: undefined } })
  on('env.get', () => ({ value: '/Users/me' }))
  on('session.cwd', () => ({ value: '/work' }))
}

test('a page Claude writes is listed in the pane, on every surface', async ($, on) => {
  engine(on)
  on('command.register', () => ({ value: { command: 'custom-artifact' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('tool.call', () => ({ result: {} }))
  on('fs.stat', () => ({ value: { kind: 'file', size: 2048, mtimeMs: 1, isLink: false } }))

  await $.session.start({ cwd: '/work', surface: 'terminal', isInteractive: true })
  await $.tool.call({ tool: 'Write', file_path: '/work/.scratch/report.html', content: '<p>hi</p>' })
  await $.tool.call({ tool: 'Write', file_path: '/work/src/app.ts', content: 'x' })

  for (const surface of ['terminal', 'desktop', 'vscode', 'mobile'] as const) {
    const ui = await $.ui.mount({ ...PANE, surface })
    expect((await ui.find({ type: 'Text', text: /report\.html/ }))?.text).toContain('report.html')
    expect(await ui.find({ type: 'Text', text: /app\.ts/ })).toBeUndefined()
    await ui.unmount()
  }
})

test('the pane says so when nothing was made', async ($, on) => {
  engine(on)
  const ui = await $.ui.mount({ ...PANE, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: /Nothing yet/ }))?.text).toContain('Nothing yet')
  await ui.unmount()
})

test('what counts as an artifact', () => {
  expect(isArtifact('/w/.scratch/a/page.prototype.html')).toBe(true)
  expect(isArtifact('/tmp/shots/l-A-light.png')).toBe(true)
  expect(isArtifact('/w/.scratch/model-tests/reports/msdecision.md')).toBe(true)
  expect(isArtifact('/w/README.md')).toBe(false)
  expect(isArtifact('/w/node_modules/x/index.html')).toBe(false)
  expect(isArtifact('/w/src/main.ts')).toBe(false)
})

test('a Bash command: files after a cd, and folders it names', () => {
  const c = bashCandidates("cd .scratch/admin-panel && python3 - <<'EOF'\nopen('admin-panel.prototype.html','w')\nEOF", '/w', '/Users/me')
  expect(c.files).toContain('/w/.scratch/admin-panel/admin-panel.prototype.html')
  const d = bashCandidates('node drive.mjs chrome file:///x/page.html /tmp/shots', '/w', '/Users/me')
  expect(d.dirs).toContain('/tmp/shots')
  const h = bashCandidates('cp out.pdf ~/Desktop/out.pdf', '/w', '/Users/me')
  expect(h.files).toContain('/Users/me/Desktop/out.pdf')
})
