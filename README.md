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
| `write` | `path`, `content`, optional `tag` | Creates a file (with parents), or overwrites one when given its current tag, keeping a CRLF file's line endings and a BOM; answers with a row map of the new file. |

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
- **Outlines.** Files over 500 lines or 20 KiB return `[PATH#TAG] N lines, about Xk tokens; outline only...` followed by real `LINE:TEXT` rows: every top-level code line, plus the first-level members of top-level classes, interfaces, structs, enums, traits, impls, namespaces and similar containers. Runs of imports collapse to the first one and a `[lines A-B: N more import lines]` note, and runs of three or more one-line declarations at the same depth (constants, type aliases, fields: lines whose next code line is not deeper) collapse the same way into `[lines A-B: N more one-line declarations]`. Comments, blank lines, closing-bracket lines (including signature tails such as `) -> None:`) and lines inside multi-line strings (Python triple quotes, JavaScript template literals, Go raw strings) are left out; quote marks inside one-line strings, comments and regex literals open no multi-line string. Indentation depth is measured in the file's own indent unit, ignoring comment lines. Markdown files outline their headings outside code fences. An outline is capped at 150 rows, counted after collapsing: when it would be longer, top-level rows stay and members fill the remaining room in file order, with a note counting what was left out. Outline rows are shown in full, so they count as seen and can be edited directly. A line starting with `#` counts as a comment, so JavaScript private fields (`#field = 1`) are left out.
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

`*` marks matches; other rows are context (1 before and 3 after by default). Context lines on one page come to at most 40, so they cannot multiply the output; within that, `before` and `after` give what they ask for. Past it, context goes to definitions first, and the other matches show bare: context that was asked for with `before` or `after` goes to every definition (up to the 40 KiB output budget), while the default context goes to definitions in page order only while it fits in the 40 lines, so a grep that lists many definitions, like `^function`, stays a list. A definition is a match in the declaring part of its line, before the parameters, type, value or body: on a top-level line whose declaring part holds a word like `function`, `const`, `class`, `def` or `fn` (the match may be that word, as with a pattern `^function`), on a member of a top-level class or similar container, or in a Markdown heading. With no definition among the matches, context that was asked for goes to the first matches in page order, up to the first that no longer fits, and the default context goes to none. A note then says which matches got context, how many lines all of them would have needed, and a range to read for another match. Every fully shown row counts as seen, so an edit can anchor on it without a read. Pages hold 20 files (continue with `skip`), with 20 matches per file, or 100 when only one file matches, the most a harness's own grep shows (OpenCode stops at 100). Matched lines over `maxLineLength` characters (default 512, at most 2000) are cut and not anchorable; when the first match sits past the cut, the row shows the stretches around the first 3 matches instead of the line's start, as `*[N chars 3142-3324, 28924-29106 of 31625]|...`. The search uses `rg --hidden` without `.git/`. It follows git's ignore rules, which count only inside a repository (ripgrep's default; outside git a `.gitignore` means nothing), plus ripgrep's own `.ignore` and `.rgignore`. A nested repository is searched by its own rules even when the repository holding it ignores it, as a workspace whose `.gitignore` keeps out every checkout it holds does: git lists the nested repositories its rules ignore, and each one ripgrep would not enter by itself becomes a root of its own. Only a repository's own worktree stays hidden, since it repeats the same files. Outside git, the search first walks up to 2000 folders looking for repositories, since one of those may ignore its own nested ones, and says so when it stopped early. `noIgnore` turns ignore files off entirely (`--no-ignore`, still without `.git/`). As in ripgrep, a `glob` include re-includes files ignored by name (never files inside ignored directories); `glob` may also be a list, such as `["!build/**", "!*.md"]`.

