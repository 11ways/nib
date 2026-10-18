# AGENTS.md

nib is a stdio MCP server that gives coding agents hashline file tools: `read`, `grep`, `find`, `edit` (a line-anchored patch language) and `write`, plus `nibash`, a shell whose own `cat`, `head`, `tail`, `grep`, `rg`, `sed -n`, `cut -c` and `wc` print the same anchored rows. See `README.md` for the full tool behavior and `docs/design-nibash.md` for the design of the shell and the Go port; keep them in sync when behavior changes.

## Stack

- Go, one binary (`cmd/nib`). Dependencies: `mvdan.cc/sh/v3` (the shell's parser and interpreter), `github.com/modelcontextprotocol/go-sdk`, `modernc.org/sqlite` (pure Go, for reading OpenCode's database read-only) and `github.com/odvcencio/gotreesitter` (pure-Go tree-sitter; only the grammar packages `internal/syntax` imports are linked), pinned in `go.mod`.
- `rg` (ripgrep) must be on `PATH` for `grep`, `find`, nibash's recursive searches and their tests; `bwrap` for nibash's programs in sandbox mode and for the benchmark.
- The black-box tests and the benchmark are TypeScript on Bun (`@modelcontextprotocol/sdk`, `diff`).

## Commands

```sh
go test ./...   # Go unit tests; must pass before committing
bun install
bun test        # the black-box suites, against the binary over MCP; must pass before committing
bun typecheck   # go vet and tsgo --noEmit; must be clean
gofmt -l .      # must print nothing
claude plugin validate mods/nibash && claude plugin test mods/nibash   # the Claude Code mod; must pass when it or nibash_status changes
```

`bun bench/run.ts` runs real model sessions through Claude Code and OpenCode and costs money. Do not run it unless asked; `--plan`, `--validate`, `--reparse`, `--recheck` and `--report` are free and make no model calls. Its checks run in bwrap (bubblewrap), which must be installed. Its output in `bench/results/` and `bench/.cache/` is ignored by git; never commit it. A change to a task's prompt or to `bench/runners.ts` setup must keep stored results honest: prompts are part of a run's identity, a setup change needs `SETUP` in `bench/runners.ts` bumped, and a change to verification that can turn a verdict needs `VERIFY` in `bench/store.ts` bumped.

`NIB_CORPUS=FILE go test ./internal/nibash -run Corpus -v` measures nibash against real commands (`research/bash-usage.ts --dump FILE` writes them, where that untracked folder exists).

`go test ./internal/nibash -run Captures -v` is the offline check of the deciders and summaries: it renders the 27 program outputs in `internal/nibash/testdata/captures/` with each decider, and with the OpenRouter decider and a summarizer, and reports the needed lines and rows each keeps, the models answering from the answers recorded in `testdata/captures/decider/` and `testdata/captures/summary/`; `NIB_CAPTURES_SHOW=NAME` prints what the summary shows of one. A capture's `loses` lists the needed lines an arm is known to lose, and `any` makes each need one fact that any line it matches shows. A change to the questions, the windows or the summary prompt needs them recorded again with `NIB_DECIDER_RECORD=1 NIB_DECIDER_KEY=...`, which calls OpenRouter and costs about fifteen cents; do not run that unless asked.

## Layout

