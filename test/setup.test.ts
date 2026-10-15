import { afterEach, describe, expect, test } from "bun:test"
import { chmod, lstat, mkdir, mkdtemp, readFile, rm, stat, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { HOOK_MATCHER } from "../src/hook"
import { setup } from "../src/setup"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })

async function folder() {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-setup-"))
  directories.push(directory)
  return directory
}

const json = async (file: string) => JSON.parse(await readFile(file, "utf8")) as Record<string, any>
const command = ["nib"]

describe("nib setup", () => {
  test("registers the server, its hook and its approval in a fresh project, and a second run changes nothing", async () => {
    const directory = await folder()
    expect(await setup({ directory, command })).toEqual([
      ".mcp.json: added the nib MCP server",
      ".claude/settings.local.json: added approval of the nib server in .mcp.json and nib's PreToolUse hook",
    ])
    expect(await json(path.join(directory, ".mcp.json"))).toEqual({ mcpServers: { nib: { type: "stdio", command: "nib", args: [] } } })
    expect(await json(path.join(directory, ".claude/settings.local.json"))).toEqual({
      enabledMcpjsonServers: ["nib"],
      hooks: { PreToolUse: [{ matcher: HOOK_MATCHER, hooks: [{ type: "command", command: "nib hook", timeout: 10 }] }] },
    })
    expect(await setup({ directory, command })).toEqual([".mcp.json: already set up", ".claude/settings.local.json: already set up"])
  })

  test("adds to existing configs without changing what is there, and tops up opencode.json when the project has one", async () => {
    const directory = await folder()
    await writeFile(path.join(directory, ".mcp.json"), JSON.stringify({ mcpServers: { sketerm: { type: "stdio", command: "sketerm", args: ["mcp"] } } }))
    await mkdir(path.join(directory, ".claude"))
    const hook = { matcher: "Bash", hooks: [{ type: "command", command: "./guard.sh" }] }
    await writeFile(path.join(directory, ".claude/settings.local.json"), JSON.stringify({ enabledMcpjsonServers: ["sketerm"], hooks: { PreToolUse: [hook] } }))
    await writeFile(path.join(directory, "opencode.json"), JSON.stringify({ permission: { bash: { "*": "allow" } }, mcp: { "zenit-dev": { type: "local", command: ["zenit-dev", "mcp"] } } }))
    // A nib copy outside PATH, at a path with a space: setup writes its Bun and entrypoint, and later knows that hook as nib's own.
    const install = path.join(directory, "opt/my nib")
    await mkdir(path.join(install, "src"), { recursive: true })
    await writeFile(path.join(install, "package.json"), JSON.stringify({ name: "@11ways/nib" }))
    const engine = ["/usr/bin/bun", path.join(install, "src/main.ts")]
    expect(await setup({ directory, command: engine })).toEqual([
      ".mcp.json: added the nib MCP server",
      ".claude/settings.local.json: added approval of the nib server in .mcp.json and nib's PreToolUse hook",
      "opencode.json: added the nib MCP server",
    ])
    expect((await json(path.join(directory, ".mcp.json"))).mcpServers).toEqual({ sketerm: { type: "stdio", command: "sketerm", args: ["mcp"] }, nib: { type: "stdio", command: "/usr/bin/bun", args: [engine[1]] } })
    const settings = await json(path.join(directory, ".claude/settings.local.json"))
    expect(settings.enabledMcpjsonServers).toEqual(["sketerm", "nib"])
    expect(settings.hooks.PreToolUse).toEqual([hook, { matcher: HOOK_MATCHER, hooks: [{ type: "command", command: `/usr/bin/bun '${engine[1]}' hook`, timeout: 10 }] }])
    const opencode = await json(path.join(directory, "opencode.json"))
    expect(opencode.permission).toEqual({ bash: { "*": "allow" } })
    expect(opencode.mcp.nib).toEqual({ type: "local", command: engine })
    // A nib entry someone changed counts as set up, whichever way it starts nib.
    expect(await setup({ directory, command })).toEqual([".mcp.json: already set up", ".claude/settings.local.json: already set up", "opencode.json: already set up"])
  })

  test("a dry run writes nothing; a server the user disabled stays off; a file that is not plain JSON is left untouched", async () => {
    const directory = await folder()
    expect(await setup({ directory, command, dryRun: true })).toEqual([
      ".mcp.json: would add the nib MCP server",
      ".claude/settings.local.json: would add approval of the nib server in .mcp.json and nib's PreToolUse hook",
    ])
    expect(await readFile(path.join(directory, ".mcp.json")).catch(() => undefined)).toBeUndefined()
    await mkdir(path.join(directory, ".claude"))
    await writeFile(path.join(directory, ".claude/settings.local.json"), JSON.stringify({ disabledMcpjsonServers: ["nib"] }))
    await writeFile(path.join(directory, ".mcp.json"), "{ // comment\n}")
    expect(await setup({ directory, command })).toEqual([
      ".mcp.json: left untouched, since it is not plain JSON (comments?)",
      ".claude/settings.local.json: added nib's PreToolUse hook (nib is in disabledMcpjsonServers there, so it stays off until you remove it)",
    ])
    expect((await json(path.join(directory, ".claude/settings.local.json"))).enabledMcpjsonServers).toBeUndefined()
  })

  test("for the user: the hook in the Claude config folder, the server through Claude Code's CLI, and OpenCode's config when there is one", async () => {
    const home = await folder()
    const calls: string[][] = []
    const claude = async (args: string[]) => {
      calls.push(args)
      return { exit: 0, output: "" }
    }
    await mkdir(path.join(home, "xdg/opencode"), { recursive: true })
    await writeFile(path.join(home, "xdg/opencode/opencode.json"), "{}")
    const env = { CLAUDE_CONFIG_DIR: path.join(home, "configs/nib"), XDG_CONFIG_HOME: path.join(home, "xdg") }
    expect(await setup({ directory: home, user: true, command, home, env, claude })).toEqual([
      "~/configs/nib/settings.json: added nib's PreToolUse hook",
      "claude mcp: added nib for this user",
      "~/xdg/opencode/opencode.json: added the nib MCP server",
    ])
    expect(calls).toEqual([["mcp", "add", "--scope", "user", "nib", "--", "nib"]])
    expect((await json(path.join(home, "configs/nib/settings.json"))).hooks.PreToolUse[0].hooks[0].command).toBe("nib hook")
  })

  test("for the user, only a user-scope server counts: one a project registers does not", async () => {
    const home = await folder()
    const env = { CLAUDE_CONFIG_DIR: path.join(home, "configs/nib") }
    await mkdir(env.CLAUDE_CONFIG_DIR, { recursive: true })
    // Claude Code keeps user-scope servers at the top of .claude.json, and a project's own under projects.
    await writeFile(path.join(env.CLAUDE_CONFIG_DIR, ".claude.json"), JSON.stringify({ projects: { [home]: { mcpServers: { nib: { command: "nib" } } } } }))
    const calls: string[][] = []
    const claude = async (args: string[]) => (calls.push(args), { exit: 0, output: "" })
    expect(await setup({ directory: home, user: true, command, home, env, claude })).toContain("claude mcp: added nib for this user")
    await writeFile(path.join(env.CLAUDE_CONFIG_DIR, ".claude.json"), JSON.stringify({ mcpServers: { nib: { command: "nib" } } }))
    expect(await setup({ directory: home, user: true, command, home, env, claude })).toContain("claude mcp: nib is already registered for this user")
    expect(calls).toHaveLength(1)
  })

  test("a config keeps its mode and its link, an unreadable one is never replaced, and only nib's own hook counts as nib's", async () => {
    const directory = await folder()
    await writeFile(path.join(directory, ".mcp.json"), JSON.stringify({ mcpServers: { secret: { command: "x", env: { TOKEN: "t" } } } }))
    await chmod(path.join(directory, ".mcp.json"), 0o600)
    const dotfiles = await folder()
    await writeFile(path.join(dotfiles, "settings.local.json"), JSON.stringify({ hooks: { PreToolUse: [{ matcher: "Bash", hooks: [{ type: "command", command: "bun /opt/other/src/main.ts hook" }] }] } }))
    await mkdir(path.join(directory, ".claude"))
    await symlink(path.join(dotfiles, "settings.local.json"), path.join(directory, ".claude/settings.local.json"))
    await setup({ directory, command })
    expect(((await stat(path.join(directory, ".mcp.json"))).mode & 0o777).toString(8)).toBe("600")
    expect((await lstat(path.join(directory, ".claude/settings.local.json"))).isSymbolicLink()).toBe(true)
    // Another program's main.ts hook is not nib's, so nib's is added beside it.
    expect((await json(path.join(dotfiles, "settings.local.json"))).hooks.PreToolUse.map((entry: { hooks: { command: string }[] }) => entry.hooks[0]!.command)).toEqual(["bun /opt/other/src/main.ts hook", "nib hook"])
    const unreadable = await folder()
    await writeFile(path.join(unreadable, ".mcp.json"), JSON.stringify({ mcpServers: { keep: { command: "x" } } }))
    await chmod(path.join(unreadable, ".mcp.json"), 0o000)
    expect((await setup({ directory: unreadable, command }))[0]).toBe(".mcp.json: left untouched, since it cannot be read (EACCES)")
    await chmod(path.join(unreadable, ".mcp.json"), 0o600)
    expect((await json(path.join(unreadable, ".mcp.json"))).mcpServers).toEqual({ keep: { command: "x" } })
  })
})