Tool arguments are forgiving where models carry habits over from other tools: numbers may come as strings, a number over its limit (`before` and `after` 20, `maxLineLength` 2000 for grep and 2 MiB for read) is held to the limit with a note instead of refused, `paths` may be one string, `read` also takes another read tool's `filePath`, `file_path` or `path` with `offset` (the first line) and `limit`, and `grep` takes a single `path`. JSON mistakes that small models make are read as meant: a `paths` or `glob` string holding a JSON list (`"[\"a.ts\"]"`) is that list, also with the end of the model's tool-call markup after it (`["a.ts"]>`, which is dropped), and an `edit` input sent as one line with escaped line breaks (`\n`) is decoded, since one line can never be a patch. A note at the top of the response, also of a call that fails, then shows nib's own form of the same call. `grep` also takes another grep tool's names for its `mode` (`content` for `matches`, `files_with_matches` for `files`). A path that does not exist is named in a note while `read`, `grep` and `find` serve the other paths (a read puts the note in that file's place, grep and find at the top); only when no path is left is the call an error, so a missing path never reads as a search that found nothing. One written `path:A:B` is told to write `path:A-B` for a range.

- **Line ranges.** A file in `paths` may carry a read selector (`app.log:1100-1250`, `app.log:-500`, `app.log:900-`) to search only those lines; context rows stay inside the selection too. Unlike a read, an open range runs to the end of the file. The same file named again without a selector searches all of it; a selector on a directory is refused.
- **Modes.** `mode: "files"` prints the matching paths and `mode: "count"` prints `COUNT PATH` per file. Both start with the total (`N matches in M files`), print no rows or tags, record nothing as seen, and page 200 files at a time. A count over more than 40 files sums them by folder instead, most matches first, as `COUNT FOLDER/ (N files)` at the deepest folder level that fits in 40 rows (a file above that level keeps its own row), and grepping one folder gives its files' counts; `skip` still pages the files one by one.
- **Big or growing files.** A file over 2 MiB, or one that changes while nib reads it (a log still being written), gets its rows from ripgrep itself: `*1147|text` for a match and ` 1148|text` for context, colors stripped and shortened at `maxLineLength` with `...`. Those rows carry no tag and are never anchors, and the block ends with a hint to read around a match with `plain: true`.
- **Output budget.** Files that do not fit the 40 KiB budget are left for the next page. The first file of a page is never left out: its rows stop at the budget, with `[output limit reached; grep path:N- to go on from line N]`.
- **Replace.** `replace` (ripgrep's syntax: `$1`, `${name}`, `$0` for the whole match) turns a search into a preview. Context defaults to none, and each matched line that would change shows only as it would become, as a `>N|TEXT` row (a long one shows the stretch around its first replacements); its current text is not repeated, since the row carries the match too. A match the replacement leaves as it is stays a `*N:` row, and so does an empty matched line, since its token hunk is a `PUT` that needs the line seen. Nothing is written: the output ends with `To apply the N replaced lines shown (>N| rows), call edit with input: REAPPLY t0k3n5`, and that token applies exactly the shown lines, as one whole-line `SUB` per line under the tags the preview printed, so it changes cut lines too and is rebased like any edit when a file changed meanwhile. A replace shows every match of each file nib holds (no 20-per-file limit), so one token covers the whole page; matches past the output budget or on later pages, and files nib cannot hold whole, are left out and say so. Applying the token repeats that: its response ends with `[N more lines that the search would change were not in this token; grep again with the same pattern and replace for them]`. A file whose token applies as previewed, under its tag and one line for one, answers with just `[PATH#TAG] N lines replaced as previewed`, since the `>N|` rows already showed every new line; those that showed whole count as seen, so follow-up edits need no read. A token that had to be rebased, or a replacement that added lines, gets the usual rows. Patterns spanning lines and the `files` and `count` modes are refused.

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
| `PUT N.=M:` / `PUT N:` | Replace lines `N..M` inclusive (`PUT N:` is line `N` alone) with the `+` rows that follow. |
| `PUT <N:` / `PUT >N:` | Insert the `+` rows before / after line `N`. |
| `PUT >$:` | Append at the end of the file; needs no line anchor, so it also applies under a stale tag (with a warning), under a tag never issued for any file (with a warning), or under a `[PATH]` header with no tag at all. |
| `CUT N.=M` / `CUT N` | Delete lines `N..M` (`CUT N` is line `N` alone). |
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
- Body rows are final content: `+` followed by the exact line. Forms borrowed from other edit languages (`@@` headers, `*** Update File:`, `N:` rows, `PUT N-M:`, text after a `PUT` line's colon) are rejected with a hint naming the correct form. `PUT N:=M:`, a range with a colon where its dot goes, can mean nothing else, so it applies as `PUT N.=M:` with a note giving that form.
- Diff-style `-` rows under a `PUT N.=M:`, `PUT N:`, `CUT N.=M` or `CUT N` (mixed with the `+` rows if need be) are accepted when they match the lines the hunk replaces, ignoring trailing whitespace and color codes. Like a `SUB` they then quote those lines, so they need no earlier read, and the response ends with a note showing the same hunk without them, since nib never needs the old text. When they do not match, the section is refused with the real lines; under any other hunk, or outside one, `-` rows are an error.
- All sections are parsed, read and checked before anything is written; files are then written one at a time, each atomically, so a write failure after the first file reports which files already changed.
- A refusal lists every problem in the patch at once: every malformed line, and for every section all its failed `~` checks, `SUB` texts and `-` rows together with its unseen lines (or its stale tag), each kind run on the hunks the kinds before it passed. One corrected resend is then enough.
- When the only problems are unseen lines (see below), the refusal ends with `To apply this patch unchanged after reading the lines above, call edit with input: REAPPLY t0k3n5`. That input applies the stored patch through the full normal validation, as an unchanged resend would; the patch's own tags still guard against files that changed meanwhile. A token belongs to its session, works once, and only the latest 64 are kept. Any other refusal gets no token, since the patch itself must change. `grep` with `replace` hands out the same kind of token for the patch it previewed.
- `SUB` replaces only text the patch quotes, so it is exempt from the seen-line rule and works on cut long lines. Several `SUB`s on one line apply to the original line and must not overlap. Its result counts as known content only when the whole original line was known.
- A successful section returns its new `[PATH#TAG]` and the kept row on each side of each change, in full, where a leftover line or a misplaced range shows (a grep replace token applied as previewed returns only the tag and a count; see grep). The new rows come back whole, so a row that landed other than the agent meant (a cut-off line, a wrong place) shows: every row of a run of up to 5, otherwise the first and last row, with a sparse map of `N~TEXT` rows between them (the line number plus the first 30 characters after the indentation; at most 40 map rows per response). Each stretch of kept lines that moved and holds a line the agent knew gets one note, such as `[lines 427-1004 are old lines 423-1000, moved down by 4]`; an edit that moves nothing adds none. A changed row past the cut (2000 characters, or 300 in a minified file) shows the stretch where it changed when a line was replaced by one line, as `[N chars 2952-3057 of 3108]|...`, instead of the line's start. Follow-up edits can use that tag and numbering without a read. A section that reproduces the current content reports `No changes` with those lines whole, so an edit resent unchanged shows what the file already holds, and leaves the file untouched.
- A replace whose range looks one line off still applies, with a warning naming the two lines: the kept line just above the new rows nearly matches the first new row (the range started one line late, as when `PUT 66.=70:` rewrites a header that sits on line 65), or the kept line just below nearly matches the last new row (it ended one line early). Lines count as alike when their common start and end, indentation aside, make up at least 75% of the longer one; lines under 12 characters never count. A new row that is even more like the line it replaced, as in an ordinary rewrite, raises nothing.
- The original UTF-8 BOM, CRLF/LF style and final-newline state are retained. To give a file without a final newline one, append a lone `+` row with `PUT >$:`: an empty last row on such a file ends the line before it rather than adding a line, so the response shows no row for it and says `[path now ends with a newline]` (on a file that already ends with one, the same hunk adds a blank line). A read that shows such a file's last line ends with `[no final newline; PUT >$: with a lone + row adds one]`, and an edit that rewrites its last line without changing anything says the same. Publication stages a complete file and atomically replaces the target, preserving POSIX ownership and mode, not ACLs or extended attributes. Hardlinked targets are refused. Linux pins parent directories through `/proc/self/fd`; neither this nor the path/inode rechecks are a compare-and-swap against arbitrary concurrent writers.

### write

```
Created [notes/brief.md#40FA] (23 lines, 19 non-blank, 5 shown)
1~# Handoff
5~## Settled
15~- item 15
19~## Do the remaining work befor...
23~Last line
```

`write` and `*** Add File:` answer with a sparse row map of the new file, so it can be edited without reading it back. Each `N~TEXT` row is a line number plus the first 30 characters after the indentation (`...` when the line goes on). The first and last non-blank lines are shown; a non-blank line right after a blank one is shown once at least 4 lines passed since the last shown row; and after 10 non-blank lines without a shown row, the 10th is shown. Blank lines are never listed. Past 300 rows the response is the outline a read would give instead, or no rows for flat data. Shown rows count as displayed; every other line counts as known, so a `~` check anchors it, and a mapped file that leaves lines out ends with a line saying so (`[to anchor a line not shown, put a ~TEXT row with its start under the PUT or CUT; no read needed]`), so the first edit of such a line needs no refusal to learn it. Edit responses show the same kind of rows inside the elided middle of a long inserted run.

Overwriting with `write` keeps the file's own style where the content cannot carry it: in a file whose line breaks are all CRLF, content with plain `\n` breaks is written with CRLF, and a BOM stays unless the content starts with its own; the response says what it kept (`; kept its CRLF line endings and BOM`). Content with `\r\n` breaks is written as given, as is anything over a file with mixed line endings. The final newline follows the content. To change a file's line endings or drop its BOM, delete it (`REM`) and write it new.

### Snapshots, seen lines and stale tags

The server keeps an in-memory snapshot store for its process (one per client connection): up to 4 versions per path, 256 paths and 64 MiB of text, evicting the least recently used path. Reads, greps, edits and writes record the version they produced plus the lines it showed in full; `info` reads record the version with no line shown, and `path#TAG` reads record it like an edit response.

- **Seen lines.** An edit may anchor on lines its tagged output showed in full, or as `N~TEXT` rows. Otherwise it is rejected with the actual content of up to 40 such lines plus 2 lines around each (`*` marks the anchors); all of those rows are then marked seen, so an unchanged retry or the `REAPPLY` token succeeds, and a miscount can be fixed from the rows around it. After an edit, only lines that kept both number and content stay displayed, plus the rows shown in the response. Moved lines are known, not seen, at their new numbers, so a hunk anchored on one needs a `~TEXT` check. A replacement on a moved line is still accepted when the new content carries that line's current text (at least 8 characters, unique in the file), a port of oh-my-pi's shifted-anchor rescue. A file with no snapshot (for example after a restart) is not restricted. A file created by `write` or `*** Add File:` with fewer than 10 lines is trusted whole.
- **Known lines and checks.** nib also remembers lines whose whole content the agent knows without having seen their current number: every line of a file it created, every row it inserted, and seen lines that an edit moved. A `~TEXT` check confirms a line's number. An insert may anchor on any line a check confirms; a replace or cut may span known lines when each end that was not displayed is confirmed (a block's end, which nib finds itself, needs no check). Lines the agent neither saw nor wrote stay refused for replacement, check or not.
- **Stale tags.** When the file no longer matches the tag, the edit is rebased only if every anchored line maps to an unchanged current line through one uniform offset with a neighbor that moved with it; rebased edits carry a warning. Otherwise the edit is rejected with the current tag and the current content around the requested lines. A tag never issued for the file tells nib nothing about what the agent saw, so it is rejected, naming the file it was issued for, unless every hunk is safe without one: `SUB` and matching `-` rows check the text they replace against the current file, and `PUT >$:` only appends (but not under a tag issued for another file, a likely mix-up). Such a section applies with a warning never to invent tags, and lines the agent never read stay unseen. A `[PATH]` header without a tag appends with no tag at all. A 4-hex tag can collide: if a retained version with that tag has different text than the current file, the file is treated as changed.
- **Not ported from oh-my-pi:** cut/paste registers, the syntax-tree check on rebases, the repeated no-op guard, and tree-sitter read summaries (nib's outline is indentation-based instead).

### Writes outside the workspace

By default nib writes anywhere the process may, without asking. Set `NIB_CONFIRM_OUTSIDE_ROOTS=1` to confine writes, creates, moves and deletes to the client's MCP roots, as reported through `roots/list` (the server's working directory when the client reports no roots); the list is refreshed when the client sends `roots/list_changed`. With it set, anything else asks the user through MCP elicitation: allow that file once, or everything under its directory for the rest of the session. Without elicitation support such writes are refused. The requested path is checked before any parent directory is created, and the canonical path again, so a symlink inside the workspace cannot redirect a write outside it. Reads are not restricted.

Set `NIB_SANDBOX=1` to keep every tool inside those same roots, reads included: `read`, `grep`, `find`, `edit` and `write` refuse any path that resolves outside them. Symlinks are followed, so a link inside a root cannot lead out, and a path that does not exist yet is judged by its nearest existing parent. A refused path in a `read`, `grep` or `find` is noted in its place, like a missing one, and the other paths are still served; an edit or write that names one fails as a whole, before any parent directory is created. ripgrep does not follow links while it walks a root, so a search never reaches past one either. The benchmark runs nib this way.

## Setup

Install nib with Bun, then register it in a project (`rg` must be on `PATH`):

```sh
bun add -g git+ssh://git@github.com/11ways/nib.git
nib setup              # in the project folder; --dry-run previews, --user registers it for every project
```

`nib setup` adds only what is missing, so an entry someone changed always wins, and running it again changes nothing:

- `.mcp.json`: the `nib` server.
- `.claude/settings.local.json`: the approval of that server in `enabledMcpjsonServers` (one listed in `disabledMcpjsonServers` stays off, and setup says so), and nib's Claude Code hook (below).
- `opencode.json`, when the project has one: the `nib` server.

With `--user` it puts the hook in the Claude config folder's `settings.json` (`$CLAUDE_CONFIG_DIR`, else `~/.claude`), registers the server through `claude mcp add --scope user`, since Claude Code rewrites the file that holds it while it runs, and tops up `~/.config/opencode/opencode.json` when there is one. The command it writes is `nib` when the `nib` on `PATH` is the copy running setup, else that copy's Bun and `src/main.ts`. A file that is not plain JSON (with comments, say) is left untouched and named.

Bun keeps a git package at the commit it first installed, even when asked for another, so to update, remove it and add the latest commit: `bun remove -g @11ways/nib && bun add -g git+ssh://git@github.com/11ways/nib.git#$(git ls-remote git@github.com:11ways/nib.git HEAD | cut -f1)`. The package is scoped (`@11ways/nib`) because `nib` on npm is an unrelated package, which `bun update --latest` would otherwise swap in. zenit-workspace's `./bootstrap` does all of this itself: it installs or upgrades nib from the spec in its `workspace.json` and runs `nib setup` in the workspace.

Any other MCP client that launches stdio servers can run nib too: the command is `nib` (or `bun /path/to/nib/src/main.ts`), started in the project directory (or with `NIB_DIRECTORY` set to it). Relative paths resolve against that directory. The tools are `read`, `grep`, `find`, `edit` and `write`; the server also sends usage instructions in its MCP `instructions` field. `nib --version` prints the version.

To get the most out of it, disable or deny the client's own find-and-replace edit and write tools, so the model edits through `edit` and creates files through `write`.

### The hook

`nib hook` is a Claude Code `PreToolUse` hook for `Bash`, `Read`, `Grep`, `Glob`, `Edit`, `MultiEdit` and `Write`. It refuses a call that one nib call does exactly, with fewer tokens, and its refusal names that call, which the model reads as the tool's result; everything it cannot match exactly runs untouched:

```
nib's hook refused this command: nib does this with fewer tokens, and its rows are edit anchors. Use instead:
mcp__nib__grep {"pattern":"hub_hello|hubHello","paths":["src/smoke_a.zig","src/smoke_b.zig"],"ignoreCase":true}
(If you need the shell's own output here, or nib's tools are missing from this session, send the same command again and it runs.)
```

- **Shape.** The command is parsed like the shell does (quotes, escapes, here-documents). It may be one command, optionally after `cd DIR &&` steps; anything else runs: `;`, `||`, `&`, other commands before or after, expansions (`$VAR`, `$(...)`, backticks, an unquoted here-document with them), subshells, loops and conditionals. The only redirections allowed are `2>/dev/null` and `2>&1`. Globs are expanded in the folder the command runs in, and a word that mixes quoted wildcards, which the shell keeps literal, with unquoted ones runs.
- **Reads.** `cat` (or `cat -n`), `head`, `tail`, `sed -n 'A,Bp'` (one file), `wc -l` and `ls` of folders become reads with line selectors or file facts. Later `head` and `tail` stages narrow the range exactly (`head -100 f | tail -20` is `f:81-100`) where it is a fixed range of one file, else the command runs; a later `grep` over one read becomes a grep of those lines; `cat`, `less` and `more` without arguments change nothing.
- **Searches.** `grep -r`, `egrep`, `fgrep` and `rg` become a nib `grep` with their options: a basic grep pattern converted to the extended syntax, `-i`, `-S`, `-F`, `-x`, `-l`, `-c`, context, `--include` and `--exclude`, `rg -g`, `-t` and `-u`. What nib's regex engine would read otherwise runs: `-P`, backreferences, `-w` around a pattern that does not start and end with a word character, a type together with an including glob (an intersection, where nib's globs add up), and inverted, only-matching or quiet greps. Later `head` and `tail` stages are dropped, since nib pages its own output, and a final `wc -l` asks for counts. `find` with `-type f`, `-name`, `-mtime -N`, `-mmin -N` or `-size +N` becomes a nib `find`; `-maxdepth` and every other test runs. Like `rg`, nib's searches skip what `.gitignore` ignores, where `grep -r` and `find` do not.
- **Plain reads for logs.** A read of a file that is only looked at gets `plain: true`, which drops the numbers and the tag: a `.log`, `.out` or `.err` file (rotations like `.log.1` too), a file in a `log` or `logs` folder unless it is code or config (a package named `log` holds code), a file over 2 MiB, which nib cannot tag, and the `tail` of any file that is not code or config. The same goes for the built-in `Read` of such a file. A refusal made only of plain reads does not claim edit anchors.
- **Writes.** Fixed text into a new file in an existing folder (`cat > F <<'EOF'`, `tee`, `echo`, `printf '%s\n' ...`) becomes a nib `write`. An append becomes a tagless `PUT >$:` only where nib writes the same bytes: the file ends with a newline (or is empty), holds no carriage return and is at most 2 MiB, and the text ends with a newline. Overwriting an existing file stays with the shell, since nib would need a read for its tag first.
- **Built-in tools.** `Read` (except PDFs and notebooks), `Grep` and `Glob` become their nib calls, except a `Grep` with a type nib cannot name, a type and a glob together, or `multiline`; `Edit`, `MultiEdit` and `Write` point at nib's `edit` and `write`. Their refusal ends with `(If you need Read itself here, ...)` for the tool it refused.
- **A resend runs.** The hook refuses a call once per session, so a command that truly needs the shell, or a session where nib's server is missing, only costs one refused call; the note says so. A call is the same by what it does: a Bash command's text, or another tool's arguments whatever their order, never a `description`. The short hashes it keeps live in `$XDG_RUNTIME_DIR/nib-hook/`, else `~/.cache/nib/hook/`; a folder that is not the user's own and private, or a state file that is a link, turns the hook's refusals off instead of being trusted.

### Using with Claude Code

By hand instead of with `nib setup`, register the server for every project, with its tools loaded up front instead of through tool search:

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

`bench/` compares, per model, a harness's own file tools with nib on the same editing tasks. Every run costs real model calls, so results are stored and reused.

```sh
bun bench/run.ts --plan       # what a run would cover, what is stored, estimated cost
bun bench/run.ts --validate   # every task fails untouched and passes with its reference solution; no model calls
bun bench/run.ts              # run what is missing for all models, tasks and both modes, then write the report
bun bench/run.ts --models "GPT-6 Luna,claude" --tasks rename,minified --repetitions 3
bun bench/report.ts           # rewrite bench/results/report.html from the stored runs
bun bench/run.ts --reparse    # read stored transcripts again (tokens, cost, tool calls, failures, notes), then rewrite the report
bun bench/run.ts --recheck    # check stored runs again whose check changed, then rewrite the report
```

- **Harnesses and models.** Claude models run through Claude Code, GPT models through OpenCode on its OpenAI subscription login, and the others through OpenCode and OpenRouter; the list with pinned versions is `bench/models.ts`. Any other id works with `--models` too (`claude-...` through Claude Code, `provider/model` through OpenCode), and a model named twice runs once. A run gets only its own provider's OpenCode login, passed in `OPENCODE_AUTH_CONTENT` so no copy lands on disk (`OPENROUTER_API_KEY` from the environment wins for OpenRouter). An OAuth login that would expire within the job plus 15 minutes stops the run as an error, since a refresh inside a run would leave OpenCode's own copy of the login stale; using OpenCode once refreshes it. Subscription runs report a cost of $0, so compare their tokens instead.
- **Sessions.** All prompts of a job go to one harness process, as a user's follow-ups would, so nib's server and what it knows (tags, seen lines, reapply tokens) live through every step: Claude Code runs as one `claude -p --input-format stream-json` process that reads each prompt from stdin, and OpenCode as one `opencode serve` on a free port behind a random password, with each prompt sent by `opencode run --attach` continuing the first prompt's session. Claude Code's `total_cost_usd` counts the whole session so far, so each step adds what it cost beyond the step before.
- **Modes.** `builtin` gives the harness's own file tools; `nib` turns all of them off, readers included, and adds nib as the only MCP server, so a model cannot read a file through the harness and then edit it through nib without a tag. Both modes turn off the shell, web and subagent tools, so only the file tools differ; `--shell` keeps the shell on, and such runs are a separate row.
- **Sandbox.** Both modes keep the model inside its work directory, so it cannot read a task's check or reference solution in `bench/`: nib runs with `NIB_SANDBOX=1`, Claude Code gets `permissions.blockReadsOutsideWorkingDirectories` and an `Edit(./**)` allow rule (so edits elsewhere need a prompt, which `-p` denies), and OpenCode denies `external_directory`. OpenCode judges a path as written, not where a symlink points, so the work directory holds no links while the model works: nib's `node_modules`, which the checks need, is linked in only when the check runs, and a model without a shell cannot make a link. OpenCode also ignores project config (`OPENCODE_DISABLE_PROJECT_CONFIG`), so an `opencode.json` a model writes cannot change the permissions of a later step. `--shell` runs are not sandboxed, since a shell reaches everything.
- **Limits.** A prompt is stopped after `--timeout` seconds or past 150 tool calls, counted as the output streams in; either way the run counts as failed without its work being checked, so a model stuck repeating a call stops costing money. Prompts that do not loop stay far below the cap. A harness runs in a process group of its own, and stopping it reaches all of it: SIGTERM, then SIGKILL after 5 seconds if anything of the group is left, and output still held open 2 seconds later (by a process that left the group) is cut off.
- **Isolation.** Each run gets a fresh copy of the fixture in the temp directory. A harness inherits only what it needs to run, find programs, reach the network and keep its login (`PATH`, `HOME`, user and locale variables, proxies and certificates, `CLAUDE_CONFIG_DIR`); overrides such as `OPENCODE_PERMISSION`, `OPENCODE_CONFIG_DIR` or `NIB_DIRECTORY`, a surrounding Claude Code session's variables and per-token API keys never reach it, and nib is told its directory with `NIB_DIRECTORY`. Claude Code skips user settings and CLAUDE.md (`--setting-sources project,local`). OpenCode gets an empty config directory, its own data directory and an inline config, so global plugins, agents and instructions stay out, and its `PWD` is set to the copy, since OpenCode takes its project directory from `PWD`. If nib's own working tree changes during a benchmark, it stops.
- **Verification.** Checks run code the model wrote, so each runs confined in bwrap (bubblewrap, which the benchmark needs): the filesystem read-only, `HOME`, `/tmp` and `/run` empty, no network and no inherited variables, with the run's changes taken before any check starts. `bench/check.ts` reads a nonce from stdin before any of the tree's code loads and prints it only once the check returned, so code that exits early with status 0 does not pass. Tasks that ask for a test name its file, and verification runs it: it must have a new test, by name, that runs, asserts something and passes, and no new test may fail. The fixture's own tests are not judged, since one of them races and can fail under load.
- **Fixture and tasks.** The fixture is nib's own source at a pinned commit plus generated files: three API logs (the newest 2.5 MB with color codes), a config file, a minified bundle and CSV files with mixed line endings. The 13 tasks in `bench/tasks.ts` are of two kinds. Ten one-shot tasks take a single prompt: a rename across files, small and large edits in a 2200-line file, a docs-and-test change, a whole-file sweep, a whole-function rewrite, finding a crash cause in the logs, a replace inside the minified bundle, copying a file with changes, and fixing final newlines without changing line endings. Three multi-step tasks give several prompts on the same files, each building on the last, which is the repeated editing nib is built for: two prompts on a file that a teammate changes in between, four prompts reworking one function of the 2200-line file with its callers and a test, and four prompts growing one parser rule with its tests. A run passes when the task's check, `tsgo --noEmit` and the tests it asks for pass.
- **Caching.** A run's identity is the task's prompts and outside changes (with the constants and helpers they use), the fixture's content hash, the harness setup, the harness, the model and its effort, the mode, the shell setting and, for nib runs, a hash of nib's `src/`. A stored run counts as a sample only on the installed harness version (`--any-harness-version` takes any) and only under a budget it would have gone the same under: a run its limits stopped counts for a `--timeout` no larger, any other run for one no smaller. A job runs only while fewer counted runs than `--repetitions` share its identity, so changing nib reruns only nib jobs, and builtin baselines stay valid. Harness or provider errors never count and run again; runs a limit stopped count as failed. Changing a check does not rerun models: `--recheck` rebuilds every stored run's tree from its stored changes and checks it again, and leaves runs a limit stopped failed. Changing how transcripts are read does not either: `--reparse` derives every stored run's tokens, cost, tool calls, failed calls, corrections and warnings again from its transcript. `run.json` is written through a temporary file and a rename, and a damaged one is skipped with a warning.
- **Corrections and warnings.** nib adds a note to a call that worked when it accepted the call outside its documented form or limits and says what it did instead (a correction: another tool's arguments such as `filePath` or grep's `mode: "content"`, a number over its limit, `-` rows that match the file, `PUT N:=M:`), or when it flags something (a warning: an edit recovered from a stale tag, a replace whose range looks one line off, or a path that does not exist next to others that do). The runner finds these notes in nib's responses by their wording, so `bench/runners.ts` keeps one pattern per note, and a test makes nib print each of them.
- **Results.** Each run is a directory under `bench/results/runs/` (ignored by git) with `run.json` (status, tokens, cost, tool calls by tool, characters written to and returned by each tool, each failed tool call's message, each correction and warning, time, budget, harness version), the raw `transcript.jsonl`, the changed files and a diff. `bench/results/report.html` is a self-contained report. Its comparisons use only pairs of task and model setup that have runs with both tools, each pair weighing the same, so a side never looks better for having more runs of the easier tasks; the headline says how many pairs it covers and how many it leaves out, and leaves shell runs out. It shows the same measures per task kind (multi-step and one-shot), then per model: pass rate, cost, output tokens, context at the start and end, tool characters, turns (model calls; Claude Code's own `num_turns` counts parallel tool calls apart, so the runner counts calls itself) and failed tool calls per model for both modes, corrections and warnings per run for nib, the pass rate per task, grouped by kind, calls and characters per call for each tool, failed tool calls grouped by message (to find the mistakes models make most), corrections and warnings grouped the same way, nib versions over time and the failed runs with links to their transcripts.
- **Token measures.** Context at the start is the input of a run's first model call, cache included: system prompt, tool definitions and prompt. Context at the end is the input of its last call: what the model had in front of it by then, counted once, so an extra call is not penalized the way a sum over all calls would. Added during the run is the difference: tool calls, tool results and the model's own messages. Tool text is kept in characters, per tool, since characters are exact for every model and tokens per character depend on the tokenizer (2 to 3.6 characters per token in the first runs). Cost still covers every call, since every call is billed.

## Files

| File | Responsibility |
| --- | --- |
| `src/main.ts` | Entrypoint: the stdio server, `nib setup`, `nib hook`, `nib --version` |
| `src/hook.ts` | The Claude Code hook: shell parsing and the nib call each guarded command or tool stands for |
| `src/setup.ts` | `nib setup`: registers the server, hook and approval in project or user configs |
| `src/server.ts` | MCP tool registration, model-facing descriptions, roots and elicitation |
| `src/engine.ts` | Reads (selectors, outlines, plain reads, file facts, changes since a tag), grep and replace previews, find, patch staging and atomic publication, writes, row maps, refusals and reapply tokens, workspace checks |
| `src/snapshots.ts` | File tags, the snapshot store with seen and known lines, line remapping and shifted-line rescue |
| `src/patch.ts` | Patch language parser |
| `bench/` | Built-in versus nib benchmark: `run.ts` (CLI), `tasks.ts`, `fixture.ts`, `models.ts`, `runners.ts` (Claude Code and OpenCode), `jobs.ts`, `store.ts`, `report.ts`, `check.ts` |
