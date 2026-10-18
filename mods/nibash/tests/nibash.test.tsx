import { describe, expect, mock, test } from 'claude-code/testing'

import { count, elapsed, parse, parseNames, parseRestored, parseSeen, parseTokens, parseTrash, rows, structuredText } from '../hooks/register'

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
    expect(rows(status, false).map(row => [row.label, row.time, row.title, row.command, row.agent])).toEqual([
      ['nibash', '0:42', 'Run all Go tests', 'go test ./...', undefined],
      ['job %1', '0:43', 'bun bench/run.ts', undefined, 'subagent'],
    ])
    expect(rows(status, true).at(-1)?.time).toBe('exit 1 after 0:30')
  })

  test('a call still in its request is left out once, as the transcript shows it', () => {
    const status = { commands: [go, { ...go, started: 2_000 }], jobs: [], at: 43_000 }
    expect(rows(status, false, ['go test ./...']).map(row => row.time)).toEqual(['0:41'])
    expect(rows(status, false, ['go vet ./...'])).toHaveLength(2)
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
    let release = () => {}
    on('tool.call', { tool: 'mcp__nib__nibash' }, async () => {
      await new Promise<void>(resolve => (release = resolve))
      return { result: 'done' }
    })
    // The engine's own band, which the mod's gives way to once nothing runs.
    on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
      const { Box } = $.ui.resolve(e)
      return <Box key="engine" />
    })
    const call = $.tool.call({ tool: 'mcp__nib__nibash', command: 'go test ./...' })
    await clock.settle()
    expect(asked.at(-1)).toEqual({ server: 'nib', tool: 'nibash_status', args: { tail: 12 } })
    const band = await $.ui.mount({
      plugin: 'nibash',
      surface,
      component: 'AbovePrompt',
      props: { hasSurvey: false, isWorking: true, maxRows: 10, bodyColumns: 100 } as never,
    })
    // The call in its request shows in the transcript, so the band leaves it out until the request ends.
    expect(await band.find({ text: /Run all Go tests/ })).toBeUndefined()
    expect(await band.find({ text: /bun bench\/run\.ts/ })).toBeDefined()
    release()
    await call
    await clock.advance(1_000)
    const row = await band.find({ text: /Run all Go tests/ })
    expect(row?.text).toContain('nibash 0:43 Run all Go tests /nibash')
    expect(row?.text).not.toContain('main')
    expect(await band.find({ text: /\/nibash/ })).toBeDefined()
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

test('/nibash opens the pane and a second /nibash closes it again', async ($, on) => {
  on('mcp.call', async () => ({ value: { content: [], isError: false, structuredContent: { commands: [], jobs: [] } } }))
  let up = false
  on('ui.panes', async () => ({ value: up ? [{ id: 'nibash', title: 'nibash', isShown: true, isFocused: true, isPlaced: true }] : [] }))
  on('ui.open', async () => { up = true; return { value: { isPlaced: true } } })
  on('ui.close', async () => { up = false; return { value: undefined } })
  await $.command.run({ command: 'nibash' })
  expect(up).toBe(true)
  await $.command.run({ command: 'nibash' })
  expect(up).toBe(false)
})

describe('the trash pane', () => {
  const gone = { path: '/work/gone.txt', shown: 'gone.txt', when: 1_000 }

  test('nib answers are read in nib shape only', () => {
    expect(parseTrash({ trash: { entries: [gone, { ...gone, dir: true }] } })).toEqual([{ ...gone, dir: undefined }, { ...gone, dir: true }])
    expect(parseTrash({ trash: { entries: [] } })).toEqual([])
    expect(parseTrash({ trash: { entries: [{ ...gone, when: 'noon' }] } })).toBeUndefined()
    expect(parseTrash({ commands: [], jobs: [] })).toBeUndefined()
    expect(parseRestored({ restore: { path: '/a', done: true, message: 'restored a' } })).toEqual({ done: true, message: 'restored a' })
    expect(parseRestored({ restore: { done: 'yes', message: 'x' } })).toBeUndefined()
  })

  test('/trash lists what nib trashed, and a button puts one back through nib', async ($, on) => {
    mock.clock(on, { now: 43_000 })
    const calls: Record<string, unknown>[] = []
    let listed = [gone, { ...gone, path: '/work/old', shown: 'old', dir: true }]
    on('mcp.call', async (_, e) => {
      const args = e.args
      calls.push(args)
      if (typeof args.restore === 'string') {
        listed = listed.filter(entry => entry.path !== args.restore)
        return { value: { content: [], isError: false, structuredContent: { commands: [], jobs: [], restore: { path: args.restore, done: true, message: 'restored gone.txt' }, trash: { entries: listed } } } }
      }
      return { value: { content: [], isError: false, structuredContent: { commands: [], jobs: [], trash: { entries: listed } } } }
    })
    let up = false
    const toasts: string[] = []
    on('ui.toast', async (_, e) => { toasts.push(e.text); return { value: undefined } })
    on('ui.panes', async () => ({ value: up ? [{ id: 'trash', title: 'trash', isShown: true, isFocused: true, isPlaced: true }] : [] }))
    on('ui.open', async () => { up = true; return { value: { isPlaced: true } } })
    await $.command.run({ command: 'trash' })
    const pane = await $.ui.mount({ plugin: 'nibash', surface: 'terminal', component: 'Pane', requestId: 'trash', props: { title: 'trash', isFocused: false } as never })
    expect(await pane.find({ text: /gone\.txt/ })).toBeDefined()
    expect(await pane.find({ text: /\(directory\)/ })).toBeDefined()
    expect(calls[0]).toEqual({ view: 'trash' })
    await pane.press({ key: 'restore:/work/gone.txt' })
    expect(calls.at(-1)).toEqual({ view: 'trash', restore: '/work/gone.txt' })
    expect(await pane.find({ text: /restored gone\.txt/ })).toBeDefined()
    expect(await pane.find({ key: 'restore:/work/gone.txt' })).toBeUndefined()
    expect(await pane.find({ key: 'restore:/work/old' })).toBeDefined()
    expect(toasts).toEqual(['restored gone.txt'])
  })
})

