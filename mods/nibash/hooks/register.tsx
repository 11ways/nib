import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { Command, Job, ModelCall, NameList, ProviderCall, SeenList, Status, Tokens, TrashEntry, Views } from '../types'

/** The pane's id, and the slash command that opens it. */
const PANE = 'nibash'
/** How many output lines of each command and job the pane shows. */
const TAIL = 12
/** How often nib is asked while something runs. */
const POLL_MS = 1000
/** How many rows the band lists before it counts the rest. */
const BAND_ROWS = 3
/** How long a hosted provider's call runs before the band names it: a program that answers at once is not worth a line. */
const PROVIDER_AFTER_MS = 3000

const status = atom({ plugin: 'nibash', key: 'status' } as const, null)
const server = atom({ plugin: 'nibash', key: 'server' } as const, 'nib')
const views = atom({ plugin: 'nibash', key: 'views' } as const, {} as Views)

/** A nibash call of any nib server, as Claude Code names it: mcp__<server>__nibash. */
const NIBASH = /^mcp__(.+)__nibash$/
/** A hosted provider's tool of a nib server, as Claude Code names it: mcp__<server>__<provider>_<tool>. */
const HOSTED = /^mcp__(.+?)__([a-z][a-z0-9-]*)_(.+)$/

/** The panes that show one list of nib's, each opened by the slash command of its name; the list is nibash_status's view of that name. */
const VIEWS = {
  trash: 'Show what this session moved to the trash, and put one back',
  tokens: 'Show how many characters nib returned in this session, by agent and tool',
  names: 'Show the named directories each agent has, where each came from and how often it was used',
  seen: 'Show the lines nib counts each agent as having seen of its files',
} as const
type ViewName = keyof typeof VIEWS
const VIEW_NAMES = Object.keys(VIEWS) as ViewName[]
/** How often the lists of open view panes are asked again. */
const VIEW_MS = 2000

/** A duration as m:ss, or h:mm:ss from an hour on. */
export function elapsed(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000))
  const pad = (n: number) => String(n).padStart(2, '0')
  if (seconds >= 3600) return `${Math.floor(seconds / 3600)}:${pad(Math.floor(seconds / 60) % 60)}:${pad(seconds % 60)}`
  return `${Math.floor(seconds / 60)}:${pad(seconds % 60)}`
}

/** A command line on one line, cut to length. */
function short(command: string, length = 60): string {
  const line = command.replace(/\s+/g, ' ').trim()
  return line.length > length ? `${line.slice(0, length - 3)}...` : line
}

const isStrings = (value: unknown): value is string[] => Array.isArray(value) && value.every(v => typeof v === 'string')
const isNumber = (value: unknown): value is number => typeof value === 'number' && Number.isFinite(value)

function command(value: unknown): Command | undefined {
  const v = value as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || typeof v.agent !== 'string' || typeof v.command !== 'string' || !isNumber(v.started)) return undefined
  if ((v.description !== undefined && typeof v.description !== 'string') || (v.tail !== undefined && !isStrings(v.tail))) return undefined
  return { agent: v.agent, command: v.command, description: v.description as string | undefined, started: v.started, tail: v.tail as string[] | undefined }
}

function job(value: unknown): Job | undefined {
  const v = value as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || !isNumber(v.id) || typeof v.agent !== 'string' || typeof v.command !== 'string') return undefined
  if (!isNumber(v.started) || typeof v.running !== 'boolean' || typeof v.log !== 'string') return undefined
  if ((v.exit !== undefined && !isNumber(v.exit)) || (v.ended !== undefined && !isNumber(v.ended)) || (v.tail !== undefined && !isStrings(v.tail))) return undefined
  return { id: v.id, agent: v.agent, command: v.command, started: v.started, ended: v.ended as number | undefined, running: v.running, exit: v.exit as number | undefined, log: v.log, tail: v.tail as string[] | undefined }
}

/** A decision or summary model nib is asking, as nibash_status gives it. */
function modelCall(value: unknown): ModelCall | undefined {
  const v = value as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || typeof v.kind !== 'string' || typeof v.model !== 'string' || !isNumber(v.started)) return undefined
  return { kind: v.kind, model: v.model, started: v.started }
}

