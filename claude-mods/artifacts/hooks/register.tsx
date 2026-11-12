import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Artifact, Scope } from '../types'

const PANE = 'artifacts'
const TITLE = 'Artifacts'
const KEEP = 200

// Change a tag when Artifact's fields change: it declines what an earlier version stored.
const sessionList = atom({ plugin: 'artifacts', key: 'session' } as const, [], { shape: 'artifacts-1' })
const projectList = atom({ plugin: 'artifacts', key: 'project' } as const, [], { shape: 'artifacts-1' })
const scope = atom({ plugin: 'artifacts', key: 'scope' } as const, 'session')
const selected = atom({ plugin: 'artifacts', key: 'selected' } as const, null)
const lastPrompt = atom({ plugin: 'artifacts', key: 'lastPrompt' } as const, '')

// ---- what counts as an artifact ----
const VISUAL = new Set(['html', 'htm', 'svg', 'png', 'jpg', 'jpeg', 'gif', 'webp', 'pdf', 'mp4', 'webm', 'mov'])
const SKIP = /\/(node_modules|\.git|\.claude-plugin|\.venv|dist|build)\//
const extOf = (p: string) => (/\.([a-z0-9]+)$/i.exec(p)?.[1] ?? '').toLowerCase()
const baseOf = (p: string) => p.slice(p.lastIndexOf('/') + 1)
const dirOf = (p: string) => p.slice(0, p.lastIndexOf('/') + 1)
export const isArtifact = (p: string): boolean => {
  if (SKIP.test(p)) return false
  const ext = extOf(p)
  if (VISUAL.has(ext)) return true
  // Markdown is mostly code-adjacent docs; only reports and scratch notes count.
  return ext === 'md' && /\/(\.scratch|tmp|reports?|out)\//.test(p)
}

