# AGENTS.md

nib is a stdio MCP server that gives coding agents hashline file tools: `read`, `grep`, `edit` (a line-anchored patch language) and `write`. See `README.md` for the full tool behavior; keep it in sync when behavior changes.

## Stack

- TypeScript run directly by Bun (no build step). Bun APIs are used on purpose (`Bun.hash.xxHash32`, `Bun.spawn`).
- `rg` (ripgrep) must be on `PATH` for `grep` and its tests.
- Dependencies: `@modelcontextprotocol/sdk`, `zod`, `diff`. Versions are pinned in `package.json`.

## Commands

```sh
bun install
bun test        # all tests; must pass before committing
bun typecheck   # tsgo --noEmit; must be clean
```

`bench/run.ts` runs real model sessions through `claude -p`, costs money, and needs `NIB_BENCH_SOURCE`. Do not run it unless asked. Its `bench/results-*.json` output is ignored by git; never commit it.

## Layout

| File | Responsibility |
| --- | --- |
| `src/main.ts` | stdio entrypoint |
| `src/server.ts` | MCP tool registration, model-facing descriptions, roots and elicitation |
| `src/engine.ts` | reads (selectors, outlines, skip rules, budget), grep, patch staging, atomic publication, writes, workspace checks |
| `src/snapshots.ts` | file tags, per-session snapshot store, line remapping, shifted-line rescue |
| `src/patch.ts` | patch language parser |
| `test/` | `engine`, `patch` and `server` (MCP client and stdio) tests |

## Rules

- Every behavior change needs a test. Tests use temporary directories; never touch real user files.
- The file tag must stay identical to oh-my-pi's `file_hash` (4 uppercase hex: xxHash32 of BOM-free text with trailing spaces, tabs and CRs stripped per line, masked to 16 bits). A test pins known vectors.
- Tool descriptions and the server `instructions` must stay under 2048 characters (clients truncate them); a test checks this.
- Writes must stay atomic (stage, then rename or exclusive link), preserve BOM, line endings, final newline, mode and ownership, and refuse hardlinked targets.
- Confirming edits outside the client's MCP roots is opt-in (`NIB_CONFIRM_OUTSIDE_ROOTS=1`). When it is on, such edits must go through `authorize` (elicitation) and are never written silently.
- Rows shown as `N:TEXT` must be complete, real file lines: anything truncated or synthetic uses a different prefix so it can never be used as an edit anchor.
- Match the surrounding code style: no semicolons, double quotes, short one-line doc comments that explain surprises, not the obvious.
