# nib

A stdio MCP server that gives coding agents hashline file tools: reads and searches tag each file with a short content hash, and edits address line numbers instead of repeating the old text. It works with any MCP client and is meant to replace a client's built-in find-and-replace edit tools. The format follows current [oh-my-pi](https://github.com/can1357/oh-my-pi) hashline (a whole-file 4-hex tag plus plain line numbers).

Runs on Bun (it uses `Bun.hash.xxHash32` and `Bun.spawn`) and needs `rg` (ripgrep) on `PATH` for `grep`.

```sh
bun install
bun test
bun typecheck
```

## Tools

| Tool | Input | Purpose |
| --- | --- | --- |
| `read` | `paths[]`, optional `maxLineLength` | Several files, ranges, directories and images in one call; outlines large files and skips lockfiles and generated files unless a range is given. |
| `grep` | `pattern`, optional `paths[]`, `glob`, `ignoreCase`, `literal`, `before`, `after`, `skip` | ripgrep search whose output rows are valid edit anchors. |
| `edit` | `input` (patch text) | Edits, creates, moves and deletes any number of files in one call. |
| `write` | `path`, `content`, optional `tag` | Creates a file (with parents), or overwrites one when given its current tag. |

### read

```
[src/example.ts#3C4D]
1:export const enabled = false;
2:export const name = "example";
```

`TAG` is four uppercase hex digits: xxHash32 of the BOM-free text with trailing spaces, tabs and CRs stripped from each line, masked to 16 bits, identical to oh-my-pi's `file_hash`. Each `paths` entry may end in a selector: `path:A-B`, `path:A+COUNT`, `path:A` (from A on), `path:-N` (last N lines) or `path:A-B,C-D` (gaps print as `...`). An existing file whose name looks like a selector is read literally. An open selector such as `path:1-` returns up to 2000 lines. One call's 40 KiB output budget (kept near common MCP client output limits) is split fairly: each file asks for what it needs, small requests are served in full, and the largest share the rest equally, so a big file cannot starve the others. Cut output ends with `[... continue with path:N-]`, and partial selections end with `[N lines total]`.

#### Large-read safeguards

These apply only to entries without a selector; any selector, such as `path:1-`, reads the file as asked.

