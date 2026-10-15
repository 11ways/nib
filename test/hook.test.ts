import { afterEach, describe, expect, test } from "bun:test"
import { chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { decide, extended, runHook, structure, tokenize } from "../src/hook"

const directories: string[] = []
afterEach(async () => { await Promise.all(directories.splice(0).map((directory) => rm(directory, { recursive: true, force: true }))) })

/** A project folder with a few files, and the call each command or tool call is refused with (the first line and the retry note left out). */
async function project() {
  const directory = await mkdtemp(path.join(tmpdir(), "nib-hook-"))
  directories.push(directory)
  await mkdir(path.join(directory, "src/mux"), { recursive: true })
  for (const file of ["src/smoke_a.zig", "src/smoke_b.zig", "src/mux/peermail.zig", "README.md", "notes.md"]) await writeFile(path.join(directory, file), "x\n")
  const call = (tool: string, input: Record<string, unknown>) => {
    const reason = decide({ cwd: directory, tool_name: tool, tool_input: input }, "/home/user")
    return reason === undefined ? undefined : reason.split("\n").slice(1, -1).join("\n")
  }
  return { directory, bash: (command: string) => call("Bash", { command }), call }
}

describe("shell commands", () => {
  test("a search with ripgrep or grep becomes the nib grep it asks for, shell globs expanded and paths relative to the session", async () => {
    const p = await project()
    expect(p.bash(`cd ${p.directory} && rg -n -i 'hub_hello|hubHello|dialed|federation' src/smoke_*.zig src/mux/peermail.zig | head -60`))
      .toBe('mcp__nib__grep {"pattern":"hub_hello|hubHello|dialed|federation","paths":["src/smoke_a.zig","src/smoke_b.zig","src/mux/peermail.zig"],"ignoreCase":true}')
    // grep reads basic regular expressions; nib reads extended ones.
    expect(p.bash("grep -rn 'a\\|b(c)' src --include='*.zig' -A 2")).toBe('mcp__nib__grep {"pattern":"a|b\\\\(c\\\\)","paths":["src"],"glob":"*.zig","after":2}')
    expect(p.bash("grep -E 'a|b' README.md")).toBe('mcp__nib__grep {"pattern":"a|b","paths":["README.md"]}')
    expect(p.bash("grep -Fw 'a.b' README.md")).toBe('mcp__nib__grep {"pattern":"\\\\b(?:a\\\\.b)\\\\b","paths":["README.md"]}')
    expect(p.bash("grep -lr TODO .")).toBe('mcp__nib__grep {"pattern":"TODO","paths":["."],"mode":"files"}')
    expect(p.bash("grep -rn TODO src | wc -l")).toBe('mcp__nib__grep {"pattern":"TODO","paths":["src"],"mode":"count"}')
    expect(p.bash("rg -t ts -S needle")).toBe('mcp__nib__grep {"pattern":"needle","glob":["*.ts","*.tsx","*.mts","*.cts"],"ignoreCase":true}')
    expect(p.bash("rg -uu -C3 needle src")).toBe('mcp__nib__grep {"pattern":"needle","paths":["src"],"noIgnore":true,"before":3,"after":3}')
    expect(p.bash("cd src && rg needle")).toBe('mcp__nib__grep {"pattern":"needle","paths":["src"]}')
    expect(p.bash("rg --files -g '*.md'")).toBe('mcp__nib__find {"glob":"*.md"}')
  })

  test("reads with cat, head, tail, sed, wc and ls become nib reads with line selectors", async () => {
    const p = await project()
    expect(p.bash("cat README.md notes.md")).toBe('mcp__nib__read {"paths":["README.md","notes.md"]}')
    expect(p.bash("head -n 30 README.md")).toBe('mcp__nib__read {"paths":["README.md:1-30"]}')
    expect(p.bash("head README.md")).toBe('mcp__nib__read {"paths":["README.md:1-10"]}')
    expect(p.bash("tail -50 README.md")).toBe('mcp__nib__read {"paths":["README.md:-50"]}')
    expect(p.bash("tail -n +5 README.md")).toBe('mcp__nib__read {"paths":["README.md:5-"]}')
    expect(p.bash("sed -n '10,40p' README.md")).toBe('mcp__nib__read {"paths":["README.md:10-40"]}')
    expect(p.bash("sed -n 7p README.md")).toBe('mcp__nib__read {"paths":["README.md:7"]}')
    // A head or tail after a read of one file is a narrower range of it.
    expect(p.bash("cat README.md | head -n 5")).toBe('mcp__nib__read {"paths":["README.md:1-5"]}')
    expect(p.bash("head -100 README.md | tail -20")).toBe('mcp__nib__read {"paths":["README.md:81-100"]}')
    expect(p.bash("sed -n '10,40p' README.md | head -3")).toBe('mcp__nib__read {"paths":["README.md:10-12"]}')
    expect(p.bash("cat -n README.md | tail -5")).toBe('mcp__nib__read {"paths":["README.md:-5"]}')
    expect(p.bash("cat README.md | grep -i todo")).toBe('mcp__nib__grep {"pattern":"todo","paths":["README.md"],"ignoreCase":true}')
    expect(p.bash("wc -l src/*.zig")).toBe('mcp__nib__read {"paths":["src/smoke_a.zig","src/smoke_b.zig"],"info":true}')
    expect(p.bash("ls src")).toBe('mcp__nib__read {"paths":["src"]}')
    expect(p.bash("find src -name '*.zig' -type f -mtime -2")).toBe('mcp__nib__find {"paths":["src"],"glob":"*.zig","newer":"2d"}')
    expect(p.bash(`cat ${path.join(p.directory, "../outside.txt")}`)).toBe(`mcp__nib__read {"paths":["${path.join(path.dirname(p.directory), "outside.txt")}"]}`)
  })

  test("logs and other files read only to look at become plain reads, without numbers or tag", async () => {
    const p = await project()
    await mkdir(path.join(p.directory, "logs"))
    await mkdir(path.join(p.directory, "src/log"))
    for (const file of ["app.log", "build.out", "logs/run.txt", "src/log/Logger.java", "notes.txt"]) await writeFile(path.join(p.directory, file), "x\n")
    await writeFile(path.join(p.directory, "dump.json"), "x".repeat(2 * 1024 * 1024 + 1))
    expect(p.bash("tail -50 app.log")).toBe('mcp__nib__read {"paths":["app.log:-50"],"plain":true}')
    expect(p.bash("cat build.out logs/run.txt")).toBe('mcp__nib__read {"paths":["build.out","logs/run.txt"],"plain":true}')
    // A file nib cannot tag is read plain whatever its kind.
    expect(p.bash("head -n 5 dump.json")).toBe('mcp__nib__read {"paths":["dump.json:1-5"],"plain":true}')
    // The end of a file that is not code or config is most likely a log; code keeps its anchors, even in a package named log.
    expect(p.bash("tail -20 notes.txt")).toBe('mcp__nib__read {"paths":["notes.txt:-20"],"plain":true}')
    expect(p.bash("tail -20 README.md")).toBe('mcp__nib__read {"paths":["README.md:-20"]}')
    expect(p.bash("cat src/log/Logger.java")).toBe('mcp__nib__read {"paths":["src/log/Logger.java"]}')
    expect(p.bash("cat notes.txt")).toBe('mcp__nib__read {"paths":["notes.txt"]}')
    expect(p.call("Read", { file_path: path.join(p.directory, "app.log"), offset: 100, limit: 50 })).toBe('mcp__nib__read {"paths":["app.log:100+50"],"plain":true}')
    // A plain read is no edit anchor, so the refusal does not claim one.
    expect(decide({ cwd: p.directory, tool_name: "Bash", tool_input: { command: "tail app.log" } })!.split("\n")[0]).toBe("nib's hook refused this command: nib does this with fewer tokens. Use instead:")
  })

  test("appends and new files written with fixed text become nib appends and writes; overwrites stay with the shell", async () => {
    const p = await project()
    expect(p.bash("cat >> notes.md <<'EOF'\n- one\n- two\nEOF")).toBe('mcp__nib__edit with input "[notes.md]\\nPUT >$:" followed by one + row per line (an append needs no tag)')
    expect(p.bash("printf '%s\\n' \"- event\" >> notes.md")).toContain("mcp__nib__edit with input")
    expect(p.bash("echo done >> notes.md")).toContain("mcp__nib__edit with input")
    expect(p.bash("tee -a notes.md <<EOF\nline\nEOF")).toContain("mcp__nib__edit with input")
    expect(p.bash("cat > new.md <<'EOF'\nhello\nEOF")).toBe('mcp__nib__write with path "new.md" and the same content')
    expect(p.bash("cat > notes.md <<'EOF'\nhello\nEOF")).toBeUndefined()
    // Text that ends without a newline cannot be appended as rows.
    expect(p.bash("printf 'no newline' >> notes.md")).toBeUndefined()
  })

  test("an append stays with the shell unless nib's would write the same bytes, and a write unless its folder exists", async () => {
    const p = await project()
    await writeFile(path.join(p.directory, "open.txt"), "old")
    await writeFile(path.join(p.directory, "crlf.txt"), "a\r\nb\r\n")
    // The shell glues the text to an unterminated last line, and writes LF into a CRLF file, where nib would do neither.
    expect(p.bash("echo new >> open.txt")).toBeUndefined()
    expect(p.bash("echo new >> crlf.txt")).toBeUndefined()
    expect(p.bash("echo x > missing/new.md")).toBeUndefined()
  })

  test("commands nib cannot stand in for run: filters of other output, follow mode, expansions, loops and programs reading the output", async () => {
    const p = await project()
    for (const command of [
      "bun test 2>&1 | grep -i fail",
      "git log --oneline | head -5",
      "tail -f README.md",
      "head -c 100 README.md",
      "cat $FILE",
      "echo \"$HOME\" >> notes.md",
      "cat <<EOF >> notes.md\n$HOME\nEOF",
      "cat <<EOF | bun run -\nconsole.log(1)\nEOF",
      "cat README.md | jq .",
      "cat README.md > copy.md",
      "grep -v x README.md",
      "grep -o x README.md",
      "grep needle",
      "ls -la src",
      "find . -name '*.zig' -exec wc -l {} +",
      "for f in src/*.zig; do wc -l $f; done",
      "(cd src && cat a)",
      "cat /proc/cpuinfo",
      "sed -i 's/a/b/' README.md",
      "echo 'unterminated",
    ]) expect(p.bash(command)).toBeUndefined()
  })

  test("a translation never drops what the command would do: control flow, redirections, every stage and argument", async () => {
    const p = await project()
    for (const command of [
      // Only cd DIR && may come before the one command; alternatives, background jobs and other commands change what runs.
      "false && echo x >> notes.md",
      "cd src & cat smoke_a.zig",
      "cd src; cat smoke_a.zig",
      "git status && cat README.md",
      "cat README.md || true",
      "cd - && cat README.md",
      // A glob mixing quoted wildcards, which the shell keeps literal, with ones it expands.
      "cat src/'smoke*'.?ig",
      "grep -e \"$PATTERN\" README.md",
      "cat README.md | cat notes.md",
      "cat README.md 2>errors.log",
      "rg needle 0<README.md",
      "cat README.md | head -c 10",
      "tail -50 README.md | head -5",
      "head -5 README.md notes.md | tail -2",
      "sed -n '1,5p' README.md notes.md",
      "find src -maxdepth 0 -type f",
      "find src -name *.zig",
      // grep -w with a non-word edge, PCRE and backreferences read otherwise in nib's regex engine.
      "grep -w @ README.md",
      "grep -P '\\d+' README.md",
      "rg -P 'a(?=b)' src",
      "grep '\\(a\\)\\1' README.md",
      "rg -t ts -g '*.test.ts' needle",
    ]) expect(p.bash(command)).toBeUndefined()
    expect(p.bash("cd -- src && cat smoke_a.zig")).toBe('mcp__nib__read {"paths":["src/smoke_a.zig"]}')
    expect(p.bash("cat 'src/smoke_a.zig' 2>/dev/null")).toBe('mcp__nib__read {"paths":["src/smoke_a.zig"]}')
  })
})

describe("built-in tools", () => {
  test("Read, Grep and Glob become their nib calls; PDFs and notebooks stay with Read", async () => {
    const p = await project()
    const file = path.join(p.directory, "README.md")
    expect(p.call("Read", { file_path: file, offset: 10, limit: 20 })).toBe('mcp__nib__read {"paths":["README.md:10+20"]}')
    expect(p.call("Read", { file_path: file })).toBe('mcp__nib__read {"paths":["README.md"]}')
    expect(p.call("Read", { file_path: path.join(p.directory, "a.pdf") })).toBeUndefined()
    expect(p.call("Read", { file_path: path.join(p.directory, "a.ipynb") })).toBeUndefined()
    expect(decide({ cwd: p.directory, tool_name: "Read", tool_input: { file_path: file } })!.split("\n").at(-1)).toBe("(If you need Read itself here, or nib's tools are missing from this session, send the same call again and it runs.)")
    expect(p.call("Grep", { pattern: "x", path: path.join(p.directory, "src"), output_mode: "count", "-i": true, "-C": 2 }))
      .toBe('mcp__nib__grep {"pattern":"x","paths":["src"],"ignoreCase":true,"mode":"count","before":2,"after":2}')
    expect(p.call("Glob", { pattern: "**/*.zig" })).toBe('mcp__nib__find {"glob":"**/*.zig"}')
    // A Grep nib would answer otherwise runs: a type nib cannot name, a type and a glob together (an intersection), a multiline search.
    expect(p.call("Grep", { pattern: "x", type: "ruby" })).toBeUndefined()
    expect(p.call("Grep", { pattern: "x", type: "ts", glob: "**/*.test.ts" })).toBeUndefined()
    expect(p.call("Grep", { pattern: "a\\nb", multiline: true })).toBeUndefined()
    expect(p.call("Grep", { pattern: "x", type: "ts" })).toBe('mcp__nib__grep {"pattern":"x","glob":["*.ts","*.tsx","*.mts","*.cts"]}')
  })

  test("Edit, MultiEdit and Write point at nib's edit and write; other tools run", async () => {
    const p = await project()
    expect(p.call("Edit", { file_path: path.join(p.directory, "README.md") })).toContain("mcp__nib__edit: read or grep the lines of README.md")
    expect(p.call("MultiEdit", { file_path: path.join(p.directory, "README.md") })).toContain("mcp__nib__edit")
    expect(p.call("Write", { file_path: path.join(p.directory, "new.md"), content: "x" })).toBe('mcp__nib__write with path "new.md" and the same content')
    expect(p.call("Write", { file_path: path.join(p.directory, "README.md"), content: "x" })).toContain("the tag of its latest read")
    expect(p.call("NotebookEdit", { notebook_path: "a.ipynb" })).toBeUndefined()
    expect(p.call("mcp__nib__read", { paths: ["README.md"] })).toBeUndefined()
  })
})

describe("hook output", () => {
  test("a refusal is Claude Code's deny decision, and the same call sent again runs", async () => {
    const p = await project()
    const state = path.join(p.directory, ".state")
    const input = JSON.stringify({ session_id: "s1", cwd: p.directory, tool_name: "Bash", tool_input: { command: "cat README.md" } })
    const first = JSON.parse(await runHook(input, state)) as { hookSpecificOutput: Record<string, string> }
    expect(first.hookSpecificOutput.hookEventName).toBe("PreToolUse")
    expect(first.hookSpecificOutput.permissionDecision).toBe("deny")
    expect(first.hookSpecificOutput.permissionDecisionReason).toBe([
      "nib's hook refused this command: nib does this with fewer tokens, and its rows are edit anchors. Use instead:",
      'mcp__nib__read {"paths":["README.md"]}',
      "(If you need the shell's own output here, or nib's tools are missing from this session, send the same command again and it runs.)",
    ].join("\n"))
    expect(await runHook(input, state)).toBe("")
    // Another session gets its own refusal.
    expect(await runHook(input.replace('"s1"', '"s2"'), state)).toContain('"deny"')
    expect(await runHook(JSON.stringify({ session_id: "s1", cwd: p.directory, tool_name: "Bash", tool_input: { command: "git status" } }), state)).toBe("")
    expect(await runHook("not json", state)).toBe("")
  })

  test("a resend counts by what the call does: another description, or another key order, is the same call", async () => {
    const p = await project()
    const state = path.join(p.directory, ".state")
    const call = (tool_input: Record<string, unknown>, tool_name = "Bash") => runHook(JSON.stringify({ session_id: "s1", cwd: p.directory, tool_name, tool_input }), state)
    expect(await call({ command: "cat README.md", description: "Show the readme" })).toContain('"deny"')
    expect(await call({ description: "Show the readme again", command: "cat README.md" })).toBe("")
    const file = path.join(p.directory, "README.md")
    expect(await call({ file_path: file, limit: 5 }, "Read")).toContain('"deny"')
    expect(await call({ limit: 5, file_path: file }, "Read")).toBe("")
  })

  test("state the hook cannot keep private turns refusals off instead of trusting it", async () => {
    const p = await project()
    const state = path.join(p.directory, ".open")
    await mkdir(state, { mode: 0o755 })
    await chmod(state, 0o755)
    expect(await runHook(JSON.stringify({ session_id: "s1", cwd: p.directory, tool_name: "Bash", tool_input: { command: "cat README.md" } }), state)).toBe("")
    // A state file that is a link is never written through.
    const private_ = path.join(p.directory, ".private")
    await mkdir(private_, { mode: 0o700 })
    await symlink(path.join(p.directory, "notes.md"), path.join(private_, "s2.txt"))
    expect(await runHook(JSON.stringify({ session_id: "s2", cwd: p.directory, tool_name: "Bash", tool_input: { command: "cat README.md" } }), private_)).toBe("")
    expect(await readFile(path.join(p.directory, "notes.md"), "utf8")).toBe("x\n")
  })
})

describe("shell parsing", () => {
  test("quotes, escapes and here-documents", () => {
    const words = (command: string) => tokenize(command)?.map((token) => "word" in token ? token.word.text : `<${token.op}>`)
    expect(words(`echo 'a b' "c \\"d\\"" e\\ f`)).toEqual(["echo", "a b", 'c "d"', "e f"])
    expect(words("a && b | c; d 2>/dev/null")).toEqual(["a", "<&&>", "b", "<|>", "c", "<;>", "d", "<2>>", "/dev/null"])
    const document = tokenize("cat <<-'EOF' > f\n\tone\n\tEOF\necho next")!
    expect(document.find((token) => "op" in token && token.op === "<<-")).toMatchObject({ body: "one\n", dynamic: false })
    expect(structure(document)!.map((pipeline) => pipeline.commands.map((command) => command.words.map((word) => word.text)))).toEqual([[["cat"]], [["echo", "next"]]])
    expect(structure(tokenize("cd a && b")!)!.map((pipeline) => pipeline.after)).toEqual([undefined, "&&"])
    expect(tokenize("echo 'open")).toBeUndefined()
    expect(structure(tokenize("if true; then cat a; fi")!)).toBeUndefined()
  })

  test("basic regular expressions convert to extended ones", () => {
    expect(extended("a\\|b")).toBe("a|b")
    expect(extended("f(x)+")).toBe("f\\(x\\)\\+")
    expect(extended("\\(ab\\)\\{2\\}")).toBe("(ab){2}")
    expect(extended("\\<word\\>")).toBe("\\bword\\b")
    expect(extended("a\\.b")).toBe("a\\.b")
  })
})
