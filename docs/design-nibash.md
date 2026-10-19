# Design: nib in Go, nibash, and named directories

Status: implemented. The design was revised after review (Fable, 2026-10-15: "sound with changes"; every finding is
addressed below and marked [Bn], [Mn], [mn] or [missing]); section 13 lists where the implementation departed from it,
and why.

## 1. Goals

1. **nibash**: nib's own shell tool. Models keep writing the bash they know. File lines that flow through nibash's own
   commands keep their origin (file, line, tag), so what the model finally sees carries `N:` edit anchors. Every other
   command is a real program on the real filesystem.
2. **Output shaped for the model** at the one place it reads output: color codes stripped, repeats collapsed, long
   output cut with the whole kept in a log, and the facts a model would otherwise ask for again (`lines 1-20 of 1,234`,
   `20 of 312 matches`).
3. **Named directories** (`~name/...`, zsh's syntax) so long paths cost a few tokens.
4. **One static binary** holding all of it plus the existing `read`, `grep`, `find`, `edit` and `write`, whose behavior
   does not change.

Non-goals: emulating GNU tools byte for byte (section 5.3 makes it unnecessary); a virtual filesystem; permission
prompts (the user runs agents in bypass mode); a decision model (section 11).

Evidence, from 197k real shell calls (`research/bash-usage.ts`, `research/shell-then-edit.ts`):
- 52% of Claude Code's Bash calls only inspect files. `grep | head` alone is 41M characters, and `sed -n N,Mp` is 20.6k
  calls. `-n` is on 47.6k of 79.3k `grep` calls and 7.8k of 9.6k `rg` calls.
- About 39% of nib edits (Opus 5.5) follow a shell view of the same file. Each then costs a re-read (23% of nib edits)
  or fails (16%, a third of those refused as "never displayed").
- Files flow into a real program in only 338 of 118k pipelines.
- Over half of all shell output characters (203M of 364M) is program output, which only shaping reduces.

## 2. Language: Go

Verified against the sources by the review:
- **mvdan.cc/sh/v3 v3.14.1** (`interp`) runs bash with real processes. It has the extension points nibash needs:
  `ExecHandlers`, `CallHandler`, `OpenHandler`, `StatHandler` and `ReadDirHandler2`.
  - Pipelines are `os.Pipe()` pairs (`runner.go:501`).
  - `HandlerContext.Stdin` is an `*os.File` (`handler.go:72-76`).
  - Exec'd programs get `hc.Stdout` directly (`handler.go:154-156`).
  - `Run` keeps shell state between calls (`api.go:992-994`), and `Subshell` deep-copies variables (`api.go:1066-1100`).
- Tilde expansion asks the environment for `HOME <name>` before `user.Lookup` (`expand/expand.go:966-977`).
- **github.com/modelcontextprotocol/go-sdk v1.8.0** has:
  - `ServerSession.ListRoots`, `ServerSession.Elicit` and `RootsListChangedHandler`. These are deprecated by SEP-2577
    within a 12-month window, so `NIB_DIRECTORY` and the server's directory remain the lasting basis [m1].
  - A handler context that is cancelled on `notifications/cancelled` (`mcp/transport.go:252-271`).
- Go strings are bytes, so BOMs, CRLF and invalid UTF-8 survive untouched. xxHash32 is not in the standard library and
  the common package is 64-bit, so `internal/tag` implements it [m4].

**Rejected alternatives:**
- **Rust:** no embeddable bash interpreter that runs host programs, and its ripgrep crates do not reproduce rg's
  command-line behavior.
- **Zig:** no MCP SDK, regex engine or ignore-rule library.
- **TypeScript:** Bun can connect processes, but there is no embeddable bash interpreter to build on [m8].

`rg` stays a runtime dependency for `grep`, `find` and nibash's `grep -r`/`rg`. `git` stays optional.

## 3. Layout

```
cmd/nib/            main: server (default), setup, hook, --version
internal/tag/       file tag (oh-my-pi file_hash), pinned vectors
internal/js/        JavaScript string semantics (UTF-16 lengths, JS whitespace) that nib's output rules were defined with
internal/diff/      port of jsdiff's diffArrays, pinned against jsdiff on a corpus
internal/snapshots/ per-session versions, seen and known lines, remapping, shifted-line rescue
internal/patch/     patch language parser, pinned against the TypeScript parser on a corpus
internal/names/     named directories: configuration, automatic names, expansion and display
internal/engine/    read, grep, find, edit, write
internal/nibash/    interpreter wiring, sessions, stage classification, record links, nib commands, the terminal sink, jobs, sandbox
internal/server/    MCP tools, descriptions, argument recovery and notes, roots, elicitation
internal/setup/     nib setup
internal/hook/      the Read guard, and subagent records for nib's own tools (section 8)
internal/hookstate/ the hook's private state folder; agent records from hook to server
test/               the bun suites, black-box against the binary (section 9)
bench/              TypeScript, launching the binary
```

`src/*.ts` is deleted once the Go binary passes the conformance suite: one implementation, not two.

## 4. Port of the existing tools

Behavior is ported, not redesigned:
- **Diff:** `internal/diff` ports jsdiff's `diffArrays`, including its 1 s timeout, and is checked against jsdiff output
  on 400 random cases.
- **Strings:** text is held as Go strings. Where TypeScript counted UTF-16 code units (`maxLineLength`, column windows,
  `alike`, `changedSpan`), `internal/js` counts the same way. Where it counted bytes (`Buffer` sizes, limits), Go counts
  bytes.
- **Regexes:** the patch parser's one backreference and the outline's one lookahead (`#(?![[!]|include\b)`, bundle
  hashes) become direct checks. `localeCompare` (`countsByFolder`) becomes a fixed comparator: by count, then bytewise
  [m4].
- **Atomic writes** keep their steps: open the parent directory, stage with `O_EXCL|O_NOFOLLOW`, fsync, rename or link
  through `/proc/self/fd`, re-check descriptors. They preserve BOM, line endings, final newline, mode and ownership, and
  refuse hardlinked targets.
- **ripgrep and git** get the same arguments (`--json`, `core.fsmonitor=false`, no link following).
- **Concurrency:** per-path locks become a keyed lock map with ordered acquisition. A cancelled call stops waiting.

## 5. nibash

### 5.1 The tool

```
nibash
  command      string, required
  description  string, optional: what the command is for, in 5 to 12 words
  timeout      number, ms; default 120000, at most 600000
  background   boolean; run detached, return a job id and log path at once
```

`description` is optional, as in Claude Code's Bash, so it costs nothing when unused [m6]. It is kept with the call and
handed to the renderer as the stated purpose, the input a later decision model would use (section 11).

`expect` is optional too: what the agent expects or wants to learn ("all pass; if not, which and why"). The decision
model's question then asks above all for a line that shows the expectation did not hold, and the summary prompt asks to
lead with what contradicts it, a run that meets it getting one sentence. On the 33 recorded captures
(`NIB_CAPTURES_EXPECT=1`), with an expectation written per case: the decision model alone kept every needed line in 27
cases against 25; the summary arm kept 27 either way (pytest-v-fail gained, go-build-x lost) with summaries a quarter
shorter (6,381 characters against 8,394) and 5% fewer bytes shown, apart from javac-errors, a list, which the decision
model then judged to need every line and cut instead of summarizing.

The `skim` tool reads files through the same shaping as a long output, its `expect` the purpose, and a repeated whole
skim shows only what the file gained (`docs/design-decider.md`, section 18).

The tool description says plainly:
- this is nibash, a bash interpreter (mvdan/sh, bash dialect);
- `cat`, `head`, `tail`, `grep`, `egrep`, `fgrep`, `rg`, `sed -n`, `cut -c` and `wc` are nib's own when their output goes
  to the model or into another of them, and then print anchored rows (the list comes from `ownCommands`, which the
  server instructions name too);
- `git` is nib's own for the forms it reshapes (status, `add -v`, `rm`, `ls-files`, diffs and stats of `diff`, `show`
  and `log`, and `show` and `log` in git's default format) when its output goes to the model: it runs the real git and
  prints each folder of a path list once, a diff's lines as rows of the files as they are now, and each commit header as
  one line (short hash, the author's email unless it is the user's own, local date to the minute), the one place its
  output differs in content;
- `find` and `ls` are nib's own the same way for the forms that print one path a line: the real program runs, and each
  folder of its paths prints once;
- every other command, and those names in any other position or written as a path (`/usr/bin/grep`), is the real
  program;
- `type cat` and `command -v cat` name the system program, since that is what runs elsewhere [missing].

The 2048-character test covers this description too.

### 5.2 Sessions and state

- **A fresh interpreter per call:** each call gets a new `interp.Runner` in the project directory (a worktree
  subagent's: its worktree, section 13); no directory, variable or function carries over (section 13, "Stateless
  calls"). Claude Code's Bash keeps only the working directory [M6], and its system prompt tells models to avoid `cd`
  anyway. What an agent keeps across calls is its jobs (section 5.7) and its named directories, which live in the
  server. Claude Code subagents share their parent's MCP connection; each one nib's hook names has its own jobs, and so
  does each OpenCode session (section 13).
- **Calls run in parallel.** No call waits for another; a command past its timeout goes on as a job instead of holding
  anything.
- **Each call:**
  - re-applies stdin `/dev/null` and the terminal sink as stdout and stderr, so an `exec >file` from an earlier call
    cannot capture later ones [m3];
  - sets `GIT_TERMINAL_PROMPT=0`, `GIT_PAGER=cat`, `PAGER=cat` and `TERM=dumb` for children [m3].
- **Shell options:** bash dialect, no `pipefail`, no `errexit`.
- **Quirk:** mvdan runs a pipeline's last stage in the parent shell (`runner.go:506-521`), so `... | read v` and
  `... | cd x` change the rest of the call, unlike bash. The description notes it [M9].

### 5.3 Stage classification and records [B1, M1, M3]

Before running, nibash parses the command with mvdan's parser and marks each simple command (`CallExpr`, keyed by its
position) as **nib** or **real**.

**A command is nib when all of the following hold:**
- Its name is a literal nib command name.
- No function of that name is defined in the command.
- Every word whose options matter is literal. A word with an expansion (`$f`, `"$@"`) counts as an operand, as it does
  to the tool unless it expands to an option: then the system program runs, or, where the command is joined to another
  nib command by a link and cannot step aside, it reports why and fails. Where the command line only fails because of an
  expansion (sed's script in `sed -n "${r}p"`, cut's list), the decision waits for the command to run: it is nib's own if
  the expanded words allow, else the system program runs with the usual note. Such a command never joins a link.
- Its standard output goes to the terminal sink (no redirect, and the pipeline's last stage), into a pipe whose next
  stage is itself nib, or, from inside a compound command (a loop, block, `if` or subshell) that is a pipeline's stage
  before a nib command, into that pipe. A stage inside `$(...)`, with an output redirect, or before a real program is
  real.

Everything else is real. Because nib commands only ever write to the model or to another nib command, their byte form
never reaches a file or a program, so GNU byte parity is never required.

A command that meets every condition but the options (an option, count, script or pattern nib's own does not implement,
written literally or reached through an expansion) runs the system program; when its output reaches the model and it
printed anything, the call ends with one note per such command,
`[grep ran as the system program: ..., so its lines are plain output, not edit anchors]`, saying whether an option or
form, or a pattern RE2 cannot run exactly, was the reason. A command written as a path is not noted, as its caller chose
the system program, nor is one whose lines nib's own would have printed plain as well: `wc`, and a row command none of
whose arguments names an existing file or directory, which reads only standard input.

**Links:** where two nib stages are joined by a pipe, the writer sends **records** to the reader over a bounded Go
channel and writes no bytes. The link is keyed by the pipe's `(dev, ino)`, which both ends share.
- A record is a line of text plus, optionally, its origin: the canonical path, the 1-based line number, the snapshot
  version it was read from, and whether it is a grep match. A record's text is the file line itself, never a prefixed
  form like `12:text`. Whether the user asked for `-n` changes only how the line would print, and the terminal prints
  anchored rows anyway.
- **The channel is bounded** (1,024 records), so the writer waits for the reader.
- **When the reader ends,** it closes a done signal. The writer then stops sending, keeps reading its own source to
  count for facts if asked (section 5.6), and ends.
- **Cleanup:** the link is removed from the call's registry when both ends finish. The registry dies with the call, so a
  reused `(dev, ino)` from another call cannot meet stale entries.
- **Input from a real program:** a nib reader whose input comes from one, or a file redirect, reads bytes and makes plain
  records without origin. It treats a missing final newline as GNU does and replaces invalid UTF-8 for display, keeping
  the bytes as read for `cut`.
- **Origin:** a nib command reading a file holds it as a snapshot through the engine (2 MiB text limit). A file it
  cannot hold (too big, binary, still changing) gives plain records.

**Rows in band:** where the writer side of a nib reader's pipe is a compound command, or a command decided as it runs,
several commands may write to the pipe: `echo`, real programs and nib commands alike. A link cannot keep their order, so
the reader reads the pipe's bytes, and each nib writer there puts each record in the call's registry and writes one
line naming it, `ESC nib-row:TOKEN:ID`, with a random token per call; the reader turns such a line back into the record
and every other line into a plain record. No program can name a row, since only the call knows the token. A reader of
rows in band cannot step aside for the system program, which would read those lines, so it fails as a linked one does.
The classifier marks such writers with `+` after their reader mode and such readers with the mode `W`.

**git for the model through head and tail:** a git command whose output goes on through nothing but nib's own `head`
and `tail` selecting whole lines by count, or `cat` without `-n`, each reading the one before it by a link, to the model
(the pipeline's last stage writing to the terminal, or into a loop's or block's pipe that such stages read), gets `>`
at the end of its marker. It then streams the records it would give the terminal, one for each line git prints, whose
text stays git's line: `head` and `tail` count and keep them as they would the system git's lines, and only the
terminal reshapes them. Each record of a commit header or of a file's part of a diff names its part, and the terminal
reshapes a part's lines only as far as it holds them: a commit header's one line only when all of its lines came, its
message without git's indent only then, and a diff's own header lines (`diff --git`, `index`, `---`, `+++`) left out
only when a line under the file's header came. Without `>`, into anything else, git sends its own lines as records
with nothing but path marks, also into a loop's pipe in band, so a nib `grep` there reads git's `+` and `-`.

**Filters keep origin:** `cat`, `head`, `tail`, `grep` (without `-c`, `-l`, `-o`), `rg` (likewise) and `sed -n` line
selections; `cut -c` keeps it for a line it leaves whole, and for a file's line it shortens keeps the stretches it kept.
**Aggregates make plain values:** `wc`, `grep -c`, `grep -l`.

**Rows piped into a real program:** a command that would be nib's own and print rows, writing into a real program
whose output reaches the model, is real, so the model gets plain text. The classifier lists such pairs, and the first
call of an agent with one ends with a note naming the command and the program and pointing at `read`'s
`path:A-B,C-D` and `maxLineLength`; the store's lesson key `rows-piped` keeps it to once per agent.

### 5.4 nib commands [m7]

The first wave is small, chosen by measured use and anchor value:

| Command | Options handled as nib |
| --- | --- |
| `cat` | files, `-` (stdin), `-n` |
| `head`, `tail` | `-n N`, `-N`, `-nN`, `-n +N` (tail), `-n -N` (head: all but the last N), `-c N`, `-c +N` (tail), `--lines=`, `--bytes=`; one or more files |
| `grep`, `egrep`, `fgrep` | `-n -r -R -E -F -G -v -i -w -x -l -c -h -H -s -q -m N -A N -B N -C N --include= --exclude= --exclude-dir= --color=never` and their bundled forms; basic regex (`\|`, `\(`, `\)`, `\{`, `\}`, `\+`, `\?`) translated to ripgrep's syntax |
| `rg` | `-n -i -S -F -w -l -c -v -g -t -T -A -B -C --hidden --no-ignore -u -m` |
| `sed` | `-n` with `Np`, `N,Mp`, `N,$p`, and several joined with `;` or given as repeated `-e` |
| `cut` | `-c LIST` and `--characters=LIST` (`N`, `N-M`, `N-`, `-M`, joined by commas), files and stdin |
| `wc` | `-l`, `-c`, `-w`, files and stdin |

Any other option makes the command real.

**cut -c counts as the system cut does.** GNU cut counted bytes for `-c` until coreutils 9 learned characters in a
UTF-8 locale, and distributions patch it differently, so nib asks: before a `cut -c` runs as nib's own, the `cut` on the
command's `PATH` (in bwrap under the sandbox) cuts characters 2-6 of a probe line holding a two-byte character, an
invalid byte, a truncated sequence and a four-byte character, in the command's environment, and the answer, bytes or
characters with each invalid byte as one, is kept per program and `LC_ALL`/`LC_CTYPE`/`LANG`. Any other answer runs the
system `cut`. A file's line it keeps whole stays its record; one it shortens keeps its origin with the stretches kept,
as UTF-16 offsets, when the line is the file's own text without color codes and every stretch falls between
characters, and prints as `[line N, chars A-B, C-D of L]|TEXT ... TEXT`, never an anchor. Otherwise the selected
bytes go on as plain text: a split character, a line with color codes, the first line of a BOM file (the program reads
the mark the view leaves out), a streamed file (cut reads its raw bytes) and grep's prefixed lines.

**Searching:**
- `grep -r` and `rg` search through `rg --json`. `grep -r` gets GNU's file selection: `--no-ignore --hidden`, `.git`
  excluded.
- A pattern feature that cannot be translated exactly makes the command real. That covers backreferences, `-P`, and `\<`
  and `\>`.
- Without `rg` on PATH, recursive search is real, with a note [missing].

**Paths:**
- Relative paths resolve against the shell's current directory (`hc.Dir`).
- Paths print through the engine's display rule (named directory, else relative to the server directory) [M9].
- Files are read through the engine: sandbox, roots, named directories and the snapshot store. A line a nib command
  showed whole counts as displayed for `edit`, exactly as after `read`.

### 5.5 Real programs and interp builtins

- **Real programs** run under nibash's exec handler:
  - `Setpgid` gives each command its own process group;
  - on the client's cancellation of a call that still waits, `kill`, or the connection's end the group gets SIGTERM,
    then SIGKILL after 3 s;
  - `WaitDelay` bounds the wait for output a daemonised child keeps open [missing].
- **Output to the model:** when `hc.Stdout` or `hc.Stderr` is the terminal sink, the program writes into a pipe that the
  handler drains into the sink before returning. Output therefore keeps its order relative to the commands before and
  after. When both are the sink they share one pipe, so `2>&1` interleaving is real.
- **`kill`, `jobs` and `wait`** are interp builtins that mvdan does not support for real processes (`builtin.go:1026`).
  nibash's `CallHandler` renames them to its own commands, which act on real process groups and on the job table
  (section 5.7) [B2].
- **Deleting commands** (`rm`, `unlink`, `git rm`, `git clean`, `find -delete`) and nibash's `trash list` /
  `trash restore` are caught in the exec handler before classification matters, wherever their output goes, so what they
  delete goes to the freedesktop.org trash (`internal/trash`, shared with `edit`'s `REM`; the record of what this server
  trashed is the session's, main agent and subagents). `rm` and `unlink` are reimplemented with coreutils' messages,
  quoting (`quoteaf`'s shell escapes), prompts and exit status, and refuse what they lack rather than run the system
  program; `git rm` and `git clean` learn what they would delete from a dry run of the same words, and `find -delete`
  from a run with each `-delete` replaced by a marked `-printf` and `-depth` set. This is the one deliberate difference
  in effect from bash: a deleted file can always be restored.

### 5.6 The terminal sink and rendering

The terminal is a Go object, not a pipe. It receives:
- records from nib commands;
- bytes from shell builtins (`echo`, `printf`) and from real programs, split into plain lines.

When the command finishes or its time runs out, the sink is rendered once:
- **Anchored runs:** consecutive records with origin in one file go under `[DISPLAY#TAG]` as `N:TEXT`, with matches as
  `*N:TEXT`.
  - The check happens at render time: TEXT must equal line N of the latest version of that path in this session's store,
    and the line must fit the row limit. Otherwise the row prints as `N|TEXT` (cut or ansi rows keep the bracketed form)
    [m5].
  - Rows printed as `N:` are recorded as seen.
  - Repeated-line collapsing never runs inside anchored runs [m5].
- **Plain lines:**
  - ANSI escapes are removed, and `\r` progress overwrites are resolved to the line's final text.
  - Invalid UTF-8 is shown as U+FFFD. A NUL byte marks the output as binary, so only its size is shown [m5].
  - Plain output over 20 lines collapses exact repeats with the `compress` that `read plain` uses, but never
    digit-masked repeats: in program output, lines that differ only in a number may differ in what matters (`seq 1 1000`
    must not become one line).
- **Budget:** output past 400 rows or 30 KiB is cut; which rows stay is the decider's choice (`docs/design-decider.md`):
  the fixed cut keeps the first 100 and last 200 rows, keywords and the OpenRouter decision model keep the first 30 and
  last 100 and choose the middle.
  - The whole output goes to a log in `$XDG_RUNTIME_DIR/nib/` (falling back to `os.TempDir()/nib-<uid>/`). The directory
    is `0700`, and a log is capped at 64 MiB [m5].
  - Every gap names the log lines it leaves out (`[nibash: log lines 101-4612 left out]`), counted in the log, not in
    rows, so `sed -n 101,4612p LOG` shows exactly that; the closing note says how many rows are shown of how many log
    lines and who chose. 30 KiB stays under Claude Code's 50,000-character result limit [m2].
  - A plain row longer than 2,000 characters is cut in `read`'s bracketed form, numbered with its log line
    (`[line 2, chars 1-2000 of 20157]|...`), and the log is kept so it can be read whole.
- **Facts** [M2]:
  - `head`/`tail` on a file: `[lines 1-20 of 1,234]`; none for a `head` of a file nib does not hold, which stops reading
    before it knows how many lines there are.
  - `head -c`/`tail -c` print the system programs' bytes as lines: whole lines as rows, the one cut at the edge as a
    bracketed row naming the characters it shows (plain text where the cut splits a character or the file is not held).
    The classifier never sends rows into them: a nib command before one writes the bytes a program would.
  - `head` on a nib `grep`: the writer keeps counting after `head` stops reading, for up to 2 s, without writing, and
    the renderer adds `[20 of 312 matches shown, from 4 of 17 files]` ("at least" when cut short).
  - A real program before `head` is closed at once, as in bash, and the fact covers only what `head` received.
  - `tail` of a program's output keeps all of it in a log and says what it left out: `[4,762 earlier lines in LOG]`.
    With a decision model or a summarizer, the lines it left out may first be checked by the decider and the ones the
    call needs added after the tail's own output (`docs/design-decider.md`, section 17).
- **Exit status:** a nonzero exit status ends the output with `[exit 1]`.
- **Files changed by the command** [missing]:
  - Before the call, nibash notes size and mtime of every file the session's store holds. Every file a redirect opens
    for writing is noted through `OpenHandler`.
  - Afterwards it lists those that changed or were created, with their new tags:
    `[changed by this command: ~orm/A.java#9C10 (was #3F2A), notes.md#1B2C (new)]`.
  - A file whose new content equals a here-document or here-string body in the command is recorded with every line
    known, as after `write`, since the model typed it.
  - A held file something else is changing is left out unless a redirect of the command wrote it, so a log a build
    appends to is not called changed by a `sleep; tail` beside it. At the start, a held file counts as changing when it
    already differs from the newest version the session holds (read only when its modification time is later than when
    the session first held that version), when another running call or job of the session names it, or when an earlier
    call found it changing and it was modified less than 10 s ago. What nib cannot tell is a command that rewrites such
    a file without a redirect: its change is left out too.

### 5.7 Background work [B2]

- **`background: true`** runs the whole command as a job: its own `Runner.Subshell()`, its own process group, output to
  a log (capped like call logs), stdin `/dev/null`. It returns `[job 3 started; output: <log>]` at once.
- **`cmd &` inside a command** is rewritten before running into a job start for that statement, with the same log and
  process-group handling. The statement runs in a subshell copy of the shell at that moment, as bash forks.
  - `$!` is rewritten to the job's spec, `%3`, which nibash's `kill` and `wait` accept. The description states this one
    difference from bash.
  - The call returns when its foreground part ends. Jobs keep running.
- **Past the call's timeout** the foreground part is not stopped: it goes on as a job. The call returns the output so
  far, with `[nibash: still running after 2m0s; it goes on as job %4, output: <log>; wait %4 or kill %4]`; the sink
  writes what it held to the job's log and sends everything later there, so the log has the whole output from the start.
  The call's process groups join the job. Only that output so far renders under the request's context: what runs on
  keeps the run's own, since the request's ends with the answer (nib's own commands after a `wait` that went on as a job
  once failed with `context canceled`).
- **`jobs`** lists the session's jobs with state, exit code and log path. **`kill %N`, `kill PID` and `kill -SIG`** work
  on jobs and on any real process. **`wait %N`** waits for a job; past the call's timeout the waiting call goes on as a
  job too. A call still in its request is no job, as one a client moved to its own background list is: a `wait` that
  finds no such job, or no running one, while another call of the same agent runs adds a note after the output, `[no
  job %2 to wait for: the call "..." still runs as a call, not a job, and its result comes back to that call]`, with
  the command shortened to 60 characters. It is nib's own line, so it shows when the command sends `wait`'s message to
  `/dev/null`; that message and the exit status stay bash's.
- **Reporting:** finished jobs are reported at the top of the next nibash result (`[job 3 finished, exit 0]`), since MCP
  cannot notify on its own.
- **Connection end:** jobs are killed when the connection ends, never sooner.

### 5.8 Sandbox [M5, M8]

With `NIB_SANDBOX=1`:
- **Every interp handler is confined with the engine's resolver:** `OpenHandler` (redirects, `source`, `$(<file)`,
  `read <file`, `mapfile`), `StatHandler` (`test -e`, `cd`), `ReadDirHandler2` (globs). `cd` outside the roots is
  refused. Process substitution's FIFO in the temp directory is the one exception.
- **nib commands** use the engine's confinement.
- **Real programs** run inside `bwrap`:
  - `/` bound read-only, a tmpfs over `$HOME`, then each `PATH` entry under `$HOME` and each directory in
    `NIB_SANDBOX_BIND` bound read-only again;
  - the roots bound read-write, a private `/tmp`, no network unless `NIB_SANDBOX_NETWORK=1`;
  - the toolchain works while the user's files outside the roots stay hidden;
  - without `bwrap`, real programs are refused in sandbox mode, with that reason.

AGENTS.md's sandbox rule gains this paragraph: system directories are readable by real programs.

### 5.9 Command rules

A repository forbids commands in its `nib.json` (`rules`; `names.ConfigFile` is the one place the file is named). An entry
is exact (`program`, a name or list matched by base name, with an optional `args` regular expression over the arguments
joined by spaces, and a required `why`) or vague (`rule`, text, with an optional `why`); anything else makes the file's
rules unreadable, which refuses every call under it, naming the file. `rules.go` checks a parsed call before anything
runs, background calls included:
- **What it finds:** every `CallExpr` the AST holds, behind wrappers (`env`, `timeout`, `nice`, `xargs`, `sudo` and
  others, skipping their options and an option's value), and the scripts of `sh -c` and `eval` whose words are literal.
  A word known before running (literals, quotes, `~` and `~name/`) is its value; any other keeps its source text, so an
  `args` pattern cannot match an expansion, which the vague rules can still catch.
- **Where:** the nearest `nib.json` holding rules at or above the call's directory and at or above each directory a
  literal `cd` names; a `cd` only roughly says where later commands run, so every found file's rules apply to the whole
  call. Under the sandbox only files inside the roots count.
- **Exact rules** refuse at once. **Vague rules** go to the decision model as one `noul` question per rule
  (`decider.RuleJudge`, the same models, endpoint, cache and observers as the decider; the band shows a `rules` card),
  with the whole command, its description and its directory; a rule counts as broken from `PickThreshold`. With no
  decision model, or when every model fails, the command runs with a note, once per agent, file and reason.
- **A refusal** prints one `[nothing ran: ...]` line per broken rule and `[exit 126]`; nothing starts, no job is made and
  the finished jobs' reports wait for the next call.

## 6. Named directories

- **Syntax:** `~name/rest`.
  - In nibash it is tilde expansion, provided by an `expand.Environ` wrapper whose `Get("HOME name")` answers and whose
    `Each` never lists it. So children and `env`/`export -p` never see it [M4].
  - Quoting suppresses it, exactly as in bash. The instructions say to leave `~name/` paths unquoted in nibash [M7].
  - A shell nibash starts with `-c SCRIPT`, directly, behind `timeout`, `env`, `nice`, `nohup`, `stdbuf`, `setsid` or
    `xargs`, or in `find -exec`, knows no names, so nibash parses SCRIPT with the same parser and expands each `~name/`
    where it would expand it itself, quoting the directory where the shell would read it otherwise. It does so on the
    program's final arguments, so a script made at run time (`bash -c "$(declare -f f); f"`) counts too; a wrapper
    whose options it cannot read for certain, a script that does not parse and an undefined name stay as they are.
  - In `read`, `grep`, `find`, `edit` and `write` arguments and in patch headers, a leading `~name/` resolves the same
    way. An existing literal path wins, as with selectors.
  - `~/` stays the home directory.
- **Display:** every path nib prints uses the deepest named directory containing it, else the path relative to the
  server's directory, else the absolute path. Program output is never rewritten.
- **Sources:**
  1. `nib.json` in the server's directory:
     `{ "directories": { "orm": "zenit/src/server/java/be/elevenways/zenit/server/orm" } }`.
  2. Automatic, unless `nib.json` sets `"autoDirectories": false`:
     - Each JVM source root below the server's directory (`src/<set>/java` or `src/<set>/kotlin`, at most 8 levels down,
       skipping ignored and hidden folders) contributes one name for its package prefix: the longest chain of
       single-child directories below the source root.
     - The name is the module directory's name, plus `-<set>` unless the set is `main`.
     - Only prefixes whose path relative to the server directory is 24 or more characters get a name, and at most 16
       names are automatic.
  3. Name conflicts:
     - A name that is a user on the machine or does not match `[a-z][a-z0-9-]*` is skipped. That also keeps clear of
       `~+` and `~-`.
     - A clash between the config and an automatic name keeps the config's.
     - Two automatic names that clash get the parent directory's name prefixed.
- **Announcing:** the server instructions end with the table, one line per name, then, with nibash offered, one line on
  `hash -d`, also with no names. They stay under 2048 characters; when the table does not fit, the longest prefixes win.
  Subagents are not given the server instructions, so a subagent's first nib call starts with the same block, built from
  the names it has (the table's and the ones it inherited).
- **Identity:** a named path resolves to the same canonical path, so tags, seen lines and sandbox checks are unaffected.
- **Scope:** named paths mean nothing outside nib and nibash (commit messages, other tools). The instructions say so.

## 7. Existing tools

Apart from the display form of paths, `read`, `grep`, `find`, `edit` and `write` behave as before, and their
descriptions do not change.

## 8. Setup and the hook

nib owns the shell, so the 780-line shell-parsing hook goes away.
- **What `nib setup` adds** (only what is missing; the one thing it removes is a stale `MultiEdit` rule):
  - To `.claude/settings.local.json`, or user settings with `--user`: `permissions.deny` for `Bash`, `Grep`, `Glob`,
    `Edit` and `Write`, next to the server registration and approval it already writes. `NotebookEdit` stays, with
    `Read`, for notebook cells. A `MultiEdit` rule an earlier setup added is removed: Claude Code 2.1 has no such tool
    and warns `Permission deny rule "MultiEdit" matches no known tool`.
  - The hook, under two matchers: `Read`, and `mcp__nib__.*` for nib's own tools. An existing nib hook entry counts for
    each tool its matcher selects (plain names split on `|` or commas, else a regular expression), so an older entry for
    `Read` gets only the second one added.
  - Claude Code documents that a bare tool name in `permissions.deny` "removes the tool from Claude's context entirely",
    that "deny rules block in every mode, including `bypassPermissions`", and that list settings from several files are
    combined.
  - For OpenCode 2, in `opencode.json`: the server under `mcp.servers.nib` with `codemode: false` (OpenCode 2 otherwise
    offers an MCP server's tools only inside code mode), plus deny rules
    (`{"action", "resource": "*", "effect": "deny"}`) appended to `permissions` for `shell`, `read`, `grep`, `glob` and
    `edit`. An action already named by a rule in `permissions`, or by OpenCode 1's `permission` or `tools` (which
    OpenCode 2 still reads, mapping `bash` to `shell` and `write` and `patch` to `edit`), keeps its setting. An OpenCode
    1 `mcp.nib` moves to `mcp.servers.nib` as OpenCode 2 would convert it. No allow-all rule is written: that would
    allow every other tool too.
- **Side effects of denying `Bash`** [M6], stated in the README:
  - a skill's `!` command lines fail the permission check ("Shell command permission check failed");
  - subagents inherit the deny;
  - nibash is the shell for both.
- **Built-in `Read`** stays enabled in Claude Code for PDFs and notebooks.
  - `nib hook` now only refuses `Read` of anything else, naming the exact `mcp__nib__read` call. Any other tool passes
    untouched, so an older install whose matcher still lists `Bash` behaves correctly [missing].
  - An unchanged resend runs.

## 9. Tests and parity

- **Conformance:**
  - `test/engine.test.ts`, `test/server.test.ts` and the engine parts of `test/patch.test.ts` keep their assertions.
  - `test/support/engine.ts` provides an `Engine` with the same methods that drives the binary over stdio MCP:
    - one process per fixture and session name;
    - `ctx.roots` served as MCP roots, `ctx.authorize` as elicitation;
    - an aborted signal sent as `notifications/cancelled`;
    - `ctx.sandbox` as `NIB_SANDBOX=1`, `ctx.directory` as `NIB_DIRECTORY`.
  - A TypeScript `fileTag` stays as a test helper for the 52 uses [m4].
  - Internals the tools cannot reach (`SnapshotStore` limits, `parsePatch`) move to Go unit tests with the same cases.
- **Go unit tests:**
  - tag vectors, the jsdiff corpus, snapshots, the patch corpus, named directories;
  - stage classification, record links (origin survives filters, an early-ending reader frees the writer, the registry
    empties), each nib command's semantics against GNU on the implemented options;
  - the renderer (budgets, facts, collapsing, binary output);
  - process-group kill, `WaitDelay`, jobs (`&`, `$!`, `kill`, `jobs`, `wait`), the sandbox.
- **Corpus:** the 197k real commands from `research/` are parsed with mvdan's parser, and the share that parses and the
  share run as nib commands are reported. It is a measurement, not a gate.
- **Benchmark** [M5]:
  - `bench/` builds the binary into `bench/.cache/nib` and launches it, and the nib identity hash covers the Go sources.
  - The two existing modes keep their meaning: without `--shell`, both modes have no shell, and nib mode starts nib with
    `NIB_SHELL=0` so nibash is not offered.
  - The existing `--shell` series becomes the shell comparison: harness Bash plus built-in tools against nibash plus nib
    tools.
  - `SETUP` is bumped. Paid runs need the user's go.
- **Commands:** `go test ./...`, `go vet ./...`, `bun test` and `bun typecheck` must pass.

## 10. Distribution

- `go install github.com/11ways/nib/cmd/nib@latest` (private module: `GOPRIVATE=github.com/11ways/*`, git over ssh).
  From a checkout, `./install.sh` builds and installs into `~/.local/bin`, or the absolute directory set by the `GOBIN`
  environment variable. The POSIX shell script requires Go, works from any working directory, prints the installed
  version, points out a missing or shadowed `PATH` entry, and reminds the user to restart the client's nib MCP server.
- `nib --version` and the MCP server report the release version (`v0.2.0`, defined in `cmd/nib/main.go`), the short
  commit when available and `+dirty` for a modified checkout. A matching Git tag must be pushed for Go to resolve a
  published release with `@v0.2.0`.
- zenit-workspace's bootstrap no longer carries the `ensureNib` step drafted for the Bun version (it was never committed
  and has since been dropped), so nothing there changes; a step would now run `go install` and `nib setup`.
- Documentation rewrites [m9]: AGENTS.md's Stack, Commands, Layout and rules (hook rule replaced, sandbox paragraph
  added), and the README's tool, setup, hook and benchmark sections.

## 11. Decision model

Specified and implemented in `docs/design-decider.md`: the renderer asks the configured decider (keywords by default,
the OpenRouter decision model opt-in) which rows of a cut program output to show; it chooses only among real rows and
every cut is announced with its log lines.

## 12. Risks and open questions

1. **mvdan/sh fidelity:** a few bash features are missing (`coproc`, some `shopt`). A parse error is shown as such, and
   the corpus measures how often that happens.
2. **Tool-name habit:** models are trained on `Bash`; the shell benchmark series measures how they take to
   `mcp__nib__nibash`.
3. **Lost shell state:** a model that relies on `cd` or a variable from an earlier call finds it gone. The description
   says so, and Claude Code's Bash already drops everything but the directory.
4. **Quoted named directories** do not expand (bash semantics). The instructions say so; the alternative, rewriting
   quoted arguments, was rejected as lying. A shell's `-c` script is no exception: nibash expands names in it only where
   the script leaves them unquoted, as it would run it itself.
5. **Port regressions:** the existing suites run unchanged against the binary. A behavior that differs is either a
   failing test or untested.

## 13. Implementation notes

Where the implementation departed from the text above, deliberately:
- **Stateless calls.** The first build kept one interpreter per agent, with its directory, variables and functions, and
  ran an agent's calls one at a time. In use that cost more than it gave: a `cd` from an earlier call ran later commands
  in the wrong checkout; a directory deleted under the shell made every program fail to start (exit 126, where bash
  would run it); a call waited minutes behind a long build; and a build past its timeout was killed. Each call now gets
  a fresh interpreter, calls run in parallel, a call past its timeout goes on as a job, and forks no longer copy a
  shell. Logs are numbered per process rather than per agent, which also stopped two agents' logs from taking the same
  name.
- **Worktree subagents and the first-call note.** Builders spawned in git worktrees put `cd <their worktree> &&` before
  nearly every nibash call and gave every file tool absolute paths, since nib resolved them against the main checkout;
  three relative paths edited the main checkout. Nobody used named directories unless told, and subagents never saw
  them: Claude Code gives subagents the tool descriptions but not the server instructions. Now the hook files the
  `worktreePath` of a subagent's meta file with its calls, and that subagent's engine context and shell use it as their
  directory; the table's names inside the server's directory are moved into the worktree, and one the worktree lacks is
  dropped rather than left naming the main checkout. A subagent's first call starts with its directory, its names and
  the `hash -d` line.
- **Roots.** A failed `roots/list` keeps the last list that worked; with none yet, the sandbox allows nothing instead of
  the server's directory (the second review's finding 2).
- **Asking the client (MCP 2026-07-28).** That protocol forbids a server's own requests (SEP-2322), so for clients on it
  a call that needs the roots or a confirmation ends with input requests and is sent again with the answers; answers of
  earlier rounds stay in the session under the request state. Older clients are still asked directly, since the SDK's
  fallback re-runs a call only once and one edit can need several confirmations. A call lists the roots it depends on
  before any work and confirms before staging, so a paused call has recorded nothing as seen; a REAPPLY token a paused
  call took is put back.
- **Panics.** A panic in any tool call becomes that call's error, and one inside a pipeline stage, which runs on the
  interpreter's own goroutine, becomes that command's error, so one bad input never ends the server.
- **Schemas.** The input schemas lost `$schema`, `minLength`, the 2^53 maxima and `additionalProperties`: nib reads
  arguments itself, recovering strings for numbers and booleans with notes, so the schemas only tell the model what to
  send. The five file tools and the instructions went from 8,131 to 7,428 characters; nibash adds 1,421.
- **Subagents.** Claude Code runs a subagent's MCP calls over its parent's connection, and sends only
  `_meta["claudecode/toolUseId"]`, so seen lines and nibash state were shared: a subagent's read let the main agent edit
  lines it never saw, and a subagent's `cd` moved the main agent's shell. The PreToolUse hook does see `agent_id` for a
  subagent's call, with the same `tool_use_id`, and finishes before the call is sent. So the hook, registered for
  `mcp__nib__.*`, files `agents/<tool_use_id>` (holding the agent id) in its private state folder, and the server takes
  it on the call's first round. Rewriting the call's arguments from the hook (`updatedInput`) would also carry the id,
  but it changes what the model sent and shows in the transcript, so the hook only reports. A file per id needs neither
  a channel to a server the hook cannot find, since one Claude session may run several nib processes, nor a Claude
  session id, since tool_use ids are unique. The agent keys the engine session (`agent:<id>`, the main agent stays
  `main`), and with it seen lines and `REAPPLY` tokens, and nibash jobs (per connection and agent, all ended with the
  connection, never sooner). Paused calls keep the agent of their first round, since the record is taken once. Roots and
  directory approvals stay per connection: they belong to the client and its user, not to an agent. Without a record
  every call is the main agent's, exactly as before; records left by calls that never came are pruned after 24 hours
  whenever the hook files one.
- **Forks.** Claude Code 2.1.301 forks with the Agent tool's `subagent_type: "fork"` (in `-p` mode only with
  `CLAUDE_CODE_FORK_SUBAGENT=1`); the fork gets a new `agent_id`, and its hook input carries `agent_type: "fork"` and no
  parent field, nor does `SubagentStart`, and its MCP calls carry only the tool_use id like any other. The parent is in
  Claude Code's meta file beside the subagent transcript,
  `<transcript_path without .jsonl>/subagents/agent-<agent_id>.meta.json` (`isFork`, `spawnDepth`, `toolUseId` of the
  parent's Agent call, and `parentAgentId` when the parent is a subagent), written before the subagent's first tool
  call; every subagent has one, forks or not. The hook reads it and adds a line `fork [parent]` (or `spawn [parent]` for
  any other subagent) to the record; it trusts depth 1 without a parent as the main agent and a deeper one only with a
  valid parent id, and a meta file whose `isFork` disagrees with `agent_type` or anything else leaves the subagent on
  its own. On the first call the server sees from a fork it copies the parent's snapshot session (`Store.Copy`: fresh
  version structs with filtered line sets, each placed beside its original in the LRU order); a server lock held for the
  seeding makes the fork's other calls wait for it. A fork gets none of its parent's jobs. `REAPPLY` tokens are not
  copied, since both could then apply one patch.
- **Spawn moments.** Seeding at the fork's first call gave it lines its parent read after the spawn, which are not in
  its context. The store now stamps each seen and known line, and each version, with the wall-clock time it first held
  it, and `Copy` takes an `asOf`: versions first held at or after it are left out, and of the rest only lines stamped
  before it; stamps travel with the copy, so a fork of a fork filters alike. The moment comes from the parent's
  transcript (the main `.jsonl`, or `subagents/agent-<parent>.jsonl`), not from a hook event: a PreToolUse record on the
  Agent call can come after a nib call in the same assistant message has already run, as the tools of one message may
  run in any order, and that call's result is not in the fork's context. Claude Code gives the fork the parent's entries
  up to the one before the assistant message holding the Agent call (`fork-context-ref.parentLastUuid` in the fork's
  transcript names it for a fork of the main agent; a nested fork's transcript copies those entries with their uuids).
  So the hook finds the Agent `tool_use` by the meta file's `toolUseId`, the first entry of that message (same
  `message.id`) and its `parentUuid`, and takes one millisecond past the latest `timestamp` of the transcript lines up
  to that entry, in file order, skipping lines it cannot parse: lines are appended as written, so all of them were
  stamped before the request that produced the message, every result in the context is among them, and every call
  outside the context started after that request. `api-request` lines, which would mark the request exactly, are written
  only in some modes (none in an interactive session's transcript), so they are not relied on. The hook files the moment
  in `spawns/<agent_id>` (also when it cannot tell one) so the transcript is read once per subagent, and records it as a
  line `at <unix ns>`. Without a moment the server copies the parent's state as it is at the first call, as before. No
  setup change was needed: the hook already runs for every nib call of a subagent.
- **Dropped contexts and held markers.** Seen lines are what the agent was shown, which after a compaction or a `/clear`
  is no longer what it has: an edit could anchor on a line it only saw before the summary, and a marker standing for
  rows it was shown would hide what it cannot see. So the store drops, per agent, every line seen or known before the
  moment its context was dropped (`Store.Forget`), as new versions so a call holding the old ones reads them unchanged,
  and keeps an empty seen set rather than an unrestricted one. Claude Code says when through documented hook events:
  `SessionStart` (every source: a new conversation, `/clear`, a resume and a compaction all start the main agent's
  context again) and `PostCompact` (with `agent_id` for a subagent). The transcript's `compact_boundary` record says the
  same, but it is not a documented interface, and reading every transcript on every call costs more than a small file
  per event. The hook keeps each agent's moments in `contexts/` and files the latest in `drops/<tool_use_id>` for every
  nib call, like an agent record; the server applies it after starting the agent, so a fork, which copies its parent's
  lines as of its spawn, also drops what the parent had seen before a compaction it inherited (the hook takes the
  parent's latest moment before the spawn). Tracking needs the session's `SessionStart` file: an older setup registered
  only `PreToolUse`, and there a missing moment cannot mean "never compacted", so such calls get no markers and edits
  work as before. The moment is taken as the hook runs, after the compaction; Claude Code keeps a tail of recent
  messages through a compaction (`preservedSegment`), but nib drops those lines too, which can only cost a re-read.
  OpenCode 2 appends a `compaction` message when one ends, so nib reads its latest per call. Markers
  (`[lines A-B unchanged since you saw them]`) replace runs of 3 or more rows held whole and unchanged since, found
  through the line map to every version the agent still holds; the lines a marker stands for are recorded seen in the
  version shown, so anchoring is unchanged. Grep's match rows always print, since they are the answer and carry the
  match; edit responses show one kept row on each side of a change, which never makes a run, and the agent checks its
  range against them. An immediate repeat of the same call is how an agent asks for the rows again, besides `full: true`
  on a read.
- **OpenCode sessions.** OpenCode 2 (`@opencode/cli` 2.0.30) sends `_meta["ai.opencode/sessionID"]` on every tool call
  and runs a subagent in a child session with its own id, over the one connection that serves every session of a
  project. One function names the caller for both clients: `opencode:<session id>` or `agent:<agent id>`, which is also
  the engine session; the shell key uses the same name. A forked session's parent is known to OpenCode's server
  (`GET /api/session/:id`), whose address and password an MCP child is not given (OpenCode removes
  `OPENCODE_SERVER_PASSWORD` from the environment tools inherit), but also to its database, which an MCP server finds
  the way OpenCode does since it inherits OpenCode's environment: `$OPENCODE_DB` relative to `$XDG_DATA_HOME/opencode`
  (else `~/.local/share/opencode`), else `opencode.db` there and the per-channel `opencode-*.db`. That works for a
  user's own setup and for the benchmark, which gives OpenCode, and so nib, its own `XDG_DATA_HOME`. nib opens it
  read-only with `modernc.org/sqlite` (`mode=ro`, `query_only`) once per session, on its first call. `session_v2` holds
  `parent_id` for a child session and `fork_session_id` with `fork_boundary` (`{"type":"through"|"before","messageID"}`)
  for a fork. OpenCode's projector copies the parent's settled `session_message` rows up to the boundary into the fork
  with their `seq`, times and data (an assistant row only once `time.completed` is set), and the fork's own rows start
  past the last copied `seq`. A tool call's result lives in its assistant row, which completes after it, and the
  parent's next message is created after that, so the moment is one millisecond past the latest `time.completed` (else
  `time_created`) among the fork's rows up to the last copied `seq`. A child session's moment is one millisecond past
  its creation. A parent session that never called nib is started first, from its own origin, up to 8 levels. A child
  session inherits only named directories; a fork also lines.
- **Named directories at runtime.** `hash -d name=dir` (zsh's form; bash has `hash -d name` only, which forgets a
  remembered command, and rejects a bare `hash -d`) is renamed by nibash's call handler, after expansion, to one of its
  own commands; any other `hash` runs mvdan's builtin. Definitions live in `names.Defined` in the server, per agent,
  each stamped like seen lines; an agent starts, at its first call, with its parent's definitions stamped before its
  spawn moment (all of them when the moment is unknown, none when the parent is). The engine context of every call
  carries a `names.View` of the caller: the server's table with the agent's names over it, read live, so a name works
  from the moment it is defined, also later in the same command. The server instructions keep listing only the table.
- **Subshells and named directories.** mvdan's background subshells (jobs, every pipeline stage but the last, process
  substitution) copy the environment through `Each` instead of keeping the parent environment, which lost the wrapper
  answering tilde expansion's `HOME name` lookups: `echo ~name/x | cat` and `echo ~name/x &` printed `~name/x`. The
  wrapper now also lists the caller's names as unexported `HOME name` variables, so every copy keeps them and programs
  never see them. The one place they show is `${!HOME*}`, which the tool description states.
- **Concurrent calls.** The SDK runs tool calls concurrently, and the snapshot store's maps were guarded only per path,
  so calls on two files could corrupt them (the race detector showed it on concurrent reads of different files); the
  store now takes a lock of its own.

Measured on 443,805 real shell commands (`NIB_CORPUS`): mvdan's parser reads 443,748 (the rest are mostly unclosed
quotes), and 323,344 (72.9%) run at least one nib command; `grep` 272,806 times, `head` 256,522, `sed` 108,447, `tail`
39,734, `rg` 32,410, `cat` 30,390, `wc` 17,290.
