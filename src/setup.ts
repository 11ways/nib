import { randomUUID } from "node:crypto"
import { constants, existsSync, readFileSync, realpathSync } from "node:fs"
import { chmod, mkdir, open, readFile, realpath, rename, stat, unlink } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import { HOOK_MATCHER } from "./hook"

type Json = Record<string, unknown>

export type SetupOptions = {
  directory: string
  /** Register nib for every project of this user instead of for `directory`. */
  user?: boolean
  dryRun?: boolean
  /** How to start nib; by default `nib` when that runs this copy, else this copy through the Bun running it. */
  command?: string[]
  home?: string
  env?: Record<string, string | undefined>
  /** Runs `claude` with arguments for the user-scope MCP registration, which Claude Code's own CLI makes. */
  claude?: (args: string[]) => Promise<{ exit: number; output: string }>
}

/** How to start this copy of nib: the bare `nib` when the one on PATH is this copy. */
export function nibCommand() {
  const entry = path.join(import.meta.dir, "main.ts")
  const found = Bun.which("nib")
  try {
    if (found && realpathSync(found) === realpathSync(entry)) return ["nib"]
  } catch {
    // A broken link on PATH is not this copy.
  }
  return [process.execPath, entry]
}

const quote = (word: string) => /^[\w./@:+-]+$/.test(word) ? word : `'${word.replace(/'/g, "'\\''")}'`
const isObject = (value: unknown): value is Json => typeof value === "object" && value !== null && !Array.isArray(value)

