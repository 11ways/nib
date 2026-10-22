# nib

An MCP server that gives coding agents hashline file tools and shell,
based on [oh-my-pi](https://github.com/can1357/oh-my-pi)'s hashline format.

Reads and searches print a `[PATH#TAG]` header (a 4-hex hash of the whole file) and `LINE:TEXT` rows.
Edits have to be made using those line numbers, instead of repeating the old text.

The shell (named nibash) is a bash interpreter whose own `cat`, `grep`, `head`, `sed -n` and similar print the same rows
when being output to the model (outputs piped in to a file or other program remain the same as normal bash)

Lines printed by these tools are also marked as "seen", so the assistant can directly edit a file from grep results.

All of these features have been added to suit my needs and may or may not fit your workflow.

nib replaces a client's built-in file tools and shell.
`nib setup` configures Claude Code and OpenCode, but only Claude Code has been battle tested.

All the rest of the README is generated with Claude

## Requirements

- Linux, macOS, or Windows (but only Linux has been tested heavily)
- `rg` (ripgrep) on `PATH`
- `bwrap` for sandbox mode (Linux only)
- Go to build; Bun for the black-box tests and the benchmark

## Install

```sh
GOPRIVATE='github.com/11ways/*' go install github.com/11ways/nib/cmd/nib@latest
```

From a checkout, `./install.sh` builds and installs to `~/.local/bin` (or `$GOBIN`). Restart the client's nib server
after updating. `nib --version` prints the version.

## Setup

```sh
nib setup             # in a project folder
nib setup --user      # for every project
nib setup --dry-run   # show what would change
```

`nib setup` only adds what is missing, so running it again changes nothing. It writes:

- `.mcp.json`: the `nib` server.
- `.claude/settings.local.json`: approval of that server, nib's hook (for `Read`, nib's own tools, `SessionStart`,
  `PostCompact` and `UserPromptSubmit`), deny rules for `Bash`, `Grep`, `Glob`, `Edit` and `Write`, and an allow rule
  for `mcp__nib__nibash_status`.
- `opencode.json`, if the project has one: the `nib` server with `codemode: false`, and deny rules for `shell`, `read`,
  `grep`, `glob` and `edit`.

With `--user` the settings go to the Claude config folder's `settings.json` (`$CLAUDE_CONFIG_DIR`, else `~/.claude`),
the server is registered with `claude mcp add --scope user`, OpenCode's user `opencode.json` is updated if it exists,
and the nibash mod is installed. Files that are not plain JSON are left alone.

Denying `Bash` also breaks a skill's `!` command lines, and subagents inherit the deny; nibash is their shell.

These options are remembered in `~/.config/nib/setup.json` and apply to every nib unless its environment overrides
them; `--decider=`, `--summary=`, `--opencode=` and `--provider NAME=` clear them:

| Option | Effect |
| --- | --- |
| `--provider NAME=PROGRAM` | host a provider's tools ([Providers](#providers)) |
| `--decider KIND` | `fixed`, `keywords` or `openrouter:MODEL,...` |
| `--summary LIST` | summarizers for long output |
| `--opencode PROGRAM` | the OpenCode 2 binary `opencode:` summarizers run |

### By hand

Any MCP client can run `nib` as a stdio server, started in the project directory or with `NIB_DIRECTORY` set. For
Claude Code:

```sh
claude mcp add-json --scope user nib '{"type":"stdio","command":"nib","alwaysLoad":true}'
```

then deny `Bash`, `Grep`, `Glob`, `Edit` and `Write` in `settings.json`. Keep the built-in `Read` for PDFs and
notebooks.

In Claude Code, MCP edits get no inline diff approval, are not covered by `acceptEdits`, checkpoints or `/rewind`, and
hooks matching `Edit|Write` only run for them when they also match `mcp__nib__.*`.

## Tools

| Tool | Input | Purpose |
| --- | --- | --- |
| `read` | `paths[]`, optional `maxLineLength`, `plain`, `collapse`, `info`, `full` | Files, ranges, declarations, directories, images, revisions and SQLite tables. |
| `grep` | `pattern`, optional `paths[]`, `glob`, `ignoreCase`, `literal`, `before`, `after`, `mode`, `noIgnore`, `maxLineLength`, `skip`, `replace` | ripgrep search; match rows are edit anchors; `replace` previews a replacement. |
| `find` | optional `paths[]`, `glob`, `minSize`, `newer`, `sort`, `noIgnore`, `skip` | Lists files with line counts, sizes and ages. |
| `edit` | `input` (a patch, or `REAPPLY TOKEN`) | Edits, creates, copies, moves and deletes files. |
| `write` | `path`, `content`, optional `tag` | Creates a file, or overwrites one given its current tag. |
| `nibash` | `command`, optional `description`, `expect`, `timeout`, `background` | Runs a command in nibash. |
| `nibash_status` | optional `tail`, `view`, `restore`, `summaries` | Lists running commands and jobs; used by the nibash mod. |
| `skim` | `paths[]`, `expect` | Reads long files (logs, test reports) for what `expect` asks, without edit anchors. |

