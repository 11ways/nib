import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, Timer } from 'claude-code'

import type { Command, Job, Status } from '../types'

/** The pane's id, and the slash command that opens it. */
const PANE = 'nibash'
/** How many output lines of each command and job the pane shows. */
const TAIL = 12
/** How often nib is asked while something runs. */
const POLL_MS = 1000
/** How many rows the band lists before it counts the rest. */
const BAND_ROWS = 3

const status = atom({ plugin: 'nibash', key: 'status' } as const, null)
const server = atom({ plugin: 'nibash', key: 'server' } as const, 'nib')

/** A nibash call of any nib server, as Claude Code names it: mcp__<server>__nibash. */
const NIBASH = /^mcp__(.+)__nibash$/

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

/** Reads a nibash_status answer; undefined for anything else, so a server that is not nib, or another version of it, shows nothing. */
export function parse(content: unknown, at: number): Status | undefined {
  const v = content as Record<string, unknown> | null
  if (typeof v !== 'object' || v === null || !Array.isArray(v.commands) || !Array.isArray(v.jobs)) return undefined
  const commands = v.commands.map(command)
  const jobs = v.jobs.map(job)
  if (commands.some(c => c === undefined) || jobs.some(j => j === undefined)) return undefined
  return { commands: commands as Command[], jobs: jobs as Job[], at }
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
    if (inFlight === 0 && next.commands.length === 0 && !next.jobs.some(j => j.running)) stop()
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

export const register: Register = on => {

  on('session.start', async ($, e, next) => {
    await $.command.register({ name: PANE, description: 'Show the nibash commands and jobs nib runs, with their output' })
    const started = await next(e)
    // After a reload jobs may still run; one look finds them.
    watch($)
    return started
  })

  // /nibash again, or Escape while the pane holds the keys, closes it.
  on('command.run', { command: PANE }, async $ => {
    if ((await $.ui.panes()).some(pane => pane.id === PANE)) {
      await $.ui.close({ id: PANE })
      return {}
    }
    watch($)
    await $.ui.open({ id: PANE, title: 'nibash', focus: true, closeOnEscape: true })
    return {}
  })

  on('tool.call', async ($, e, next) => {
    const match = NIBASH.exec(e.tool)
    if (match === null) return next(e)
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
          <Text dimColor>Nothing runs in nibash, and no job ended in the last 10 minutes.</Text>
        </Box>
      )
    }
    // Each row gets a share of the pane's height for its output, the running ones first.
    const room = Math.max(all.length * 2, (e.viewport?.rows ?? 24) - 2)
    const lines = Math.max(0, Math.floor(room / all.length) - 1)
    return (
      <Box flexDirection="column">
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
}