| Path | Responsibility |
| --- | --- |
| `cmd/nib/` | entrypoint: stdio server, `nib setup`, `nib hook`, `nib --version` |
| `internal/server/` | MCP tool registration, model-facing descriptions and lean schemas, argument recovery with notes, roots and elicitation |
| `internal/engine/` | reads (selectors, outlines, skip rules, budget, plain reads, file facts, changes since a tag), grep and replace previews, find, patch staging (checks, SUB, blocks, MV, CP), refusals and reapply tokens, row maps, what counts as repeated output (`Repeats`), atomic publication, writes, workspace checks, the API nibash's commands read files through, files `skim` reads, and that tool's name (`engine.Skim`, its one declaration: a rename is that edit plus the docs) |
| `internal/syntax/` | tree-sitter parsing: the grammars by file extension (the one home of that mapping), each language's declaration rules, what a file declares (`path:Name` reads), where a block ends (`PUT N*:`), and the decoded-grammar cache limits |
| `internal/nibash/` | the shell: per-session interpreter, classification of nib's own commands, record links between them and the rows nib commands inside a compound command send one in band through its pipe, the commands, the terminal sink and rendering, the cut, the deduplicated form a decision model and a summarizer read, and the assembly of a decider's choice, `skim` (files shaped like long output for what the agent expects, and how far each agent read each one), real programs in process groups, jobs, the sandbox |
| `internal/decider/` | which rows of a cut nibash output the model sees: the decider kinds and every `NIB_DECIDER_*` variable, keywords, the OpenRouter decision models in order of preference, the summarizers (OpenRouter chat models, `claude -p` and OpenCode 2's `opencode run`, isolated from the user's OpenCode data) and the checking of their answers against the log, the answer cache |
| `internal/names/` | named directories (`~name/`): `nib.json`, JVM source roots, the names agents define at runtime (`hash -d`) and the inheritance of them by later subagents, the names nib makes for directories an agent keeps seeing and the learned file that hands the names two sessions used to later ones, per-agent views, expansion, display, the instructions table |
| `internal/snapshots/` | per-session snapshot store with seen and known lines and when each was first seen, copies as of a moment for forks, what an agent still holds of a version and dropping what it saw before its context was compacted or cleared, line remapping, shifted-line rescue |
| `internal/trash/` | the freedesktop.org trash everything nib deletes goes to: entries with their `.trashinfo`, a filesystem's own trash or a checked copy across devices, and what this server trashed, for `trash list` and `trash restore` |
| `internal/patch/` | patch language parser (pinned against the TypeScript parser by `testdata/corpus.json`) |
| `internal/diff/` | port of jsdiff's `diffArrays` (pinned against jsdiff by `testdata/jsdiff.json`) |
| `internal/tag/` | the file tag (xxHash32) |
| `internal/js/` | JavaScript string semantics (UTF-16 lengths and offsets, JS whitespace) that nib's output rules were defined with |
| `internal/setup/`, `internal/hook/`, `internal/hookstate/` | `nib setup` (server, approval, hook matchers, deny rules; order-preserving JSON); the hook for the built-in `Read` and for nib's own tools, which reads a subagent's parent from Claude Code's meta file and its spawn moment from the parent's transcript; the hook's private state, the per-call agent records it hands the server and the spawn moments it found; the session starts and compactions that drop an agent's context, the moments it keeps for each agent and the per-call drop records |
| `internal/opencode/` | read-only lookups in OpenCode 2's sqlite database, found by OpenCode's own environment: a session's parent, whether it is a fork, its spawn moment, when it was last compacted, and a provider's login for `opencode:` summarizers |
| `internal/providers/` | hosted providers (`--provider NAME=PROGRAM`): starting `PROGRAM nib-provider`, listing and calling its tools, starting it again after it ended, and reading its answers into prose and locations (`docs/design-providers.md`); `providertest` is a fake provider for tests |
| `mods/nibash/` | the Claude Code mod that shows what nibash runs (a band above the prompt, a `/nibash` pane, a toast when a job ends) and the lists nib holds (panes `/trash`, `/tokens`, `/names`, `/seen`) from `nibash_status`; its tests run under `claude plugin test`; `mods/embed.go` carries it into the binary, and `nib setup --user` installs it |
| `test/` | `engine`, `patch` and `server` suites against the binary (`support/engine.ts` gives them the engine's interface over MCP), and the `bench` tests |
| `bench/` | benchmark: tasks and checks, fixture (nib at a pinned commit plus generated files), Claude Code and OpenCode runners, cached run store, HTML report |

## Rules

- Every behavior change needs a test. Tests use temporary directories; never touch real user files.
- A decider only chooses among real rows of plain output, never nib's own file rows; a file `skim` reads is such output too, its lines plain rows that are never anchors; every gap names the log lines it leaves out, and the closing note says who chose. A summary is the one text of a model's the agent sees: one `<summary>` element, beside nib's own counts and a way to read the full log, and the rows it points at are real log lines inside what the model was given. Output leaves the machine only with `NIB_DECIDER=openrouter` or `NIB_DECIDER_SUMMARY`; the decision model under `NIB_SANDBOX=1` needs `NIB_DECIDER_SANDBOX=1` as well, and any other combination stops nib at startup.
- The file tag must stay identical to oh-my-pi's `file_hash` (4 uppercase hex: xxHash32 of BOM-free text with trailing spaces, tabs and CRs stripped per line, masked to 16 bits). A test pins known vectors.
- Tool descriptions and the server `instructions` must stay under 2048 characters (clients truncate them); a test checks this, and the size of all definitions together.
- Writes must stay atomic (stage, then rename or exclusive link), preserve BOM, line endings, final newline, mode and ownership, and refuse hardlinked targets.
- Confirming edits outside the client's MCP roots is opt-in (`NIB_CONFIRM_OUTSIDE_ROOTS=1`). When it is on, such edits must go through `authorize` (elicitation) and are never written silently.
- `NIB_SANDBOX=1` confines every tool, reads included, to the roots. Each path is judged after symlinks are resolved (a new path by its nearest existing parent) and before anything is created for it; ripgrep must never be told to follow links. nibash's own commands and its interpreter's file access (redirects, `source`, globs, `cd`) are confined the same way; real programs run in bwrap, where system directories and the toolchain on `PATH` are readable but only the roots are writable.
- Searches follow git's ignore rules only inside repositories, and a nested repository its parent ignores is still searched by its own rules (only the parent's own worktrees stay hidden); `git` is optional, and without it ripgrep's walk is all there is.
- nibash never lies: a command runs as nib's own only where it does what the system program would mean (asking that program where it differs by machine, as how `cut -c` counts), and its output differs only in form at the model's end, except that a `tail` of a real program's output at the model's end may be followed, after its own output and under a bracketed header saying so and who chose, by log lines it left out that a decision model chose or a `<summary>` of the whole output. Anything else, an option it lacks included, runs the real program, and its description states every difference from bash. The one deliberate difference in effect is deletion: `rm`, `unlink`, `git rm`, `git clean`, `find -delete` and `edit`'s `REM` move what they delete to the trash, with the deleting program's messages and exit status, and an option nibash's `rm` lacks is refused, never run by the system `rm`.
- nib never writes to a harness's own data: OpenCode's database is opened read-only (`mode=ro`, `query_only`), Claude Code's transcripts and meta files only read, and anything unexpected in them leaves an agent on its own rather than guessed.
- The hook refuses only the built-in `Read` of files nib reads, naming the exact nib call, and an unchanged resend always runs, so a session without nib's server keeps its files.
- Rows shown as `N:TEXT` must be complete, real file lines: anything cut, color-stripped or synthetic uses a different prefix so it can never be used as an edit anchor. The one exception is `N~TEXT` row-map rows, and only for lines the agent wrote or saw, whose whole content it already knows. Plain read rows carry no number at all; removed lines in a `path#TAG` read (`-|TEXT`), grep replace previews (`>N|TEXT`) and nibash rows that do not match the session's latest version (`N|TEXT`) never use the `N:` form.
- Match the surrounding code style: Go as `gofmt` writes it, TypeScript with no semicolons and double quotes; short one-line doc comments that explain surprises, not the obvious.
- Prose in Markdown (`README.md`, `docs/`) wraps at about 120 characters per line, so reads, greps and edits of it stay small; code blocks, tables, headings and long code spans stay as they are, and the rendered page must not change.