- **Skipped files.** Above 4 KiB, lockfiles (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock`, `Cargo.lock`, `go.sum` and similar), minified files and source maps (`*.min.js`, `*.min.css`, `*.map`, or an average line over 400 bytes) and files inside `node_modules/` or `dist/` return one line with the reason, size and an approximate token count, and no tag.
- **Outlines.** Files over 500 lines or 20 KiB return `[PATH#TAG] N lines, about Xk tokens; outline only...` followed by real `LINE:TEXT` rows: every top-level code line, plus the first-level members of top-level classes, interfaces, structs, enums, traits, impls, namespaces and similar containers. Runs of imports collapse to the first one and a `[lines A-B: N more import lines]` note. Comments, blank lines, closing-bracket lines (including signature tails such as `) -> None:`) and lines inside multi-line strings (Python triple quotes, JavaScript template literals) are left out. Indentation depth is measured in the file's own indent unit, ignoring comment lines. Markdown files outline their headings outside code fences. An outline is capped at 150 rows: top-level rows stay and members fill the remaining room in file order, with a note counting what was left out. Outline rows are shown in full, so they count as seen and can be edited directly.
- **Flat data.** When fewer than 5 outline rows exist, or more than half of the non-blank lines would be outline rows (logs, CSV, generated lists, pretty-printed JSON), the read returns the first 100 lines instead, with a hint to continue or grep.

Lines over 2000 characters print as `[N truncated at 2000 of M chars; ...]|prefix` and cannot be edited until read alone as `path:N` with a larger `maxLineLength` (up to 2 MiB). Directories list their entries; PNG, JPEG, GIF and WebP files are returned as MCP image content; PDFs are refused, so use the client's own reader for them.

Headers show paths relative to the server's working directory when inside it, absolute otherwise. Text files are limited to 2 MiB, images to 5 MiB.

### grep

```
2 matches in 2 files

[src/login.ts#1F2A]
 4:import { session } from "./session"
*5:export function login(user: User) {
 6:  const token = session.create(user)
```

`*` marks matches; other rows are context (1 before and 3 after by default). Every fully shown row counts as seen, so an edit can anchor on it without a read. Pages hold 20 files (continue with `skip`), with 20 matches per file, or 200 when only one file matches. Matched lines over 512 characters are truncated and not anchorable. The search uses `rg --hidden --no-require-git` without `.git/`; `.gitignore` applies even outside a git repository, but, as in ripgrep, a `glob` include re-includes files ignored by name (never files inside ignored directories).

### edit

```
[src/greet.ts#3C4D]
PUT 2.=2:
+  return `Hello, ${name}!`
PUT >$:
+export const version = 2
[src/old.ts#9F3E]
MV src/new.ts
[src/dead.ts#1A2B]
REM
*** Add File: src/created.ts
+export const created = true
```

| Hunk | Effect |
| --- | --- |
| `PUT N.=M:` | Replace lines `N..M` inclusive with the `+` rows that follow. |
| `PUT <N:` / `PUT >N:` | Insert the `+` rows before / after line `N`. |
| `PUT >$:` | Append at the end of the file; needs no line anchor, so it also applies under a stale tag (with a warning). |
| `CUT N.=M` | Delete lines `N..M`. |
| `PUT N*:` / `CUT N*` | Replace / delete the block that starts at line `N`. |
| `PUT >N*:` | Insert the `+` rows after the block that starts at line `N`. |
| `REM` | Delete the file; must be the only operation in its section, and needs a current tag. |
| `MV DEST` | After the section's edits, write the file to `DEST` (missing parents are created) and delete the source. Refuses an existing destination. |
| `*** Add File: PATH` | Create a file from the `+` rows that follow; refuses an existing path. |

- Line numbers refer to the tagged output and are never shifted by earlier hunks. Hunks are sorted by position and must not overlap. At a shared gap, `PUT >N:` content lands before `PUT <N+1:` content.
- Blocks: `N` in a block hunk is the block's first line: a leading comment or decorator run, or the opener itself. nib resolves the end by indentation, like its outline: the opener's deeper body, branch lines at the opener's depth that continue it (`} else {`, a lone `{`, `):`, `else:`, `except:`), and a closing line at that depth (`}`, `)`, `]`, `end`). Python-style blocks end at their last deeper line. A line that opens no multi-line block is refused, and the resolved range then follows the same seen-line, overlap and stale-tag rules as `PUT N.=M:`. oh-my-pi resolves blocks with tree-sitter instead.
- Sections repeating a path with the same tag are merged; a different tag for the same path is rejected. Paths may be quoted. Blank lines between hunks are ignored. A Codex-style `*** Begin Patch`/`*** End Patch` envelope is accepted but not needed (the tool description no longer shows it, since it only costs tokens).
- Body rows are final content: `+` followed by the exact line. Forms borrowed from other edit languages (`-` rows, `@@` headers, `*** Update File:`, `N:` rows, `PUT N-M:`, `PUT N:`) are rejected with a hint naming the correct form.
- All sections are parsed, read and checked before anything is written; files are then written one at a time, each atomically, so a write failure after the first file reports which files already changed.
- A successful section returns its new `[PATH#TAG]` and numbered rows around each change (long inserted runs are elided in the middle). Follow-up edits can use that tag and numbering without a read. A section that reproduces the current content reports `No changes` and leaves the file untouched.
- The original UTF-8 BOM, CRLF/LF style and final-newline state are retained. Publication stages a complete file and atomically replaces the target, preserving POSIX ownership and mode, not ACLs or extended attributes. Hardlinked targets are refused. Linux pins parent directories through `/proc/self/fd`; neither this nor the path/inode rechecks are a compare-and-swap against arbitrary concurrent writers.

### Snapshots, seen lines and stale tags

The server keeps an in-memory snapshot store for its process (one per client connection): up to 4 versions per path, 256 paths and 64 MiB of text, evicting the least recently used path. Reads, greps, edits and writes record the version they produced plus the lines it showed in full.

- **Seen lines.** An edit anchored on lines that version never showed in full (outside a read range, a truncated line, rows elided from an edit response) is rejected with the actual content of up to 40 such lines, which are then marked seen, so an unchanged retry succeeds. After an edit, only lines that kept both number and content stay editable, plus the rows shown in the response. A replacement on a moved line is still accepted when the new content carries that line's current text (at least 8 characters, unique in the file), a port of oh-my-pi's shifted-anchor rescue. A file with no snapshot (for example after a restart) is not restricted.
- **Stale tags.** When the file no longer matches the tag, the edit is rebased only if every anchored line maps to an unchanged current line through one uniform offset with a neighbor that moved with it; rebased edits carry a warning. Otherwise the edit is rejected with the current tag and the current content around the requested lines. Tags never issued for the file are rejected, naming the file they were issued for. A 4-hex tag can collide: if a retained version with that tag has different text than the current file, the file is treated as changed.
- **Not ported from oh-my-pi:** cut/paste registers, the syntax-tree check on rebases, the repeated no-op guard, and tree-sitter read summaries (nib's outline is indentation-based instead).

### Writes outside the workspace

By default nib writes anywhere the process may, without asking. Set `NIB_CONFIRM_OUTSIDE_ROOTS=1` to confine writes, creates, moves and deletes to the client's MCP roots, as reported through `roots/list` (the server's working directory when the client reports no roots); the list is refreshed when the client sends `roots/list_changed`. With it set, anything else asks the user through MCP elicitation: allow that file once, or everything under its directory for the rest of the session. Without elicitation support such writes are refused. The requested path is checked before any parent directory is created, and the canonical path again, so a symlink inside the workspace cannot redirect a write outside it. Reads are not restricted.

## Setup

Any MCP client that launches stdio servers can run nib: the command is `bun /path/to/nib/src/main.ts`, started in the project directory (or with `NIB_DIRECTORY` set to it). Relative paths resolve against that directory. The tools are `read`, `grep`, `edit` and `write`; the server also sends usage instructions in its MCP `instructions` field.

To get the most out of it, disable or deny the client's own find-and-replace edit and write tools, so the model edits through `edit` and creates files through `write`.

### Using with Claude Code

Register the server for every project, with its tools loaded up front instead of through tool search:

```sh
claude mcp add-json --scope user nib '{"type":"stdio","command":"bun","args":["/path/to/nib/src/main.ts"],"alwaysLoad":true}'
```

Claude Code reports its launch directory plus every `--add-dir`, `/add-dir` and `additionalDirectories` entry as MCP roots, and supports elicitation, so with `NIB_CONFIRM_OUTSIDE_ROOTS=1` (add `"env":{"NIB_CONFIRM_OUTSIDE_ROOTS":"1"}` to the server JSON) writes outside those directories ask the user. Claude Code's `--dangerously-skip-permissions` does not answer MCP elicitation, so leave it unset there. It warns about tool output above 10k tokens and truncates at 25k, which is why reads share a 40 KiB budget.

Then, in your Claude Code `settings.json`, remove the built-in edit tools from the model's context and allow the nib tools without prompts:

```json
{
  "permissions": {
    "allow": ["mcp__nib__read", "mcp__nib__grep", "mcp__nib__edit", "mcp__nib__write"],
    "deny": ["Edit", "Write", "MultiEdit"]
  }
}
```

A bare tool name in `deny` removes that tool from the model's context entirely. The built-in `Read` stays for PDFs and notebooks (and `NotebookEdit` for notebooks).

Known trade-offs of MCP edit tools in Claude Code: no inline diff approval UI, `acceptEdits` mode does not cover them (hence the allow rules), checkpoints and `/rewind` are not documented to cover MCP file changes, and formatters or hooks configured for `Edit|Write` matchers do not run for them unless their matcher also names `mcp__nib__.*`.

## Benchmark

`NIB_BENCH_SOURCE=/path/to/project bun bench/run.ts [repetitions] [concurrency] [tasks]` runs editing tasks against fresh copies of a TypeScript fixture project through headless Claude Code (`claude -p`), once with the built-in file tools and once with nib, then checks each result (a behavioral script plus `tsgo --noEmit`) and prints pass rates, output tokens, input tokens, tool calls, tool errors, turns, cost and time. The tasks and checks in `bench/run.ts` refer to specific files and functions of that fixture project, so adapt them to your own codebase. User settings are skipped in both setups so a personal deny rule cannot disable the baseline. Raw results are written to `bench/results-*.json` (ignored by git); each run's directory and stream-json transcript stay in the system temp directory.

## Files

| File | Responsibility |
| --- | --- |
| `src/main.ts` | stdio entrypoint |
| `src/server.ts` | MCP tool registration, model-facing descriptions, roots and elicitation |
| `src/engine.ts` | Reads, grep, patch staging and atomic publication, writes, workspace checks |
| `src/snapshots.ts` | File tags, the snapshot store, line remapping and shifted-line rescue |
| `src/patch.ts` | Patch language parser |
| `bench/run.ts` | Built-in versus nib comparison through headless Claude Code |