/** A hosted provider's tool call running, as nibash_status gives it. */
function providerCall(value: unknown): ProviderCall | undefined {
  const v = value as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || typeof v.agent !== 'string' || typeof v.tool !== 'string' || !isNumber(v.started)) return undefined
  return { agent: v.agent, tool: v.tool, started: v.started }
}

/** Reads a nibash_status answer; undefined for anything else, so a server that is not nib, or another version of it, shows nothing. */
export function parse(content: unknown, at: number): Status | undefined {
  const v = content as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || !Array.isArray(v.commands) || !Array.isArray(v.jobs)) return undefined
  const commands = v.commands.map(command)
  const jobs = v.jobs.map(job)
  if (commands.some(c => c === undefined) || jobs.some(j => j === undefined)) return undefined
  const status: Status = { commands: commands as Command[], jobs: jobs as Job[], at }
  // Answers of an older nib hold neither; one that does is read in full or not at all.
  if (v.models !== undefined) {
    const models = Array.isArray(v.models) ? v.models.map(modelCall) : undefined
    if (models === undefined || models.some(m => m === undefined)) return undefined
    status.models = models as ModelCall[]
  }
  if (v.summaries !== undefined) {
    const on = (v.summaries as { on?: unknown } | null)?.on
    if (typeof on !== 'boolean') return undefined
    status.summaries = on
  }
  if (v.calls !== undefined) {
    const calls = Array.isArray(v.calls) ? v.calls.map(providerCall) : undefined
    if (calls === undefined || calls.some(c => c === undefined)) return undefined
    status.calls = calls as ProviderCall[]
  }
  return status
}

/** The trash a nibash_status answer lists; undefined when it lists none or not in nib's shape. */
export function parseTrash(content: unknown): TrashEntry[] | undefined {
  const trash = (content as { trash?: { entries?: unknown } } | null)?.trash
  if (typeof trash !== 'object' || trash === null || !Array.isArray(trash.entries)) return undefined
  const entries: TrashEntry[] = []
  for (const value of trash.entries) {
    const v = value as Record<string, unknown> | null
    if (typeof v !== 'object' || v === null || typeof v.path !== 'string' || typeof v.shown !== 'string' || !isNumber(v.when)) return undefined
    if (v.dir !== undefined && typeof v.dir !== 'boolean') return undefined
    entries.push({ path: v.path, shown: v.shown, when: v.when, dir: v.dir as boolean | undefined })
  }
  return entries
}

/** What nib said of a restore it was asked for: the line to show; undefined when the answer holds none. */
export function parseRestored(content: unknown): { done: boolean; message: string } | undefined {
  const restore = (content as { restore?: Record<string, unknown> } | null)?.restore
  if (typeof restore !== 'object' || restore === null || typeof restore.done !== 'boolean' || typeof restore.message !== 'string') return undefined
  return { done: restore.done, message: restore.message }
}

const isCount = (value: unknown): value is number => isNumber(value) && value >= 0

/** The token report a nibash_status answer holds; undefined when it holds none in nib's shape. */
export function parseTokens(content: unknown): Tokens | undefined {
  const v = (content as { tokens?: Record<string, unknown> } | null)?.tokens
  if (typeof v !== 'object' || v === null || !isCount(v.chars) || !isCount(v.calls) || !Array.isArray(v.agents) || !Array.isArray(v.largest)) return undefined
  const agents: Tokens['agents'] = []
  for (const a of v.agents as Record<string, unknown>[]) {
    if (typeof a !== 'object' || a === null || typeof a.agent !== 'string' || !isCount(a.calls) || !isCount(a.chars) || !Array.isArray(a.tools)) return undefined
    const tools: Tokens['agents'][number]['tools'] = []
    for (const t of a.tools as Record<string, unknown>[]) {
      if (typeof t !== 'object' || t === null || typeof t.tool !== 'string' || !isCount(t.calls) || !isCount(t.chars)) return undefined
      tools.push({ tool: t.tool, calls: t.calls, chars: t.chars })
    }
    agents.push({ agent: a.agent, calls: a.calls, chars: a.chars, tools })
  }
  const largest: Tokens['largest'] = []
  for (const b of v.largest as Record<string, unknown>[]) {
    if (typeof b !== 'object' || b === null || typeof b.agent !== 'string' || typeof b.tool !== 'string' || !isCount(b.chars) || typeof b.call !== 'string') return undefined
    largest.push({ agent: b.agent, tool: b.tool, chars: b.chars, call: b.call })
  }
  return { chars: v.chars, calls: v.calls, agents, largest }
}

