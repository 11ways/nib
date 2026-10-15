#!/usr/bin/env bun
// Each command loads only what it needs: the hook runs before every guarded tool call, so it stays clear of the MCP server's modules.
const [command, ...rest] = process.argv.slice(2)

const USAGE = `nib: hashline file tools for coding agents
  nib                  run the MCP server on stdio
  nib setup            register nib in this folder (.mcp.json, .claude/settings.local.json, opencode.json); --user for every project, --dry-run to preview
  nib hook             Claude Code PreToolUse hook: refuses shell file reads and built-in file tools that nib does better
  nib --version        print the version`

if (command === "hook") {
  const { runHook } = await import("./hook")
  process.stdout.write(await runHook(await Bun.stdin.text()))
} else if (command === "setup") {
  const { setupMain } = await import("./setup")
  process.exitCode = await setupMain(rest)
} else if (command === "--version" || command === "-v") {
  const { version } = await Bun.file(new URL("../package.json", import.meta.url)).json() as { version: string }
  console.log(`nib ${version}`)
} else if (command === "--help" || command === "-h") {
  console.log(USAGE)
} else if (command !== undefined) {
  console.error(`Unknown command ${command}\n${USAGE}`)
  process.exitCode = 2
} else {
  const { StdioServerTransport } = await import("@modelcontextprotocol/sdk/server/stdio.js")
  const { createServer } = await import("./server")
  const server = createServer({ directory: process.env.NIB_DIRECTORY || process.cwd(), confirmOutsideRoots: process.env.NIB_CONFIRM_OUTSIDE_ROOTS === "1", sandbox: process.env.NIB_SANDBOX === "1" })
  const shutdown = () => { void server.close().finally(() => process.exit(0)) }
  process.stdin.once("close", shutdown)
  process.once("SIGTERM", shutdown)
  process.once("SIGINT", shutdown)
  await server.connect(new StdioServerTransport())
}
