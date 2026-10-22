# nib

A stdio MCP server that gives coding agents hashline file tools and a shell: reads and searches tag each file with a
short content hash, and edits address line numbers instead of repeating the old text. Its shell, nibash, runs the bash
agents write, and the file lines its own `cat`, `grep`, `head`, `sed -n` and friends show carry the same edit anchors.
It works with any MCP client and is meant to replace a client's built-in file tools and shell. The format follows
current [oh-my-pi](https://github.com/can1357/oh-my-pi) hashline (a whole-file 4-hex tag plus plain line numbers).

nib is one Go binary for Linux, macOS, the BSDs and Windows ([Platforms](#platforms)). It needs `rg` (ripgrep) on `PATH`
for `grep`, `find` and nibash's recursive searches, and `bwrap` (Linux) for programs in sandbox mode. The tests and the
benchmark are TypeScript on Bun.

```sh
./install.sh           # build and install to ~/.local/bin; GOBIN overrides
go test ./...           # Go unit tests
bun install && bun test # the black-box suites, run against the binary over MCP
bun typecheck           # go vet and tsgo --noEmit
```

## Tools

| Tool | Input | Purpose |
| --- | --- | --- |
| `read` | `paths[]`, optional `maxLineLength`, `plain`, `collapse`, `info` | Several files, ranges, directories and images in one call; outlines large files and skips lockfiles and generated files unless a range is given; `plain` reads logs cheaply; `path#TAG` shows what changed since that tag; `path@REV` reads a file as of a git commit, or a folder's commit with its diff; a SQLite database reads as its tables, their rows and queries; `info` gives one line of facts per file instead of content. |
| `grep` | `pattern`, optional `paths[]`, `glob`, `ignoreCase`, `literal`, `before`, `after`, `mode`, `noIgnore`, `maxLineLength`, `skip`, `replace` | ripgrep search whose output rows are valid edit anchors; can also just list matching files or count matches; `replace` previews a search-and-replace and returns a token that applies it. |
| `find` | optional `paths[]`, `glob`, `minSize`, `newer`, `sort`, `noIgnore`, `skip` | Lists files recursively with line counts, sizes and ages; filters by size and age, sorts by path, size or age. |
| `edit` | `input` (patch text, or `REAPPLY TOKEN` from a refusal or a grep replace) | Edits, creates, copies, moves and deletes any number of files in one call. |
| `write` | `path`, `content`, optional `tag` | Creates a file (with parents), or overwrites one when given its current tag, keeping a CRLF file's line endings and a BOM; answers with a row map of the new file. |
| `nibash` | `command`, optional `description`, `expect` (offered with a decision model or a summarizer), `timeout`, `background` | A shell whose calls each start afresh in the project directory and run in parallel; its own `cat`, `head`, `tail`, `grep`, `rg`, `sed -n`, `cut -c` and `wc` print anchored rows, everything else runs as real programs, and output is shaped for the model. |
| `nibash_status` | optional `tail`, `view`, `restore`, `summaries` | Lists what nibash runs for the client, every agent of it: commands still in their call and jobs (running, or ended in the last 10 minutes), with how long each ran, its log and, with `tail`, its last output lines; as text and as structured content, which Claude Code's nib mod polls. `view` adds one more list for the mod's panes (`trash`, `seen`, `names`, `tokens`); `restore` puts one trashed path back through the trash code behind `trash restore`, refusing, as it does, a path where a file is now; `summaries` turns summaries of long output on or off for the session. |
| `skim` | `paths[]`, `expect` | Offered with nibash. Reads any long text file the agent wants answers from rather than to edit (logs, test reports, data dumps, generated output, `PATH@REV`) for what it expects to learn: plain lines, the long ones shaped as nibash shapes long output; a reread of a whole file with the same `expect` shows only what was added, and one with another `expect` everything but the lines the agent was shown. |

### read

```
[src/example.ts#3C4D]
1:export const enabled = false;
2:export const name = "example";
```

`TAG` is four uppercase hex digits: xxHash32 of the BOM-free text with trailing spaces, tabs and CRs stripped from each
line, masked to 16 bits, identical to oh-my-pi's `file_hash`. Each `paths` entry may end in a selector: `path:A` (that
one line), `path:A-B`, `path:A+COUNT`, `path:A-` (from A on), `path:-N` (last N lines) or `path:A-B,C-D` (gaps print as
`...`). An existing file whose name looks like a selector is read literally. An open selector such as `path:1-` returns
up to 2000 lines. One call's 40 KiB output budget (kept near common MCP client output limits) is split fairly: each file
asks for what it needs, small requests are served in full, and the largest share the rest equally, so a big file cannot
starve the others. Cut output ends with `[... continue with path:N-]`, and partial selections end with
`[N lines total]`.

#### Large-read safeguards

Skipping, outlines and the flat-data head apply only to entries without a selector; any selector, such as `path:1-`,
reads the file as asked, though a minified file still gets shorter lines.

- **Declared names.** A selector may instead name what a file declares: `path:formatAge`, `Repo.java:Repo.save` (an
  inner declaration directly inside an outer one, or belonging to it: a Go method to its receiver's type, a C++
  `Foo::bar` definition to `Foo`, a Rust `impl` block's functions to its type), or several, `path:save,load`. nib parses
  the file with tree-sitter grammars (gotreesitter, pure Go) for Java, Kotlin, TypeScript, TSX, JavaScript, Go, Python,
  Rust, C, C++ and Zig, and reads each declaration of the name at the least depth, so overloads come together, from the
  comments, annotations, decorators and attributes directly above it through its last line, as ordinary anchored rows
  that `PUT N*:` can replace. Declarations are classes and their kin, functions and methods, and variables, fields and
  constants outside function bodies; a local, a call or a name in a comment or string is none. A declaration whose lines
  did not parse cleanly is refused with its line, a name the file lacks says so (adding, when part of the file did not
  parse, that it may be there), and a file in any other language is refused rather than guessed at; each refusal points
  to grep. In a Hawkeye template (`.hwk`) a name is a `{% block "name" %}` or a `{% snippet name(...) %}`, read through
  its closing tag. A refused name comes back as a note, the other paths still read. grep takes no names, and a name
  cannot be read `plain`. A decoded grammar takes 12 to 33 MB of memory, so nib keeps the two used last and drops one
  after ten idle minutes.

- **Skipped files.** Above 4 KiB, lockfiles (`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `bun.lock`,
  `Cargo.lock`, `go.sum` and similar), minified or bundled files and source maps, and files inside `node_modules/` or
  `dist/` return one line with the reason, size and an approximate token count, and no tag. A file counts as minified by
  its name (`*.min.js`, `*.min.css`, `*.map`, `*.bundle.js`, `*.chunk.js`, or a content hash such as `main.3f9a2c1b.js`)
  or when more than half of its text sits in lines over 300 characters that are dense: more code punctuation
  (`{}()[];,=<>+*&|!?`) than whitespace, or almost no whitespace at all. Prose written one paragraph per line is not
  dense, so long Markdown paragraphs never count.
- **Minified lines.** When a range read of a minified file has lines over 300 characters and no `maxLineLength` was
  passed, its lines are cut at 300 characters instead of 2000, and the header says so; passing `maxLineLength` restores
  the usual cut.
- **Imports.** In a Java or Kotlin file, a read whose rows go past a run of 10 or more imports (blank lines between them
  included) shows that run as a package tree instead of one row per import: a note
  `[lines 3-110: 105 imports as a package tree; read that range for their lines]`, then one row per package with the
  classes it imports, a space between them, and its sub-packages two spaces deeper by their own segments; a package
  that imports nothing itself and holds one sub-package shares its row:

  ```
  be.elevenways:
    protoblast.common: Blast
      time: Backoff Now Pace Retry
  java:
    util: ArrayList HashMap List
      function: Consumer Function
  static org.junit.Assert: assertEquals
  ```

  A wildcard shows as `*`, a Kotlin alias as `Alias=Name`, a module import as `module NAME`. The rows are no anchors
  (no package or class name starts with a digit). On a real file of 105 imports they take 2,157 bytes instead of 6,324,
  about a third. A run whose every line the agent holds is left to the held rows, which are shorter still. The folded
  lines are not seen, so an edit of them is refused until they are read; a range that ends within the imports, like
  `PATH:3-110`, shows them as rows, and `IMPORT` adds one without reading any.
- **Outlines.** Code files over 200 lines or 8 KiB, whose outline is at most a third of their length, and any file over
  500 lines or 20 KiB return `[PATH#TAG] N lines, about Xk tokens; outline only...` followed by real `LINE:TEXT` rows:
  every top-level code line, plus the first-level members of top-level classes, interfaces, structs, enums, traits,
  impls, namespaces and similar containers. Runs of imports collapse to the first one and a
  `[lines A-B: N more import lines]` note, and runs of three or more one-line declarations at the same depth (constants,
  type aliases, fields: lines whose next code line is not deeper) collapse the same way into
  `[lines A-B: N more one-line declarations]`. Comments, blank lines, closing-bracket lines (including signature tails
  such as `) -> None:`) and lines inside multi-line strings (Python triple quotes, JavaScript template literals, Go raw
  strings) are left out; quote marks inside one-line strings, comments and regex literals open no multi-line string.
  Indentation depth is measured in the file's own indent unit, ignoring comment lines. Markdown files outline their
  headings outside code fences. An outline is capped at 150 rows, counted after collapsing: when it would be longer,
  top-level rows stay and members fill the remaining room in file order, with a note counting what was left out. Outline
  rows are shown in full, so they count as seen and can be edited directly. A line starting with `#` counts as a
  comment, except a JavaScript private member: `#name` followed by `(`, `=`, `:`, `;`, `<`, `!` or `?`. A template
  literal's `${...}` is read as code, so a template nested inside one does not end it.
- **Flat data.** When fewer than 5 outline rows exist, or more than half of the non-blank lines would be outline rows
  (logs, CSV, generated lists, pretty-printed JSON), the read returns the first 100 lines instead, with a hint to
  continue or grep.

Lines longer than `maxLineLength` (default 2000) print as `[line N, chars 1-2000 of M]|prefix`, which is not an edit
anchor; the first time an agent is shown one, the response ends with a note saying so (and again after its context
was dropped), once per response however many blocks hold one. `SUB` changes such a line in place without reading it;
to see it whole, read it with a larger `maxLineLength`. A larger value works for any selection within the shared 40 KiB
budget, and one `path:N` entry alone may go up to 2 MiB; a row bigger than the whole budget is replaced by a hint to
read that line alone. A line with terminal color codes prints as `[N ansi]|text` with the codes stripped, which is not an
anchor either; the tag still covers the real bytes, and `SUB` edits such a line too. Directories list their entries;
PNG, JPEG, GIF and WebP files are returned as MCP image content, as the model will see them. One within what the
newest models take (2576 pixels on the long edge, 4784 image tokens of 28 by 28 pixels, 3.75 MB) goes as it is; a
bigger one is scaled down to fit, its aspect kept, and sent as lossless WebP, or as JPEG (quality 90) when it was a
JPEG or its WebP would still be over 3.75 MB, so nothing between nib and the model rescales it unseen. The note names
both sizes: `shot.png: image/png 1400x2911 attached as image/webp 1239x2576, the most the model sees; crop a part for
more detail`. An image file over 64 MiB, or over 64 megapixels, is skipped with a note, and one nib cannot decode goes
as it is. PDFs are refused, so use the client's own reader for them.

Headers show paths relative to the server's working directory when inside it, absolute otherwise. Text files are limited
to 2 MiB (plain reads excepted), images to 5 MiB.

#### Plain reads

`plain: true` reads for looking only, typically logs:

```
[app.log] lines 1147-1185 of 90210, plain
2026-10-14 12:00:01 ERROR disk full
[the line above repeats 37 more times]
2026-10-14 12:00:09 retrying in 5s with a much lo...
```

Rows carry no line numbers and the header no tag, so nothing a plain read prints is an edit anchor. The lines it showed
whole (a file within the 2 MiB text limit, not a revision) are recorded as content the agent knows, so a later numbered
read prints them as `N~TEXT` row-map rows, however many, instead of in full again: the option is for files no one will
edit, and a plain read of one that then needs an edit costs a third of a second read rather than a whole one. Terminal
escape codes and other control characters are stripped, lines longer than `maxLineLength` end in
`...`, repetition is collapsed (see below), and gaps between selected ranges print as `[lines X-Y skipped]`. The file is
streamed up to the size it had when opened, without the 2 MiB text limit and without the consistency check that refuses
a file changing during a normal read, so huge logs and logs still being written read fine. Selectors work as usual (a
tail of up to 2000 lines, like `path:-500`, is collected in one pass; a longer one counts the lines first); at most 2000
lines are kept per file, and the 40 KiB budget and continuation hints apply as in a normal read. Images and PDFs are
handled as in a normal read; other binary files are refused.

Plain reads compare lines with their digits ignored, so timestamps, counters and durations do not count as differences.
A line or a block of up to 8 lines repeating back to back prints once, then a count and, unless every copy is identical,
the last copy: `[4 more like the line above; the last one:]` or `[the line above repeats 4 more times]`. A block of at
least 3 lines that comes back later, such as a stack trace, prints once as `[#1, 23 lines] first line` and each later
copy as one row, `[#1 again, 23 lines] its own first line`; the first time an agent sees these markers, in a plain read
or in nibash, a closing line explains them. Collapsing only happens where it prints fewer rows and its marker is shorter,
in characters, than the lines it stands for, so three copies of a short line print as they are; never across a gap
between selected ranges, and when it hid other numbers the block ends with a note; `collapse: false` shows every line.

nibash folds output across calls too: a block of at least 5 rows (20 when the call failed, or a program in it failed
where its exit status does not say so, so an error shows again) of
program output that one of the agent's last 50 calls
showed, row for row, prints as one row naming that call and where the lines are,
``[nibash: the same 15 rows from `--- FAIL: TestNames (0.01s)` your previous call showed; log lines 40-54 in LOG]``, where it is shorter
than the rows. Each agent folds only against its own calls, never against calls from before its context was compacted
or cleared, and never across a restart of the server; a row that changed prints, so only identical runs fold.

#### skim: answers from long files

Where nibash is offered, `skim` reads any long text file the agent wants answers from rather than to edit (a log, a
test report, a data dump, generated output) for `expect`, what it needs to learn, like
`did the unit phase pass; if not, which tests failed and why`. Each file is read as plain lines (nothing to edit from)
and shaped the way nibash shapes a long program output, with `expect` as the purpose: within 150 lines and 12 KB it
prints whole; past them the decider chooses the lines that bear on it (keywords when no model is configured), or a
summary leads the lines it points at, each numbered with the file's own line as `N|TEXT`, under the same notes, which
say `skim` and name the file (`N| is a row's line in the whole file: read run.log:A-B plain:true for others`), so
`read run.log:31-1179 plain:true` shows what a gap left out.

```
[run.log] lines 4001-5213 of 5213, plain: new since your last skim
<summary>The unit phase timed out after 601 s with 6 failures; SyncedRefTest.everySubscriberReceivesPushes was still running.</summary>
...
```

A skim remembers which of the file's lines the agent was shown, not how far the decider or summarizer read. Skimmed
again with the same `expect` (in any case and spacing), a file shows only the lines added since, numbered as the file
numbers them, so polling a test log costs only what it grew by; a last line still being written shows as it stands and
again once it is finished, and `[...] N lines, plain: nothing new since your last skim with this expect` answers a skim
of a log that did not grow. With another `expect` the whole file is shaped again, its deciders reading every line, but
the lines the agent was shown are left out before the cut, named in one row
(`[skim: lines 1-30, 351-450 left out, which your earlier skim showed]`), so the budget goes to lines it has not seen.
What an agent was shown no longer counts once its context is compacted or cleared. A file that shrank or whose first
4 KiB changed is read whole again and says so. A selector (`run.log:200-900`, `run.log:-500`) reads those lines, an open
range to the end, leaves nothing out, and leaves where the next whole skim goes on alone; the lines it shows count as
shown.
Several files are read in one call, a missing one noted in its place; at most 64 MiB of a file is read per call. The
limits hold for the call, not for each file: files that are short apiece but past them together are shaped as one
output, one decider or summary serving them all, each file's head row a line of it that a row shown without it gets
first, and their rows numbered as lines of a log joining them (`N| is a row's line in the joined files: read LOG:A-B
plain:true for others`). That output is cut file by file: every head row stays, and each file gets a fair share of the
budget for the decider's picks, or without them for its first lines, so one file cannot crowd out the others. Its
summary gives up to 3 sentences for each file that bears on `expect`, and the lines it points at, up to 5 ranges for
each file and 30 in all, share what one output's would get for each file.
A file that is a unified diff (each `diff ...` line, or `--- ` then `+++ ` without them, followed by a hunk) is cut as
nibash cuts `git diff`: every file's first lines stay, each file's hunks show whole from its start as far as its share
of the budget goes, or as a decision model picks them for the `expect`, and what a file leaves out is named under it by
hunk, `[skim: 3 hunks left out, +120 -120: func alpha1() {, ...]`, its rows numbered with the file's lines as above.
`expect` is required. `read` itself never takes it, so a read always gives rows to edit from.

#### Changes since a tag

A `paths` entry written `path#TAG`, with a tag from an earlier read, grep, edit or write in this session, shows only
what changed since that version:

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

Added lines print as `+N:TEXT`, unchanged context (2 lines around each change) as ` N:TEXT`, both numbered as the file
is now and anchorable, and removed lines as `-|TEXT`, which carry no number. For each stretch of
unchanged lines that moved and holds a line the agent saw or wrote, a note gives the old numbers (at most 10 notes). The
new version is recorded like an edit response: lines that kept both number and content stay seen, the rows shown are
seen, and moved lines the agent knew stay known, so a `~` check anchors them. An unchanged file answers
`unchanged since #TAG`. A tag nib no longer keeps (it keeps 4 versions per file), or one issued for another file, gets a
note saying so instead of a diff; a diff that takes over a second asks for a new read. `path#TAG` takes no line selector
and cannot be combined with `info` or `plain`; an existing file whose name ends in `#TAG` is read literally.

#### Revisions: `path@REV`

A `paths` entry written `PATH@REV`, where `PATH` names nothing as typed and git resolves `REV` (a hash, a branch,
`origin/x`, `HEAD~2`) to a commit in the repository holding `PATH` (the nearest `.git` above it, a worktree's too, so a
folder holding many repositories needs no setting), reads that revision. Each `@` is tried from the left, so a path
holding `@` still works, and a path that exists as typed is always that file. A file prints as it was in that commit,
deleted since or not, with read's selectors (`File.java@abc123:40-80`, `File.java@abc123:Class.method`) and its
outline when large. A folder prints the commit limited to that folder: its header as one line, the message, each file
changed with its exact counts and then the diff, as git shows it otherwise. A large commit prints its header and
files first, naming the line its diff starts at; `repo@REV:Foo.java` narrows it to the files the commit changed that
are or end in that path, and a range pages it.

```
[zenit-cms@c3c30be5]
1|commit c3c30be5 Sun 2026-10-18 23:12
2|A row operation's hiding condition can be read once per page
3|---
4| src/common/RowOperation.java +12 -3
```

Every row is `N|TEXT`, never an anchor, and nothing of it counts as seen, since it is not the file as it is now. A
revision that does not resolve says so in one line (`a.go@nope: nope is no commit in zenit`). It takes no `info`, and
`plain` prints the same rows. `skim` takes `PATH@REV` the same way. nibash points at it once per agent when a command
writes `git diff`, `show` or `format-patch` to a file.

#### SQLite databases

A file that starts like a SQLite 3 database (its header, whatever its name) reads as one, strictly read-only. Without a
selector it is an outline: each table and view in the order they were made, with its row count, whether it is a
table without rowid, and its columns with their declared types as SQLite reports them. `db:TABLE` gives that table's
rows, and read's line selectors after it pick rows by position: `db:runs:-15` the last 15, `db:runs:1-20`,
`db:runs:40+10`, and `db:runs,pins` several tables. Rows come in rowid order, a table without rowid in primary key
order and a view in its own order, as the heading says, under a row of the column names:

```
[state.db] table runs, rows 2-4 of 30, in rowid order
id | status | log | data
2 | ok | first line\nsecond \| line | NULL
3 | fail | run 3 | <blob 3 bytes>
```

Columns are separated by ` | `; a value shows `\`, newlines, tabs and `|` escaped, is cut after 120 characters with
`... (+N chars)`, and a blob shows its size. One read fetches at most 500 rows, and the note under them names the rows
left out and how to read on. Rows are never anchors and count as nothing seen; an edit of a database is still refused
as a binary file. nib opens it the way it opens OpenCode's database (`internal/sqlitero`): read-only, with writes
refused by the connection too, attached to the shared memory a writer holds, or opened immutable when no one holds a
WAL database, since even a read-only connection would otherwise create its `-wal` and `-shm` files. `info` still gives
the file's facts.

`db:` followed by a statement starting with `SELECT`, `WITH`, `VALUES`, `PRAGMA` or `EXPLAIN` (any case) runs it as a
query: everything after the colon is the statement, so a colon inside it stays and no row selector follows it. It runs
on the same read-only connection, so a write fails with SQLite's own error on one line, and one statement at most (a
second one after `;` is refused); a query still running after 5 seconds is stopped. Its rows print exactly as a
table's do, under `[state.db] query, N rows`; past 500 the heading says the first 500 came and more follow. A column
declared `DATE`, `DATETIME` or `TIMESTAMP` shows its text as stored: such a statement runs held in a CTE that drops the
declared types, in its own order.

```
[state.db] query, 2 rows
id | status
30 | fail
20 | fail
```

#### File facts

`info: true` returns one line per entry instead of content:

```
[src/engine.ts#35C8] 1796 lines, 84.2 KiB, about 22k tokens; longest line 312 chars (line 1130); LF; modified 3m ago
[logs/boot.log] 120345 lines, 9.5 MiB, about 2490k tokens; longest line 412 chars (line 88); LF; color codes; over 2 MiB, so no tag: read it with plain: true, or grep it; modified 2h ago
src/ directory, 12 entries (3 directories); modified 1d ago
```

Lines are counted like a read counts them and the longest line is measured in characters with its color codes; line
endings show as `LF`, `CRLF` or `mixed line endings (N CRLF, M LF)`, followed where they apply by `no final newline`,
`BOM`, `color codes`, and the reason a whole read would skip the file (minified, lockfile, inside `node_modules/`).
Images and PDFs give their type and size, binary files and files that are not UTF-8 say so. Files over 2 MiB, or ones
changing while read, are streamed and get no tag. A file nib can hold gets a tag, recorded with no line shown, so an
edit through it is refused for unseen lines like after a read that displayed nothing, `PUT >$:` appends work, and the
tag is a base for a later `path#TAG` read. `info` takes no selectors, `plain`, `collapse` or `maxLineLength`.

### grep

```
2 matches in 2 files

[src/login.ts#1F2A]
 4:import { session } from "./session"
*5:export function login(user: User) {
 6:  const token = session.create(user)
```

`*` marks matches; other rows are context (1 before and 1 after by default). Context lines on one page come to at most
40, so they cannot multiply the output; within that, `before` and `after` give what they ask for. Past it, context goes
to definitions first, and the other matches show bare: context that was asked for with `before` or `after` goes to every
definition (up to the 40 KiB output budget), while the default context goes to definitions in page order only while it
fits in the 40 lines, so a grep that lists many definitions, like `^function`, stays a list. A definition is a match in
the declaring part of its line, before the parameters, type, value or body: on a top-level line whose declaring part
holds a word like `function`, `const`, `class`, `def` or `fn` (the match may be that word, as with a pattern
`^function`), on a member of a top-level class or similar container, or in a Markdown heading. With no definition among
the matches, context that was asked for goes to the first matches in page order, up to the first that no longer fits,
and the default context goes to none. A note then says that every match row is still shown, which matches got context,
how many lines all of them would have needed, and a range to read for another match. Every fully shown row counts as
seen, so an edit can anchor on it without a read. Pages hold 20 files (continue with `skip`), with 20 matches per file,
or 100 when only one file matches, the most a harness's own grep shows (OpenCode stops at 100). A matched line over 200
characters shows only its first 40 and the 40 on each side of its first matches, as
`*[line N, chars 1-40, 62-147 of 408]|start ... match`, not an anchor, unless `maxLineLength` was given, which then
bounds it instead. Matched lines over `maxLineLength` characters (default 512, at most 2000) are cut and not
anchorable; when the first match sits past the cut, the row shows the stretches around the first 3 matches instead of
the line's start, as `*[line N, chars 3142-3324, 28924-29106 of 31625]|...`. The search uses `rg --hidden` without
`.git/`, and `--engine auto`, so a pattern with look-around or backreferences (`\.migrated\((?!\))`, `(\w+) \1`) runs
on PCRE2 where ripgrep was built with it, and every other pattern on ripgrep's own engine. It follows git's ignore
rules, which count only inside a repository (ripgrep's default; outside git a `.gitignore` means nothing), plus
ripgrep's own `.ignore` and `.rgignore`. A nested repository is searched by its own rules even when the repository
holding it ignores it, as a workspace whose `.gitignore` keeps out every checkout it holds does: git lists the nested
repositories its rules ignore, and each one ripgrep would not enter by itself becomes a root of its own. Only a
repository's own worktree stays hidden, since it repeats the same files. Outside git, the search first walks up to 2000
folders looking for repositories, since one of those may ignore its own nested ones, and says so when it stopped early.
`noIgnore` turns ignore files off entirely (`--no-ignore`, still without `.git/`). As in ripgrep, a `glob` include
re-includes files ignored by name (never files inside ignored directories); `glob` may also be a list, such as
`["!build/**", "!*.md"]`.

Tool arguments are forgiving where models carry habits over from other tools: numbers may come as strings, a number over
its limit (`before` and `after` 20, `maxLineLength` 2000 for grep and 2 MiB for read) is held to the limit with a note
instead of refused, `paths` may be one string, `read` also takes another read tool's `filePath`, `file_path` or `path`
with `offset` (the first line) and `limit`, and `grep` takes a single `path`. JSON mistakes that small models make are
read as meant: a `paths` or `glob` string holding a JSON list (`"[\"a.ts\"]"`) is that list, also with the end of the
model's tool-call markup after it (`["a.ts"]>`, which is dropped), a `glob` string of globs separated by commas outside
braces and brackets (`"*.ts, *.md"`, which as one glob would only match names holding a comma) is the list it spells,
and an `edit` input sent as one line with escaped line breaks (`\n`) is decoded, since one line can never be a patch.
A note at the top of the response, also of a call
that fails, then shows nib's own form of the same call. `grep` also takes another grep tool's names for its `mode`
(`content` for `matches`, `files_with_matches` for `files`). A path that does not exist is named in a note while `read`,
`grep` and `find` serve the other paths (a read puts the note in that file's place, grep and find at the top); only when
no path is left is the call an error, so a missing path never reads as a search that found nothing. One written
`path:A:B` is told to write `path:A-B` for a range.

- **Line ranges.** A file in `paths` may carry a read selector (`app.log:1100-1250`, `app.log:-500`, `app.log:900-`) to
  search only those lines; context rows stay inside the selection too. Unlike a read, an open range runs to the end of
  the file. The same file named again without a selector searches all of it; a selector on a directory is refused.
- **Modes.** `mode: "files"` prints the matching paths and `mode: "count"` prints `COUNT PATH` per file. Both start with
  the total (`N matches in M files`), print no rows or tags, record nothing as seen, and page 200 files at a time. Their
  paths are laid out as `find`'s are ([below](#find)). A count over more than 40 files sums them by folder instead, most
  matches first, as `COUNT FOLDER/ (N files)` at the deepest folder level that fits in 40 rows (a file above that level
  keeps its own row), and grepping one folder gives its files' counts; `skip` still pages the files one by one.
- **Big or growing files.** A file over 2 MiB, or one that changes while nib reads it (a log still being written), gets
  its rows from ripgrep itself: `*1147|text` for a match and ` 1148|text` for context, colors stripped and shortened at
  `maxLineLength` with `...`. Those rows carry no tag and are never anchors, and the block ends with a hint to read
  around a match with `plain: true`.
- **Output budget.** Files that do not fit the 40 KiB budget are left for the next page. The first file of a page is
  never left out: its rows stop at the budget, with `[output limit reached; grep path:N- to go on from line N]`.
- **Replace.** `replace` (ripgrep's syntax: `$1`, `${name}`, `$0` for the whole match) turns a search into a preview.
  Context defaults to none, and each matched line that would change shows only as it would become, as a `>N|TEXT` row (a
  long one shows the stretch around its first replacements); its current text is not repeated, since the row carries the
  match too. A match the replacement leaves as it is stays a `*N:` row, and so does an empty matched line, since its
  token hunk is a `PUT` that needs the line seen. Nothing is written: the output ends with
  `To apply the N replaced lines shown (>N| rows), call edit with input: REAPPLY t0k3n5`, and that token applies exactly
  the shown lines, as one whole-line `SUB` per line under the tags the preview printed, so it changes cut lines too and
  is rebased like any edit when a file changed meanwhile. A replace shows every match of each file nib holds (no
  20-per-file limit), so one token covers the whole page; matches past the output budget or on later pages, and files
  nib cannot hold whole, are left out and say so. Applying the token repeats that: its response ends with
  `[N more lines that the search would change were not in this token; grep again with the same pattern and replace for them]`.
  A line selector on a path (`src/a.ts:110-136`) limits the replace to those lines, for changing some matches of a file
  and not others.
  A file whose token applies as previewed, under its tag and one line for one, answers with just
  `[PATH#TAG] N lines replaced as previewed`, since the `>N|` rows already showed every new line; those that showed
  whole count as seen, so follow-up edits need no read. A token that had to be rebased, or a replacement that added
  lines, gets the usual rows. Patterns spanning lines and the `files` and `count` modes are refused.

### find

```
3 files, largest first

src/engine.ts (1796 lines, 84.2 KiB, 3m ago)
assets/logo.png (image/png, 12.0 KiB, 40d ago)
README.md (189 lines, 16.1 KiB, 1h ago)
```

`find` lists the files grep would search (`rg --files`: hidden files included, `.git/` skipped, ignore rules as in grep,
`noIgnore` to list ignored files too), under `paths` (default the working directory). Each row has the path, the line
count for text files up to 2 MiB (larger files show only their size), size and age; files whose first 8000 bytes hold
control bytes count as binary, and images name their type. `glob` narrows by name or path like grep's. `minSize` (`512`,
`20k`, `1.5M`, `2G`, in 1024s) and `newer` (`30s`, `10m`, `2h`, `3d`, `1w`) filter, and `sort` orders by `path`
(default), `size` (largest first) or `modified` (newest first). Pages hold 200 files; continue with `skip`. Nothing is
recorded as seen.

A listing of more files than a page holds, asked for without `skip`, shows the shape of its folders instead of its
first 200 files: from the folder they share, the folders with the most files are opened while the listing stays
within 30 rows, a chain of folders holding only one folder opening as one, and each folder left closed is one row with
its count and what the sort is by (`plugins/ (495 files, newest 1h ago)`, `largest 2.6 MiB`, or their total size),
in the listing's order among the files. It ends with
`[a folder of many files shows as one row with their count; find it for its files, or skip 0 lists every file]`.

A listing names each folder once. When every path on the page lies in one folder, the head names it
(`28 files under /home/me/x/`, absolute and `~name/` paths included) and the rows continue it. All of a folder's files
on the page, its subfolders' included, become one row `FOLDER/`, with ` (N)` from 10 files on, followed by the rest of
each file's path indented two spaces, so a file's path is the head's folder, the folder row and its indented row joined.
Of all such layouts the shortest is printed, so a lone file, or two in a short folder, keep their whole path, and a
broad folder is not a row where narrower ones under it save more. In a grep over this repository from outside it, that
halved the output (4,448 to 2,352 characters for 39 files).

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
| `PUT N.=M:` / `PUT N:` | Replace lines `N..M` inclusive (`PUT N:` is line `N` alone) with the `+` rows that follow. |
| `PUT <N:` / `PUT >N:` | Insert the `+` rows before / after line `N`. |
| `PUT >$:` | Append at the end of the file; needs no line anchor, so it also applies under a stale tag (with a warning), under a tag never issued for any file (with a warning), or under a `[PATH]` header with no tag at all. |
| `CUT N.=M` / `CUT N` | Delete lines `N..M` (`CUT N` is line `N` alone). |
| `PUT N*:` / `CUT N*` | Replace / delete the block that starts at line `N`. |
| `PUT >N*:` | Insert the `+` rows after the block that starts at line `N`. |
| `SUB N: OLD` | Replace the text `OLD`, which must occur exactly once on line `N`, with the `+` rows: several rows split the line, a lone `+` deletes `OLD`. The old text is everything after `SUB N: ` up to the end of that patch line, so nothing is quoted or escaped. The text around `OLD` stays, indentation included; when `OLD` starts where the line's indentation ends and the first `+` row repeats that indentation, it is written once, with a note. |
| `~TEXT` | A check row right under a `PUT` or `CUT` line, before its `+` rows: line `N` must start with `TEXT` after its indentation (an empty `~` means a blank line). A second `~` row checks the last line `M` of a range. |
| `REM` | Delete the file; must be the only operation in its section, and needs a current tag. |
| `MV DEST` | After the section's edits, write the file to `DEST` (missing parents are created) and delete the source. Refuses an existing destination. |
| `CP DEST` | Like `MV DEST`, but the source stays as it was: the section's edits land only in `DEST`, which keeps the source's mode. The response gives the copy's tag, and the copy inherits what the agent saw and knew of the source, so it can be edited right away. |
| `IMPORT NAME, ...` | In a Java or Kotlin file, add an import for each name (`a.b.C`, `a.b.*`, `static a.b.C.m`, Kotlin's `a.b.C as D`; `import` and `;` may be written too) where the file keeps its own: beside the imports that share most of its package, in their alphabetical order, else after the last import, else after the package line. It depends on no line the agent saw and needs no tag. An import the file has, or that another hunk of the patch writes, adds nothing and gets a note; one whose simple name another import already brings in is refused, since the two would clash. |
| `*** Add File: PATH` | Create a file from the `+` rows that follow; refuses an existing path. |
| `[PATH]` | A section header without `#TAG`; it may only append with `PUT >$:` and add imports with `IMPORT`. |

- Line numbers refer to the tagged output and are never shifted by earlier hunks. Hunks are sorted by position (after
  block ends are resolved) and must not overlap: a line can be replaced by one hunk only, two hunks cannot insert at the
  same place, and an insert may sit right before or after a replaced range, so `PUT 2.=2:` with `PUT >2:` replaces line
  2 and adds rows after it. At a shared gap, `PUT >N:` content lands before `PUT <N+1:` content.
- Blocks: `N` in a block hunk is the block's first line: a leading comment or decorator run, or the opener itself. In a
  language nib parses (see declared names under [read](#read)), the syntax tree resolves the end: the outermost node
  that starts on line `N`, or whose first part after its comments, annotations and modifiers does, so a signature line
  under its annotations ends with its method; from a comment or attribute line, the declaration below it. In any other
  file nib resolves the end by indentation, like its outline: the opener's deeper body, branch lines at the opener's
  depth that continue it (`} else {`, a lone `{`, `):`, `else:`, `except:`), and a closing line at that depth (`}`, `)`,
  `]`, `end`); Python-style blocks end at their last deeper line. A line that opens no multi-line block, or a block that
  did not parse cleanly, is refused, and the resolved range then follows the same seen-line, overlap and stale-tag rules
  as `PUT N.=M:`.
- Sections repeating a path with the same tag are merged; a different tag for the same path is rejected. Paths may be
  quoted. Blank lines between hunks are ignored. A Codex-style `*** Begin Patch`/`*** End Patch` envelope is accepted
  but not needed (the tool description no longer shows it, since it only costs tokens).
- Body rows are final content: `+` followed by the exact line. Forms borrowed from other edit languages (`@@` headers,
  `*** Update File:`, `N:` rows, `PUT N-M:`, text after a `PUT` line's colon) are rejected with a hint naming the
  correct form. `PUT N:=M:`, a range with a colon where its dot goes, can mean nothing else, so it applies as
  `PUT N.=M:` with a note giving that form.
- Diff-style `-` rows under a `PUT N.=M:`, `PUT N:`, `CUT N.=M` or `CUT N` (mixed with the `+` rows if need be) are
  accepted when they match the lines the hunk replaces, ignoring trailing whitespace and color codes. Like a `SUB` they
  then quote those lines, so they need no earlier read, and the response ends with a note showing the same hunk without
  them, since nib never needs the old text. When they do not match, the section is refused with the real lines; under
  any other hunk, or outside one, `-` rows are an error.
- All sections are parsed, read and checked before anything is written; files are then written one at a time, each
  atomically, so a write failure after the first file reports which files already changed.
- A refusal lists every problem in the patch at once: every malformed line, and for every section all its failed `~`
  checks, `SUB` texts and `-` rows together with its unseen lines (or its stale tag), each kind run on the hunks the
  kinds before it passed. One corrected resend is then enough.
- Every refusal of a patch that parses ends with a token for it. When the only problems are unseen lines (see below),
  the token applies the patch unchanged: the refusal ends with
  `To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY t0k3n5`, or after an
  agent's first such refusal with just `REAPPLY t0k3n5`. Unseen lines the refusal could not display whole (lines cut
  past its width, more than 40 lines) must be read first, and it says so; a token sent before that is refused again,
  with a new token. Any other refusal ends with `To resend a file's section corrected, call edit with input: REAPPLY
  t0k3n5 on the first line, then that [PATH#TAG] section whole, since it stands in for the stored one; ...`, later just
  `REAPPLY t0k3n5, then corrected sections, each whole`: sections under the `REAPPLY` line replace every stored section
  for their path, in its place, sections for other paths join the patch, and the patch stays all or nothing. A resent
  section with fewer hunks than the stored one drops the rest, and the response says so (`[REAPPLY: the section you
  resent for a.go stood in for the stored one whole: its 1 hunk applied, not the stored 3]`), since a model resending
  one corrected hunk takes the others as applied. A line with color codes can never be read whole, so
  an unseen one gets that kind of token too. Either way the stored patch goes through the full normal validation, as a
  resend would; the patch's own tags still guard against files that changed meanwhile. A token belongs to its session,
  works once, and only the latest 64 are kept; corrections that do not parse are refused before the token is spent.
  `grep` with `replace` hands out the same kind of token for the patch it previewed. Likewise only an agent's first
  unseen-line refusal explains what can cause one and how to proceed; later ones keep the rows and a one-line
  `Meant these? Resend unchanged; otherwise fix the numbers.`
- `SUB` replaces only text the patch quotes, so it is exempt from the seen-line rule and works on cut long lines.
  Several `SUB`s on one line apply to the original line and must not overlap. Its result counts as known content only
  when the whole original line was known. An old text that is not on line `N` but on line `N`±1 or ±2 applies there,
  with a note naming both lines, when the line it is on is certain: it is the only line of the file that holds the text,
  the text is at least 8 characters after trimming (the bar the shifted-anchor rescue uses), the agent saw or wrote that
  line, and no other hunk of the section anchors it. Otherwise the `SUB` is refused as before, showing the line.
- A successful section returns its new `[PATH#TAG]` and the kept row on each side of each change, in full, where a
  leftover line or a misplaced range shows (a grep replace token applied as previewed returns only the tag and a count;
  see grep). The new rows the agent sent whole come back as one marker per run naming their lines,
  `[lines 40-46 as you sent them]` (`[line 40 as you sent it]` for one), since the agent knows them, and a run of more
  than 5 keeps a sparse map of `N~TEXT` rows under the marker (the line number plus the first 30 characters after the
  indentation; at most 40 map rows per response), so no number needs counting. The rows nib composed from the agent's
  words, a `SUB`'s result or an `IMPORT`, print as rows: every row of a run of up to 5, otherwise the first and last
  row with the map between them.
  Each stretch of kept lines that moved and holds a line the agent knew gets one note, such as
  `[lines 427-1004 are old lines 423-1000, moved down by 4]`; an edit that moves nothing adds none. An agent's first
  response with such a note also says how to anchor on a moved line without a read: a `~TEXT` check of its start at its
  new number. An edit response cuts a composed row past 300 characters and a kept row past 120, as the agent knows part
  of the one and only needs the other to place the change; a changed row past the cut shows the stretch where it changed
  when a line was replaced by one line, as `[N chars 2952-3057 of 3108]|...`, instead of the line's start. A cut row still
  counts as seen when the agent wrote it, or knew the line whole before the edit moved it, so it anchors the next edit;
  every new row does, cut, mapped or elided, except a `SUB` result on a line the agent never knew whole. The note on such
  cut rows says a plain `read PATH:N` shows one whole, which needs no `maxLineLength` below 2000 characters. A response
  that changes nothing still shows the lines whole, up to 2000 characters. Follow-up edits can use that tag and numbering
  without a read. A section that reproduces the current content reports `No changes` with those lines whole, so an edit
  resent unchanged shows what the file already holds, and leaves the file untouched.
- A replace whose range looks one line off still applies, with a warning naming the two lines: the kept line just above
  the new rows nearly matches the first new row (the range started one line late, as when `PUT 66.=70:` rewrites a
  header that sits on line 65), or the kept line just below nearly matches the last new row (it ended one line early).
  Lines count as alike when their common start and end, indentation aside, make up at least 75% of the longer one; lines
  under 12 characters count only when they are the same apart from indentation and hold a letter or digit (a repeated
  `cancel()`, never a blank line or a lone `}`). A new row that is even more like the line it replaced, as in an ordinary rewrite,
  raises nothing. The warning ends with the hunks that undo the slip under the new tag: for a late start, `CUT` of the
  kept line plus a `PUT >N:` that puts back the line the range overwrote; for an early end, `CUT` of the kept line.
- A replace that drops one edge line of its range still applies, with a warning, when the range likely ran one line too
  long: its first or last old line, at least 12 characters, is like no new row of the patch, while every other line of
  the range that is not blank is like one (as when `PUT 117.=119:` rewrites two lines and takes the call on line 119
  with them). The warning names the line and ends with the hunk that puts it back under the new tag, `PUT >N:` after the
  new rows or `PUT <N:` before them. A rewrite of the whole range, a deletion, or a line the patch moves to another hunk
  raises nothing; a deliberate removal of an edge line beside lines that stay alike does, so the warning says "if".
- An edit that leaves a file's brackets unbalanced, when they balanced before, still applies, with a warning that says
  which brackets are now extra and names each hunk whose new rows changed the count (`Brackets no longer balance: ...`).
  Brackets are counted outside strings, comments and regex literals, with `(`, `[` and `{` each kept apart; a file that
  never balanced, such as a shell script with `case` labels, gets no such warning. This catches what the line comparison
  cannot: a closing `}` left just outside a replaced block, too short to compare. That slip is corrected before anything
  is written when it is certain: when the last new row equals the line just below the range (or the first new row the
  line just above), and widening the range by that one line, and no other widening, keeps the brackets balanced, the
  edit replaces the wider range and says so
  (`The edit of lines 1-2 replaced lines 1-3: its last new row repeats line 3, which would have stayed below it with the brackets unbalanced, so the range ended one line early.`).
  It is just as certain when the rows are a copy taken one line too far: the first new row equals the line just above
  the range and holds a letter or digit, and the next row is the range's own first line (or, at the end, the last row
  equals the line just below and the row before it is the range's last line), as when `PUT 3.=4:` starts with line 2's
  `cancel()` and goes on with line 3's `c.killAll()`. The range then takes that line in, with the same note ending
  `and its rows go on from there as line 3, so the range started one line late.`
  A line another hunk anchors is never taken in. In a Hawkeye template (`.hwk`) only its comments, `{# #}` and
  `<!-- -->`, are left out: its text is prose, where an apostrophe opens no string.
- An edit that leaves a file in a language nib parses (see declared names under [read](#read)) no longer parsing, when
  it parsed before, warns with the line of the first error (`The file no longer parses as Java: ...`); a parse of a
  file of a few hundred lines takes a few milliseconds. Silence must never pass for a clean check, so a check skipped
  for a cause the agent would not expect says so: a file over 512 KiB, which is not parsed on every edit, and a file that
  did not parse before the edit, where nib cannot tell what an edit broke, each say it once per file until the agent's
  context is dropped.
- An edit to a Java file whose new rows use a class by a name that nothing in the file imports or declares, that
  `java.lang` does not hold and that the file did not mention before, warns with the classes nib knows by that name: the
  public classes of the JDK's `java.base`, `java.sql`, `java.net.http` and `java.logging` (a table from JDK 25 that
  `internal/syntax/ListJdk.java` writes), and the Java or Kotlin files named after it in the workspace roots, read for
  their package. One class gives the hunk that adds it (`[PATH]` and `IMPORT java.time.Duration`); several are listed,
  as a workspace `Clock` beside `java.time.Clock`; none, a class in the file's own package or a package it imports with
  `*`, says nothing. A wildcard import of a package neither the JDK table nor the workspace holds (a library's), a
  static wildcard import, a module import, or a file with no top-level class (a compact source file) turns the check
  off, since nib cannot tell what those bring in. Names count as classes by Java's naming: a capital first and a small
  letter in them, in a type, an annotation, a `new`, a cast, a `catch`, a method reference or before a call or field
  (`Duration.ofSeconds`). When unbalanced brackets already say the edit broke the file, the parse warning is left out.
  When the search for a class cannot run (a file outside the workspace roots, an `rg` failure, or a search past 5
  seconds), the warning names the classes it could not look for and why, instead of saying nothing.
- An edit or write that leaves a `.json` file unsound, when it was sound before or is new, warns with the first fault
  and its line: a syntax error, or a key that repeats within one object
  (`The file is not valid JSON now: line 4: key "save" repeats in its object (first on line 2).`), which parsers resolve
  silently. A file that never parsed, such as one with comments, gets no such warning.
- The original UTF-8 BOM, CRLF/LF style and final-newline state are retained. To give a file without a final newline
  one, append a lone `+` row with `PUT >$:`: an empty last row on such a file ends the line before it rather than adding
  a line, so the response shows no row for it and says `[path now ends with a newline]` (on a file that already ends
  with one, the same hunk adds a blank line). A read that shows such a file's last line ends with
  `[no final newline; PUT >$: with a lone + row adds one]`, and an edit that rewrites its last line without changing
  anything says the same. Publication stages a complete file and atomically replaces the target, preserving POSIX
  ownership and mode, not ACLs or extended attributes. Hardlinked targets are refused. The parent directory is pinned as
  an open root, so a path to it that changes meanwhile cannot redirect the write; neither this nor the path/inode
  rechecks are a compare-and-swap against arbitrary concurrent writers.

### write

```
Created [notes/brief.md#40FA] (23 lines, 19 non-blank, 5 shown)
1~# Handoff
5~## Settled
15~- item 15
19~## Do the remaining work befor...
23~Last line
```

`write` and `*** Add File:` answer with a sparse row map of the new file, so it can be edited without reading it back.
Each `N~TEXT` row is a line number plus the first 30 characters after the indentation (`...` when the line goes on). The
first and last non-blank lines are shown; a non-blank line right after a blank one is shown once at least 4 lines passed
since the last shown row; and after 10 non-blank lines without a shown row, the 10th is shown. Blank lines are never
listed. Past 300 rows the response is the outline a read would give instead, or no rows for flat data. Every line the
agent wrote counts as displayed, shown in the map or not, so any of them anchors an edit at once; a `~` check still
confirms a line's number where the agent wants one. Edit responses show the same kind of rows inside the elided middle
of a long inserted run.

Overwriting with `write` keeps the file's own style where the content cannot carry it: in a file whose line breaks are
all CRLF, content with plain `\n` breaks is written with CRLF, and a BOM stays unless the content starts with its own;
the response says what it kept (`; kept its CRLF line endings and BOM`). Content with `\r\n` breaks is written as given,
as is anything over a file with mixed line endings. The final newline follows the content. To change a file's line
endings or drop its BOM, delete it (`REM`) and write it new.

### nibash

nibash is a bash interpreter ([mvdan/sh](https://github.com/mvdan/sh), bash dialect). Every call starts a fresh
interpreter in the project directory: no `cd`, variable, `export` or function carries over to the next call, so a
directory that moved or vanished since can never break a later command, and calls run in parallel, none waiting behind
another. What does carry over is an agent's jobs, kept per client session, per Claude Code subagent where nib's hook
tells them apart, and per OpenCode session ([below](#the-hook)), and its named directories, which live in nib, not in
the shell. Every program is a real process in its own process group (a job object on Windows). A command still
running when the call's `timeout` ends (default 2 minutes, at most 10) is never stopped: the call returns the output so
far and the command goes on as a job, its whole output, from the start, in the job's log. A call that only waits
(`wait`, `wait %2`) gets 10 minutes unless it asks for less, and then stops waiting rather than go on as a job that
waits for jobs, with a note naming the jobs, `[job %2 still running after 10m0s; wait %2 again]`, which run on. Only
the client cancelling a call that still waits, `kill`, or the end of the connection stops a program (SIGTERM, then
SIGKILL after 3 s). Standard input is empty, and `GIT_TERMINAL_PROMPT=0`, `PAGER=cat`, `GIT_PAGER=cat` and
`TERM=dumb` keep programs from waiting on a terminal; `GIT_ADVICE=0` keeps git from printing its `hint:` advice.

- **nib's own commands.** `cat`, `head`, `tail`, `grep`, `egrep`, `fgrep`, `rg`, `sed -n` (line selections), `cut -c`
  (character selections) and `wc` run as nib's own when their output goes to the model or into another of them and
  nibash implements their options. Before running, nibash parses the command and decides this per command; a command
  written with a path (`/usr/bin/grep`), shadowed by a function, given an option nibash lacks (`grep -P`, `head -c 2K`,
  `sed -i`, `cut -f`), redirected to a file, inside `$(...)` or feeding a real program is the real program. Where only an
  expansion gives what decides it (`sed -n "${r}p"`, `cut -c "$n"`), it is decided as the command runs, from the words
  the expansion gave. One given an option or pattern nibash lacks whose output reaches the model ends the call with a
  note once it printed, unless nib's own would have printed its lines plain too (`wc`, or one naming no file or
  directory, reading only standard input), like
  `[grep ran as the system program: its pattern needs a feature nib's own cannot run exactly, so its lines are plain output, not edit anchors]`.
  Between nib commands, lines pass with their origin (file, line, version) over a channel, never as bytes, so no GNU
  byte format is ever imitated where a program could read it. A loop, block or subshell, or a command decided as it
  runs, that writes into a nib command sends its nib commands' lines through the pipe in band: each line waits in the
  call and the pipe carries a line naming it with a token only that call knows, so the rows keep their place among what
  `echo` and real programs write there, and `for f in a b; do echo "== $f"; sed -n "$r" "$f"; done | cut -c1-200` still
  prints anchored rows. At the end, file lines print as `[PATH#TAG]` blocks of `N:TEXT` rows, grep matches as `*N:`, and
  each row is checked against the file version the session holds, so an `N:` row is always a real line and counts as
  displayed for `edit`. The first time an agent pipes such rows into a real program whose output reaches it, the call
  ends with one note, `[sed's rows lost their line numbers in awk; read path:A-B,C-D keeps them]`.
- **cut -c.** nib's `cut -c LIST` (`N`, `N-M`, `N-`, `-M`, comma lists, `--characters`) keeps a file's line it leaves
  whole as its `N:` row, and prints one it shortens as a bracketed row naming what it kept,
  `[line 40, chars 1-220 of 431]|...` (stretches joined by ` ... `), which is no anchor. It selects exactly what the
  system `cut` would: before its first run in an environment it runs that `cut` once on a probe line, to learn whether it
  counts bytes (the C locale, older coreutils) or characters with each invalid byte as one (coreutils 9 in a UTF-8
  locale), and if the answer is neither, the system `cut` runs. A line where the selection would split a character, a
  line with color codes, a BOM file's first line (the program reads the mark the row leaves out), program output and
  grep's prefixed lines print the selected text as plain output, as the system `cut` would print it. Its other forms
  (`-b`, `-f`, `-d`, `-s`, `--complement`, `--output-delimiter`) run the system `cut`.
- **git, reshaped.** `git` is nib's own only where its output goes to the model, directly or through nib's own commands,
  and only for the forms it reads: `status` (short, porcelain v1, long), `add -v`/`-n`, `rm`, `ls-files`, `diff`, `show`
  and `log` with a patch, `--stat`, `--name-only`, `--name-status` or `--numstat`, `show` and `log` in git's default
  format, and the subcommands that change a repository (below). It runs the real git
  with the command line's own arguments, so the exit status, standard error and every side
  effect are git's; what it prints changes form, and in a few places content. Path lists name each folder once, as `find`
  does, in a bracketed row (`[src/server/java/be/elevenways/cms/]`) with the rest of each path under it and the rest of
  every line kept (the `XY` of a status, a stat's counts); a folder within a folder gets its own row under it, and a run
  is laid out so only where that saves at least 16 characters, never making the output longer. A `--stat` file row
  prints its exact counts instead of git's bar, `Store.java +34 -34` (`+25` for a file only grown, `-9` for one only
  shrunk, `0` for one with no changed lines), read from the bar git drew: a `--stat` without a width runs twice, as git
  would print it and with `--stat-width=1000000 --stat-graph-width=1000000` (every path whole, one bar character per
  changed line), and prints whichever is shorter laid out. A row whose bar git still scaled, a binary file's
  `Bin ...` row, the summary line and a `--stat=N` the command line sized print as git printed them; into anything that
  reads text the bars stay too. A diff prints each changed file under its `[PATH#TAG]` header as it is now: a line the
  diff adds or keeps is the file's `N:` row (`+N:` for an added one) where the file holds
  exactly that text at that line, a removed line is `-|TEXT`, and a line the file no longer holds there (a staged change
  since edited, an old commit) is `+N|TEXT`, never an anchor. Those rows count as displayed, so an edit can anchor on
  them with no read. A deleted, binary or quoted-path file's part, and anything else git prints, is as git printed it.
- **git's commit headers, one line.** A commit header of `log` or `show` in git's default format prints as one line,
  `commit 1a2b3c4 o@example.com Fri 2026-10-16 10:38`: the hash as git abbreviates it (nib adds `--abbrev-commit`), the
  author's email, left out where it is the repository's own `user.email` (as `-C` and `-c` give it; the first such
  header an agent sees ends the call with a note saying so), and the author date in the machine's local time to the
  minute. Decorations stay on that line, a merge's `Merge:` line stays under it, and the message follows without git's
  indent and without the blank line before it. This is the one place nibash changes what a program says rather than
  how: the full hash, the author's name, the seconds and the zone are gone. A header in another format (`--format`,
  `--pretty`, `--oneline`, `--graph`, `--date`, or a configured date or format that does not read as git's default),
  and a tag's own header in `git show TAG`, print as git printed them.
- **git that changes a repository.** `commit`, `merge`, `pull`, `rebase`, `cherry-pick`, `revert`, `push`, `fetch`,
  `checkout`, `switch`, `reset` and `stash` are nib's own with any option (except `-p`/`--patch` and `--porcelain`,
  which run the system git), and where their output reaches the model, both of git's streams are tidied. A path in a
  commit's summary (`create mode 100644 PATH` prints as ` create PATH`, `delete` likewise; another mode stays), a
  conflict (`CONFLICT (content): Merge conflict in PATH`), an `Auto-merging PATH`, a checkout's or reset's `M\tPATH`
  and a status a commit prints is laid out by folder as above. The diffstat a merge or pull prints runs with
  `COLUMNS=1000000` and `-c diff.statGraphWidth=1000000`, so its rows print every path whole with exact counts, as a
  `--stat` does. Hidden from the model, and kept in the log: every `hint:` line (pull's divergent-branches hint prints
  whatever `GIT_ADVICE` says), the progress a carriage return writes over (`Rebasing (1/1)`), the ` Date:` line a
  cherry-pick's or amend's summary repeats, and an unindented line printed already, as ssh repeats a warning for each
  connection. A ref's line from `push` or `fetch` loses its padding (`   1a2b3c4..5d6e7f8 main -> origin/main`). A
  merge or pull that cannot fast-forward ends with how the branches diverged, from `git rev-list --left-right --count`:
  `[nibash: HEAD and origin/main have diverged: 2 commits only on HEAD, 1 commit only on origin/main]` (for a pull
  without arguments against `@{upstream}`, with arguments against `FETCH_HEAD`). Into a file or a real program, their
  output is git's own.
- **git through head and tail.** Into nib's own `head`, `tail` (whole lines by count: `-n N`, `-N`, `tail -n +N`,
  `head -n -N`) or `cat` without `-n`, one after another and on to the model, git streams, so `head` still stops it
  early, and every line git prints counts as one line for them. The lines they keep then print as they would at the
  terminal, as far as they kept each part whole: a commit header they cut prints as git printed it (its hash already
  abbreviated), its message with git's indent, and a diff's own `diff --git`, `index`, `---` and `+++` lines print only
  where no row of the file follows them. A stat is always the wide one there, which has the same lines in the same
  order, one a file. Into anything else (nib's `grep`, `cut -c`, `sed -n` or `wc`, `head -c`, `cat -n`, a real program,
  a file, `$(...)`), and into a loop's or block's pipe that one of those reads, git's own lines go as git printed them,
  so what reads them sees exactly what the system git prints; only its path lines carry marks, so a run of them that
  `grep` passes on whole is still laid out by folder where it reaches the model. Any other subcommand, a pipe into a
  real program or a redirect, or `-z` and the like run the system git untouched; an option nib's git lacks on a
  subcommand it handles ends the call with `[git ran as the system program: ..., so its output is as git printed it]`.
- **git-each.** `git-each ARGS...` is a command of nibash's own, which no system program has (one on `PATH` of that
  name runs instead): it runs `git -C REPOSITORY ARGS...` in every repository at and below the shell's directory, found
  as searches find them (outside git by walking, inside a repository from git's lists of its ignored and untracked
  folders, and each one's own in turn; a worktree or submodule is none), one after another in path order. Only those
  whose git printed anything, to standard output or error, print, each under a `==> PATH <==` header, the path
  relative to the shell's directory or as nib prints it when that is shorter; where it reaches the model, each git is
  nib's as above. `{base}` in an argument is that repository's integration branch: `"base"` in its own `nib.json`,
  else what `origin/HEAD` points at (`origin/master`); a repository with neither prints
  `[skipped: no "base" in its nib.json and no origin/HEAD]` and runs nothing. Its exit status is 1 when any git
  failed, 2 for no arguments or for `git rm` and `git clean`, which it refuses: what those delete goes to the trash
  only when they run in one repository. The command rules see it as `git ARGS...` too, and the rules of every
  repository it runs in count. The server instructions mention it only where the server's directory holds two
  repositories or more.
- **find and ls, reshaped.** `find` and `ls` are nib's own the same way: only where their output goes to the model,
  directly or through nib's own commands, and only with what they print one path a line. `find` takes its tests and
  operators (`-name`, `-path`, `-type`, `-maxdepth`, `-newer`, `-mtime`, `-size`, `-not`, `-o`, parentheses, `-prune`,
  `-print` and the like) and no action that prints otherwise or acts (`-exec`, `-delete`, `-printf`, `-print0`, `-ls`);
  `ls` takes `-1aAlhRtSrdgGoFpXUvc`. Each runs the real program with the command line's own arguments, in its own order,
  ignore rules and hidden files as it would, and prints the runs of its paths with each folder once, as above; under
  `ls -l` the columns before a name stay. A line counts as a path only where it names a file that is there, so nothing
  else the program prints is ever regrouped. Names in a directory's own listing carry no folder, so those print as the
  program printed them.
- **Deletes go to the trash.** `rm`, `unlink`, `git rm`, `git clean` and `find -delete` move what they delete to the
  user's trash (on Linux and the BSDs the freedesktop.org trash, `$XDG_DATA_HOME/Trash`, which file managers and
  `gio trash` read; macOS and Windows in [Platforms](#platforms)) instead of deleting it, wherever their output goes
  and with nothing excepted. This is the one place where nibash's own commands differ from the system programs in what
  they do, not only in the form of their output. `rm` keeps coreutils' options (`-r -R -f -d -v -i -I --interactive
  --one-file-system --preserve-root`, abbreviated long options and bundles), messages, prompts and exit status; a
  prompt reads stdin, so with nibash's empty stdin `-i` answers no.
  `--no-preserve-root` and options it lacks are refused, never handed to the system `rm`; `--help` and `--version` are
  the system program's. A directory goes to the trash whole, `-v` still naming each entry. `git rm` first makes a dry
  run with the same words, which refuses what git would refuse, trashes the files it lists, then lets git update the
  index. `git clean` (with `-f`, or with `clean.requireForce` off) trashes what `git clean -n` lists and prints git's
  `Removing` lines; it refuses `-i`. `find -delete` has find print what `-delete` would remove, and nib trashes it in
  find's order, reporting a directory that is not empty as `-delete` would. `edit`'s `REM` goes to the trash too. A file
  on another filesystem goes to that filesystem's own `.Trash-$UID`, or under `NIB_SANDBOX=1`, whose one writable place
  outside the roots is the home trash, as a checked copy to the home trash. `trash list` shows what this session, its
  main agent and subagents, moved to the trash and is still there; `trash restore [-f] PATH` puts one back and makes its
  directories again, refusing a path in use unless `-f`, which moves that path to the trash first. nib never empties the
  trash. An agent's first call that trashes something ends with `[moved to the trash, not deleted: ...]`, later ones
  with `[in the trash: ...; trash restore PATH]`. Deletes a program makes itself (`xargs rm`, `bash -c 'rm ...'`, a
  script) are the system's.
- **A failed cd ends the call.** A `cd`, `pushd` or `popd` that fails where the command line does not handle it ends
  the call: nothing after it runs, in a subshell, function, loop or `$(...)` either, since under bash the rest would run
  in the directory before, where `cd build; rm -r *` or `cd ../other; git reset --hard` does its harm. The call ends
  with `[cd build failed, so nothing after it ran]` and the change's exit status. One the command handles runs on as
  under bash, by the rules of bash's `set -e`: in an `if` or `while` condition, before `&&` or `||` (`cd x || exit`,
  `cd x && make`), after `!`, in a pipeline before its last stage, or in the background. A script a program runs
  (`bash -c`, `sh script.sh`) and a file `source` reads are not guarded.
- **Facts.** `head` and `tail` of a file say which lines they show (`[a.txt: lines 1-20 of 1,234]`; a `head` of a file
  nib does not hold stops reading, so it says nothing); `head` after a nib grep says how much there was
  (`[head: 20 of 312 matching lines shown, from 4 of 17 files]`; `files` after `-l` and `-c`, `matches` after `-o`, and
  no files when grep read one input, counting for up to 2 s after `head` stops reading); `tail` of a program's output
  that leaves 10 lines or more out keeps all of it in a log and names it with the lines left out
  (`[4,762 earlier lines in <log>]`; fewer, which the agent cut on purpose, get no
  note); with a decision model or a summarizer, those lines may be checked first
  ([below](#long-output-and-deciders)).
- **Byte counts.** `head -c N` and `tail -c N` (or `+N`) print exactly the bytes the system programs print, as lines: a
  line they print whole is the file's row, and the line cut at the edge shows as `[line 4012, chars 61-75 of 75]|...`,
  never an anchor (plain text where the cut splits a character). Their input is always bytes, so a nib command before
  them writes as the real program would, without rows.
- **Output.** Color codes go, a `\r` progress line keeps its final text, invalid UTF-8 shows as U+FFFD and binary output
  only as its size. Plain output shaped like rows (`N:` or `N~`, as `grep -n` through a real program prints) that
  follows a file's rows is set apart by a line of nib's own, `[plain program output follows, not anchors]`. Plain output
  over 20 lines collapses exact repeats, and a plain row over 2,000 characters is cut in `read`'s bracketed form with
  its log line (`[line 2, chars 1-2000 of 20157]|...`), so it never looks like an anchor; the footer that explains this
  is whole the first time, then `[cut rows: read LOG:N whole]`. A plain row holding encoded data (a run of 100 or more
  base64 or hex characters that mixes letters and digits over 16 distinct ones and holds at most one `+`, `/`, `-` or `_`
  in 16 characters, such as a token or a signature, never a path or a UUID) shows in that form without it
  (`[line 1, chars 1-6, 145-157 of 157]|token= ...  expires=3600`). Output over 400 rows or 30 KiB
  is cut by the decider ([below](#long-output-and-deciders)) and the whole saved to a log in `$XDG_RUNTIME_DIR/nib/` (or
  the temp directory), capped at 64 MiB. A nonzero exit ends the result with `[exit N]`, which names the program that
  failed with that status when the command line has more than one command: `[exit 128: git pull --rebase]`.
- **Failures that could go unseen.** A program (not a builtin or a function) that failed without the call's exit status
  saying so is named in one closing line when its standard error went to `/dev/null`, an `&&` chain skipped what followed
  it, or it is a pipeline's stage before the last, whose status the pipeline's is not: `[nibash: git pull -q exited 1,
  its stderr discarded; go test ./... exited 1 before | grep FAIL]` (up to three, a loop's repeats counted). A failure
  is handled, and not named, in an `if` or `while` condition, after `!`, before `||` with anything but `true` or `:`
  after it, or when the next command reads `$?`; so is exit status 1 of `grep`, `rg`, `diff`, `cmp` and `which`, which
  answers a question, and a stage's end by SIGPIPE (141) when what reads it stopped, as `head` does; nib's own commands
  end with 141 too when their reader stopped and they failed, as the system program would. nibash finds each program by
  a `NIBASH_STEP` variable it sets for that one command, which no program gets.
- **Changed files.** Files the session holds versions of, and files a redirect wrote, are listed with their new tags
  when the command changed them: `[changed by this command: a.txt#9C10 (was #3F2A, +1 -1), notes.md#1B2C (new)]`. A
  file the agent saw some of, changed in at most 10 lines, then shows the change under its header: removed lines as `-|`
  rows, added ones as `+N:` rows of the new version, which count as seen, so an edit can anchor on them at once, and the
  kept line before a hunk that only removes, so its place shows; at most 30 such rows per command, the rest counts only.
  A run of added lines that a here-document of the command holds in that order, as `cat >> notes.md <<EOF` appends,
  prints as one `[lines 1233-1239 as you sent them]` row instead, and those lines count as seen and known.
  Files in the temp folder (`$TMPDIR`, else `/tmp`; `%TEMP%` on Windows) the agent never read or wrote a line of are
  only counted: `[changed by this command: 2 new files in the temp folder]`, or `1 changed file`; once read, such a file
  is listed like any other.
  A file whose content is exactly a here-document of the command counts as known, like a `write`, so a checked edit
  needs no read. A held file something else was already changing as the command began (it differed from the version
  the session holds,
  another running call or job of the session names it, or it was found so before and changed in the last 10 s) is left
  out unless the command redirected to it, as a log a build writes is: its change need not be the command's.
  A change that started while the command ran is credited to it only when the file lies in a directory one of its
  commands ran in, or under a path one of their arguments named, and no `edit` or `write` and no other nibash call that
  reached it ran meanwhile, in any session of the server. Any other change is listed apart, with counts and no rows:
  `[changed while this command ran, by it or another writer: ../main/README.md#1B2C (was #9C10, +4 -1)]`. A writer
  outside the server, such as an editor, is told apart only where it writes beyond what the command reached.
- **Jobs.** `background: true` runs the whole command as a job and returns its id and log at once. `cmd &` starts a job
  too, and `$!` is its spec (`%3`) for `kill`, `wait` and `jobs`, which nibash implements for real processes; jobs
  outlive the call that starts them, and a later call reaches them by their spec. Finished jobs are reported at the top
  of the next call, unless a `wait` already told their end, and only the end of the connection ends the rest. In a
  Claude Code session started naming nib's channel (`--channels server:nib`, or
  `--dangerously-load-development-channels server:nib` while channels are a research preview) and with `NIB_CHANNEL=1`
  in its environment, since Claude Code tells a server nothing of its channels (claude-capture sets both), the end of a
  `background: true` job, or of a call that went on as a job, comes to the main agent on its own as a channel message,
  waking it: `[job %1 finished, exit 0; output: LOG]` and what the job printed after its call returned, shaped as long
  output is, so no `wait` is needed; the job's note says so. For this, nib speaks MCP 2025-11-25 with such a session,
  the last revision on which Claude Code takes a message a server sends unasked. A subagent's jobs keep the report,
  since a channel message lands in the main conversation, and so does every job when nib's hook cannot tell the main
  agent's calls apart. A call still running in its request is no job: when `wait` finds no such job, or no running one,
  while another call of the agent runs, a note after the output names that call and says its result comes back to it
  (`[no job %2 to wait for: the call "make test" still runs as a call, not a job, and its result comes back to that
  call]`); `wait`'s own message and exit status stay bash's. `nibash_status` lists the client's commands in flight and
  its jobs across all its agents, `[agent] running 0:42: command (description)` and `job %1 [agent] exit 0 after 1:05:
  command; output: LOG`, their last output lines behind `  | ` with `tail`.
- **Differences from bash** are in the tool description: `$!` holds a job spec, a pipeline's last stage runs in the
  shell itself (`... | read v` sets `v`), `type cat` names the system program, which is what runs where nib's own does
  not, a program does not start in a directory deleted under the call (bash's child would inherit it; nibash says why
  and exits 126), and zsh's `hash -d name=dir` and bare `hash -d` define and list named directories
  ([below](#named-directories)). Bash's own `hash -d name` (forget a remembered command) keeps its meaning; the
  `name=dir` form is invalid in bash, and bare `hash -d` is an error there. So that jobs, pipeline stages and other
  subshells expand `~name/` too, the shell keeps each name as an unexported variable `HOME name`: never in a program's
  environment, but `${!HOME*}` lists them.

With `NIB_SANDBOX=1`, nib's own commands, redirects, `source`, globs and `cd` are confined to the roots like the file
tools, and real programs run in bwrap: the system read-only, an empty home except the `PATH` entries under it and the
directories in `NIB_SANDBOX_BIND`, the roots writable, a private `/tmp` and no network unless `NIB_SANDBOX_NETWORK=1`.
With no bwrap, as on every OS but Linux, nib refuses to start in sandbox mode unless `NIB_SHELL=0` turns nibash off.

#### Long output and deciders

When output is cut, a decider chooses which rows stay. It only chooses among real rows: every row shown is a row of the
output, unchanged, and every note is one of nib's fixed templates. A cut that chose its middle numbers each row with
its log line, as `1180|error: boom` (a row standing for several, such as a collapsed run, as `N-M|`), so the numbers
show every gap; the number is never an edit anchor. The fixed cut names its one gap in its closing note, counted in the
log rather than in rows, so `sed -n 101,4800p LOG` shows exactly what is missing. The closing note says how many rows
are shown of how many log lines, who chose and where the log is (`N| is a row's line in the whole output: read
LOG:A-B plain:true for others`).

Output under file headers, the rows of nib's own `cat`, `grep` and `git diff`, is cut file by file instead: every file's
header stays, each file shows its rows from its start as far as its fair share of the budget goes, and a diff's hunks go
in whole once a file's first is shown. What a file leaves out is named under its header, by hunk where it is a diff
(`[nibash: 3 hunks left out, +40 -3: fn flush, fn dueInMs, fn tick]`, named as git names them) and by count otherwise,
and the closing note says to run the command for the path of a file left out, since the log holds git's or the program's
plain text, not rows. Keywords never judge such rows (code names errors all the time); a decision model does, when the
call gives a `description` or an `expect`, and its picks keep their hunk's header and a fair share of each file. Without
an `expect`, the note says that one would let the model choose; a model that judges the purpose needs every line leaves
the cut by file.

- **`keywords`** (the default): the first 30 and last 100 rows, plus the rows between that name a problem (`error`,
  `failed`, `panic`, `Traceback`, `TypeError`, pytest's `E`, `FAIL`, `not ok`, `Expected`/`Received`, Bun's cross; not
  inside file names such as `src/errors.ts`, and not counts of none such as `0 errors`), each with 3 rows around it, up
  to 300 rows and 30 KiB. The last hit goes first, then the rest in order. With no hit, the fixed cut.
- **`fixed`**: the first 100 and last 200 rows.
- **`openrouter`**: keywords plus a decision model on OpenRouter (`typesafe/jev-1.14` unless `NIB_DECIDER_MODEL` names
  others; with several, comma separated, the next one decides when one gives no answer at all, and the note names who
  chose and which failed). The rows between the kept start and end go in windows of 25 rows, plus a second set offset by
  half a window so a line at a window's edge is also judged with what follows it, each row cut to 400 characters, at
  most 128 requests of 30,000 bytes, and the model rates each window against the call's `description` and its `expect`,
  what the agent expects or wants to learn, when given (else, for a failed or timed-out command, against its failure);
  windows rated 0.5 or more are kept best first, then keyword hits. When the model judges that the description needs
  every line (a count or a total, 0.7 or more), only the start and end are shown and the note says to filter or count
  the log instead. Without a description, a command that exits 0 is cut by keywords alone. A failed request leaves its
  lines unjudged, named in the note; when every request fails (timeout, 4xx or 5xx after one retry for 429 and 5xx, an
  invalid answer) keywords decide and the note gives the reason; 401, 402 and 403 turn the model off for the session. A
  decision adds about 0.4 to 1.2 s and $0.002 to $0.006 per cut output, measured on the offline check's captures;
  answers are cached by request in memory and, with `NIB_DECIDER_CACHE`, on disk (no output text is stored, only the
  ratings).

**Summaries.** With `NIB_DECIDER_SUMMARY`, a model condenses plain output past 150 lines or 12 KB (leading with what
contradicts the call's `expect`, when it has one), whether or not it is over the cut's budget (at 8 KB the recorded
captures lost an end-to-end run's results; output within the budget stays whole when the summary fails), except output
within the budget that lists 10 or more problem lines, 3 or more of each kind (type errors at many call sites): each is
a fix to make, so it stays whole. The variable is a comma separated list in order of preference: an OpenRouter chat
model id (`deepseek/deepseek-v4.1-flash-1017`), `claude-p:MODEL` (`claude-p:haiku`), which runs
`claude -p --model MODEL` on the user's own Claude login, in an empty temporary directory with no tools, MCP servers,
settings or saved session, so it loads neither nib nor hooks and needs no key, or `opencode:PROVIDER/MODEL`
(`opencode:deepseek/deepseek-flash`, with `#VARIANT` where OpenCode takes one), which runs OpenCode 2's
`opencode run --standalone` on a provider the user logged into in OpenCode. That run gets a temporary folder of its own
for its data, config, state, cache and temporary files, removed after, and an in-memory database, so it neither reads
nor writes the user's sessions, settings, MCP servers or plugins; its one agent carries the prompt with every
permission denied, which leaves it no tools. Its provider's API key comes from the user's logins, read only (OpenCode
2's database, then the `auth.json` of OpenCode 1), and goes in through the config the run is given; an OAuth login is
refused, since a run that refreshed it could spend the user's refresh token, and a provider with no login runs as it
is (keys in the environment). The program is `opencode` on `PATH`, or `NIB_DECIDER_OPENCODE`; one that is missing or
not OpenCode 2 fails its entry when it runs, so the next one answers. An OpenRouter entry takes one option,
`@reasoning=off|default|minimal|low|medium|high` (`off` is the default, `default` leaves it to the model). What the
model reads is numbered by log line, each row cut to 300 characters: an output of at most 600 lines, or one that takes
at most 96 KiB as the model reads it, goes whole, and no decider is asked unless the summary fails; a longer one goes
after the decider has chosen, as the excerpt of its picks: the first 30 and last 100 deduplicated entries and the
decider's picks rated 0.3 or more, best first (a pick mostly like what is already in waits for the rest, so a failure
repeated for every test class cannot fill it), each widened by 10 lines, up to 600 entries. It answers with a summary of
at most 3 sentences and up to 5 ranges of the log lines the agent
must read as printed (an error, a stack frame, a value the summary cites), or none when the summary answers the
description on its own; nib keeps the ranges inside the log, of at most 60 lines, that touch a line the model read; an
answer whose ranges all miss, not JSON, or failed falls to the next entry, and when every entry fails the decider's cut
stands with `no summary` in its note. A checked summary takes the place of the decider's cut: `<summary>...</summary>`
(its text on one line, without the model's name or any summary tags of its own), then the real log rows of every range
with 1 line around each, the ranges sharing 30 rows (a range that fits an even split of what is left shows whole; a
longer one drops its surrounding lines first, then shows its start and
`[nibash: log lines A-B left out, the rest of this passage]`), each row numbered as above, and a closing note. The
rows share 4 KiB as well: each is at most its share wide (80 to 240
characters), and a longer line, or one holding encoded data (a run of 100 or more base64 or hex characters mixing
letters and digits, with few separators), shows as a cut row naming the stretches it keeps, without the encoded runs, as
`[line 12, chars 1-60, 1061-1240 of 1300]|{"type":"thinking","signature":" ... "},...`; the log holds it whole.
Summaries are not asked for when the decider judged that the description
needs every line, nor for a successful run without a description. The prompt tells the model to state only what the
lines show and to leave counts to the shell. Answers are cached like decisions, checked again whenever used. On the 19
cut find cases of the research set (`research/decider-eval/` on the research branch), `google/gemini-3.1-flash-lite` and
`anthropic/claude-haiku-5.5` on the excerpt (`@reasoning=default`) covered every needed line in 18, in 1.4 and 3.1 s,
and `claude-p:haiku` in 17, in about 5 s; the offline check (`go test ./internal/nibash -run Captures -v`) measures
`deepseek/deepseek-v4.1-flash-1017` with reasoning off.
`opencode:deepseek/deepseek-flash` on OpenCode 2.0.30 answered in 2.8 to 4 s;
its token and cost counts are added when OpenCode prints them, which it does not always do before it exits.

**Tails.** `cmd | tail -N` at the model's end hands only N lines to the cut, far under any limit, so a failure printed
before them would cost the agent another call. With a decision model or a summarizer configured, when the lines a
`tail` of a real program's output left out are past the summary limits (150 lines, 12 KB, lower when they repeat) and
the call has a description or failed, the decider reads those lines, deduplicated, with the call's description, as it
would read a long output's middle. The tail's own output stays as it is. When the decider picks nothing (the usual case)
the note is the plain `[N earlier lines in LOG]`; when it picks some, the note becomes
`[N earlier lines in LOG; MODEL and keywords chose these of them for your description; N| is a row's log line:]`,
followed by those log lines with 3 lines around each, best first, up to 30 rows and 8 KB (a passage that does not fit
whole shows its start), each numbered with its log line. Picks over 30 rows, with a summarizer, become a `<summary>`
of the whole output and the lines it points at that the tail left out, under `[...; a summary of the whole output,
then the lines it points at that the tail left out; N| is a row's log line:]`. A decision model that fails adds
`not checked, since the decision model failed (...)` to the note.
The keyword decider alone never checks a tail, nor does nib check `head` (the program before it is closed at once, as
in bash, so what follows was never printed) or a `tail` of a file or of nib's own commands. The exit status a program
before the pipe returned is not known to nib (the pipeline's is `tail`'s), so only the call's own status counts.

**What the models read.** The decision model and the summary model read an output deduplicated: each line that occurs
more than once and each block that recurs is defined once, as `@L1 = TEXT` or `@B1 =` and its lines, and every
occurrence stays in place under its log line numbers: `1203-2372: @L1 x1170` for a run, `5001-5014: @B1` for a block.
Only a back-to-back run collapses copies that differ in their numbers (marked `[numbers vary]`), and not across a number
that tells lines apart, such as a status code that takes a few values often; elsewhere a copy with other numbers is
spelled out. Lines under 24 characters are not named, so short numbered lines stay as they are. The decision model's
windows are built over the deduplicated rows and each request carries the definitions its windows use; the summary's
600-line cap counts deduplicated lines. The agent still gets the real rows. On the recorded captures this took the
models' input from 190,254 lines (10.1 MB) to 119,701 entries (5.8 MB). A decision is up to 128 requests of 30 KB, 16 at
a time.

**Repetitive output and logs.** Output that shrinks more than 1.5 times under that deduplication gets a summary sooner:
the 150-line and 12 KB limits are divided by how much more it shrinks, never below 50 lines and 3 KB (every recorded
output under the limits shrinks 1.06 times or less, so none of them changes). And when nibash's own `cat`, `head`,
`tail`, `sed -n`, `grep` or `rg` read a log, they print it as plain program output, not `N:TEXT` rows, so deduplication,
the decision model and the summary apply to it: a log is a file named `*.log`, `*.out` (or `*.log.N`) or kept in a
`log`, `logs` or `test-logs` directory that git does not track, or a file of 100 lines or more that shrinks at least 2
times under deduplication (no tracked file of nib's own passes 1.42; a test checks every one). The commands still print
what the system programs would; only the form of the rows differs. `read` shows a log as anchored rows for editing, and
`skim` reads it as program output for a purpose ([skim](#skim-answers-from-long-files)).

| variable | default | meaning |
| --- | --- | --- |
| `NIB_DECIDER` | `keywords` | `fixed`, `keywords` or `openrouter`; anything else stops nib at startup |
| `NIB_DECIDER_KEY` | `$OPENROUTER_API_KEY` | the OpenRouter key; without either, nib reads `~/.config/nib/openrouter-key` (refused unless only you may read it), then the `openrouter` entry of OpenCode's `auth.json` (`$XDG_DATA_HOME/opencode/`); `openrouter` with no key found stops nib at startup, naming where it looked |
| `NIB_DECIDER_MODEL` | `typesafe/jev-1.14` | models of OpenRouter's Decisions endpoint, comma separated, in order of preference |
| `NIB_DECIDER_URL` | `https://openrouter.ai/api/alpha/decisions` | the endpoint |
| `NIB_DECIDER_TIMEOUT` | `30000` | milliseconds for the whole decision, every model of the list included |
| `NIB_DECIDER_SUMMARY` | unset | summarizers, comma separated, in order of preference (above); with `fixed`, or an OpenRouter model and no key, nib stops at startup |
| `NIB_DECIDER_SUMMARY_URL` | `https://openrouter.ai/api/v1/chat/completions` | the chat endpoint |
| `NIB_DECIDER_SUMMARY_TIMEOUT` | `30000` | milliseconds for the whole summary, every entry of the list included |
| `NIB_DECIDER_OPENCODE` | `opencode` | the OpenCode 2 program `opencode:` summarizers run |
| `NIB_DECIDER_CACHE` | unset | a directory for answers on disk, decisions and summaries |
| `NIB_DECIDER_SANDBOX` | unset | `1` allows `openrouter` under `NIB_SANDBOX=1`; without it, that combination stops nib at startup |
| `NIB_AUTO_NAMES` | unset | `0` turns off naming the directories agents keep seeing and keeping names for the next session ([named directories](#named-directories)) |

**Choosing once for every nib.** `nib setup --decider KIND` (`fixed`, `keywords`, or `openrouter:MODEL,...` for decision
models in order of preference), `nib setup --summary LIST` and `nib setup --opencode PROGRAM` remember `NIB_DECIDER`,
`NIB_DECIDER_MODEL`, `NIB_DECIDER_SUMMARY` and `NIB_DECIDER_OPENCODE` in `~/.config/nib/setup.json`, beside the
providers; every nib reads them when it starts, unless its own environment sets one of the four, which then replaces
the remembered choice whole. `--decider=`, `--summary=` and `--opencode=` forget them. A choice nib would refuse to
start with is not remembered, and the key never is: nib finds it
as above. Since claude-capture and zenit-workspace's bootstrap run `nib setup` anyway, the choice made once on a machine
holds for every Claude Code profile and every OpenCode session there.

**Logs other tools keep.** A build or test wrapper that saves its program's output can hand the agent what nibash would
have shown of it, so the agent needs no second call to find the cause:

```sh
nib shape --log PATH --purpose TEXT [--exit N] [--command TEXT]
```

prints to stdout what nibash shows at the model's end of a program run as `--command` (default `cat PATH`), with the
call's description `--purpose`, that ended with status `--exit` (default 0) and whose whole output is the file: short
output whole, long output cut by the same deciders, or a `<summary>` and the lines it points at, with the same
configuration as the server. Its notes name `PATH` as the whole output, no copy is made, and no `[exit N]` line is
printed, since the wrapper states its own verdict. Rows are plain, never anchors. Bad arguments and an unreadable log
exit 2 with one line on stderr; a decider choice nib would refuse to start with exits 1.

When `nib shape` runs in the same Claude Code session as a nib server, as zenit-dev and nib both do, or under a program
nibash started, it asks that server, through a socket in nib's private runtime folder named after the session
(`CLAUDE_CODE_SESSION_ID`, which every program of the session sees) or named to nibash's programs in
`NIB_SHAPE_SOCKET`, and prints its answer. The log is then shaped with the session's state: `/nibash summaries off`
applies, the mod's band shows a `shape` card naming the log while it runs, and `/tokens` counts it under `nib shape`.
What it showed went into another tool's result, and nothing tells which agent got that, so it counts as no agent's
skim: a later `skim` of the same log leaves those lines out in one row that names when and how to read them
(`[skim: lines 1-100, 204-403 left out, which a tool's result showed through nib shape at 14:16; read
build.log:1-100,204-403 if you lack them]`). Under `NIB_SANDBOX=1` a log outside the roots is refused with exit 2. With
no such server, or one that cannot be asked, `nib shape` shapes the log itself, the same way.

**Privacy.** `NIB_DECIDER=openrouter` sends program output to OpenRouter: the command as written, the `description`, the
exit status, the lines of a file `skim` reads and the rows between the kept start and end of each output long
enough to be cut. That is whatever the program printed, so a long `env`, `printenv` or `sort .env` sends the secrets in
it; setting the variable is consent to that. A command checked against [vague rules](#command-rules) sends the command
as written, its `description`, its directory and those rules. Otherwise file rows of nib's own commands, the log path,
the working directory and anything the command did not print are never sent. Every request asks for zero data retention
and no data collection
(`provider: {"zdr": true, "data_collection": "deny"}`); OpenRouter's own prompt logging is an account setting nib cannot
override, so check it in the [privacy settings](https://openrouter.ai/settings/privacy).

#### Command rules

A repository can forbid commands in its `nib.json`, for agents that should use a project's own tools instead of running
builds and tests through nibash:

```json
{"rules": [
  {"program": "zenit-dev", "args": "^(build|test)( |$)", "why": "builds and tests go through the zd_build / zd_test tools"},
  {"program": ["gradlew", "gradle"], "why": "Gradle runs through zenit-dev's MCP tools"},
  {"rule": "Do not run full test suites or long builds directly.", "match": "gradle|test", "why": "Use the zenit-dev MCP tools."}
]}
```

Before a call runs, nibash finds every command it may run: in pipelines, lists, loops, subshells and `$(...)`, behind
`env`, `timeout`, `nice`, `xargs` and similar wrappers, and inside `bash -c` and `eval` strings it can read. A program
matches by its base name, so `~/tools/zenit-dev` is `zenit-dev`; `args`, a regular expression, is matched against its
arguments joined by spaces, a word nibash cannot know before running (`$cmd`) standing as written. An exact rule
(`program`) refuses a matching call always. A vague rule (`rule`) is judged by the decision model
(`NIB_DECIDER=openrouter`) against the whole command and its `description`, only when its `match`, a regular expression
over the command as written, finds something (a rule without one is always asked; a call no vague rule matches asks no
model at all), and only a score of 0.7 or more refuses; its answers are cached, so the same command
asks once. A refused call runs nothing, background calls included, and prints one line per broken rule,
`[nothing ran: zenit-dev build is against a rule in /path/nib.json: builds and tests go through the zd_build / zd_test tools]`,
then `[exit 126]`; the agent cannot override it. When the refused call reads one of nibash's saved logs, as an argument
or through `<`, one more line before `[exit 126]` names them all: `[skim LOG with an expect answers this without a pipe]`.
Without a decision model, or when it fails, a vague rule is not checked: the command runs, and the agent is told so
once per file and reason. The rules that apply are those of the
nearest `nib.json` holding rules at or above the call's directory and at or above every directory the call `cd`s into by
a literal path; they apply to the whole call. Under `NIB_SANDBOX=1` only a `nib.json` inside the roots counts. A
`nib.json` whose rules cannot be read refuses every call under it, naming the file and the error.

### Named directories

Long paths cost tokens in every row and header, so nib names deep directories: `~orm/workflow/Repo.java` stands for
`zenit/src/server/java/be/elevenways/zenit/server/orm/workflow/Repo.java`. Every path nib prints uses the named directory
that prints it shortest, but only when that is shorter than the path relative to the server's directory (or whole,
outside it), and every tool and patch header accepts the short form. In nibash it is ordinary tilde expansion,
so it works only unquoted, and programs receive the real path; the names never reach a program's environment. A shell
knows no names, so a script nibash hands one with `-c` (`bash -c`, `sh -c`, also behind `timeout`, `env`, `nice`,
`nohup`, `stdbuf`, `setsid` or `xargs`, and in `find -exec`) gets them expanded before it starts, where nibash would
expand them in that script and nowhere else, also when the script is made at run time (`bash -c "$(declare -f f); f"`).

Names come from `nib.json` in the server's directory (`{"directories": {"orm": "path/to/orm"}}`) and, unless it sets
`"autoDirectories": false`, from JVM source roots: each `src/<set>/java` or `src/<set>/kotlin` below the server's
directory names its package prefix after its module, plus `-<set>` unless the set is `main` (`~zenit-server`,
`~protoblast-test`). Only prefixes of 24 or more characters get a name, at most 16 automatically; a name that is also a
user on the machine is skipped. The server instructions end with the table, and a name that does not fit there does not
resolve; with nibash offered, they close with one line on `hash -d`, also when there are no names, so the agent knows it
can make one.

An agent can name a directory itself in nibash, with zsh's `hash -d name=dir`: `dir` is taken from the shell's
directory, may start with `~/` or a `~name/` the agent already has, must exist, and is kept canonical; nibash confirms
with `[~name now names /abs/dir]`. Bare `hash -d` lists every name the agent has as `name=dir`. A name follows the
table's rules (lowercase letters, digits and dashes, starting with a letter; not a user on the machine), may not
redefine a name of the table, and must lie inside the roots under `NIB_SANDBOX=1`; redefining one's own name replaces
it. A name defined this way works for the agent that defined it, in every tool and in nibash, including display, and for
the subagents and forks it spawns afterwards; never for its parent, its siblings, or agents it spawned before.
"Afterwards" is the spawn moment the hook finds for a Claude Code subagent ([the hook](#the-hook)) or OpenCode records
for a session; when the moment is unknown, a subagent gets the names its parent had at the subagent's first nib call,
and when its parent is unknown, none. Names live in nib's process, at most 64 definitions per agent, and do not appear
in the server instructions of the session that defined them.

Agents seldom name directories themselves, so nib names the ones they keep seeing. Every path nib prints to an agent,
and every absolute path a nibash command line spells (one handed to a real program, which nib never prints, costs the
same characters each time it is typed; a path inside quoted code counts up to where it ends, a path joined to an
expansion such as `$HOME/x` does not count, and a typed path counts from its deepest directory that exists), counts
toward each directory it runs through, below the deepest named directory holding it (and below the server's directory,
for a path inside it). Among the directories whose prefix, as nib would print it, is 30 characters or more, nib names
the one that saves the most once a name is worth it: when the characters the name would have saved over the paths so far
(at least 3 of them) pay for the note that announces it, as after 6 paths for a 41-character prefix, or for a prefix of
100 characters or more, too long to print even twice (a skill's files under Claude Code's
`/tmp/claude-1000/bundled-skills/2.1.302/<hash>/...`, say), in the second call that shows or spells it, so a build copy
seen once costs no note. Such a prefix is named the first time instead when the agent works in the directory (a nibash
`cd` into it or below it, or an edit or write of a file there, a move's or copy's destination included) or an earlier
server of the same directory gave agents a name for it, unless git's ignore rules cover it (build output); without git
nothing counts as ignored. nib makes the name from the first letters of the two folders
above it that say something, outer first, and its own folder, or that folder's last word when its name is over 10
characters: `~bewidgets` for `.../java/be/elevenways/widgets`, `~bptypes` for
`bundled-skills/2.1.302/<hash>/plugin-authoring/types`, `~cpnib` for `claude/projects/-home-skerit-projects-nib`, as a
hash or a version says nothing and is passed over. A name that a directory named the same way already holds goes to the
other directory with the folder that sets it apart in front, cut to 6 characters: `~test-quirkyq` for
`src/test/java/be/elevenways/quirkyquack` beside `src/main/...`'s `~bequirkyquac`; a name taken otherwise gets a number
after it, and no name nib makes is over 12 characters. The name holds at once: `read`, `grep`, `find` and hosted
tools' locations count every path a response will print before printing any, so all of them print under a name they
earn, and a listing names each folder
once; nibash, whose output streams, prints its later paths under it. One note, which names the directory whole, opens
the response:
`[nib named ~bewidgets = /home/me/zenit/src/server/java/be/elevenways/widgets, which 6 of the paths shown to you ran through (or, named at once, `too
long to print again`); ~bewidgets/ works in every tool and in nibash, for you and the subagents you start]`. Only an
agent's first such note is told in full, and the first again once 25 more calls have shown it paths; the others are
brief: `[~bewidgets = /home/me/zenit/src/server/java/be/elevenways/widgets, 6 paths shown]`. Such a name behaves as one
the agent defined with `hash -d`, at most 8 per agent. When nib learns that an agent's context was compacted or
cleared (see held rows below), the notes went with it: the first path printed under a name the agent got before then,
its own, nib's or inherited, tells of it again
(`[~bewidgets = /home/me/zenit/src/server/java/be/elevenways/widgets, a name you got before your context was compacted or cleared; ...]`
in full for the first, `[~bewidgets = ...]` after), and a subagent, whose first call's greeting listed its names, is
greeted again; the server's own names are in the instructions, which a compaction keeps. Both kinds are kept for later
sessions of the same server directory, the 16 used last, in `~/.local/state/nib/names/` (`$XDG_STATE_HOME` when set),
and a server starts with a name once two sessions made it, defined it or printed a path under it, as a folder one session
used (a transcript's, a test run's logs) is seldom used again and every name in the instructions costs every session; a
later session that names the folder again gets the name it had. The next server then names them from the start, so they
are in its instructions with the table's names. Left out, and not kept, are a name a directory or another name already
holds, one that prints nothing shorter from that directory (a name made while an agent worked in a worktree for a folder
of the server's own), one for a folder that is gone, in the temporary folder (unless the server's directory is too) or in
another worktree of the server's repository, and one no session used for 30 days. `NIB_AUTO_NAMES=0` turns naming and
keeping off; without a home folder they are off too. Bare `hash -d` in nibash lists every name an agent has, the
table's, its own and nib's.

A message the user sends reaches the agent with no notes of nib's, so a handoff written after a `/clear` that goes on
using `~bewidgets/...` would name a directory the agent no longer knows. nib's hook (see [the hook](#the-hook)) finds
every `~name` in the message and asks the session's server which of them the main agent must be told, and Claude Code
adds the answer to the message as context:
`nib's named directories in this message, which work in every nib tool and in nibash:` and a line `~name = directory`
for each. Told are a name the agent got before its context was last compacted or cleared (nib's later notes then do not
tell it again), and a name the agent does not see that another agent of the same server has, or that an earlier session
of the same server directory used (the kept names above, at any count), which then works for the agent as one it
defined. A name the instructions list, or one the agent holds, is told only when an earlier session had it for another
directory, which the line names (`(in an earlier session ~w was ...)`); a name nothing knows adds nothing. Only the
names leave the hook, never the message.

Subagents are not given the server instructions (Claude Code's are not), so a subagent's first nib call starts with what
the main agent read there: the names it has, the table's and the ones it inherited, one per line, and the `hash -d`
line. A subagent working in a worktree is told that first (see [the hook](#the-hook)). Only the first call carries it,
and only for subagents: a Claude Code subagent the hook names, or an OpenCode child or forked session.

### Snapshots, seen lines and stale tags

The server keeps an in-memory snapshot store for its process: up to 4 versions per path, 4096 agent and file pairs and
64 MiB of text, each distinct text held and counted once however many agents hold it, evicting the least recently used
pair; a pair whose file the agent wrote lines of goes to the back once first, so an orchestrator's own files outlast what
its subagents only read since. Evicting one drops what that agent saw of the file, as a restart
does; nib remembers that it did, and an edit then refused for lines read before says so instead of calling them never
displayed. The main agent's calls share one session in it; each Claude Code subagent that nib's hook names, and each
OpenCode session, gets its own, so lines a subagent saw never let the main agent, or another subagent, edit them, and
the other way around; a fork, Claude Code's or OpenCode's, starts with a copy of the lines its parent had seen when it
was forked (see [the hook](#the-hook)). Reads, greps, nibash's own commands, edits and writes record the version they
produced plus the lines it showed in full, and when each line was first shown; `info` reads record the version with no
line shown, and `path#TAG` reads record it like an edit response.

- **Seen lines.** An edit may anchor on lines its tagged output showed in full, or as `N~TEXT` rows. Otherwise it is
  rejected with the actual content of up to 40 such lines plus 2 lines around each (`*` marks the anchors); all of those
  rows are then marked seen, so an unchanged retry or the `REAPPLY` token succeeds, and a miscount can be fixed from the
  rows around it. An anchor the agent saw only cut prints whole there when it fits (up to 4096 characters a line and 16
  KiB in all), so the retry applies too; a longer one, or one with color codes, gets a cut preview and a token that
  applies once it was read whole. After an edit, only lines that kept both number and content stay displayed, plus the
  rows shown in the response. Moved lines are known, not seen, at their new numbers, so a hunk anchored on one needs a
  `~TEXT` check; without one, the refusal names them as lines that moved since the agent saw or wrote them, and an
  unchanged resend applies. A new version made any other way, by a program the agent ran, another agent or a formatter,
  keeps the same rule against the version before it: lines the change left at their number stay seen, moved ones stay
  known. A replacement on a moved line is still accepted when the new content carries that line's current text (at
  least 8 characters, unique in the file), a port of oh-my-pi's shifted-anchor rescue. A file with no snapshot (for
  example after a restart) is not restricted. Every line of a file that `write` or `*** Add File:` wrote whole counts as
  seen, since the agent wrote it; the response's row map only helps it find lines.
- **Across restarts.** Where nib can tell when an agent's context drops (nib's hook for Claude Code, OpenCode's
  database), what each agent saw is saved in `~/.local/state/nib/seen/` (`$XDG_STATE_HOME`): one private file per
  Claude Code session, subagent or OpenCode session, holding each file's newest version as a hash of its text, never the
  text, with the lines seen and known in it and when. A server started later, for a resumed session say, restores them
  for a file whose text still hashes the same, leaving out what a drop since took; a changed file needs reading again.
  Files untouched for 7 days are removed.
- **Known lines and checks.** nib also remembers lines whose whole content the agent knows without having seen their
  current number: every row it inserted and seen lines that an edit moved. A `~TEXT` check confirms a line's number. An
  insert may anchor on any line a check confirms; a replace or cut may span known lines when each end that was not
  displayed is confirmed (a block's end, which nib finds itself, needs no check). Lines the agent neither saw nor wrote
  stay refused for replacement, check or not.
- **Held markers.** Where nib can tell when an agent's context is dropped (below), a run of 3 or more rows that agent
  was shown whole before, in this version or one it still holds, and that have not changed since, prints shorter. A run
  of up to 30 lines in `read`, in `grep`'s context rows and in nibash's own file rows prints as row-map rows under the
  lines' current numbers, each line's indentation and start: `41~    return row`. A longer run prints as one
  row: `[lines 40-120 unchanged since you saw them]`, or `[lines 40-120 = your earlier 37-117, unchanged]` when an edit
  moved them. When nib's own edit or write tools last wrote the file after the lines were seen, the marker says so,
  `[lines 40-120 unchanged since you saw them, before the last edit of this file]`, or names the lines that were
  (`..., 42-120 of them before the last edit of this file]`): an agent reading after an edit it expected there would
  otherwise take the marker for the edit's result. A marker or an `N~` row means the line is as when it was last seen
  whole, so an edit expected there did not happen, as the lesson says.
  Lines the agent knows only the content of, shown by a plain read or seen before an edit moved them, print
  as row-map rows whatever the run's length, since it never had their numbers (the import fold still wins over them,
  being shorter). A grep match row always prints, since it is what the search is for. The lines a marker or a row-map row
  stands for stay seen at their current numbers, so an edit anchors on them with no read. The first marker an agent gets
  ends with a line saying what it means; the same call sent again straight away, or a read with `full: true`, prints the
  rows. Edit responses are unchanged: their kept rows are one on each side of a change, never a run of 3, and the agent's
  own new rows print as their `[lines A-B as you sent them]` marker.
- **Dropped contexts.** When Claude Code compacts an agent's conversation, or a session starts anew (`/clear`, a
  resume), the agent no longer has what it was shown: every line it saw or knew before that moment stops counting, for
  markers and for edits, which then need the lines shown again (the usual refusal, and an unchanged resend), and the
  lessons nib teaches once come again. Claude Code tells nib's hook (below); OpenCode 2 marks a compaction in its
  database. A client that tells neither, or a Claude Code session whose hooks an older setup registered, never gets
  markers, and its edits work as before.
- **Stale tags.** When the file no longer matches the tag, the edit is rebased only if every anchored line maps to an
  unchanged current line with a neighbor that moved with it, and each hunk's lines moved by one offset of their own
  (lines inserted inside a replaced range refuse it); rebased edits carry a warning. Otherwise the edit is rejected
  with the current tag and the current content around the requested lines. So after an edit, an agent may keep using
  an older output's numbers with that output's tag, as long as nib still holds that version (the latest 4 per file).
  A tag never issued for the file tells nib nothing about what the agent saw, so it is rejected, naming the file it was
  issued for, unless every hunk is safe without one: `SUB` and matching `-` rows check the text they replace against
  the current file, and `PUT >$:` only appends (but not under a tag issued for another file, a likely mix-up). Such a
  section applies with a warning never to invent tags, and lines the agent never read stay unseen. A `[PATH]` header
  without a tag appends with no tag at all. A 4-hex tag can collide: if a retained version with that tag has different
  text than the current file, the file is treated as changed.
- **Not ported from oh-my-pi:** cut/paste registers, the syntax-tree check on rebases, the repeated no-op guard, and
  tree-sitter read summaries (nib's outline is indentation-based instead).

### Writes outside the workspace

By default nib writes anywhere the process may, without asking. Set `NIB_CONFIRM_OUTSIDE_ROOTS=1` to confine writes,
creates, moves and deletes to the client's MCP roots, as reported through `roots/list` (the server's working directory
when the client reports no roots); the list is refreshed when the client sends `roots/list_changed`, and when a refresh
fails the last list that worked stays in force (with none yet, the sandbox allows nothing rather than falling back to
the server's directory). With it set, anything else asks the user through MCP elicitation: allow that file once, or
everything under its directory for the rest of the session. Clients on MCP 2026-07-28 or later get both the roots and
these questions as input requests, which they answer before sending the call again; older clients are asked directly.
Without elicitation support such writes are refused. The requested path is checked before any parent directory is
created, and the canonical path again, so a symlink inside the workspace cannot redirect a write outside it. Reads are
not restricted.

Set `NIB_SANDBOX=1` to keep every tool inside those same roots, reads included: `read`, `grep`, `find`, `edit` and
`write` refuse any path that resolves outside them, and nibash confines itself as described above. Symlinks are
followed, so a link inside a root cannot lead out, and a path that does not exist yet is judged by its nearest existing
parent. A refused path in a `read`, `grep` or `find` is noted in its place, like a missing one, and the other paths are
still served; an edit or write that names one fails as a whole, before any parent directory is created. ripgrep does not
follow links while it walks a root, so a search never reaches past one either. The benchmark runs nib this way.

### Providers

nib can host other programs and offer their tools as its own, so the client configures one MCP server.
`nib --provider NAME=PROGRAM` (repeatable) runs `PROGRAM nib-provider`, an MCP server on stdio, in nib's directory,
lists its tools at startup and offers each as `NAME_TOOL`: zemble's `search` becomes `zemble_search`. A provider never
prints file lines itself: its answer is prose plus locations (a file and a line range), and nib prints each location as
`read` prints that range of the file as it is now, so its `N:TEXT` rows are real lines that count as seen for the agent
that called, and edit anchors on them as on a read. Locations of one file in a row print as one block, prose and rows
share read's 40 KiB budget, prose shaped like rows follows a note that it is not anchors, and a location whose lines
changed since the provider indexed them is followed to where they are now or noted. An answer nib cannot read (another
format version, an unknown key) shows as the provider's plain text. The server instructions name the hosted tools. A
tool whose name or description nib cannot offer is left out with a note on stderr; a provider that fails to start within
30 seconds is left out as a whole; one whose process ended starts again on the next call, and a call it was answering
then fails without being sent again. Under `NIB_SANDBOX=1` providers are off unless `NIB_PROVIDERS_SANDBOX=1` is set
too, since a provider runs outside the sandbox (nib's rows for it are still confined). The protocol is in
`docs/design-providers.md`; zemble implements it.

## Setup

Install nib with Go, then register it in a project (`rg` must be on `PATH`):

```sh
GOPRIVATE='github.com/11ways/*' go install github.com/11ways/nib/cmd/nib@latest
nib setup              # in the project folder; --dry-run previews, --user registers it for every project
```

`nib setup` adds only what is missing, so an entry someone changed always wins, and running it again changes nothing.
The exception is a nib server entry that does not start the nib running setup, by any path to it (another program or
other arguments, or a program that is gone, such as an older nib's Bun entry point): setup points it at this nib,
keeping its other keys, and says which command it replaced.

`nib setup --provider NAME=PROGRAM` adds `--provider NAME=PROGRAM` to nib's server entries and takes out what it
replaces: a `NAME` server in `.mcp.json` or `opencode.json`, and Claude Code's local-scope `NAME` registration for the
folder (with `--user`, the user-scope one and every folder's). A user-scope `NAME` registration that a project setup
leaves in place is named in the report. `NAME=` stops hosting it, and a setup without `--provider` keeps what an entry
hosts. The providers asked for are remembered in `~/.config/nib/setup.json` (in `$XDG_CONFIG_HOME` when set), so a later
`nib setup` without `--provider`, as a script runs it for a new project or a new Claude Code config folder, hosts them
too. A relative program path is made absolute; a bare name is looked up on `PATH` when nib starts.

- `.mcp.json`: the `nib` server.
- `.claude/settings.local.json`: the approval of that server in `enabledMcpjsonServers` (one listed in
  `disabledMcpjsonServers` stays off, and setup says so), nib's Claude Code hook (below) for `Read` and for nib's own
  tools (`mcp__nib__.*`), for `SessionStart` and `PostCompact`, which tell nib when an agent's context is dropped, and for
  `UserPromptSubmit`, which tells the agent what the named directories in a message the user sends stand for,
  and `permissions.deny` for `Bash`, `Grep`, `Glob`, `Edit` and `Write`, with `permissions.allow` for
  `mcp__nib__nibash_status`, which only reads what nib runs and which the nibash mod (below) asks every second while
  something runs; a `MultiEdit` deny rule an older setup wrote is removed, since Claude Code no longer has that tool and
  warns about the rule. A bare tool name there removes the tool from the model's context, also in bypass-permissions
  mode, and deny lists from several settings files add up.
- `opencode.json`, when the project has one, in OpenCode 2's form: the `nib` server under `mcp.servers`, with
  `codemode: false` so OpenCode offers nib's tools directly rather than only inside code mode scripts, and deny rules in
  `permissions` for `shell`, `read`, `grep`, `glob` and `edit` (which covers writing and patching; a tool denied
  outright is not offered at all). An action any rule there already names, in `permissions` or in OpenCode 1's
  `permission` or `tools`, which OpenCode 2 still reads, keeps its setting, and setup says so; a nib server in
  `mcp.servers` without `codemode` gets it set to `false`. A `nib` entry in OpenCode 1's form (`mcp.nib`, as an older
  setup wrote it) moves to `mcp.servers.nib`, converted as OpenCode 2 converts one; setup reports the move. No allow-all
  rule is added, so OpenCode asks about nib's tools as it asks about any other unless the config allows them.

Denying `Bash` has side effects: a skill's `!` command lines fail Claude Code's permission check, and subagents inherit
the deny; nibash is their shell. With `--user` setup puts the hook and the deny rules in the Claude config folder's
`settings.json` (`$CLAUDE_CONFIG_DIR`, else `~/.claude`), registers the server through `claude mcp add --scope user`,
since Claude Code rewrites the file that holds it while it runs (a stale user-scope registration, or a stale local-scope
one for any project folder, which would win over it there, is replaced at its own scope with `claude mcp remove` and
`claude mcp add-json`, in that folder for a local one; a plain `nib setup` does the same for the folder's local-scope
one), and tops up OpenCode's user `opencode.json` (in `$OPENCODE_CONFIG_DIR`, else `~/.config/opencode`) when there is
one. The command it writes is `nib` when the `nib` on `PATH` is the copy running setup, else that binary's path. A file
that is not plain JSON (with comments, say) is left untouched and named; a file setup changes keeps its key order, its
numbers as written, its mode and, when it is a link, its link.

To update from the remote repository, run the `go install` again. To build and install the current checkout, use the
included script:

```sh
./install.sh                       # install to ~/.local/bin
GOBIN="$HOME/bin" ./install.sh      # or choose another absolute directory
```

The script requires Go, works from any working directory, and defaults to `~/.local/bin` unless `GOBIN` is set in the
environment. It prints the installed version and tells you if another copy on `PATH` would take precedence. Restart the
client's nib MCP server after updating so it launches the new binary.

The equivalent command from the checkout root is `GOBIN="$HOME/.local/bin" go install ./cmd/nib`. Without an explicit
`GOBIN`, plain `go install` uses `go env GOBIN`, or `$(go env GOPATH)/bin` when that is empty.

The CLI and MCP server report the release version (`v0.2.0`, defined in `cmd/nib/main.go`), plus the short commit when
available and `+dirty` for a modified checkout. Publishing a Go module release also requires a matching Git tag pushed
to the remote; only then can `go install github.com/11ways/nib/cmd/nib@v0.2.0` select that release.

Any other MCP client that launches stdio servers can run nib too: the command is `nib`, started in the project directory
(or with `NIB_DIRECTORY` set to it). Relative paths resolve against that directory. The tools are `read`, `grep`,
`find`, `edit`, `write` and `nibash`; the server also sends usage instructions in its MCP `instructions` field.
`nib --version` prints the version.

### Platforms

nib builds and runs on Linux, macOS, the BSDs (FreeBSD, NetBSD, OpenBSD) and Windows. Each mechanism an OS provides its
own way sits in `internal/platform`, with one implementation for Unix and one for Windows, and nothing reads `/proc`.
What differs:

- **Folders.** nib keeps its files where Claude Code and OpenCode keep theirs, under the home folder on every OS
  (`USERPROFILE` on Windows): settings in `~/.config/nib`, learned names and seen lines in `~/.local/state/nib`, the mod
  in `~/.local/share/nib`, the hook's state in `~/.cache/nib/hook`. The `XDG_*` variables move them wherever they are
  set. Sockets and logs go to `$XDG_RUNTIME_DIR/nib`, else a folder of the user's own under the temporary folder.
- **Pinned writes.** An edit's target directory is opened as an `os.Root`, so staging, the rename or exclusive link
  and the checks all happen in that directory even if the path to it changes. A move into the trash renames from the
  open directory on Unix; on Windows, which has no rename relative to an open directory, by path right after the
  directory is checked again. Windows cannot always replace a file that is open, so nib closes its own handle on the
  target just before the rename. Owners are preserved where the OS has POSIX owners; on Windows a read-only file is
  not replaced.
- **Private folders.** A folder counts as private on Unix when the user owns it and group and others have no access,
  on Windows when its owner is the user's security identifier.
- **Trash.** Linux and the BSDs use the freedesktop.org trash. macOS moves deletions to `~/.Trash`, where Finder shows
  them, with the `.trashinfo` records in `~/.local/share/nib/trash-info` (with `XDG_DATA_HOME` set, the freedesktop
  trash under it). Windows uses a trash in the freedesktop layout at `~/.local/share/Trash`, since the Recycle Bin
  cannot be filled and emptied entry by entry reliably from outside the shell. `trash list` and `trash restore` work
  the same everywhere, and a file on another filesystem than the trash is copied in, checked and then removed.
- **Processes.** A program runs in its own process group on Unix. On Windows it starts suspended, joins a job object
  of its own and then runs, so a timeout or `kill` ends everything it started. Windows has no signals: `kill` knows
  `HUP`, `INT`, `QUIT`, `KILL` and `TERM` there, each ending the job with the status a shell gives (128 plus the
  signal's number).
- **Sandbox.** `NIB_SANDBOX=1` runs nibash's programs in bwrap, which exists on Linux only; elsewhere nib refuses to
  start with it unless `NIB_SHELL=0` turns nibash off.
- **nibash on Windows** runs the programs on `PATH`, so the commands agents use (`git`, `rg`, coreutils) come from an
  install such as Git for Windows. Process substitution needs FIFOs and is Unix only, and the files a command names are
  tracked by Unix paths only. nib prints paths with forward slashes on every OS, which Windows accepts too, so a
  `~name/` path or a header path an agent sends back resolves.

### The hook

`nib hook` is a Claude Code `PreToolUse` hook for the built-in `Read`, the one built-in file tool setup leaves on, for
PDFs and notebooks. For any other file it refuses the call, naming the nib read that does the same (with `plain: true`
for a log or output file, a file in a `log` or `logs` folder that is not code or config, or a file over 2 MiB, which nib
cannot tag):

```
nib's hook refused Read: nib replaces it with fewer tokens. Use instead:
mcp__nib__read {"paths":["app.log:100+50"],"plain":true}
(If you need Read itself here, or nib's tools are missing from this session, send the same call again and it runs.)
```

The hook refuses a call once per session, so a session where nib's server is missing only costs one refused call. A call
is the same by its arguments, whatever their order. The short hashes it keeps live in `$XDG_RUNTIME_DIR/nib-hook/`, else
`~/.cache/nib/hook/`; a folder that is not the user's own and private, or a state file that is a link, turns the hook's
refusals off instead of being trusted. Every other tool passes untouched, so an older setup whose hook matcher still
names `Bash` or `Grep` keeps working.

For nib's own tools the hook never refuses or changes a call; it only reports which agent makes it. Claude Code sends
nib a call's `tool_use` id (in `_meta`) but not the agent behind it, while the hook's input names a subagent
(`agent_id`). So for a subagent's call the hook writes a record named by the `tool_use` id, holding the agent's id, its
parent, spawn moment and worktree when it can tell them (below), into `agents/` under that same private state folder
(written whole, then renamed into place), and the server takes the record, and deletes it, when the call arrives. That
subagent then has its own seen lines, `REAPPLY` tokens, named directories and nibash jobs; a call paused for input keeps
the agent of its first round. Records whose call never came (the user refused it, say) are removed after 24 hours. The
hook also notes, in `contexts/`, every `SessionStart` (a new conversation, `/clear`, a resume, or a compaction) for the
session's main agent and every `PostCompact` for the agent it reports (`agent_id` for a subagent), the moment it runs;
and for every nib call of a session whose start it saw, it files the latest such moment of the calling agent in
`drops/`, under the `tool_use` id, which the server takes like an agent record. A fork's conversation is its parent's as
it was at the spawn, so the parent's latest drop before the spawn moment counts for the fork too (without a moment, the
parent's latest at all, which can only drop more). A call with no drop record, from a session whose start the hook never
saw, gets no markers. Without a record, because the hook is not installed, Claude Code stops sending these fields, or
the client is not Claude Code, every call is the main agent's, as before. A subagent's jobs end with the connection,
never sooner. A subagent with no version of a file is not restricted, like any session after a restart, so a tag passed
to it in its prompt edits as it would after a restart.

For a message the user sends (`UserPromptSubmit`), the hook takes every `~name` the message uses, a `~` that starts no
word or path followed by a name, and asks the session's server, through the socket `nib shape` asks, which of them the
main agent must be told (see named directories above), with the latest moment the session's hooks filed for its context,
which the server applies at once rather than at the next nib call. A `/clear` gives the session a new id that hooks and
commands see, while its MCP servers keep the one they started with, so a server under Claude Code also listens on a
socket named after its parent, Claude Code's own process, which hooks see as `CLAUDE_PID`; the hook asks the session's
socket first, then that one. With no server answering within 3 seconds, the message goes on with nothing added.

A subagent Claude Code spawned in a git worktree (the Agent tool's `isolation: "worktree"`; its meta file, below, holds
`spawnedWithWorktree` and `worktreePath`) works in that worktree, as Claude Code's own tools do for it: its relative
paths in every nib tool resolve there, nibash calls start there, paths print relative to it, and the table's names that
lie in the server's directory name the same place in the worktree (one the worktree lacks is left out, rather than
naming the main checkout's copy). Names an agent defined keep the directory it gave. Its first nib call says so:
`You work in your worktree /abs/path: relative paths in every nib tool mean it, and nibash calls start there.` A
worktree that is not a directory when the hook runs leaves the subagent in the server's directory.

Hook input does not name a subagent's parent; Claude Code keeps it in the meta file next to the subagent's transcript
(`<transcript without .jsonl>/subagents/agent-<agent_id>.meta.json`: `parentAgentId`, absent for a subagent of the main
agent, `isFork`, and `toolUseId`, the parent's Agent call). The hook reads it, and the spawn moment from the parent's
transcript (the main transcript, or `subagents/agent-<parent>.jsonl`): the subagent was given the parent's conversation
up to the entry just before the assistant message holding that Agent call, so the moment is one millisecond past the
latest timestamp of the transcript lines up to that entry. Every call whose result is in that conversation was stamped
before, and every later call, one in the same message as the Agent call included, started after the request that
produced the message. The hook keeps each subagent's moment in `spawns/` so it reads the transcript once. Neither file
is a documented interface: when the meta file is missing or reads in a way the hook does not expect, the subagent starts
on its own, and when the moment cannot be read, nib takes the parent's state at the subagent's first nib call instead,
which can include lines and names the parent got after the spawn.

A fork (Claude Code's Agent tool with `subagent_type: "fork"`, which Claude Code 2.1.301 offers in `-p` mode only with
`CLAUDE_CODE_FORK_SUBAGENT=1`) starts with its parent's conversation, so it has seen what its parent saw. On the first
call nib sees from a fork, the fork gets a copy of the lines its parent had seen before the spawn moment, in the
versions it saw them, but none of its jobs. It is a copy, not a share: later reads count for one of them only. `REAPPLY`
tokens stay the parent's, since each applies once. A version the parent no longer holds (4 per path) cannot be copied.

OpenCode 2 names the session making each call itself (`_meta["ai.opencode/sessionID"]`), and a subagent runs in a child
session with an id of its own, so nib keeps seen lines, `REAPPLY` tokens, named directories and nibash jobs per OpenCode
session without a hook; calls without that field share the main state as before. On a session's first call nib looks it
up, read-only, in OpenCode's own database, which it finds where OpenCode does, by the environment OpenCode starts it
with (`$OPENCODE_DB`, else `opencode.db` and then `opencode-*.db` in `$XDG_DATA_HOME/opencode` or
`~/.local/share/opencode`). A child session (`parent_id`) starts with only its prompt, so it gets only the named
directories its parent had when the child session was created. A forked session (`/fork`, `opencode run --fork`,
`fork_session_id` and `fork_boundary`) holds a copy of its parent's settled messages up to the boundary, which keep
their times, so it starts like a Claude Code fork, with the parent's lines and names from before one millisecond past
the latest completion among those messages; a parent that never called nib is looked up and started first. A session the
database does not hold, or a database nib cannot read, starts on its own. On every call nib also reads when OpenCode
last compacted the session: the latest `compaction` message in `session_message`, which OpenCode appends when the
compaction ends, and which a fork holds a copy of when its parent compacted before the boundary. Lines shown before one
millisecond past it stop counting. A session no database holds in OpenCode 2's tables gets no markers. A revert, which
also takes messages out of the conversation, is not tracked.

### Using with Claude Code

By hand instead of with `nib setup`, register the server for every project, with its tools loaded up front instead of
through tool search:

```sh
claude mcp add-json --scope user nib '{"type":"stdio","command":"nib","alwaysLoad":true}'
```

Claude Code reports its launch directory plus every `--add-dir`, `/add-dir` and `additionalDirectories` entry as MCP
roots, and supports elicitation, so with `NIB_CONFIRM_OUTSIDE_ROOTS=1` (add `"env":{"NIB_CONFIRM_OUTSIDE_ROOTS":"1"}` to
the server JSON) writes outside those directories ask the user. Claude Code's `--dangerously-skip-permissions` does not
answer MCP elicitation, so leave it unset there. It warns about tool output above 10k tokens and truncates at 25k, which
is why reads share a 40 KiB budget.

Then, in your Claude Code `settings.json`, remove the built-in shell and file tools from the model's context:

```json
{
  "permissions": {
    "deny": ["Bash", "Grep", "Glob", "Edit", "Write"]
  }
}
```

A bare tool name in `deny` removes that tool from the model's context entirely. The built-in `Read` stays for PDFs and
notebooks (and `NotebookEdit` for notebooks).

Known trade-offs of MCP edit tools in Claude Code: no inline diff approval UI, `acceptEdits` mode does not cover them
(hence the allow rules), checkpoints and `/rewind` are not documented to cover MCP file changes, and formatters or hooks
configured for `Edit|Write` matchers do not run for them unless their matcher also names `mcp__nib__.*`.

### The nibash mod

`mods/nibash` is a Claude Code mod (a plugin of function hooks) that shows what nibash runs. A band above the prompt shows
up to three running jobs and calls as cards, side by side where each gets at least 34 columns and stacked otherwise, as
many rows of them as the band has room for: a rounded box with `nibash 0:42 Run all Go tests` or
`job %1 3:10 Benchmark three runs` (the call's description; for a job, that of the call the mod saw it begin as, with
`background`, past its timeout, or as a call's one `cmd &`, since `nibash_status` leaves job descriptions out so an
agent reading it pays nothing for them; else the command), `subagent` for a subagent's, the command dimmed beneath when
there is a description, and the last output line. A job that ended stays for ten
seconds, its box green for exit 0 and red otherwise. Beneath the cards, `/nibash shows the full output`, or
`+2 more in /nibash` for those that did not fit. A call still in its
request is left out, since the transcript already shows it, until Claude Code moves it to the background. `/nibash`
opens a pane with every command and job, the ones ended in the last 10 minutes included, each with its command and
last output lines (Escape closes it while it holds the keys, and a second `/nibash` closes it too); and a job that
ends while the mod watched it gets a toast with its exit status. The mod hooks every
`mcp__<server>__nibash` call, which tells it nib's server name and when to look, and while anything runs it asks that
server's `nibash_status` once a second (with `tail: 12`), stopping once nothing runs or just ended; nib is the only
source of what runs, so subagents' commands and jobs show too. Claude Code passes a tool's structured content on as the
JSON of its one text block, which the mod reads. The poll needs `mcp__nib__nibash_status` allowed, which `nib setup`
adds; without it Claude Code would ask about every poll.

Besides `/nibash`, one slash command per list opens a pane of its own, which Escape or the same command again closes.
Such a pane asks `nibash_status` for its list (`view`) every two seconds while it is open, and the lists only read what
nib already holds. `/trash` lists what this server moved to the trash, whichever agent moved it, as `trash list` does,
with a `restore` button per entry: it asks nib to put the path back through the code behind `trash restore`, which
refuses a path where a file is now and never replaces it, and the pane shows nib's answer.
`/tokens` reports the characters of text this server's tools returned in the session, which is the unit nib's output
rules count in: in all, per agent and per tool with its calls, and the eight largest results with the start of the
arguments that asked for each. nib counts them where it builds a result, `nibash_status` itself and the rounds of a call
that paused to ask a question left out; the mod adds nothing.
`/names` lists, for each agent that called, the named directories it sees: nib's own (`nib.json`, detected source
roots, names kept from earlier sessions), those it defined with `hash -d`, those nib made for it and those it inherited
from the agent that started it, each with the directory and the paths nib printed under the name.
`/seen` lists, for each agent that called, the files nib holds a version of for it and the lines of each it counts as
seen in full, the ones an edit may anchor on, as ranges, with the tag of the version, the lines it knows only by their
content, and "edits may anchor anywhere" for a file nib has no record of seen lines for. It reads the snapshot store
without touching it, so asking changes nothing about what an agent may edit; it lists the 40 files used most recently.

While nib asks a decision or summary model about a long output, the band also shows it as a card,
`summary 0:07 deepseek/...` or `decider 0:02 typesafe/jev-1.14`, so a call that waits shows what it waits for: nib
reports them in `nibash_status` from the places where the decider and the summarizer ask a model (an answer from the
cache asks none), and the mod keeps
asking nib while one runs. `/nibash summaries off` turns the summaries of long output off for this server, `on` turns
them on again, and `/nibash summaries` alone flips them; the pane says which they are. It is `nibash_status`'s
`summaries` argument, so a client without the mod can send it too, and with no summarizer set there is nothing to turn.

A call to a hosted provider's tool (`zemble_graph_callers`) that has run three seconds gets a card too,
`zemble 0:04 graph_callers`, since the program behind it may start or index for minutes: nib lists the hosted calls
running in `nibash_status` (`calls`; its own tools are not among them), and the mod, which asks nib while one runs,
names a call only from its third second.

A log another MCP server's tool hands nib through `nib shape`, a zenit-dev test run's, gets a card too,
`shape 0:03 build.log`, beside the decider and summary cards it waits for: the mod asks nib while any other server's
tool runs, since that tool may be the wrapper, and stops when nothing of nib's runs.

`nib setup --user` installs it: the binary carries the mod, writes it to `~/.local/share/nib/mods/nibash` (in
`$XDG_DATA_HOME` when set; replaced whole when this nib's differs, so updating nib updates it) and adds that folder to
`CLAUDE_CODE_PLUGIN_DIRS` in the `env` block of the config folder's `settings.json` (`$CLAUDE_CONFIG_DIR`, else
`~/.claude`), keeping the folders already listed. Run it once per config folder, as for one Claude Code profile each; a
launcher that keeps one config folder per profile can run `nib setup --user --quiet` with that folder as
`CLAUDE_CONFIG_DIR` every time it starts Claude Code, which prints only what it changed, so a new profile, a new nib or
a new provider reaches every profile on its next start. By hand, `claude --plugin-dir /path/to/nib/mods/nibash` loads it
for one session. It needs a nib with `nibash_status`; against an older one it shows nothing.

## Benchmark

`bench/` compares, per model, a harness's own file tools with nib on the same editing tasks. Every run costs real model
calls, so results are stored and reused.

```sh
bun bench/run.ts --plan       # what a run would cover, what is stored, estimated cost
bun bench/run.ts --validate   # every task fails untouched and passes with its reference solution; no model calls
bun bench/run.ts              # run what is missing for all models, tasks and both modes, then write the report
bun bench/run.ts --models "GPT-6 Luna,claude" --tasks rename,minified --repetitions 3
bun bench/run.ts --shell --decider keywords,openrouter --tasks build-log,noisy-service,long-run   # nibash's deciders as arms
bun bench/report.ts           # rewrite bench/results/report.html from the stored runs
bun bench/run.ts --reparse    # read stored transcripts again (tokens, cost, tool calls, failures, notes), then rewrite the report
bun bench/run.ts --recheck    # check stored runs again whose check changed, then rewrite the report
```

- **Harnesses and models.** Claude models run through Claude Code, GPT models through OpenCode v2 on its OpenAI
  subscription login, and the others through OpenCode v2 and OpenRouter; the list with pinned versions is
  `bench/models.ts`. Any other id works with `--models` too (`claude-...` through Claude Code, `provider/model` through
  OpenCode), and a model named twice runs once. OpenCode is pinned as well: the benchmark installs `@opencode/cli` at
  the version in `bench/runners.ts` (`OPENCODE_VERSION`) with npm into `bench/.cache/` on first use and runs that copy,
  whatever `opencode` is on `PATH`; `NIB_BENCH_OPENCODE` names another binary. A run gets only its own provider's
  OpenCode login, read from OpenCode 2's database (`opencode.db` in its data directory) once OpenCode 2 has taken in
  `auth.json` there, and from `auth.json` before that, and handed to the run's own server through its API, so it lands
  only in the run's scratch database, deleted with the run (`OPENROUTER_API_KEY` from the environment wins for
  OpenRouter; OpenCode's own models run their free ones without a login). An OAuth login that would expire within the
  job plus 15 minutes stops the run as an error, since a refresh inside a run would leave OpenCode's own copy of the
  login stale; using OpenCode once refreshes it. Subscription runs report a cost of $0, so compare their tokens instead.
- **Sessions.** All prompts of a job go to one harness process, as a user's follow-ups would, so nib's server and what
  it knows (tags, seen lines, reapply tokens) live through every step: Claude Code runs as one
  `claude -p --input-format stream-json` process that reads each prompt from stdin, and OpenCode as one `opencode serve`
  on a free port behind a random password, with each prompt sent by `opencode run --server` continuing the first
  prompt's session. OpenCode starts MCP servers only once a request names the directory and does not wait for them
  before a model call, so the benchmark asks its API until nib is connected before the first prompt. OpenCode 2's client
  stops printing once the session is idle, which can leave out the last model calls, so after each prompt the runner
  reads that prompt's messages from the server (tokens, cost and tool calls of every model call) and appends them to the
  transcript, where they count instead of the client's events. Claude Code's `total_cost_usd` counts the whole session
  so far, so each step adds what it cost beyond the step before.
- **Modes.** `builtin` gives the harness's own file tools; `nib` turns all of them off, readers included, and adds nib
  as the only MCP server, so a model cannot read a file through the harness and then edit it through nib without a tag.
  Both modes turn off the shell, web and subagent tools, so only the file tools differ (nib runs with `NIB_SHELL=0`).
  OpenCode drops a tool whose permission is denied outright, so its setup is an allow-all rule followed by denials
  (`read`, `grep`, `glob` and `edit`, which also covers `write` and `patch`, in nib mode; `shell`, `subagent`,
  `webfetch`, `websearch`, `question`, `skill` and `execute` in both). `execute` is OpenCode 2's code mode, which runs
  scripts that call tools and fetch URLs; nib's server is configured with `codemode: false`, so its tools are offered to
  the model directly rather than only inside such scripts. The parser still counts an `execute` call as the calls its
  script made. `--shell` runs give each mode its shell, the harness's Bash or nibash, and are a separate row.
- **Decider arms.** In `--shell` runs nib mode's nibash cuts long output with a decider
  ([above](#long-output-and-deciders)), named explicitly in `NIB_DECIDER`. `--decider` picks the arms, comma-separated:
  `fixed`, `keywords` (the default, as in nib) and `openrouter` (`typesafe/jev-1.14`, or `openrouter:MODEL`). Each arm
  is a series of its own, while built-in runs, which no decider touches, are run once and compared with every arm. The
  `openrouter` arm gets `NIB_DECIDER_SANDBOX=1`, the key from `OPENROUTER_API_KEY` or else OpenCode's OpenRouter login,
  and `NIB_DECIDER_CACHE=bench/.cache/decider`, so the same output is not paid for twice; without a key the benchmark
  stops before any job runs. The vocabulary mirrors `internal/decider`, and a test holds it there.
- **Sandbox.** Both modes keep the model inside its work directory, so it cannot read a task's check or reference
  solution in `bench/`: nib runs with `NIB_SANDBOX=1`, Claude Code gets
  `permissions.blockReadsOutsideWorkingDirectories` and an `Edit(./**)` allow rule (so edits elsewhere need a prompt,
  which `-p` denies), and OpenCode denies `external_directory`. OpenCode judges a path as written, not where a symlink
  points, so the work directory holds no links while the model works: nib's `node_modules`, which the checks need, is
  linked in only when the check runs, and a model without a shell cannot make a link. OpenCode also ignores project
  config (`OPENCODE_DISABLE_PROJECT_CONFIG`), so an `opencode.json` a model writes cannot change the permissions of a
  later step. In `--shell` runs nibash is sandboxed with nib (programs in bwrap), while the harness's shell is not,
  since a shell reaches everything.
- **Limits.** A prompt is stopped after `--timeout` seconds or past 150 tool calls, counted as the output streams in;
  either way the run counts as failed without its work being checked, so a model stuck repeating a call stops costing
  money. Prompts that do not loop stay far below the cap. A harness runs in a process group of its own, and stopping it
  reaches all of it: SIGTERM, then SIGKILL after 5 seconds if anything of the group is left, and output still held open
  2 seconds later (by a process that left the group) is cut off.
- **Isolation.** Each run gets a fresh copy of the fixture in the temp directory. A harness inherits only what it needs
  to run, find programs, reach the network and keep its login (`PATH`, `HOME`, user and locale variables, proxies and
  certificates, `CLAUDE_CONFIG_DIR`); overrides such as `OPENCODE_PERMISSION`, `OPENCODE_CONFIG_DIR` or `NIB_DIRECTORY`,
  a surrounding Claude Code session's variables and per-token API keys never reach it, and nib is told its directory
  with `NIB_DIRECTORY`. Claude Code skips user settings and CLAUDE.md (`--setting-sources project,local`). OpenCode gets
  the run's scratch directory as `HOME`, empty config, data and state directories inside it, and an inline config, so
  global config, plugins, agents, skills and instructions (`~/.config/opencode`, `~/.agents`, `~/.claude`) stay out;
  only its cache is shared between runs. nib keeps the real `HOME`, so its sandbox still hides the user's files from the
  programs nibash runs. OpenCode's `PWD` is set to the copy, since OpenCode takes its project directory from `PWD`. If
  nib's own working tree changes during a benchmark, it stops.
- **Verification.** Checks run code the model wrote, so each runs confined in bwrap (bubblewrap, which the benchmark
  needs): the filesystem read-only, `HOME`, `/tmp` and `/run` empty, no network and no inherited variables, with the
  run's changes taken before any check starts. `bench/check.ts` reads a nonce from stdin before any of the tree's code
  loads and prints it only once the check returned, so code that exits early with status 0 does not pass. Tasks that ask
  for a test name its file, and verification runs it: it must have a new test, by name, that runs, asserts something and
  passes, and no new test may fail. The fixture's own tests are not judged, since one of them races and can fail under
  load.
- **Fixture and tasks.** The fixture is nib's own source at a pinned commit plus generated files: three API logs (the
  newest 2.5 MB with color codes), a config file, a minified bundle and CSV files with mixed line endings. The 19 tasks
  in `bench/tasks.ts` are of three kinds. Ten one-shot tasks take a single prompt: a rename across files, small and
  large edits in a 2200-line file, a docs-and-test change, a whole-file sweep, a whole-function rewrite, finding a crash
  cause in the logs, a replace inside the minified bundle, copying a file with changes, and fixing final newlines
  without changing line endings. Three multi-step tasks give several prompts on the same files, each building on the
  last, which is the repeated editing nib is built for: two prompts on a file that a teammate changes in between, four
  prompts reworking one function of the 2200-line file with its callers and a test, and four prompts growing one parser
  rule with its tests. Six program tasks run programs and read their output, so they run only with `--shell`: a type
  check failing in 45 places, a test failing with a long diff, a 3,000-line colored build log with one error, counting
  slow requests in a 9,000-line log, a replayed service log whose cause (a route reload) hides among hundreds of
  unrelated `ERROR` lines, and 2,394 checks, many named for errors, with one failure marked only by a cross. The last
  two print the made-up captures of nib's offline decider check byte for byte, where keywords cannot find the cause. A
  run passes when the task's check, `tsgo --noEmit` and the tests it asks for pass.
- **Caching.** A run's identity is the task's prompts and outside changes (with the constants and helpers they use), the
  fixture's content hash, the harness setup, the harness, the model and its effort, the mode, the shell setting, for nib
  runs a hash of nib's Go sources (`cmd/`, `internal/` without tests, `go.mod`, `go.sum`) and, for nib runs with a
  shell, the decider arm (runs from before deciders have none and match no arm). The benchmark builds the binary into
  `bench/.cache/nib` from the working tree. A stored run counts as a sample only on the installed harness version
  (`--any-harness-version` takes any) and only under a budget it would have gone the same under: a run its limits
  stopped counts for a `--timeout` no larger, any other run for one no smaller. A job runs only while fewer counted runs
  than `--repetitions` share its identity, so changing nib reruns only nib jobs, and builtin baselines stay valid.
  Harness or provider errors never count and run again; runs a limit stopped count as failed. Changing a check does not
  rerun models: `--recheck` rebuilds every stored run's tree from its stored changes and checks it again, and leaves
  runs a limit stopped failed. Changing how transcripts are read does not either: `--reparse` derives every stored run's
  tokens, cost, tool calls, failed calls, notes and log reads again from its transcript. `run.json` is written through a
  temporary file and a rename, and a damaged one is skipped with a warning.
- **Notes.** nib adds a note to a call that worked when it accepted the call outside its documented form or limits and
  says what it did instead (a correction: another tool's arguments such as `filePath` or grep's `mode: "content"`, a
  number over its limit, `-` rows that match the file, `PUT N:=M:`), or when it flags something (a warning: an edit
  recovered from a stale tag, a replace whose range looks one line off, or a path that does not exist next to others
  that do); nibash also notes where it cut long output and which long rows it cut (cut notes). The runner finds these
  notes in nib's responses by their wording, so `bench/runners.ts` keeps one pattern per note, and a test makes nib
  print each of them. A call whose arguments name a saved nibash output log (`PID-output-N.log` in nib's log directory)
  counts as a log read: a follow-up for what a cut left out.
- **Results.** Each run is a directory under `bench/results/runs/` (ignored by git) with `run.json` (status, tokens,
  cost, tool calls by tool, characters written to and returned by each tool, each failed tool call's message, each note,
  log reads, the decider arm, time, budget, harness version), the raw `transcript.jsonl`, the changed files and a diff.
  `bench/results/report.html` is a self-contained report. Its comparisons use only pairs of task and model setup that
  have runs with both tools, each pair weighing the same, so a side never looks better for having more runs of the
  easier tasks; the headline says how many pairs it covers and how many it leaves out, and leaves shell runs out. It
  shows the same measures per task kind and setup (each shell setup per decider arm), then per model: pass rate, cost,
  output tokens, context at the start and end, tool characters, turns (model calls; Claude Code's own `num_turns` counts
  parallel tool calls apart, so the runner counts calls itself) and failed tool calls per model for both modes,
  corrections and warnings per run for nib, the pass rate per task, grouped by kind, log reads, cut notes and nibash
  characters per decider arm on program tasks, calls and characters per call for each tool, failed tool calls grouped by
  message (to find the mistakes models make most), notes grouped the same way, nib versions over time and the failed
  runs with links to their transcripts.
- **Token measures.** Context at the start is the input of a run's first model call, cache included: system prompt, tool
  definitions and prompt. Context at the end is the input of its last call: what the model had in front of it by then,
  counted once, so an extra call is not penalized the way a sum over all calls would. Added during the run is the
  difference: tool calls, tool results and the model's own messages. Tool text is kept in characters, per tool, since
  characters are exact for every model and tokens per character depend on the tokenizer (2 to 3.6 characters per token
  in the first runs). Cost still covers every call, since every call is billed.

## Files

| File | Responsibility |
| --- | --- |
| `cmd/nib/` | Entrypoint: the stdio server, `nib setup`, `nib hook`, `nib shape`, `nib --version` |
| `internal/server/` | MCP tools, model-facing descriptions and schemas, argument recovery and notes, roots and elicitation |
| `internal/engine/` | Reads (selectors, outlines, plain reads, file facts, changes since a tag), grep and replace previews, find, patch staging and atomic publication, writes, row maps, refusals and reapply tokens, workspace checks |
| `internal/nibash/` | The shell: classification of nib's own commands, record links, the commands, the terminal sink and rendering, real programs, jobs, the sandbox |
| `internal/names/` | Named directories: `nib.json`, JVM source roots, the names agents define at runtime and who inherits them, expansion and display |
| `internal/snapshots/` | The snapshot store with seen and known lines, what an agent still holds of a version, dropping what an agent saw before its context was dropped, line remapping and shifted-line rescue, and what agents saw kept across restarts |
| `internal/patch/`, `internal/diff/`, `internal/tag/`, `internal/js/` | Patch parser; jsdiff's array diff; the oh-my-pi file tag; JavaScript string semantics the output rules were defined with |
| `internal/setup/`, `internal/hook/`, `internal/hookstate/` | `nib setup`; the Claude Code hook for the built-in `Read` and nib's own tools, which finds a subagent's parent and spawn moment, and for the session starts and compactions that drop an agent's context; the hook's private state folder, the agent and drop records it hands the server, the spawn moments it found and the moments each agent's context was dropped |
| `internal/opencode/` | Read-only lookups in OpenCode 2's database: which session a session was forked or spawned from, and when, and when it was last compacted |
| `internal/sqlitero/` | Opens a SQLite database nib does not own strictly read-only, creating no files beside it; used for OpenCode's database and by `read` |
| `test/` | Black-box suites run against the binary over MCP (`support/engine.ts` drives it with the engine's old interface), and the benchmark's tests |
| `bench/` | Built-in versus nib benchmark: `run.ts` (CLI), `tasks.ts`, `fixture.ts`, `models.ts`, `runners.ts` (Claude Code and OpenCode), `jobs.ts`, `store.ts`, `report.ts`, `check.ts` |
| `docs/design-nibash.md` | The design of the Go port, nibash and named directories, with the review it went through |