/** The named directories a nibash_status answer lists, per agent; undefined when it lists none in nib's shape. */
export function parseNames(content: unknown): NameList | undefined {
  const v = (content as { names?: { agents?: unknown } } | null)?.names
  if (typeof v !== 'object' || v === null || !Array.isArray(v.agents)) return undefined
  const agents: NameList = []
  for (const a of v.agents as Record<string, unknown>[]) {
    if (typeof a !== 'object' || a === null || typeof a.agent !== 'string' || !Array.isArray(a.names)) return undefined
    const rows: NameList[number]['names'] = []
    for (const n of a.names as Record<string, unknown>[]) {
      if (typeof n !== 'object' || n === null || typeof n.name !== 'string' || typeof n.directory !== 'string' || typeof n.source !== 'string' || !isCount(n.uses)) return undefined
      rows.push({ name: n.name, directory: n.directory, source: n.source, uses: n.uses })
    }
    agents.push({ agent: a.agent, names: rows })
  }
  return agents
}

/** The seen lines a nibash_status answer lists, per agent; undefined when it lists none in nib's shape. */
export function parseSeen(content: unknown): SeenList | undefined {
  const v = (content as { seen?: { agents?: unknown } } | null)?.seen
  if (typeof v !== 'object' || v === null || !Array.isArray(v.agents)) return undefined
  const agents: SeenList = []
  for (const a of v.agents as Record<string, unknown>[]) {
    if (typeof a !== 'object' || a === null || typeof a.agent !== 'string' || !Array.isArray(a.files) || (a.more !== undefined && !isCount(a.more))) return undefined
    const files: SeenList[number]['files'] = []
    for (const f of a.files as Record<string, unknown>[]) {
      if (typeof f !== 'object' || f === null || typeof f.path !== 'string' || typeof f.tag !== 'string' || !isCount(f.lines) || !Array.isArray(f.seen)) return undefined
      if ((f.anywhere !== undefined && typeof f.anywhere !== 'boolean') || (f.known !== undefined && !isCount(f.known)) || (f.moreRanges !== undefined && !isCount(f.moreRanges))) return undefined
      const seen: [number, number][] = []
      for (const range of f.seen as unknown[]) {
        if (!Array.isArray(range) || range.length !== 2 || !isCount(range[0]) || !isCount(range[1])) return undefined
        seen.push([range[0], range[1]])
      }
      files.push({ path: f.path, tag: f.tag, lines: f.lines, anywhere: f.anywhere as boolean | undefined, seen, moreRanges: f.moreRanges as number | undefined, known: f.known as number | undefined })
    }
    agents.push({ agent: a.agent, files, more: a.more as number | undefined })
  }
  return agents
}

/** The part of Views a view's answer fills; undefined when the answer holds that list in no shape of nib's. */
export function parseView(name: ViewName, content: unknown): Partial<Views> | undefined {
  switch (name) {
    case 'trash': {
      const trash = parseTrash(content)
      return trash === undefined ? undefined : { trash }
    }
    case 'tokens': {
      const tokens = parseTokens(content)
      return tokens === undefined ? undefined : { tokens }
    }
    case 'names': {
      const names = parseNames(content)
      return names === undefined ? undefined : { names }
    }
    case 'seen': {
      const seen = parseSeen(content)
      return seen === undefined ? undefined : { seen }
    }
  }
}