/** Opens a view pane with its slash command, nib answering every ask with answer, and mounts it. */
async function openView($: Parameters<Parameters<typeof test>[1]>[0], on: Parameters<Parameters<typeof test>[1]>[1], name: string, answer: unknown) {
  mock.clock(on, { now: 43_000 })
  let up = false
  on('mcp.call', async () => ({ value: { content: [], isError: false, structuredContent: answer } }))
  on('ui.panes', async () => ({ value: up ? [{ id: name, title: name, isShown: true, isFocused: true, isPlaced: true }] : [] }))
  on('ui.open', async () => { up = true; return { value: { isPlaced: true } } })
  const ran = await $.command.run({ command: name })
  const pane = await $.ui.mount({ plugin: 'nibash', surface: 'terminal', component: 'Pane', requestId: name, props: { title: name, isFocused: false } as never })
  // The first ask may still be on its way when the pane is drawn; the surface draws again when it lands, a test does so itself.
  await pane.redraw()
  return pane
}

describe('the tokens pane', () => {
  const report = {
    chars: 12_345,
    calls: 3,
    agents: [
      { agent: 'main', calls: 2, chars: 12_000, tools: [{ tool: 'read', calls: 1, chars: 9_000 }, { tool: 'grep', calls: 1, chars: 3_000 }] },
      { agent: 'agent:a1', calls: 1, chars: 345, tools: [{ tool: 'nibash', calls: 1, chars: 345 }] },
    ],
    largest: [{ agent: 'main', tool: 'read', chars: 9_000, call: '{"paths":["big.go"]}' }],
  }

  test('nib answers are read in nib shape only', () => {
    expect(count(1234567)).toBe('1,234,567')
    expect(count(12)).toBe('12')
    expect(parseTokens({ tokens: report })).toEqual(report)
    expect(parseTokens({ tokens: { ...report, calls: 'three' } })).toBeUndefined()
    expect(parseTokens({ tokens: { ...report, agents: [{ agent: 'main' }] } })).toBeUndefined()
    expect(parseTokens({ commands: [], jobs: [] })).toBeUndefined()
  })

  test('/tokens shows the characters by agent and tool, and the largest results', async ($, on) => {
    const pane = await openView($, on, 'tokens', { commands: [], jobs: [], tokens: report })
    for (const text of [/12,345/, /agent:a1/, /grep.*3,000/, /9,000 read/]) expect(await pane.find({ text })).toBeDefined()
  })

  test('with nothing returned yet the pane says so', async ($, on) => {
    const pane = await openView($, on, 'tokens', { commands: [], jobs: [], tokens: { chars: 0, calls: 0, agents: [], largest: [] } })
    expect(await pane.find({ text: /No tool call of nib has returned anything/ })).toBeDefined()
  })
})

