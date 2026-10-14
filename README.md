# nib

A stdio MCP server that gives coding agents hashline file tools: reads and searches tag each file with a short content hash, and edits address line numbers instead of repeating the old text. It works with any MCP client and is meant to replace a client's built-in find-and-replace edit tools. The format follows current [oh-my-pi](https://github.com/can1357/oh-my-pi) hashline (a whole-file 4-hex tag plus plain line numbers).

Runs on Bun (it uses `Bun.hash.xxHash32` and `Bun.spawn`) and needs `rg` (ripgrep) on `PATH` for `grep` and `find`.

```sh
bun install
bun test
bun typecheck
```

## Tools

| Tool | Input | Purpose |
| --- | --- | --- |
| `read` | `paths[]`, optional `maxLineLength`, `plain`, `collapse`, `info` | Several files, ranges, directories and images in one call; outlines large files and skips lockfiles and generated files unless a range is given; `plain` reads logs cheaply; `path#TAG` shows what changed since that tag; `info` gives one line of facts per file instead of content. |
| `grep` | `pattern`, optional `paths[]`, `glob`, `ignoreCase`, `literal`, `before`, `after`, `mode`, `noIgnore`, `maxLineLength`, `skip`, `replace` | ripgrep search whose output rows are valid edit anchors; can also just list matching files or count matches; `replace` previews a search-and-replace and returns a token that applies it. |
| `find` | optional `paths[]`, `glob`, `minSize`, `newer`, `sort`, `noIgnore`, `skip` | Lists files recursively with line counts, sizes and ages; filters by size and age, sorts by path, size or age. |
| `edit` | `input` (patch text, or `REAPPLY TOKEN` from a refusal or a grep replace) | Edits, creates, copies, moves and deletes any number of files in one call. |
| `write` | `path`, `content`, optional `tag` | Creates a file (with parents), or overwrites one when given its current tag; answers with a row map of the new file. |

### read

```
[src/example.ts#3C4D]
1:export const enabled = false;
2:export const name = "example";
```

`TAG` is four uppercase hex digits: xxHash32 of the BOM-free text with trailing spaces, tabs and CRs stripped from each line, masked to 16 bits, identical to oh-my-pi's `file_hash`. Each `paths` entry may end in a selector: `path:A` (that one line), `path:A-B`, `path:A+COUNT`, `path:A-` (from A on), `path:-N` (last N lines) or `path:A-B,C-D` (gaps print as `...`). An existing file whose name looks like a selector is read literally. An open selector such as `path:1-` returns up to 2000 lines. One call's 40 KiB output budget (kept near common MCP client output limits) is split fairly: each file asks for what it needs, small requests are served in full, and the largest share the rest equally, so a big file cannot starve the others. Cut output ends with `[... continue with path:N-]`, and partial selections end with `[N lines total]`.

#### Large-read safeguards

Skipping, outlines and the flat-data head apply only to entries without a selector; any selector, such as `path:1-`, reads the file as asked, though a minified file still gets shorter lines.

