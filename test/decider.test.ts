import { afterEach, describe, expect, test } from "bun:test"
import { Client } from "@modelcontextprotocol/sdk/client/index.js"
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js"
import { mkdir, mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { binary } from "./support/engine"
import { skimName } from "./support/tools"

const directories: string[] = []
const clients: Client[] = []
afterEach(async () => {
  await Promise.all(clients.splice(0).map((client) => client.close().catch(() => undefined)))
  await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true })))
})

async function workspace() {
  const directory = await realpath(await mkdtemp(path.join(tmpdir(), "nib-decider-")))
  directories.push(directory)
  return directory
}

const base = () => ({ PATH: process.env.PATH ?? "", HOME: process.env.HOME ?? "", XDG_STATE_HOME: process.env.XDG_STATE_HOME ?? "", XDG_DATA_HOME: process.env.XDG_DATA_HOME ?? "", XDG_CONFIG_HOME: process.env.XDG_CONFIG_HOME ?? "", NIB_SHELL: "1" })

async function connect(directory: string, env: Record<string, string>) {
  const client = new Client({ name: "test", version: "1" })
  await client.connect(new StdioClientTransport({ command: await binary(), cwd: directory, env: { ...base(), ...env }, stderr: "pipe" }))
  clients.push(client)
  const nibash = async (command: string, description?: string) => {
    const result = await client.callTool({ name: "nibash", arguments: { command, ...description && { description } } }) as { content: { text: string }[] }
    return result.content[0]!.text
  }
  return { client, nibash }
}

/** A program printing 3,000 numbered lines with one cause in the middle that names no error, so only a reader of the purpose finds it. */
const LONG_RUN = "seq 1 1500; echo 'routes: /v1/payments -> upstream payments-v2 (0 hosts)'; echo 'error: pool exhausted'; seq 1501 3000; exit 1"