/** The words of a hook command as a shell splits plain and single-quoted words, the forms setup writes. */
function words(command: string) {
  return [...command.matchAll(/'((?:[^']|'\\'')*)'|(\S+)/g)].map((match) => match[1] !== undefined ? match[1].replace(/'\\''/g, "'") : match[2]!)
}

/** True for a hook command that runs nib's hook: `nib hook`, or Bun running the main.ts of a nib checkout or install. */
function isNibHook(command: string) {
  const parts = words(command)
  if (parts.at(-1) !== "hook") return false
  if (parts.length === 2) return path.basename(parts[0]!) === "nib"
  if (parts.length !== 3 || path.basename(parts[1]!) !== "main.ts") return false
  try {
    const manifest = JSON.parse(readFileSync(path.join(path.dirname(parts[1]!), "..", "package.json"), "utf8")) as { name?: string }
    return manifest.name === "@11ways/nib"
  } catch {
    return false
  }
}

/** True for a hook entry that already runs nib's hook, whichever way nib is started. */
function runsNibHook(entry: unknown) {
  return isObject(entry) && Array.isArray(entry.hooks) && entry.hooks.some((hook) => isObject(hook) && typeof hook.command === "string" && isNibHook(hook.command))
}

/** Adds nib to an MCP config's servers unless one named nib is there. @return what was added */
function addServer(json: Json, key: "mcpServers" | "mcp", server: Json) {
  const servers = json[key] ?? {}
  if (!isObject(servers)) throw new Error(`its "${key}" is not an object`)
  if (servers.nib !== undefined) return []
  json[key] = { ...servers, nib: server }
  return ["the nib MCP server"]
}

/** Adds nib's PreToolUse hook to Claude Code settings unless a nib hook is there. @return what was added */
function addHook(json: Json, command: string[]) {
  const hooks = json.hooks ?? {}
  if (!isObject(hooks)) throw new Error('its "hooks" is not an object')
  const entries = hooks.PreToolUse ?? []
  if (!Array.isArray(entries)) throw new Error('its "hooks.PreToolUse" is not a list')
  if (entries.some(runsNibHook)) return []
  json.hooks = { ...hooks, PreToolUse: [...entries, { matcher: HOOK_MATCHER, hooks: [{ type: "command", command: `${command.map(quote).join(" ")} hook`, timeout: 10 }] }] }
  return ["nib's PreToolUse hook"]
}

/** Approves the nib server of the project's .mcp.json, so Claude Code does not ask; one the user disabled stays disabled. @return what was added */
function approve(json: Json, notes: string[]) {
  const disabled = json.disabledMcpjsonServers
  if (Array.isArray(disabled) && disabled.includes("nib")) {
    notes.push("nib is in disabledMcpjsonServers there, so it stays off until you remove it")
    return []
  }
  const enabled = json.enabledMcpjsonServers ?? []
  if (!Array.isArray(enabled)) throw new Error('its "enabledMcpjsonServers" is not a list')
  if (json.enableAllProjectMcpServers === true || enabled.includes("nib")) return []
  json.enabledMcpjsonServers = [...enabled, "nib"]
  return ["approval of the nib server in .mcp.json"]
}

/**
 * Registers nib where Claude Code and OpenCode look for it, adding only what is missing, so a setting someone changed always wins.
 * @return one line per file: what changed, or why nothing did
 */
export async function setup(options: SetupOptions) {
  const command = options.command ?? nibCommand()
  const home = options.home ?? os.homedir()
  const env = options.env ?? process.env
  const report: string[] = []
  const display = (file: string) => {
    const relative = path.relative(options.directory, file)
    return !options.user && !relative.startsWith("..") ? relative : file.startsWith(`${home}/`) ? `~/${path.relative(home, file)}` : file
  }
  const change = async (file: string, update: (json: Json, notes: string[]) => string[]) => {
    // A link is followed, so a config kept elsewhere, as in a dotfiles repository, stays a link to it.
    const real = await realpath(file).catch(() => file)
    let text: string | undefined
    try {
      text = await readFile(real, "utf8")
    } catch (error) {
      // Only a file that is not there is a fresh start; one that cannot be read must never be replaced by a new one.
      const code = error instanceof Error && "code" in error ? String(error.code) : "an unknown error"
      if (code !== "ENOENT") {
        report.push(`${display(file)}: left untouched, since it cannot be read (${code})`)
        return
      }
    }
    let json: Json
    try {
      const parsed: unknown = text === undefined || !text.trim() ? {} : JSON.parse(text)
      if (!isObject(parsed)) throw new Error("it does not hold a JSON object")
      json = parsed
    } catch (error) {
      report.push(`${display(file)}: left untouched, since ${error instanceof SyntaxError ? "it is not plain JSON (comments?)" : error instanceof Error ? error.message : String(error)}`)
      return
    }
    const notes: string[] = []
    let added: string[]
    try {
      added = update(json, notes)
    } catch (error) {
      report.push(`${display(file)}: left untouched, since ${error instanceof Error ? error.message : String(error)}`)
      return
    }
    const extra = notes.length ? ` (${notes.join("; ")})` : ""
    if (!added.length) {
      report.push(`${display(file)}: already set up${extra}`)
      return
    }
    report.push(`${display(file)}: ${options.dryRun ? "would add" : "added"} ${added.join(" and ")}${extra}`)
    if (options.dryRun) return
    await mkdir(path.dirname(real), { recursive: true })
    // Written beside the file under a name no one else holds and renamed over it, keeping its mode, so a config holding credentials stays private.
    const mode = text === undefined ? 0o666 & ~process.umask() : (await stat(real)).mode & 0o7777
    const staged = path.join(path.dirname(real), `.${path.basename(real)}.nib-${randomUUID()}`)
    const handle = await open(staged, constants.O_WRONLY | constants.O_CREAT | constants.O_EXCL | constants.O_NOFOLLOW, 0o600)
    try {
      await handle.writeFile(`${JSON.stringify(json, null, 2)}\n`)
      await handle.sync()
    } finally {
      await handle.close()
    }
    await chmod(staged, mode)
    await rename(staged, real).catch(async (error: unknown) => {
      await unlink(staged).catch(() => undefined)
      throw error
    })
  }
  const opencodeServer = { type: "local", command }
  if (!options.user) {
    await change(path.join(options.directory, ".mcp.json"), (json) => addServer(json, "mcpServers", { type: "stdio", command: command[0], args: command.slice(1) }))
    await change(path.join(options.directory, ".claude", "settings.local.json"), (json, notes) => [...approve(json, notes), ...addHook(json, command)])
    const opencode = path.join(options.directory, "opencode.json")
    if (existsSync(opencode)) await change(opencode, (json) => addServer(json, "mcp", opencodeServer))
    return report
  }
  const config = env.CLAUDE_CONFIG_DIR || path.join(home, ".claude")
  await change(path.join(config, "settings.json"), (json) => addHook(json, command))
  // Claude Code keeps user-scope servers in a file it rewrites while it runs: setup only reads it to see whether nib is there, and its own CLI adds it.
  const registry = env.CLAUDE_CONFIG_DIR ? path.join(env.CLAUDE_CONFIG_DIR, ".claude.json") : path.join(home, ".claude.json")
  const registered = await readFile(registry, "utf8").then((content) => {
    const parsed: unknown = JSON.parse(content)
    return isObject(parsed) && isObject(parsed.mcpServers) && parsed.mcpServers.nib !== undefined
  }, () => false).catch(() => false)
  const claude = options.claude ?? (Bun.which("claude") ? runClaude : undefined)
  if (registered) report.push("claude mcp: nib is already registered for this user")
  else if (!claude) report.push(`claude is not on PATH; register the server with: claude mcp add --scope user nib -- ${command.map(quote).join(" ")}`)
  else if (options.dryRun) report.push("claude mcp: would add nib for this user")
  else {
    const added = await claude(["mcp", "add", "--scope", "user", "nib", "--", ...command])
    report.push(added.exit === 0 ? "claude mcp: added nib for this user" : `claude mcp add failed: ${added.output.trim()}`)
  }
  const opencode = path.join(env.XDG_CONFIG_HOME || path.join(home, ".config"), "opencode", "opencode.json")
  if (existsSync(opencode)) await change(opencode, (json) => addServer(json, "mcp", opencodeServer))
  return report
}

async function runClaude(args: string[]) {
  const child = Bun.spawn(["claude", ...args], { stdout: "pipe", stderr: "pipe" })
  const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited])
  return { exit, output: stdout + stderr }
}

const USAGE = `Usage: nib setup [--user] [--dry-run]
Registers nib in this folder: the MCP server in .mcp.json, its Claude Code hook and approval in .claude/settings.local.json,
and the server in opencode.json when there is one. With --user, for every project of this user instead. Only what is missing is added.`

/** `nib setup` from the command line. @return the exit code */
export async function setupMain(args: string[]) {
  const unknown = args.filter((arg) => arg !== "--user" && arg !== "--dry-run")
  if (unknown.length) {
    console.error(`${unknown.includes("--help") || unknown.includes("-h") ? "" : `Unknown option ${unknown[0]}\n`}${USAGE}`)
    return unknown.includes("--help") || unknown.includes("-h") ? 0 : 2
  }
  for (const line of await setup({ directory: process.cwd(), user: args.includes("--user"), dryRun: args.includes("--dry-run") })) console.log(line)
  return 0
}