- **Skipped files.** Above 4 KiB, lockfiles (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock`, `Cargo.lock`, `go.sum` and similar), minified or bundled files and source maps, and files inside `node_modules/` or `dist/` return one line with the reason, size and an approximate token count, and no tag. A file counts as minified by its name (`*.min.js`, `*.min.css`, `*.map`, `*.bundle.js`, `*.chunk.js`, or a content hash such as `main.3f9a2c1b.js`) or when more than half of its text sits in lines over 300 characters that are dense: more code punctuation (`{}()[];,=<>+*&|!?`) than whitespace, or almost no whitespace at all. Prose written one paragraph per line is not dense, so long Markdown paragraphs never count.
- **Minified lines.** When a range read of a minified file has lines over 300 characters and no `maxLineLength` was passed, its lines are cut at 300 characters instead of 2000, and the header says so; passing `maxLineLength` restores the usual cut.
- **Outlines.** Files over 500 lines or 20 KiB return `[PATH#TAG] N lines, about Xk tokens; outline only...` followed by real `LINE:TEXT` rows: every top-level code line, plus the first-level members of top-level classes, interfaces, structs, enums, traits, impls, namespaces and similar containers. Runs of imports collapse to the first one and a `[lines A-B: N more import lines]` note. Comments, blank lines, closing-bracket lines (including signature tails such as `) -> None:`) and lines inside multi-line strings (Python triple quotes, JavaScript template literals) are left out. Indentation depth is measured in the file's own indent unit, ignoring comment lines. Markdown files outline their headings outside code fences. An outline is capped at 150 rows: top-level rows stay and members fill the remaining room in file order, with a note counting what was left out. Outline rows are shown in full, so they count as seen and can be edited directly.
- **Flat data.** When fewer than 5 outline rows exist, or more than half of the non-blank lines would be outline rows (logs, CSV, generated lists, pretty-printed JSON), the read returns the first 100 lines instead, with a hint to continue or grep.

Lines longer than `maxLineLength` (default 2000) print as `[N cut at 2000 of M chars]|prefix`, which is not an edit anchor, and the block ends with a note saying so. `SUB` changes such a line in place without reading it; to see it whole, read it with a larger `maxLineLength`. A larger value works for any selection within the shared 40 KiB budget, and one `path:N` entry alone may go up to 2 MiB; a row bigger than the whole budget is replaced by a hint to read that line alone. A line with terminal color codes prints as `[N ansi]|text` with the codes stripped, which is not an anchor either; the tag still covers the real bytes, and `SUB` edits such a line too. Directories list their entries; PNG, JPEG, GIF and WebP files are returned as MCP image content; PDFs are refused, so use the client's own reader for them.

Headers show paths relative to the server's working directory when inside it, absolute otherwise. Text files are limited to 2 MiB (plain reads excepted), images to 5 MiB.

#### Plain reads

`plain: true` reads for looking only, typically logs:

```
[app.log] lines 1147-1185 of 90210, plain
2026-10-14 12:00:01 ERROR disk full
[the line above repeats 37 more times]
2026-10-14 12:00:09 retrying in 5s with a much lo...
```

Rows carry no line numbers and the header no tag, so nothing a plain read prints is an edit anchor, and nothing is recorded. Terminal escape codes and other control characters are stripped, lines longer than `maxLineLength` end in `...`, repetition is collapsed (see below), and gaps between selected ranges print as `[lines X-Y skipped]`. The file is streamed up to the size it had when opened, without the 2 MiB text limit and without the consistency check that refuses a file changing during a normal read, so huge logs and logs still being written read fine. Selectors work as usual (a tail of up to 2000 lines, like `path:-500`, is collected in one pass; a longer one counts the lines first); at most 2000 lines are kept per file, and the 40 KiB budget and continuation hints apply as in a normal read. Images and PDFs are handled as in a normal read; other binary files are refused.

Plain reads compare lines with their digits ignored, so timestamps, counters and durations do not count as differences. A line or a block of up to 8 lines repeating back to back prints once, then a count and, unless every copy is identical, the last copy: `[4 more like the line above; the last one:]` or `[the line above repeats 4 more times]`. A block of at least 3 lines that comes back later, such as a stack trace, prints once as `[#1, 23 lines] first line` and each later copy as one row, `[#1 again, 23 lines] its own first line`. Collapsing only happens where it prints fewer rows, never across a gap between selected ranges, and when it hid other numbers the block ends with a note; `collapse: false` shows every line.

#### Changes since a tag

A `paths` entry written `path#TAG`, with a tag from an earlier read, grep, edit or write in this session, shows only what changed since that version:

```
[src/app.ts#9F1C] changed since #3C4D: 2 lines added, 1 removed
 11:import { log } from "./log"
 12:const limit = 10
-|const retries = 3
+13:const retries = 5
+14:const backoff = 200
 15:
 16:export function run() {
[lines 15-240 are old lines 14-239, moved down by 1]
```

Added lines print as `+N:TEXT`, unchanged context (2 lines around each change) as ` N:TEXT`, both numbered as the file is now and anchorable, and removed lines as `-|TEXT`, which carry no number. Gaps print as `...`. For each stretch of unchanged lines that moved and holds a line the agent saw or wrote, a note gives the old numbers (at most 10 notes). The new version is recorded like an edit response: lines that kept both number and content stay seen, the rows shown are seen, and moved lines the agent knew stay known, so a `~` check anchors them. An unchanged file answers `unchanged since #TAG`. A tag nib no longer keeps (it keeps 4 versions per file), or one issued for another file, gets a note saying so instead of a diff; a diff that takes over a second asks for a new read. `path#TAG` takes no line selector and cannot be combined with `info` or `plain`; an existing file whose name ends in `#TAG` is read literally.

#### File facts

`info: true` returns one line per entry instead of content:

```
[src/engine.ts#35C8] 1796 lines, 84.2 KiB, about 22k tokens; longest line 312 chars (line 1130); LF; modified 3m ago
[logs/boot.log] 120345 lines, 9.5 MiB, about 2490k tokens; longest line 412 chars (line 88); LF; color codes; over 2 MiB, so no tag: read it with plain: true, or grep it; modified 2h ago
src/ directory, 12 entries (3 directories); modified 1d ago
```

Lines are counted like a read counts them and the longest line is measured in characters with its color codes; line endings show as `LF`, `CRLF` or `mixed line endings (N CRLF, M LF)`, followed where they apply by `no final newline`, `BOM`, `color codes`, and the reason a whole read would skip the file (minified, lockfile, inside `node_modules/`). Images and PDFs give their type and size, binary files and files that are not UTF-8 say so. Files over 2 MiB, or ones changing while read, are streamed and get no tag. A file nib can hold gets a tag, recorded with no line shown, so an edit through it is refused for unseen lines like after a read that displayed nothing, `PUT >$:` appends work, and the tag is a base for a later `path#TAG` read. `info` takes no selectors, `plain`, `collapse` or `maxLineLength`.

### grep

```
2 matches in 2 files

[src/login.ts#1F2A]
 4:import { session } from "./session"
*5:export function login(user: User) {
 6:  const token = session.create(user)
```

`*` marks matches; other rows are context (1 before and 3 after by default). Every fully shown row counts as seen, so an edit can anchor on it without a read. Pages hold 20 files (continue with `skip`), with 20 matches per file, or 200 when only one file matches. Matched lines over `maxLineLength` characters (default 512, at most 2000) are cut and not anchorable; when the first match sits past the cut, the row shows the stretches around the first 3 matches instead of the line's start, as `*[N chars 3142-3324, 28924-29106 of 31625]|...`. The search uses `rg --hidden` without `.git/`. Inside a git repository it follows git's ignore rules, so a parent repository's `.gitignore` never hides a nested repository; outside one, `.gitignore` still applies (`--no-require-git`). `noIgnore` turns ignore files off entirely (`--no-ignore`, still without `.git/`), for sweeps across repositories that a parent repository ignores. As in ripgrep, a `glob` include re-includes files ignored by name (never files inside ignored directories); `glob` may also be a list, such as `["!build/**", "!*.md"]`.

- **Line ranges.** A file in `paths` may carry a read selector (`app.log:1100-1250`, `app.log:-500`, `app.log:900-`) to search only those lines; context rows stay inside the selection too. Unlike a read, an open range runs to the end of the file. The same file named again without a selector searches all of it; a selector on a directory is refused.
- **Modes.** `mode: "files"` prints the matching paths and `mode: "count"` prints `COUNT PATH` per file. Both start with the total (`N matches in M files`), print no rows or tags, record nothing as seen, and page 200 files at a time.
- **Big or growing files.** A file over 2 MiB, or one that changes while nib reads it (a log still being written), gets its rows from ripgrep itself: `*1147|text` for a match and ` 1148|text` for context, colors stripped and shortened at `maxLineLength` with `...`. Those rows carry no tag and are never anchors, and the block ends with a hint to read around a match with `plain: true`.
- **Output budget.** Files that do not fit the 40 KiB budget are left for the next page. The first file of a page is never left out: its rows stop at the budget, with `[output limit reached; grep path:N- to go on from line N]`.
- **Replace.** `replace` (ripgrep's syntax: `$1`, `${name}`, `$0` for the whole match) turns a search into a preview. Context defaults to none, and under each matched line that would change comes a `>N|TEXT` row with the line as it would become (a long one shows the stretch around its first replacements). Nothing is written: the output ends with `To apply the N replaced lines shown (>N| rows), call edit with input: REAPPLY t0k3n5`, and that token applies exactly the shown lines, as one whole-line `SUB` per line under the tags the preview printed, so it changes cut lines too and is rebased like any edit when a file changed meanwhile. Matches past the per-file limit or the output budget, and files nib cannot hold whole, are left out of the token and say so; grep again after applying. Patterns spanning lines and the `files` and `count` modes are refused.

### find

```
3 files, largest first

src/engine.ts (1796 lines, 84.2 KiB, 3m ago)
assets/logo.png (image/png, 12.0 KiB, 40d ago)
README.md (189 lines, 16.1 KiB, 1h ago)
```

`find` lists the files grep would search (`rg --files`: hidden files included, `.git/` skipped, ignore rules as in grep, `noIgnore` to list ignored files too), under `paths` (default the working directory). Each row has the path, the line count for text files up to 2 MiB (larger files show only their size), size and age; files whose first 8000 bytes hold control bytes count as binary, and images name their type. `glob` narrows by name or path like grep's. `minSize` (`512`, `20k`, `1.5M`, `2G`, in 1024s) and `newer` (`30s`, `10m`, `2h`, `3d`, `1w`) filter, and `sort` orders by `path` (default), `size` (largest first) or `modified` (newest first). Pages hold 200 files; continue with `skip`. Nothing is recorded as seen.

### edit

```
[src/greet.ts#3C4D]
PUT 2.=2:
+  return `Hello, ${name}!`
PUT >$:
+export const version = 2
[src/old.ts#9F3E]
MV src/new.ts
[src/template.ts#5D21]
PUT 1.=1:
+// copied from the template
CP src/variant.ts
[src/dead.ts#1A2B]
REM
*** Add File: src/created.ts
+export const created = true
```

| Hunk | Effect |
| --- | --- |
| `PUT N.=M:` | Replace lines `N..M` inclusive with the `+` rows that follow. |
| `PUT <N:` / `PUT >N:` | Insert the `+` rows before / after line `N`. |
| `PUT >$:` | Append at the end of the file; needs no line anchor, so it also applies under a stale tag that was issued for this file (with a warning), or under a `[PATH]` header with no tag at all. |
| `CUT N.=M` | Delete lines `N..M`. |
| `PUT N*:` / `CUT N*` | Replace / delete the block that starts at line `N`. |
| `PUT >N*:` | Insert the `+` rows after the block that starts at line `N`. |
| `SUB N: OLD` | Replace the text `OLD`, which must occur exactly once on line `N`, with the `+` rows: several rows split the line, a lone `+` deletes `OLD`. The old text is everything after `SUB N: ` up to the end of that patch line, so nothing is quoted or escaped. |
| `~TEXT` | A check row right under a `PUT` or `CUT` line, before its `+` rows: line `N` must start with `TEXT` after its indentation (an empty `~` means a blank line). A second `~` row checks the last line `M` of a range. |
| `REM` | Delete the file; must be the only operation in its section, and needs a current tag. |
| `MV DEST` | After the section's edits, write the file to `DEST` (missing parents are created) and delete the source. Refuses an existing destination. |
| `CP DEST` | Like `MV DEST`, but the source stays as it was: the section's edits land only in `DEST`, which keeps the source's mode. The response gives the copy's tag, and the copy inherits what the agent saw and knew of the source, so it can be edited right away. |
| `*** Add File: PATH` | Create a file from the `+` rows that follow; refuses an existing path. |
| `[PATH]` | A section header without `#TAG`; it may only append with `PUT >$:`. |

- Line numbers refer to the tagged output and are never shifted by earlier hunks. Hunks are sorted by position (after block ends are resolved) and must not overlap: a line can be replaced by one hunk only, two hunks cannot insert at the same place, and an insert may sit right before or after a replaced range, so `PUT 2.=2:` with `PUT >2:` replaces line 2 and adds rows after it. At a shared gap, `PUT >N:` content lands before `PUT <N+1:` content.
- Blocks: `N` in a block hunk is the block's first line: a leading comment or decorator run, or the opener itself. nib resolves the end by indentation, like its outline: the opener's deeper body, branch lines at the opener's depth that continue it (`} else {`, a lone `{`, `):`, `else:`, `except:`), and a closing line at that depth (`}`, `)`, `]`, `end`). Python-style blocks end at their last deeper line. A line that opens no multi-line block is refused, and the resolved range then follows the same seen-line, overlap and stale-tag rules as `PUT N.=M:`. oh-my-pi resolves blocks with tree-sitter instead.
- Sections repeating a path with the same tag are merged; a different tag for the same path is rejected. Paths may be quoted. Blank lines between hunks are ignored. A Codex-style `*** Begin Patch`/`*** End Patch` envelope is accepted but not needed (the tool description no longer shows it, since it only costs tokens).
- Body rows are final content: `+` followed by the exact line. Forms borrowed from other edit languages (`-` rows, `@@` headers, `*** Update File:`, `N:` rows, `PUT N-M:`, `PUT N:`) are rejected with a hint naming the correct form.
- All sections are parsed, read and checked before anything is written; files are then written one at a time, each atomically, so a write failure after the first file reports which files already changed.
- A refusal lists every problem in the patch at once: every malformed line, and for every section its unseen lines, stale tag, failed checks or `SUB` texts. One corrected resend is then enough.
- When the only problems are unseen lines (see below), the refusal ends with `To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY t0k3n5`. That input applies the stored patch through the full normal validation, as an unchanged resend would; the patch's own tags still guard against files that changed meanwhile. A token belongs to its session, works once, and only the latest 64 are kept. Any other refusal gets no token, since the patch itself must change. `grep` with `replace` hands out the same kind of token for the patch it previewed.
- `SUB` replaces only text the patch quotes, so it is exempt from the seen-line rule and works on cut long lines. Several `SUB`s on one line apply to the original line and must not overlap. Its result counts as known content only when the whole original line was known.
- A successful section returns its new `[PATH#TAG]` and numbered rows around each change. Up to 40 `N~TEXT` row-map rows follow in file order: in the middle of a long inserted run instead of every line, and over moved lines the agent already knew (an insert or delete shifts everything below it; a row for a line it never saw would be no anchor). Moved lines past those 40 rows get one note per stretch, such as `[lines 427-1004 are old lines 423-1000, moved down by 4]`. An edit that moves nothing adds no map. A changed row past the cut (2000 characters, or 300 in a minified file) shows the stretch where it changed when a line was replaced by one line, as `[N chars 2952-3057 of 3108]|...`, instead of the line's start. Follow-up edits can use that tag and numbering without a read. A section that reproduces the current content reports `No changes` and leaves the file untouched.
- The original UTF-8 BOM, CRLF/LF style and final-newline state are retained. Publication stages a complete file and atomically replaces the target, preserving POSIX ownership and mode, not ACLs or extended attributes. Hardlinked targets are refused. Linux pins parent directories through `/proc/self/fd`; neither this nor the path/inode rechecks are a compare-and-swap against arbitrary concurrent writers.

### write

```
Created [notes/brief.md#40FA] (23 lines, 19 non-blank, 5 shown)
1~# Handoff
5~## Settled
15~- item 15
19~## Do the remaining work befor...
23~Last line
```

`write` and `*** Add File:` answer with a sparse row map of the new file, so it can be edited without reading it back. Each `N~TEXT` row is a line number plus the first 30 characters after the indentation (`...` when the line goes on). The first and last non-blank lines are shown; a non-blank line right after a blank one is shown once at least 4 lines passed since the last shown row; and after 10 non-blank lines without a shown row, the 10th is shown. Blank lines are never listed. Past 300 rows the response is the outline a read would give instead, or no rows for flat data. Shown rows count as displayed; every other line counts as known, so a `~` check anchors it. Edit responses show the same kind of rows inside the elided middle of a long inserted run.

### Snapshots, seen lines and stale tags

The server keeps an in-memory snapshot store for its process (one per client connection): up to 4 versions per path, 256 paths and 64 MiB of text, evicting the least recently used path. Reads, greps, edits and writes record the version they produced plus the lines it showed in full; `info` reads record the version with no line shown, and `path#TAG` reads record it like an edit response.

- **Seen lines.** An edit may anchor on lines its tagged output showed in full, or as `N~TEXT` rows. Otherwise it is rejected with the actual content of up to 40 such lines plus 2 lines around each (`*` marks the anchors); all of those rows are then marked seen, so an unchanged retry or the `REAPPLY` token succeeds, and a miscount can be fixed from the rows around it. After an edit, only lines that kept both number and content stay displayed, plus the rows shown in the response, including `N~TEXT` rows of moved lines the agent already knew. A replacement on a moved line is still accepted when the new content carries that line's current text (at least 8 characters, unique in the file), a port of oh-my-pi's shifted-anchor rescue. A file with no snapshot (for example after a restart) is not restricted. A file created by `write` or `*** Add File:` with fewer than 10 lines is trusted whole.
- **Known lines and checks.** nib also remembers lines whose whole content the agent knows without having seen their current number: every line of a file it created, every row it inserted, and seen lines that an edit moved. A `~TEXT` check confirms a line's number. An insert may anchor on any line a check confirms; a replace or cut may span known lines when each end that was not displayed is confirmed (a block's end, which nib finds itself, needs no check). Lines the agent neither saw nor wrote stay refused for replacement, check or not.
- **Stale tags.** When the file no longer matches the tag, the edit is rebased only if every anchored line maps to an unchanged current line through one uniform offset with a neighbor that moved with it; rebased edits carry a warning. Otherwise the edit is rejected with the current tag and the current content around the requested lines. Tags never issued for the file are rejected, naming the file they were issued for, even for a section that only appends; a `[PATH]` header without a tag appends with no tag at all. A 4-hex tag can collide: if a retained version with that tag has different text than the current file, the file is treated as changed.
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
| `src/engine.ts` | Reads (selectors, outlines, plain reads, file facts, changes since a tag), grep and replace previews, find, patch staging and atomic publication, writes, row maps, refusals and reapply tokens, workspace checks |
| `src/snapshots.ts` | File tags, the snapshot store with seen and known lines, line remapping and shifted-line rescue |
| `src/patch.ts` | Patch language parser |
| `bench/run.ts` | Built-in versus nib comparison through headless Claude Code |