const FILE_RE = /(?:~|\.{1,2})?[\w@%+=:,.\/-]*[\w-]\.(?:html?|svg|png|jpe?g|gif|webp|pdf|mp4|webm|mov|md)(?![\w.])/gi
const DIR_RE = /(?:^|[\s'"=(])((?:~|\/|\.{1,2}\/)[\w@%+=:,.\/-]*)/g
const CD_RE = /(?:^|[;&|]\s*|\n\s*)cd\s+("[^"]+"|'[^']+'|[^\s;&|]+)/g
const unq = (s: string) => s.replace(/^["']|["']$/g, '')

function absolute(p: string, base: string, home: string): string {
  const raw = p.startsWith('~') ? home + p.slice(1) : p.startsWith('/') ? p : base.replace(/\/$/, '') + '/' + p
  const out: string[] = []
  for (const part of raw.split('/')) {
    if (part === '' || part === '.') continue
    if (part === '..') out.pop()
    else out.push(part)
  }
  return '/' + out.join('/')
}

/** Candidate files and folders a Bash command names, relative paths tried from the cwd and every `cd` target. */
export function bashCandidates(command: string, cwd: string, home: string): { files: string[]; dirs: string[] } {
  const bases = [cwd]
  for (const m of command.matchAll(CD_RE)) bases.push(absolute(unq(m[1] ?? ''), cwd, home))
  const files = new Set<string>()
  for (const m of command.matchAll(FILE_RE)) {
    const p = m[0]
    if (p.startsWith('/') || p.startsWith('~')) files.add(absolute(p, cwd, home))
    else for (const b of bases) files.add(absolute(p, b, home))
  }
  const dirs = new Set<string>()
  for (const m of command.matchAll(DIR_RE)) {
    const p = m[1] ?? ''
    if (p.length < 2 || FILE_RE.test(p)) { FILE_RE.lastIndex = 0; continue }
    FILE_RE.lastIndex = 0
    dirs.add(absolute(p, cwd, home))
  }
  return { files: [...files].slice(0, 24), dirs: [...dirs].slice(0, 8) }
}

// ---- recording ----
type Found = { path: string; size: number; at: number }

async function remember($: EngineInterface, found: Found[], by: string) {
  if (!found.length) return
  const asked = (await read($, lastPrompt)).slice(0, 120)
  const merge = (list: Artifact[]) => {
    const next = [...list]
    for (const f of found) {
      const i = next.findIndex((a) => a.path === f.path)
      const old = i >= 0 ? next.splice(i, 1)[0] : undefined
      next.unshift({ path: f.path, name: baseOf(f.path), kind: extOf(f.path), size: f.size, at: f.at, writes: (old?.writes ?? 0) + 1, by, asked: asked || old?.asked || '' })
    }
    return next.slice(0, KEEP)
  }
  const wasEmpty = (await read($, sessionList)).length === 0
  await update($, sessionList, merge)
  const project = await update($, projectList, merge)
  await $.store.set(await projectKey($), project)
  await update($, selected, () => found[0]?.path ?? null)
  if (wasEmpty) void $.ui.open({ id: PANE, title: TITLE })
  $.ui.toast(found.length === 1 ? `◆ artifact: ${baseOf(found[0]!.path)}` : `◆ ${found.length} artifacts`)
}

const projectKey = async ($: EngineInterface) => 'project:' + (await $.session.cwd())

async function statFile($: EngineInterface, path: string, since: number): Promise<Found | null> {
  try {
    const s = await $.fs.stat(path)
    if (s.kind !== 'file' || s.mtimeMs < since) return null
    return { path, size: s.size, at: s.mtimeMs }
  } catch {
    return null
  }
}

async function foundByBash($: EngineInterface, command: string, since: number): Promise<Found[]> {
  const cwd = await $.session.cwd()
  const home = (await $.env.get('HOME')) ?? ''
  const { files, dirs } = bashCandidates(command, cwd, home)
  const out = new Map<string, Found>()
  for (const p of files) {
    if (!isArtifact(p)) continue
    const f = await statFile($, p, since)
    if (f) out.set(p, f)
  }
  for (const d of dirs) {
    let entries
    try { entries = await $.fs.list(d) } catch { continue }
    for (const en of entries) {
      const p = d.replace(/\/$/, '') + '/' + en.name
      if (en.kind === 'file' && en.mtimeMs >= since && isArtifact(p)) out.set(p, { path: p, size: en.size, at: en.mtimeMs })
      if (out.size >= 40) break
    }
  }
  return [...out.values()]
}

// ---- actions ----
async function openPath($: EngineInterface, path: string, reveal = false) {
  const argv = reveal ? ['open', '-R', path] : ['open', path]
  const r = await $.process.run(argv).catch(() => null)
  if (!r || r.exitCode !== 0) {
    const r2 = reveal ? null : await $.process.run(['xdg-open', path]).catch(() => null)
    if (!r2 || r2.exitCode !== 0) { $.ui.toast(`Could not open ${baseOf(path)}`); return }
  }
  $.ui.toast(reveal ? `Revealed ${baseOf(path)}` : `Opened ${baseOf(path)}`)
}

// ---- drawing helpers ----
const ago = (now: number, t: number) => {
  const s = Math.max(0, (now - t) / 1000)
  return s < 60 ? 'now' : s < 3600 ? Math.round(s / 60) + 'm ago' : s < 86400 ? Math.round(s / 3600) + 'h ago' : Math.round(s / 86400) + 'd ago'
}
const bytes = (n: number) => (n < 1024 ? n + ' B' : n < 1048576 ? Math.round(n / 1024) + ' KB' : (n / 1048576).toFixed(1) + ' MB')
const fit = (s: string, n: number) => (s.length <= n ? s.padEnd(n) : s.slice(0, Math.max(1, n - 1)) + '…')
const tilde = (p: string, home: string) => (home && p.startsWith(home + '/') ? '~' + p.slice(home.length) : p)

export const register: Register = (on) => {
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'custom-artifact', description: 'Show the pages, images and PDFs Claude made, in a pane', argumentHint: '[n]' })
    const stored = await $.store.get('project:' + e.cwd)
    if (Array.isArray(stored)) await update($, projectList, () => stored as Artifact[])
    if ((await read($, sessionList)).length) void $.ui.open({ id: PANE, title: TITLE })
    return next(e)
  })

  on('command.run', { command: 'custom-artifact' }, async ($, e) => {
    const n = Number.parseInt(e.args.trim(), 10)
    const list = await read($, sessionList)
    if (Number.isFinite(n)) {
      const a = list[n - 1]
      if (!a) return { text: `No artifact ${n}. This session has ${list.length}.` }
      await openPath($, a.path)
      return { text: `Opened ${a.path}` }
    }
    await $.ui.open({ id: PANE, title: TITLE, focus: true })
    return { text: list.length ? `Artifacts pane opened: ${list.length} in this session.` : 'Artifacts pane opened. Nothing made in this session yet.' }
  })

  on('prompt.submit', async ($, e, next) => {
    if (!e.command && e.text.trim()) await update($, lastPrompt, () => e.text.trim().replace(/\s+/g, ' '))
    return next(e)
  })

  on('tool.call', async ($, e, next) => {
    const tool = String(e.tool)
    if (tool !== 'Write' && tool !== 'Edit' && tool !== 'Bash') return next(e)
    const since = (await $.clock.now()) - 1500
    const ran = await next(e)
    if (ran.deny !== undefined || ran.isError) return ran
    const input = e as unknown as { file_path?: unknown; command?: unknown }
    try {
      if (tool === 'Bash') {
        if (typeof input.command === 'string') await remember($, await foundByBash($, input.command, since), 'Bash')
      } else if (typeof input.file_path === 'string' && isArtifact(input.file_path)) {
        const f = await statFile($, input.file_path, 0)
        await remember($, [f ?? { path: input.file_path, size: 0, at: await $.clock.now() }], tool)
      }
    } catch {
      // Recording is best effort; the call's result stands.
    }
    return ran
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const sc: Scope = await read($, scope)
    const list = sc === 'session' ? await read($, sessionList) : await read($, projectList)
    const sel = await read($, selected)
    const now = await $.clock.now()
    const home = (await $.env.get('HOME')) ?? ''
    const cols = Math.max(30, e.props.bodyColumns)
    const room = Math.max(3, e.props.scroll.bodyRows - 12)
    const nameW = Math.max(10, cols - 26)
    const cur = list.find((a) => a.path === sel) ?? list[0]
    const pick = (path: string) => update($, selected, () => path)

    return (
      <Box flexDirection="column">
        <Box>
          <Button key="scope-session" plain hotkey="s" onPress={() => update($, scope, () => 'session')}>
            {sc === 'session' ? <Text bold underline>This session</Text> : <Text dimColor>This session</Text>}
          </Button>
          <Text>   </Text>
          <Button key="scope-project" plain hotkey="p" onPress={() => update($, scope, () => 'project')}>
            {sc === 'project' ? <Text bold underline>This project</Text> : <Text dimColor>This project</Text>}
          </Button>
          <Text dimColor>   {list.length} {list.length === 1 ? 'file' : 'files'}</Text>
        </Box>
        <Text> </Text>
        {list.length === 0 && <Text dimColor>Nothing yet. Pages, images and PDFs Claude writes show up here.</Text>}
        {list.slice(0, room).map((a, i) => (
          <Button key={'row:' + a.path} plain hotkey={i < 9 ? String(i + 1) : undefined} onPress={() => (cur && cur.path === a.path ? openPath($, a.path) : pick(a.path))}>
            {cur && cur.path === a.path ? <Text color="claude">▸ </Text> : <Text>  </Text>}
            <Text color={a.kind === 'png' || a.kind === 'jpg' || a.kind === 'jpeg' ? 'warning' : a.kind === 'md' ? 'success' : 'suggestion'}>{fit(a.kind, 5)}</Text>
            <Text bold={cur?.path === a.path}>{fit(a.name, nameW)}</Text>
            <Text dimColor> {fit(ago(now, a.at), 8)}</Text>
          </Button>
        ))}
        {list.length > room && <Text dimColor>  +{list.length - room} more</Text>}
        {cur && (
          <Box flexDirection="column" marginTop={1}>
            <Text dimColor>{'─'.repeat(cols)}</Text>
            <Text bold>{cur.name}</Text>
            <Text><Text dimColor>path     </Text>{tilde(cur.path, home)}</Text>
            <Text><Text dimColor>made by  </Text>{cur.by} · {cur.writes} {cur.writes === 1 ? 'write' : 'writes'} · {bytes(cur.size)} · {ago(now, cur.at)}</Text>
            {cur.asked ? <Text><Text dimColor>asked    </Text>"{cur.asked}"</Text> : null}
            <Box marginTop={1}>
              <Button key="open" hotkey="o" variant="primary" label="Open" onPress={() => openPath($, cur.path)} />
              <Text> </Text>
              <Button key="reveal" hotkey="r" label="Reveal" onPress={() => openPath($, cur.path, true)} />
              <Text> </Text>
              <Button key="copy" hotkey="c" label="Copy path" onPress={async () => { const r = await $.ui.copy({ text: cur.path, surface: e.surface }); $.ui.toast(r.isCopied ? 'Path copied' : 'Could not copy the path') }} />
              <Text> </Text>
              <Button key="mention" hotkey="m" label="Mention" onPress={() => $.prompt.fill({ text: '@' + cur.path + ' ', mode: 'insert' })} />
            </Box>
          </Box>
        )}
      </Box>
    )
  })
}