`skim` is offered only with nibash. Tool arguments are lenient: numbers as strings, another tool's argument names
(`file_path`, `offset`, `limit`), and JSON lists sent as strings are accepted with a note.

### read

```
[src/example.ts#3C4D]
1:export const enabled = false;
2:export const name = "example";
```

`TAG` is xxHash32 of the BOM-free text with trailing spaces, tabs and CRs stripped per line, masked to 16 bits
(identical to oh-my-pi's `file_hash`). A path may carry a selector:

| Selector | Reads |
| --- | --- |
| `path:A`, `path:A-B`, `path:A+N`, `path:A-`, `path:-N`, `path:A-B,C-D` | lines |
| `path:name`, `path:Type.member`, `path:a,b` | declarations, with their comments and annotations |
| `path#TAG` | what changed since that version (nib keeps 4 per file) |
| `path@REV` | the file at a git revision; a folder gives the commit and its diff |
| `file.db:TABLE`, `file.db:TABLE:-15`, `file.db:SELECT ...` | rows of a SQLite database file, or a read-only query |

Declarations are found with tree-sitter for Java, Kotlin, TypeScript, TSX, JavaScript, Go, Python, Rust, C, C++ and
Zig, and as blocks and snippets in Hawkeye (`.hwk`) templates.

Without a selector:

- Files over 500 lines or 20 KiB (code over 200 lines or 8 KiB) return an outline: real rows for top-level lines and
  first-level members. Flat data (logs, CSV, JSON) returns its first 100 lines instead.
- Lockfiles, minified or bundled files and files in `node_modules/` or `dist/` over 4 KiB are skipped with their size.
- A SQLite database lists its tables, columns and row counts.

In Java and Kotlin, runs of 10 or more imports print as a package tree. A call's output is capped at 40 KiB, shared
fairly between its files. Lines over `maxLineLength` (default 2000) print as `[line N, chars 1-2000 of M]|...`, which
is not an anchor; `maxLineLength` goes up to 2 MiB. Text files are limited to 8 MiB. Images (PNG, JPEG, GIF, WebP) are
returned as image content, scaled down to what the model sees; PDFs are refused.

- `plain: true` prints lines without numbers or tag, strips color codes and collapses repeats; it has no size limit and
  suits logs.
- `info: true` prints one line of facts per file: lines, size, longest line, line endings, age.
- Runs of 3 or more lines the agent already saw unchanged print as `N~TEXT` rows or one
  `[lines 40-120 unchanged since you saw them]` row; `full: true` prints them whole.

### skim

`skim` reads a file as plain lines and shapes it like nibash's long output, for the `expect` given (`did the build
pass; if not, which tests failed and why`). Rows are `N|TEXT`, numbered by file line, never anchors. Skimming again
with the same `expect` shows only lines added since; with another `expect`, the lines already shown are left out.

### grep

```
2 matches in 2 files

[src/login.ts#1F2A]
 4:import { session } from "./session"
*5:export function login(user: User) {
 6:  const token = session.create(user)
```

`*` marks matches; context rows default to 1 before and 1 after, at most 40 per page, given first to definitions. Pages
hold 20 files (`skip` for more) and 20 matches per file (100 when one file matches). Every row shown whole is an edit
anchor.

- The search is `rg --hidden` without `.git/`, with ripgrep's `--engine auto`, so look-around runs on PCRE2.
- Git's ignore rules apply inside repositories; a nested repository its parent ignores is still searched by its own
  rules. `noIgnore` turns ignore files off.
- A path may carry a line selector (`app.log:1100-1250`) to search only those lines.
- `mode: "files"` lists matching paths; `mode: "count"` counts per file.
- Files over 8 MiB or still being written print `N|TEXT` rows, which are not anchors.
- `replace` (ripgrep's syntax: `$1`, `${name}`) previews changed lines as `>N|TEXT` rows and returns a token; `edit`
  with `REAPPLY TOKEN` applies exactly those lines.

### find

```
3 files, largest first

src/engine.ts (1796 lines, 84.2 KiB, 3m ago)
assets/logo.png (image/png, 12.0 KiB, 40d ago)
README.md (189 lines, 16.1 KiB, 1h ago)
```

Lists the files grep would search. `glob` filters by name, `minSize` (`20k`, `1.5M`) and `newer` (`30m`, `2h`, `3d`)
by size and age, `sort` is `path`, `size` or `modified`. Pages hold 200 files; a larger listing without `skip` shows
folders with their file counts instead. Each folder is printed once, with its files indented under it.

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
| `PUT N.=M:` / `PUT N:` | Replace lines `N..M` (or line `N`) with the `+` rows below. |
| `PUT <N:` / `PUT >N:` | Insert before / after line `N`. |
| `PUT >$:` | Append at the end of the file; works without a tag. |
| `CUT N.=M` / `CUT N` | Delete lines. |
| `PUT N*:` / `CUT N*` | Replace / delete the block starting at line `N`. |
| `PUT >N*:` | Insert after the block starting at line `N`. |
| `SUB N: OLD` | Replace text `OLD`, which occurs once on line `N`, with the `+` rows; a lone `+` deletes it. |
| `~TEXT` | Under a `PUT` or `CUT`: check that line `N` starts with `TEXT`; a second `~` checks line `M`. |
| `REM` | Delete the file (to the trash). |
| `MV DEST` | Move the file after the section's edits. |
| `CP DEST` | Copy the file; the section's edits land only in the copy. |
| `IMPORT a.b.C, ...` | Add Java or Kotlin imports where the file keeps its own. |
| `*** Add File: PATH` | Create a file from the `+` rows. |
| `[PATH]` | A header without a tag; allows only `PUT >$:` and `IMPORT`. |

- Line numbers refer to the tagged output and are never shifted by earlier hunks. Hunks must not overlap.
- A hunk may only anchor on lines the agent has seen whole (in a read, grep, nibash rows or an edit response) or
  confirmed with a `~` check. Otherwise the edit is refused, showing those lines, with a token that applies it
  unchanged.
- A stale tag is rebased when every anchored line maps to an unchanged current line; otherwise the edit is refused with
  the current content.
- Block ends (`N*`) come from tree-sitter in the languages `read` parses, from indentation elsewhere.
- Diff-style `-` rows that match the replaced lines are accepted.
- Every section is checked before anything is written. Each file is written atomically, keeping its BOM, line endings,
  final newline, mode and owner. Hardlinked targets are refused.
- The response gives the new tag and one unchanged row on each side of every change; rows the agent sent print as
  `[lines 40-46 as you sent them]`.
- The edit applies but warns when a range looks one line off, brackets stop balancing, the file stops parsing, a Java
  class is used without an import (giving the `IMPORT` hunk), or a JSON file becomes invalid or repeats a key.

### write

Creates a file with its parents, or overwrites one given its current tag. Overwriting keeps a CRLF file's line endings
and its BOM. The response is a sparse map of `N~TEXT` rows; every written line counts as seen, so it can be edited
right away.

## nibash

A bash interpreter ([mvdan/sh](https://github.com/mvdan/sh)). Every call starts afresh in the project directory: `cd`,
variables and functions do not carry over, and calls run in parallel. Jobs and named directories do carry over.
Programs run as real processes with empty stdin, `PAGER=cat`, `GIT_PAGER=cat`, `TERM=dumb` and
`GIT_TERMINAL_PROMPT=0`. A command still running at `timeout` (default 2 minutes, at most 10) is not stopped; it goes on
as a job.

`cat`, `head`, `tail`, `grep`, `egrep`, `fgrep`, `rg`, `sed -n`, `cut -c` and `wc` run as nib's own when their output
reaches the model and nib supports their options; they print `[PATH#TAG]` blocks of anchored rows. Anything else,
including those commands with an unsupported option, piped into a real program or redirected to a file, runs the system
program. `type cat` names the system program.

- **git.** Where its output reaches the model, `status`, `diff`, `show`, `log`, `ls-files` and the subcommands that
  change a repository run the real git and are reformatted: paths grouped by folder, `--stat` with exact counts, diffs
  as anchored rows, commit headers on one line (abbreviated hash, author email unless it is yours, local time to the
  minute), and hints and progress lines hidden. The full output stays in the log.
- **find and ls** print paths grouped by folder.
- **git-each ARGS** runs `git ARGS` in every repository below the current directory. `{base}` is the repository's
  `base` in `nib.json`, else `origin/HEAD`.
- **Deletes go to the trash.** `rm`, `unlink`, `git rm`, `git clean`, `find -delete` and `edit`'s `REM` move files to
  the user's trash. `trash list` shows them, `trash restore [-f] PATH` puts one back.
- **A failed `cd`** (or `pushd`, `popd`) ends the call unless the command line handles it (`if`, `&&`, `||`), as under
  `set -e`.
- **Jobs.** `background: true` or `cmd &` start a job; `jobs`, `wait` and `kill` work on them, and `$!` is a job spec
  (`%3`). Finished jobs are reported at the next call.
- **Changed files** are listed with their new tags after the command; small changes to files the agent saw print as
  rows.
- **Failures** that the exit status hides (stderr sent to `/dev/null`, an earlier pipeline stage) are named in a closing
  line.
- `hash -d name=dir` defines a named directory; bare `hash -d` lists them. A pipeline's last stage runs in the shell, so
  `... | read v` sets `v`.

### Long output

Output over 400 rows or 30 KiB is cut and saved whole to a log in `$XDG_RUNTIME_DIR/nib/` (or the temp directory). A
decider picks the rows that stay; rows of a cut are numbered `N|TEXT` by log line, so `read LOG:A-B plain:true` shows
a gap.

| Decider | Keeps |
| --- | --- |
| `keywords` (default) | the first 30 and last 100 rows, plus rows naming errors or failures with 3 rows around each |
| `fixed` | the first 100 and last 200 rows |
| `openrouter` | keyword hits plus windows a decision model rates as relevant to the call's `description` and `expect` |

With `NIB_DECIDER_SUMMARY`, output over 150 lines or 12 KB is summarized instead: a `<summary>` of up to 3 sentences,
then the log lines it points at. Summarizers are OpenRouter model ids, `claude-p:MODEL` (runs `claude -p` with no
tools) or `opencode:PROVIDER/MODEL` (runs OpenCode 2 with an isolated data folder). `/nibash summaries off` or
`nibash_status`'s `summaries` turns them off for a session. With `openrouter`, output and commands are sent to
OpenRouter with zero data retention requested.

Files named `*.log` or `*.out`, or in an untracked `log`, `logs` or `test-logs` folder, print as plain output when read
through nibash, so they are cut and summarized too.

`nib shape --log PATH --purpose TEXT [--exit N] [--command TEXT]` prints what nibash would show of a saved log, for
build and test wrappers.

### Command rules

A `nib.json` can forbid commands:

```json
{"rules": [
  {"program": ["gradlew", "gradle"], "why": "Gradle runs through zenit-dev's MCP tools"},
  {"program": "zenit-dev", "args": "^(build|test)( |$)", "why": "use zd_build / zd_test"},
  {"rule": "Do not run full test suites directly.", "match": "gradle|test", "why": "Use the zenit-dev MCP tools."}
]}
```

nibash checks every command a call may run, including inside pipelines, `$(...)`, wrappers like `env` and `xargs`, and
`bash -c` strings. `program` rules match by base name, `args` is a regex over the arguments. `rule` entries are judged
by the decision model when `match` hits, and are skipped without one. A refused call runs nothing and exits 126. The
nearest `nib.json` with rules at or above the call's directory applies.

## Named directories

nib shortens long paths with names: `~orm/workflow/Repo.java`. Names work in every tool and, unquoted, in nibash;
programs receive the real path. They come from:

- `nib.json` in the server's directory: `{"directories": {"orm": "path/to/orm"}}`.
- JVM source roots (`src/<set>/java`, `src/<set>/kotlin`) with a package prefix of 24 characters or more, unless
  `"autoDirectories": false`.
- `hash -d name=dir` in nibash, for that agent and the subagents it starts afterwards.
- Directories an agent keeps seeing, which nib names itself and announces in a note. Names used in two sessions are kept
  in `~/.local/state/nib/names/` for later ones. `NIB_AUTO_NAMES=0` turns this off.

## Agents and seen lines

nib tracks what each agent has seen, per file version, to decide which lines an edit may anchor on. Claude Code
subagents are told apart by nib's hook, OpenCode sessions by their session id; a fork starts with a copy of what its
parent had seen. When an agent's context is compacted or cleared, what it saw before stops counting. What agents saw is
kept across restarts in `~/.local/state/nib/seen/` as hashes and line numbers, never text.

The hook also refuses the built-in `Read` for files nib can read, naming the equivalent nib call; sending the same call
again lets it through.

## Sandboxing

- `NIB_CONFIRM_OUTSIDE_ROOTS=1`: writes outside the client's MCP roots ask the user through elicitation, and are refused
  without it. Reads are not restricted.
- `NIB_SANDBOX=1`: every tool, reads included, stays inside the roots, judged after resolving symlinks. nibash's
  programs run in bwrap with the system read-only, the roots writable, a private `/tmp` and no network
  (`NIB_SANDBOX_NETWORK=1` allows it). Without bwrap nib refuses to start unless `NIB_SHELL=0`.

## Providers

`nib --provider NAME=PROGRAM` runs `PROGRAM nib-provider`, an MCP server on stdio, and offers its tools as `NAME_TOOL`
(zemble's `search` becomes `zemble_search`). A provider answers with prose and locations, which nib prints as anchored
rows of the file as it is now. The protocol is in `docs/design-providers.md`.

## The nibash mod

`mods/nibash` is a Claude Code mod, installed by `nib setup --user`. It shows running nibash calls and jobs in a band
above the prompt and a toast when a job ends. Its slash commands open panes: `/nibash` (commands and their output),
`/trash` (with restore buttons), `/tokens` (characters returned per agent and tool), `/names` and `/seen`.

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `NIB_DIRECTORY` | working directory | the project directory |
| `NIB_SHELL` | on | `0` turns nibash off |
| `NIB_SANDBOX` | unset | `1` confines every tool to the roots |
| `NIB_SANDBOX_NETWORK` | unset | `1` gives sandboxed programs the network |
| `NIB_SANDBOX_BIND` | unset | directories (`:` separated) bound read-only in the sandbox |
| `NIB_PROVIDERS_SANDBOX` | unset | `1` allows providers under `NIB_SANDBOX=1` |
| `NIB_CONFIRM_OUTSIDE_ROOTS` | unset | `1` asks before writes outside the roots |
| `NIB_AUTO_NAMES` | on | `0` turns off automatic named directories |
| `NIB_CHANNEL` | unset | `1` when Claude Code was started with nib's channel, so job ends wake the agent |
| `NIB_DECIDER` | `keywords` | `fixed`, `keywords` or `openrouter` |
| `NIB_DECIDER_KEY` | `$OPENROUTER_API_KEY` | OpenRouter key; else `~/.config/nib/openrouter-key`, then OpenCode's login |
| `NIB_DECIDER_MODEL` | `typesafe/jev-1.14` | decision models, comma separated, in order of preference |
| `NIB_DECIDER_URL` | `https://openrouter.ai/api/alpha/decisions` | decision endpoint |
| `NIB_DECIDER_TIMEOUT` | `30000` | milliseconds per decision |
| `NIB_DECIDER_SUMMARY` | unset | summarizers, comma separated, in order of preference |
| `NIB_DECIDER_SUMMARY_URL` | `https://openrouter.ai/api/v1/chat/completions` | chat endpoint |
| `NIB_DECIDER_SUMMARY_TIMEOUT` | `30000` | milliseconds per summary |
| `NIB_DECIDER_OPENCODE` | `opencode` | the OpenCode 2 binary |
| `NIB_DECIDER_CACHE` | unset | directory to cache decisions and summaries on disk |
| `NIB_DECIDER_SANDBOX` | unset | `1` allows `openrouter` under `NIB_SANDBOX=1` |

nib keeps its files under the home folder on every OS (`XDG_*` variables move them): settings in `~/.config/nib`,
state in `~/.local/state/nib`, the mod in `~/.local/share/nib`.

## Platforms

- On macOS the trash is `~/.Trash`; on Windows nib uses a freedesktop-layout trash in `~/.local/share/Trash`.
- On Windows, programs run in a job object; `kill` supports `HUP`, `INT`, `QUIT`, `KILL` and `TERM`. nibash runs
  programs from `PATH` (e.g. Git for Windows), and process substitution is not available.
- Sandbox mode needs bwrap, so it works on Linux only.

## Development

```sh
go test ./...     # Go unit tests
bun install
bun test          # black-box suites against the binary over MCP
bun typecheck     # go vet and tsgo --noEmit
gofmt -l .
```

`bun bench/run.ts` compares a harness's built-in tools with nib on real editing tasks through Claude Code and OpenCode.
It makes paid model calls; `--plan`, `--validate`, `--reparse`, `--recheck` and `--report` do not. Results are stored
in `bench/results/` and reused.

See `AGENTS.md` for the source layout, `docs/` for design notes.
