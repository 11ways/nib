import { describe, expect, mock, test } from 'claude-code/testing'

import { elapsed, parse, rows, structuredText } from '../hooks/register'

const go = { agent: 'main', command: 'go test ./...', description: 'Run all Go tests', started: 1_000, tail: ['ok  internal/engine'] }
const bench = { id: 1, agent: 'agent:a1', command: 'bun bench/run.ts', started: 0, running: true, log: '/run/nib/job-1.log', tail: ['run 1 of 3', 'run 2 of 3'] }

describe('reading nibash_status', () => {
  test('a duration is m:ss, or h:mm:ss from an hour on', () => {
    expect(elapsed(42_000)).toBe('0:42')
    expect(elapsed(3_723_000)).toBe('1:02:03')
    expect(elapsed(-5)).toBe('0:00')
  })

  test('Claude Code passes structured content on as the JSON of one text block', () => {
    expect(structuredText([{ type: 'text', text: '{"commands":[],"jobs":[]}' }])).toEqual({ commands: [], jobs: [] })
    expect(structuredText([{ type: 'text', text: 'Nothing running.' }])).toBeUndefined()
    expect(structuredText([])).toBeUndefined()
  })

  test('only nib answer shapes are read', () => {
    expect(parse({ commands: [go], jobs: [bench] }, 43_000)).toEqual({ commands: [go], jobs: [bench], at: 43_000 })
    expect(parse(undefined, 0)).toBeUndefined()
    expect(parse({ commands: [{ ...go, started: 'soon' }], jobs: [] }, 0)).toBeUndefined()
    expect(parse({ commands: [], jobs: [{ ...bench, running: 'yes' }] }, 0)).toBeUndefined()
  })

  test('the band leaves ended jobs out; the pane keeps them', () => {
    const status = { commands: [go], jobs: [bench, { ...bench, id: 2, running: false, exit: 1, ended: 30_000 }], at: 43_000 }
    expect(rows(status, false).map(row => `${row.label} ${row.time} ${row.title}`)).toEqual([
      'nibash 0:42 Run all Go tests  go test ./...',
      'job %1 0:43 bun bench/run.ts',
    ])
    expect(rows(status, true).at(-1)?.time).toBe('exit 1 after 0:30')
  })
})

for (const surface of ['terminal', 'desktop'] as const) {
  test(`a nibash call shows in the band while it runs, and a job that ends gets a toast (${surface})`, async ($, on) => {
    const clock = mock.clock(on, { now: 43_000 })
    let answer: unknown = { commands: [go], jobs: [bench] }
    const asked: unknown[] = []
    on('mcp.call', async (_, e) => {
      asked.push(e)
      // As Claude Code passes it on: the structured content's JSON as the one text block.
      return { value: { content: [{ type: 'text', text: JSON.stringify(answer) }], isError: false } }
    })
    const toasts: string[] = []
    on('ui.toast', async (_, e) => {
      toasts.push(e.text)
      return { value: undefined }
    })
    on('tool.call', { tool: 'mcp__nib__nibash' }, async () => ({ result: 'done' }))
    // The engine's own band, which the mod's gives way to once nothing runs.
    on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
      const { Box } = $.ui.resolve(e)
      return <Box key="engine" />
    })
    await $.tool.call({ tool: 'mcp__nib__nibash', command: 'go test ./...' })
    await clock.settle()
    expect(asked.at(-1)).toEqual({ server: 'nib', tool: 'nibash_status', args: { tail: 12 } })
    const band = await $.ui.mount({
      plugin: 'nibash',
      surface,
      component: 'AbovePrompt',
      props: { hasSurvey: false, isWorking: true, maxRows: 10, bodyColumns: 100 } as never,
    })
    expect((await band.find({ text: /Run all Go tests/ }))?.text).toContain('0:42')
    expect(await band.find({ text: /bun bench\/run\.ts/ })).toBeDefined()
    // The job ends: the next look toasts it once, and the band empties.
    answer = { commands: [], jobs: [{ ...bench, running: false, exit: 0, ended: 50_000 }] }
    await clock.advance(1_000)
    await clock.advance(1_000)
    expect(toasts).toEqual(['nibash job %1 ended, exit 0: bun bench/run.ts'])
    expect(await band.find({ text: /bun bench/ })).toBeUndefined()
    // Nothing runs, so nib is asked no more.
    const count = asked.length
    await clock.advance(5_000)
    expect(asked.length).toBe(count)
  })
}

test('the pane shows each command and job with its last output lines', async ($, on) => {
  const clock = mock.clock(on, { now: 43_000 })
  on('mcp.call', async () => ({ value: { content: [], isError: false, structuredContent: { commands: [go], jobs: [bench] } } }))
  on('tool.call', { tool: 'mcp__nib__nibash' }, async () => ({ result: 'done' }))
  await $.tool.call({ tool: 'mcp__nib__nibash', command: 'go test ./...' })
  await clock.settle()
  const pane = await $.ui.mount({ plugin: 'nibash', surface: 'terminal', component: 'Pane', requestId: 'nibash', props: { title: 'nibash', isFocused: false } as never })
  for (const line of ['ok  internal/engine', 'run 1 of 3', 'run 2 of 3']) {
    expect(await pane.find({ text: new RegExp(line) })).toBeDefined()
  }
})