describe("deciders", () => {
  test("keywords are the default: the error is shown and every row names its log line", async () => {
    const { client, nibash } = await connect(await workspace(), {})
    const { tools } = await client.listTools()
    const description = tools.find((tool) => tool.name === "nibash")!.description!
    expect(description).toContain("long output cut to its start, its end and the lines between that name errors or failures")
    expect(description.length).toBeLessThanOrEqual(2048)
    const out = await nibash(LONG_RUN)
    expect(out).toContain("\n1500|1500\n1501|routes: /v1/payments -> upstream payments-v2 (0 hosts)\n1502|error: pool exhausted\n1503|1501\n")
    const rows = out.split("\n")
    const closing = rows.at(-2)!
    const where = /^\[nibash: \d+ rows shown of 3,002 log lines; keywords chose the middle; N\| is a row's line in the whole output: read (\S+):A-B plain:true for others\]$/.exec(closing)
    expect(where).not.toBeNull()
    // Every row is the log line its number names, so the numbers show each gap.
    const log = (await readFile(where![1]!, "utf8")).split("\n")
    for (const row of rows.slice(0, -2)) {
      const [, line, text] = /^(\d+)\|(.*)$/.exec(row) ?? []
      expect([row, log[Number(line) - 1]]).toEqual([row, text])
    }
  })

  test("the fixed cut keeps its start and end, and its description says so", async () => {
    const { client, nibash } = await connect(await workspace(), { NIB_DECIDER: "fixed" })
    const { tools } = await client.listTools()
    expect(tools.find((tool) => tool.name === "nibash")!.description).toContain("long output cut to its start and end, with the whole kept in a log")
    const out = await nibash("seq 1 5000")
    expect(out.split("\n")[100]).toStartWith("[nibash: 300 rows shown of 5,000 log lines; log lines 101-4800 left out; whole output: ")
  })

  test("the OpenRouter decider asks for program output only, with the call's description, and shows what the model chose", async () => {
    const requests: any[] = []
    const server = Bun.serve({
      port: 0,
      async fetch(request) {
        const body = await request.json() as any
        requests.push({ body, authorization: request.headers.get("authorization") })
        const answers: Record<string, { type: string; noul: number }> = {}
        for (const id of Object.keys(body.questions)) answers[id] = { type: "noul", noul: id !== "whole" && String(body.state.output[id]).includes("payments-v2") ? 0.97 : 0.06 }
        return Response.json({ id: "gen", model: "typesafe/jev-1.14-20261014", answers, usage: { input_tokens: 10, output_tokens: 1, cost: 0 } })
      },
    })
    try {
      const directory = await workspace()
      await writeFile(path.join(directory, "big.txt"), Array.from({ length: 1000 }, (_, index) => `line ${index + 1}`).join("\n") + "\n")
      // The black-box suite's servers run like the benchmark's, in sandbox mode, which needs the explicit permission to send.
      const { client, nibash } = await connect(directory, {
        NIB_DECIDER: "openrouter", NIB_DECIDER_KEY: "test-key", NIB_DECIDER_URL: `http://127.0.0.1:${server.port}/api/alpha/decisions`,
        NIB_SANDBOX: "1", NIB_DECIDER_SANDBOX: "1", NIB_DECIDER_TIMEOUT: "5000",
      })
      const { tools } = await client.listTools()
      const description = tools.find((tool) => tool.name === "nibash")!.description!
      expect(description).toContain("a decision model on OpenRouter judges your description needs")
      expect(description.length).toBeLessThanOrEqual(2048)
      const file = await nibash("cat big.txt")
      expect(file).toContain("; log lines 100-800 left out; whole output: ")
      expect(requests).toHaveLength(0)
      const out = await nibash(LONG_RUN, "find why payments fail")
      expect(requests.length).toBeGreaterThan(0)
      expect(requests[0].authorization).toBe("Bearer test-key")
      expect(requests[0].body.state.purpose).toBe("find why payments fail")
      expect(requests[0].body.provider).toEqual({ zdr: true, data_collection: "deny" })
      expect(JSON.stringify(requests)).not.toContain("line 500")
      expect(out).toContain("\n1500|1500\n1501|routes: /v1/payments -> upstream payments-v2 (0 hosts)\n1502|error: pool exhausted\n1503|1501\n")
      expect(out).toMatch(/\n\[nibash: \d+ rows shown of 3,002 log lines; typesafe\/jev-1\.14 and keywords chose the middle for your description; N\| is a row's line in the whole output: read \S+:A-B plain:true for others\]\n\[exit 1\]$/)
      // skim is the same for a log file: the model reads its lines with expect as the purpose, and notes name the file; read never takes expect.
      const skim = skimName(tools as never)
      expect(JSON.stringify(tools.find((tool) => tool.name === "read")!.inputSchema)).not.toContain('"expect"')
      const lines = [...Array.from({ length: 1500 }, (_, index) => String(index + 1)), "routes: /v1/payments -> upstream payments-v2 (0 hosts)", "error: pool exhausted", ...Array.from({ length: 1500 }, (_, index) => String(index + 1501))]
      await writeFile(path.join(directory, "run.log"), lines.join("\n") + "\n")
      requests.length = 0
      const read = await client.callTool({ name: skim, arguments: { paths: ["run.log"], expect: "why do payments fail" } }) as { content: { text: string }[] }
      const log = read.content[0]!.text
      expect(requests.length).toBeGreaterThan(0)
      expect(requests[0].body.state.purpose).toBe("why do payments fail")
      expect(log).toStartWith("[run.log] 3002 lines, plain\n1|1\n")
      expect(log).toContain("\n1500|1500\n1501|routes: /v1/payments -> upstream payments-v2 (0 hosts)\n1502|error: pool exhausted\n1503|1501\n")
      expect(log).toMatch(new RegExp(`\\n\\[${skim}: \\d+ rows shown of 3,002 log lines; typesafe/jev-1\\.14 and keywords chose the middle for what you expect; N\\| is a row's line in the whole file: read run\\.log:A-B plain:true for others\\]$`))
      // Read again, a log that did not grow is nothing new, and the model is not asked.
      requests.length = 0
      const again = await client.callTool({ name: skim, arguments: { paths: ["run.log"], expect: "why do payments fail" } }) as { content: { text: string }[] }
      expect(again.content[0]!.text).toBe(`[run.log] 3002 lines, plain: nothing new since your last ${skim} with this expect`)
      expect(requests).toHaveLength(0)
    } finally {
      server.stop(true)
    }
  })

  test("configuration it cannot honour stops nib at startup with the reason", async () => {
    const cases: [Record<string, string>, string][] = [
      [{ NIB_DECIDER: "magic" }, "NIB_DECIDER is \"magic\"; use one of fixed, keywords, openrouter"],
      [{ NIB_DECIDER: "openrouter" }, "NIB_DECIDER=openrouter needs an OpenRouter key; nib looked in NIB_DECIDER_KEY, OPENROUTER_API_KEY, "],
      [{ NIB_DECIDER: "openrouter", NIB_DECIDER_KEY: "k", NIB_SANDBOX: "1" }, "which NIB_SANDBOX=1 allows only with NIB_DECIDER_SANDBOX=1"],
    ]
    const directory = await workspace()
    for (const [env, message] of cases) {
      const child = Bun.spawn([await binary()], { cwd: directory, env: { ...base(), ...env }, stdin: "pipe", stdout: "pipe", stderr: "pipe" })
      child.stdin.end()
      expect(await child.exited).toBe(1)
      expect(await new Response(child.stderr).text()).toContain(message)
    }
  })

  test("a decider choice nib setup remembered is read at startup, and a choice in the environment replaces it", async () => {
    const config = await mkdtemp(path.join(tmpdir(), "nib-remembered-"))
    await mkdir(path.join(config, "nib"))
    await writeFile(path.join(config, "nib", "setup.json"), JSON.stringify({ providers: [], decider: { NIB_DECIDER: "openrouter" } }))
    const directory = await workspace()
    const start = async (env: Record<string, string>) => {
      const child = Bun.spawn([await binary()], { cwd: directory, env: { ...base(), XDG_CONFIG_HOME: config, ...env }, stdin: "pipe", stdout: "pipe", stderr: "pipe" })
      child.stdin.end()
      return { exit: await child.exited, stderr: await new Response(child.stderr).text() }
    }
    const remembered = await start({})
    expect(remembered.exit).toBe(1)
    expect(remembered.stderr).toContain("NIB_DECIDER=openrouter needs an OpenRouter key")
    expect((await start({ NIB_DECIDER: "keywords" })).exit).toBe(0)
    await rm(config, { recursive: true, force: true })
  })
})