describe('the names pane', () => {
  const agents = [
    { agent: 'main', names: [{ name: 'core', directory: 'packages/core', source: 'server', uses: 4 }, { name: 'bewidgets', directory: 'zenit/widgets', source: 'automatic', uses: 1_200 }] },
    { agent: 'agent:a1', names: [] },
  ]

  test('nib answers are read in nib shape only', () => {
    expect(parseNames({ names: { agents } })).toEqual(agents)
    expect(parseNames({ names: { agents: [{ agent: 'main', names: [{ name: 'x', directory: 'y', source: 'defined', uses: -1 }] }] } })).toBeUndefined()
    expect(parseNames({ names: {} })).toBeUndefined()
  })

  test('/names lists each agent with where its names came from and how often they printed', async ($, on) => {
    const pane = await openView($, on, 'names', { commands: [], jobs: [], names: { agents } })
    for (const text of [/main/, /~core = packages\/core/, /automatic, 1,200 paths/, /agent:a1/, /no names/]) expect(await pane.find({ text })).toBeDefined()
  })
})

describe('the seen pane', () => {
  const agents = [
    {
      agent: 'main',
      files: [
        { path: 'src/a.go', tag: '3C4D', lines: 1_200, seen: [[1, 40], [80, 80]], moreRanges: 3, known: 4 },
        { path: 'b.txt', tag: '9F12', lines: 12, anywhere: true, seen: [] },
        { path: 'c.txt', tag: 'AAAA', lines: 5, seen: [] },
      ],
      more: 5,
    },
    { agent: 'agent:a1', files: [] },
  ]

  test('nib answers are read in nib shape only', () => {
    expect(parseSeen({ seen: { agents } })).toMatchObject([{ agent: 'main', files: [{ seen: [[1, 40], [80, 80]] }, {}, {}], more: 5 }, { agent: 'agent:a1', files: [] }])
    expect(parseSeen({ seen: { agents: [{ agent: 'main', files: [{ path: 'a', tag: 'T', lines: 1, seen: [[1]] }] }] } })).toBeUndefined()
    expect(parseSeen({ seen: {} })).toBeUndefined()
  })

  test('/seen shows the lines each agent holds as seen, and the files nib has no record for', async ($, on) => {
    const pane = await openView($, on, 'seen', { commands: [], jobs: [], seen: { agents } })
    for (const text of [/src\/a\.go#3C4D 1,200 lines/, /seen 1-40, 80 and 3 more ranges/, /known 4/, /edits may anchor anywhere/, /none seen/, /and 5 more files/, /nothing held/]) {
      expect(await pane.find({ text })).toBeDefined()
    }
  })
})

describe('the summary indicator and its switch', () => {
  const summarizing = { kind: 'summary', model: 'deepseek/deepseek-v4.1-flash-1017', started: 40_000 }

  test('models being asked and the summaries state are read in nib shape only', () => {
    const answer = { commands: [], jobs: [], models: [summarizing], summaries: { on: true } }
    expect(parse(answer, 43_000)).toEqual({ commands: [], jobs: [], models: [summarizing], summaries: true, at: 43_000 })
    expect(parse({ ...answer, summaries: { on: 'yes' } }, 0)).toBeUndefined()
    expect(parse({ ...answer, models: [{ ...summarizing, started: 'soon' }] }, 0)).toBeUndefined()
    expect(parse({ ...answer, models: 'none' }, 0)).toBeUndefined()
    expect(rows({ commands: [], jobs: [], models: [summarizing, { kind: 'decider', model: 'typesafe/jev-1.14', started: 42_000 }], at: 43_000 }, false)
      .map(row => [row.label, row.time, row.title])).toEqual([['summary', '0:03', 'deepseek/deepseek-v4.1-flash-1017'], ['decider', '0:01', 'typesafe/jev-1.14']])
  })

  test('the band names the model being asked while a call waits, and polling stops when none is', async ($, on) => {
    const clock = mock.clock(on, { now: 43_000 })
    let answer: unknown = { commands: [], jobs: [], models: [summarizing], summaries: { on: true } }
    let asked = 0
    on('mcp.call', async () => {
      asked += 1
      return { value: { content: [], isError: false, structuredContent: answer } }
    })
    on('tool.call', { tool: 'mcp__nib__nibash' }, async () => ({ result: 'done' }))
    on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
      const { Box } = $.ui.resolve(e)
      return <Box key="engine" />
    })
    await $.tool.call({ tool: 'mcp__nib__nibash', command: 'go test ./...' })
    await clock.settle()
    const band = await $.ui.mount({ plugin: 'nibash', surface: 'terminal', component: 'AbovePrompt', props: { hasSurvey: false, isWorking: true, maxRows: 10, bodyColumns: 100 } as never })
    expect((await band.find({ text: /summary 0:03/ }))?.text).toContain('summary 0:03 deepseek/deepseek-v4.1-flash-1017')
    // The call has returned, yet a model is still asked, so nib is asked again.
    const before = asked
    await clock.advance(2_000)
    expect(asked).toBeGreaterThan(before)
    answer = { commands: [], jobs: [], models: [], summaries: { on: true } }
    await clock.advance(1_000)
    await clock.advance(1_000)
    expect(await band.find({ text: /summary/ })).toBeUndefined()
    const after = asked
    await clock.advance(5_000)
    expect(asked).toBe(after)
  })

  test('/nibash summaries turns them on or off, or flips them, through nib', async ($, on) => {
    mock.clock(on, { now: 43_000 })
    const sent: Record<string, unknown>[] = []
    let current: unknown = { on: true }
    on('mcp.call', async (_, e) => {
      sent.push(e.args)
      if (typeof e.args.summaries === 'boolean' && current !== undefined) current = { on: e.args.summaries }
      return { value: { content: [], isError: false, structuredContent: { commands: [], jobs: [], ...(current === undefined ? {} : { summaries: current }) } } }
    })
    const say = async (args: string) => ((await $.command.run({ command: 'nibash', args })) as { text?: string }).text
    expect(await say('summaries off')).toContain('are off')
    expect(sent).toEqual([{ summaries: false }])
    expect(await say('summaries')).toContain('are on')
    expect(sent.at(-1)).toEqual({ summaries: true })
    expect(sent.slice(-2)[0]).toEqual({})
    expect(await say('summaries sometimes')).toBe('usage: /nibash summaries [on|off]')
    expect(await say('whatever')).toBe('usage: /nibash summaries [on|off]')
    // A nib without a summary model has no state to turn.
    current = undefined
    expect(await say('summaries off')).toContain('no summary model')
    expect(await say('summaries')).toContain('no summary model')
  })
})

