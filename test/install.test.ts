import { afterEach, beforeEach, expect, test } from "bun:test"
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"

const script = path.resolve(import.meta.dir, "../install.sh")
let directory: string
beforeEach(async () => { directory = await mkdtemp(path.join(tmpdir(), "nib install-")) })
afterEach(async () => { await rm(directory, { recursive: true, force: true }) })

async function install(env: Record<string, string>) {
  const child = Bun.spawn([script], {
    cwd: directory,
    env: { ...process.env, ...env },
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exit, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { exit, stdout, stderr }
}

test("installs to ~/.local/bin from outside the checkout", async () => {
  // Keep Go's existing caches while giving the installer a temporary home.
  const config = Bun.spawnSync(["go", "env", "-json", "GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"])
  expect(config.exitCode).toBe(0)
  const result = await install({ ...JSON.parse(config.stdout.toString()), HOME: directory, GOBIN: "" })
  expect(result.stderr).toBe("")
  expect(result.exit).toBe(0)
  expect(await Bun.file(path.join(directory, ".local/bin/nib")).exists()).toBe(true)
  expect(result.stdout).toContain("nib v0.2.0")
  expect(result.stdout).toContain(`Put ${directory}/.local/bin first on PATH`)
  expect(result.stdout).toContain("Restart your nib MCP server")
}, 120_000)

test("honors GOBIN with spaces and recognizes the active PATH entry", async () => {
  const bin = path.join(directory, "custom bin")
  const result = await install({ GOBIN: bin, PATH: `${bin}:${process.env.PATH}` })
  expect(result.stderr).toBe("")
  expect(result.exit).toBe(0)
  expect(await Bun.file(path.join(bin, "nib")).exists()).toBe(true)
  expect(result.stdout).toContain("nib v0.2.0")
  expect(result.stdout).not.toContain("first on PATH")
}, 120_000)

test("stops on a failed build without running the old binary", async () => {
  await writeFile(path.join(directory, "go"), "#!/bin/sh\nexit 42\n", { mode: 0o755 })
  const old = "#!/bin/sh\nprintf 'old binary ran\\n'\n"
  await writeFile(path.join(directory, "nib"), old, { mode: 0o755 })
  const result = await install({ GOBIN: directory, PATH: `${directory}:${process.env.PATH}` })
  expect(result.exit).toBe(42)
  expect(result.stdout).not.toContain("old binary ran")
  expect(result.stdout).not.toContain("Restart")
  expect(await readFile(path.join(directory, "nib"), "utf8")).toBe(old)
})

test("explains when Go is missing", async () => {
  const result = await install({ GOBIN: directory, PATH: directory })
  expect(result.exit).toBe(1)
  expect(result.stderr).toContain("Go is required")
  expect(await Bun.file(path.join(directory, "nib")).exists()).toBe(false)
})