/** A moment as the time of day. */
export function clock(ms: number): string {
  const d = new Date(ms)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`
}

/** A count with its thousands set apart. */
export function count(n: number): string {
  return String(n).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
}

/** One row of the band or the pane: a running command or a job. */
type Row = { key: string; label: string; time: string; title: string; command?: string; agent?: string; tail: string[]; isDone: boolean }

/** A row's agent as it shows: nothing for the main one. */
const who = (agent: string) => (agent === 'main' ? undefined : 'subagent')

/**
 * The rows a status shows; with ended, the jobs that ended too.
 * @param waiting the commands of nibash calls still in their request, which the transcript already shows; one row each is left out
 */
export function rows(s: Status, ended: boolean, waiting: readonly string[] = []): Row[] {
  const left = [...waiting]
  const out: Row[] = []
  s.commands.forEach((c, index) => {
    const at = left.indexOf(c.command)
    if (at >= 0) {
      left.splice(at, 1)
      return
    }
    out.push({
      key: `c${index}:${c.agent}:${c.started}`,
      label: 'nibash',
      time: elapsed(s.at - c.started),
      title: c.description || short(c.command),
      command: c.description ? short(c.command) : undefined,
      agent: who(c.agent),
      tail: c.tail ?? [],
      isDone: false,
    })
  })
  for (const m of s.models ?? []) {
    out.push({
      key: `m:${m.kind}:${m.model}:${m.started}`,
      label: m.kind === 'summary' ? 'summary' : 'decider',
      time: elapsed(s.at - m.started),
      title: m.model,
      tail: [],
      isDone: false,
    })
  }
  for (const c of s.calls ?? []) {
    if (s.at - c.started < PROVIDER_AFTER_MS) continue
    const [provider, ...tool] = c.tool.split('_')
    out.push({
      key: `p:${c.agent}:${c.tool}:${c.started}`,
      label: provider ?? c.tool,
      time: elapsed(s.at - c.started),
      title: tool.join('_') || c.tool,
      agent: who(c.agent),
      tail: [],
      isDone: false,
    })
  }
  for (const j of s.jobs) {
    if (!j.running && !ended) continue
    out.push({
      key: `j${j.id}:${j.agent}`,
      label: `job %${j.id}`,
      time: j.running ? elapsed(s.at - j.started) : `exit ${j.exit ?? '?'} after ${elapsed((j.ended ?? s.at) - j.started)}`,
      title: short(j.command),
      agent: who(j.agent),
      tail: j.tail ?? [],
      isDone: !j.running,
    })
  }
  return out
}

/** The structured content Claude Code passes on in its place: the JSON of it as the answer's one text block; undefined for anything else. */
export function structuredText(content: readonly unknown[]): unknown {
  const block = content[0] as { type?: unknown; text?: unknown } | undefined
  if (content.length !== 1 || block?.type !== 'text' || typeof block.text !== 'string') return undefined
  try {
    return JSON.parse(block.text)
  } catch {
    return undefined
  }
}

// What runs is nib's to say: the hooks only decide when to ask. A reload starts these over, and the next poll restores them.
let inFlight = 0
let timer: Timer | undefined
let asking = false
const watched = new Set<string>()
/** The commands of nibash calls still in their request; Claude Code moving one to the background ends its request. */
const waiting: string[] = []

function stop(): void {
  timer?.cancel()
  timer = undefined
}

/** Asks nib what runs, keeps the answer for the drawings, toasts the jobs that ended since, and stops asking once nothing runs. */
async function poll($: EngineInterface): Promise<void> {
  if (asking) return
  asking = true
  try {
    const name = await read($, server)
    const answer = await $.mcp.call(name, 'nibash_status', { tail: TAIL })
    const next = answer.isError ? undefined : parse(answer.structuredContent ?? structuredText(answer.content), await $.clock.now())
    if (next === undefined) {
      stop()
      return
    }
    for (const j of next.jobs) {
      const key = `${j.agent}:${j.id}`
      if (j.running) {
        watched.add(key)
      } else if (watched.delete(key)) {
        $.ui.toast(`nibash job %${j.id} ended, exit ${j.exit ?? '?'}: ${short(j.command, 50)}`)
      }
    }
    await update($, status, () => next)
    if (inFlight === 0 && next.commands.length === 0 && !(next.models?.length) && !(next.calls?.length) && !next.jobs.some(j => j.running)) stop()
  } catch {
    // No nib server by that name (yet), or it went away: nothing to show until a nibash call names one.
    stop()
  } finally {
    asking = false
  }
}

/** Asks now, and every POLL_MS until poll stops it. */
function watch($: EngineInterface): void {
  timer ??= $.clock.every(POLL_MS, () => void poll($))
  void poll($)
}

let viewTimer: Timer | undefined

/** Asks nib for a list, and for a restore when given; the structured answer, undefined when nib gave none in its shape. */
async function ask($: EngineInterface, view: ViewName, extra: Record<string, unknown> = {}): Promise<unknown> {
  const answer = await $.mcp.call(await read($, server), 'nibash_status', { view, ...extra })
  return answer.isError ? undefined : (answer.structuredContent ?? structuredText(answer.content))
}

/** Asks nib again for the list of every open view pane, and lets the timer go once none is open. */
async function refresh($: EngineInterface): Promise<void> {
  try {
    const open = (await $.ui.panes()).map(pane => pane.id).filter((id): id is ViewName => (VIEW_NAMES as string[]).includes(id))
    if (open.length === 0) {
      viewTimer?.cancel()
      viewTimer = undefined
      return
    }
    for (const name of open) {
      const got = parseView(name, await ask($, name))
      if (got !== undefined) await update($, views, v => ({ ...v, ...got }))
    }
  } catch {
    // nib did not answer: the pane keeps what it has.
  }
}

/** Asks now, and every VIEW_MS while a view pane is open. */
function watchViews($: EngineInterface): void {
  viewTimer ??= $.clock.every(VIEW_MS, () => void refresh($))
  void refresh($)
}

/** Puts a trashed path back through nib's own trash code, and says what nib answered. */
async function restore($: EngineInterface, path: string): Promise<void> {
  try {
    const content = await ask($, 'trash', { restore: path })
    const trash = parseTrash(content)
    const done = parseRestored(content)
    await update($, views, v => ({ ...v, trash: trash ?? v.trash, note: done?.message }))
    if (done !== undefined) $.ui.toast(done.message)
  } catch {
    $.ui.toast('nib did not answer the restore')
  }
}

/** /nibash summaries [on|off]: turns nib's summaries of long output on or off, or flips them; the line to show of what nib answered. */
async function setSummaries($: EngineInterface, args: string): Promise<string> {
  const [word, setting] = args.split(/\s+/)
  if (word !== 'summaries' || (setting !== undefined && setting !== 'on' && setting !== 'off')) return 'usage: /nibash summaries [on|off]'
  const none = 'nib has no summary model set, so there are no summaries to turn on or off.'
  try {
    const name = await read($, server)
    const ask = async (extra: Record<string, unknown>) => {
      const answer = await $.mcp.call(name, 'nibash_status', extra)
      return answer.isError ? undefined : parse(answer.structuredContent ?? structuredText(answer.content), await $.clock.now())
    }
    let on = setting === undefined ? undefined : setting === 'on'
    if (on === undefined) {
      const now = await ask({})
      if (now?.summaries === undefined) return none
      on = !now.summaries
    }
    const done = await ask({ summaries: on })
    if (done?.summaries === undefined) return none
    const summaries = done.summaries
    await update($, status, s => (s === null ? s : { ...s, summaries }))
    return summaries ? 'Summaries of long nibash output are on.' : 'Summaries of long nibash output are off: long output is cut as without a summary model.'
  } catch {
    return 'nib did not answer.'
  }
}

export const register: Register = on => {

  on('session.start', async ($, e, next) => {
    await $.command.register({ name: PANE, description: 'Show the nibash commands and jobs nib runs, with their output' })
    for (const name of VIEW_NAMES) await $.command.register({ name, description: VIEWS[name] })
    const started = await next(e)
    // After a reload jobs may still run; one look finds them.
    watch($)
    return started
  })

  // /nibash again, or Escape while the pane holds the keys, closes it.
  on('command.run', { command: PANE }, async ($, e) => {
    const args = (e.args ?? '').trim()
    if (args !== '') return { text: await setSummaries($, args) }
    if ((await $.ui.panes()).some(pane => pane.id === PANE)) {
      await $.ui.close({ id: PANE })
      return {}
    }
    watch($)
    await $.ui.open({ id: PANE, title: 'nibash', focus: true, closeOnEscape: true })
    return {}
  })

  // Each list is a pane of its own, opened and closed by its slash command as /nibash is.
  for (const name of VIEW_NAMES) {
    on('command.run', { command: name }, async $ => {
      if ((await $.ui.panes()).some(pane => pane.id === name)) {
        await $.ui.close({ id: name })
        return {}
      }
      await $.ui.open({ id: name, title: name, focus: true, closeOnEscape: true })
      watchViews($)
      return {}
    })
  }

  on('tool.call', async ($, e, next) => {
    const match = NIBASH.exec(e.tool)
    if (match === null) {
      // A hosted provider's tool, mcp__nib__zemble_search: the program behind it may take long to start or to answer, which nib reports.
      const hosted = HOSTED.exec(e.tool)
      if (hosted === null || hosted[1] !== (await read($, server)) || e.tool.endsWith('__nibash_status')) return next(e)
      inFlight += 1
      watch($)
      try {
        return await next(e)
      } finally {
        inFlight -= 1
        void poll($)
      }
    }
    await update($, server, () => match[1])
    const command = (e as { command?: unknown }).command
    if (typeof command === 'string') waiting.push(command)
    inFlight += 1
    watch($)
    try {
      return await next(e)
    } finally {
      inFlight -= 1
      if (typeof command === 'string') waiting.splice(waiting.indexOf(command), 1)
      void poll($)
    }
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const s = await read($, status)
    const running = s === null ? [] : rows(s, false, waiting)
    if (e.props.hasSurvey || running.length === 0) return next(e)
    const { Box, Text } = $.ui.resolve(e)
    const more = running.length - BAND_ROWS
    return (
      <Box flexDirection="column">
        {running.slice(0, BAND_ROWS).map((row, index) => (
          <Box key={row.key} flexDirection="row">
            <Box flexGrow={1} flexShrink={1}>
              <Text wrap="truncate-end">
                <Text color="cyan">{row.label}</Text> <Text dimColor>{row.time}</Text> {row.title}
                {row.agent ? <Text dimColor> {row.agent}</Text> : null}
              </Text>
            </Box>
            {index === 0 ? <Text dimColor>{more > 0 ? ` +${more} more /${PANE}` : ` /${PANE}`}</Text> : null}
          </Box>
        ))}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    const s = await read($, status)
    const all = s === null ? [] : rows(s, true)
    if (all.length === 0) {
      return (
        <Box flexDirection="column">
          {s?.summaries === undefined ? null : <Text dimColor>summaries are {s.summaries ? 'on; /nibash summaries off turns them off' : 'off; /nibash summaries on turns them on'}</Text>}
          <Text dimColor>Nothing runs in nibash, and no job ended in the last 10 minutes.</Text>
        </Box>
      )
    }
    // Each row gets a share of the pane's height for its output, the running ones first.
    const room = Math.max(all.length * 2, (e.viewport?.rows ?? 24) - 2)
    const lines = Math.max(0, Math.floor(room / all.length) - 1)
    return (
      <Box flexDirection="column">
        {s?.summaries === undefined ? null : <Text dimColor>summaries are {s.summaries ? 'on; /nibash summaries off turns them off' : 'off; /nibash summaries on turns them on'}</Text>}
        {all.map(row => (
          <Box key={row.key} flexDirection="column">
            <Text wrap="truncate-end" dimColor={row.isDone}>
              <Text color={row.isDone ? undefined : 'cyan'}>{row.label}</Text> <Text dimColor>{row.time}</Text> {row.title}
              {row.command ? <Text dimColor>  {row.command}</Text> : null}
              {row.agent ? <Text dimColor>  {row.agent}</Text> : null}
            </Text>
            {row.tail.slice(-lines).map((line, index) => (
              <Text key={`${row.key}:${index}`} dimColor wrap="truncate-end">
                {'  '}
                {line}
              </Text>
            ))}
          </Box>
        ))}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: 'trash' }, async ($, e) => {
    const { Box, Text, Button } = $.ui.resolve(e)
    const v = await read($, views)
    const entries = v.trash ?? []
    return (
      <Box flexDirection="column">
        {v.note ? <Text>{v.note}</Text> : null}
        {entries.length === 0 ? <Text dimColor>Nothing this session moved to the trash is still there.</Text> : null}
        {entries.map(entry => (
          <Box key={`entry:${entry.path}`} flexDirection="row">
            <Button key={`restore:${entry.path}`} label="restore" onPress={() => void restore($, entry.path)} />
            <Text wrap="truncate-end">
              {' '}
              <Text dimColor>{clock(entry.when)}</Text> {entry.shown}
              {entry.dir ? <Text dimColor> (directory)</Text> : null}
            </Text>
          </Box>
        ))}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: 'tokens' }, async ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    const t = (await read($, views)).tokens
    if (t === undefined || t.calls === 0) {
      return (
        <Box flexDirection="column">
          <Text dimColor>No tool call of nib has returned anything in this session yet.</Text>
        </Box>
      )
    }
    return (
      <Box flexDirection="column">
        <Text>
          <Text color="cyan">{count(t.chars)}</Text> characters in {count(t.calls)} calls
        </Text>
        {t.agents.map(agent => (
          <Box key={`agent:${agent.agent}`} flexDirection="column">
            <Text wrap="truncate-end">
              <Text color="cyan">{agent.agent}</Text> <Text dimColor>{count(agent.calls)} calls</Text> {count(agent.chars)}
            </Text>
            {agent.tools.map(tool => (
              <Text key={`tool:${agent.agent}:${tool.tool}`} wrap="truncate-end">
                {'  '}
                {tool.tool} <Text dimColor>{count(tool.calls)} calls</Text> {count(tool.chars)}
              </Text>
            ))}
          </Box>
        ))}
        {t.largest.length > 0 ? <Text dimColor>largest results</Text> : null}
        {t.largest.map((big, index) => (
          <Text key={`big:${index}`} wrap="truncate-end">
            {'  '}
            {count(big.chars)} {big.tool} <Text dimColor>{big.agent} {big.call}</Text>
          </Text>
        ))}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: 'names' }, async ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    const list = (await read($, views)).names ?? []
    if (list.length === 0) {
      return (
        <Box flexDirection="column">
          <Text dimColor>No agent has called nib yet.</Text>
        </Box>
      )
    }
    return (
      <Box flexDirection="column">
        {list.map(agent => (
          <Box key={`agent:${agent.agent}`} flexDirection="column">
            <Text color="cyan">{agent.agent}</Text>
            {agent.names.length === 0 ? <Text dimColor>{'  '}no names</Text> : null}
            {agent.names.map(name => (
              <Text key={`name:${agent.agent}:${name.name}`} wrap="truncate-end">
                {'  '}~{name.name} = {name.directory} <Text dimColor>{name.source}, {count(name.uses)} paths</Text>
              </Text>
            ))}
          </Box>
        ))}
      </Box>
    )
  })

  on('ui.render', { component: 'Pane', requestId: 'seen' }, async ($, e) => {
    const { Box, Text } = $.ui.resolve(e)
    const list = (await read($, views)).seen ?? []
    if (list.length === 0) {
      return (
        <Box flexDirection="column">
          <Text dimColor>No agent has called nib yet.</Text>
        </Box>
      )
    }
    const ranges = (seen: [number, number][]) => seen.map(([from, to]) => (from === to ? String(from) : `${from}-${to}`)).join(', ')
    return (
      <Box flexDirection="column">
        {list.map(agent => (
          <Box key={`agent:${agent.agent}`} flexDirection="column">
            <Text color="cyan">{agent.agent}</Text>
            {agent.files.length === 0 ? <Text dimColor>{'  '}nothing held</Text> : null}
            {agent.files.map(file => (
              <Text key={`file:${agent.agent}:${file.path}`} wrap="truncate-end">
                {'  '}
                {file.path}
                <Text dimColor>#{file.tag} {count(file.lines)} lines </Text>
                {file.anywhere ? <Text dimColor>edits may anchor anywhere</Text> : file.seen.length === 0 ? <Text dimColor>none seen</Text> : <Text>seen {ranges(file.seen)}{file.moreRanges ? ` and ${file.moreRanges} more ranges` : ''}</Text>}
                {file.known ? <Text dimColor> known {file.known}</Text> : null}
              </Text>
            ))}
            {agent.more ? <Text dimColor>{'  '}and {agent.more} more files</Text> : null}
          </Box>
        ))}
      </Box>
    )
  })
}