describe('the band line for a hosted provider that takes long', () => {
  const searching = { agent: 'main', tool: 'zemble_graph_callers', started: 39_000 }

  test('a hosted call is read in nib shape only, and named once it has run a few seconds', () => {
    const answer = { commands: [], jobs: [], calls: [searching] }
    expect(parse(answer, 43_000)).toEqual({ commands: [], jobs: [], calls: [searching], at: 43_000 })
    expect(parse({ ...answer, calls: [{ ...searching, started: 'soon' }] }, 0)).toBeUndefined()
    expect(parse({ ...answer, calls: 'none' }, 0)).toBeUndefined()
    expect(rows({ commands: [], jobs: [], calls: [searching, { agent: 'agent:a1', tool: 'zemble_search', started: 42_000 }], at: 43_000 }, false)
      .map(row => [row.label, row.time, row.title, row.agent])).toEqual([['zemble', '0:04', 'graph_callers', undefined]])
  })

  test('the band names the hosted call that runs long, and the mod asks nib while it runs', async ($, on) => {
    const clock = mock.clock(on, { now: 43_000 })
    const asked: string[] = []
    let calls: unknown[] = [searching]
    on('mcp.call', async (_, e) => {
      asked.push(e.tool)
      return { value: { content: [], isError: false, structuredContent: { commands: [], jobs: [], calls } } }
    })
    let release = () => {}
    on('tool.call', { tool: 'mcp__nib__zemble_graph_callers' }, async () => {
      await new Promise<void>(resolve => (release = resolve))
      return { result: 'done' }
    })
    on('ui.render', { component: 'AbovePrompt' }, ($, e) => {
      const { Box } = $.ui.resolve(e)
      return <Box key="engine" />
    })
    const call = $.tool.call({ tool: 'mcp__nib__zemble_graph_callers' })
    await clock.settle()
    expect(asked).toContain('nibash_status')
    const band = await $.ui.mount({ plugin: 'nibash', surface: 'terminal', component: 'AbovePrompt', props: { hasSurvey: false, isWorking: true, maxRows: 10, bodyColumns: 100 } as never })
    expect((await band.find({ text: /zemble/ }))?.text).toContain('zemble 0:04 graph_callers')
    release()
    await call
    calls = []
    await clock.advance(1_000)
    await clock.advance(1_000)
    expect(await band.find({ text: /zemble/ })).toBeUndefined()
    const after = asked.length
    await clock.advance(5_000)
    expect(asked.length).toBe(after)
  })

  test("a tool of another server, and nib's own status tool, start no polling", async ($, on) => {
    const clock = mock.clock(on, { now: 43_000 })
    const asked: string[] = []
    on('mcp.call', async (_, e) => {
      asked.push(e.tool)
      return { value: { content: [], isError: false, structuredContent: { commands: [], jobs: [] } } }
    })
    on('tool.call', { tool: 'mcp__other__zemble_search' }, async () => ({ result: 'done' }))
    on('tool.call', { tool: 'mcp__nib__nibash_status' }, async () => ({ result: 'done' }))
    await $.tool.call({ tool: 'mcp__other__zemble_search' })
    await $.tool.call({ tool: 'mcp__nib__nibash_status' })
    await clock.settle()
    expect(asked).toEqual([])
  })
})
