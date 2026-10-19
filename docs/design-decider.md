# Design: a decider for long nibash output

Status: implemented. Phase 1 (log-line notes, the plain-row cap, the decider interface, keywords as the default, the
offline check) and phase 2 (the OpenRouter decision model, opt-in only) are both in. Revised on 2026-10-15 after a
review; section 15 answers it point by point. Research figures name their source or measurement. It replaces section 11
of `docs/design-nibash.md`.

## 1. Goal and rules

**Goal:** when nibash output is too long to show whole, choose which rows the model sees by what they are for, instead
of always the first 100 and last 200. Same trigger and budget as before (over 400 rows or 30 KiB; at most 300 rows and
30 KiB shown); a decider may only change which rows of an already cut output are shown (user decision 1).

**Rules for every decider:**
1. **Choices only.** A decider returns ranges of rows and scores. Every row shown is a row of the output, unchanged;
   every note is a fixed template with numbers, paths and the decider's name filled in.
2. **Every cut is announced** at each gap with the log lines it leaves out, and a closing note says who chose.
3. **The whole output stays in the log.**
4. **Output leaves the machine only when asked:** `NIB_DECIDER=openrouter`, and under `NIB_SANDBOX=1` also
   `NIB_DECIDER_SANDBOX=1`; any other combination with `openrouter` stops nib at startup.
5. **Failure is never worse than keywords:** a failed model leaves keywords to decide, with the reason in the note.

**Non-goals:** summaries; rewriting, reordering or merging rows; background jobs (their output goes only to a log);
changing the budget (a lower threshold is a later benchmark arm); nib's own file rows (section 4).

## 2. Findings: decision models on OpenRouter

- **Endpoint:** `POST https://openrouter.ai/api/alpha/decisions` (`createApiAlphaDecisions` in
  [openapi.json](https://openrouter.ai/openapi.json)). Request: `model`, `state` (any JSON), `questions` (id to
  question), optional `provider`. A `noul` question has `instructions` and optional `criteria {true, false}` and is
  answered `{type, noul}`, the probability of yes. Response:
  `{id, model, provider, answers, usage: {input_tokens, output_tokens, cost}}`; `model` names the dated snapshot
  (`typesafe/jev-1.14-20261014`). Errors: `{"error": {"code", "message"}}` with 400, 401, 402, 403, 404, 413, 429
  (TypeSafe sends `retry-after`), 500, 502, 503. The path says alpha and the schema changed several times, so decoding
  is lenient (section 8.6).
- **Models** (`GET /api/v1/models?output_modalities=decisions`, 24 entries on 2026-10-15). One schema worked on all
  seven called. On a build log as 16 windows (Tests A and B): indexing an array (`windows[i]`) defeated most models;
  named fields (`output.w0`...) worked. jev-1.14 answered in 0.26 to 0.5 s per request, billed the state once (8,349
  tokens against about 100,000 for pplx, liquid and solar on the same request), costs $0.042/M input, has a ZDR
  endpoint, and ranked a noisy log right where mercury-decide did not (Test D). Hence **`typesafe/jev-1.14`, pinned**
  (user decision 5): thresholds are tuned against one version.
- **Limits:** requests over about 32,000 bytes of text run in Jev's slower, less accurate long-context mode ([Jev
  hub](https://openrouter.ai/docs/guides/community/jev)); identical calls move probabilities by up to 0.08
  ([cookbook](https://openrouter.ai/docs/cookbook/building-agents/gate-tool-calls-with-jev)). The thresholds (0.5, 0.7)
  come from single calls per case, so a window near a threshold can flip between runs: known nondeterminism, which the
  cache removes for repeated outputs.
- **Privacy flags:** `provider: {"zdr": true, "data_collection": "deny"}` routes only to endpoints that retain nothing
  and do not train; every decision endpoint listed is ZDR, and a live Jev call with these flags succeeded.

## 3. Findings: the offline check

`go test ./internal/nibash -run Captures -v` renders each capture in `internal/nibash/testdata/captures/` through the
real renderer (`collect`, then `fit`) with each decider, and counts the needed log lines some shown row stands for. Four
captures are the benchmark's program tasks, run offline from a fixture copy; two are made up (`generate.ts`) as cases
where keywords cannot work (user decision 6). The OpenRouter arm answers from recorded real answers in
`testdata/captures/decider/`, so the check is deterministic and free; recording them cost $0.0161 in all. Since the
summaries (section 16) the set is the research branch's 27 cases (21 real outputs added: Go and pytest failures, a
panic, a hang, a 109,000-line verbose run, ruff, javac, `go build -x`, `bun install`, git logs and diffs, listings and
counts), with a fourth arm, Jev with `deepseek/deepseek-v4.1-flash-1017` summarizing, recorded for $0.15. The losses
listed in `captures.json` as `loses`: Jev drops a line of `git-log-p` that keywords keep; the summary on Jev's excerpt
loses both needed lines of `go-build-x` and all three of `git-log-stat`, and on the whole of `pytest-v-fail` (under 400
lines, cut for its bytes) the `FAILED` line. Recording the summaries on the excerpts cost $0.02.

| capture | needed lines | fixed | keywords | jev-1.14 and keywords |
| --- | --- | --- | --- | --- |
| build-log: 3,013 lines, one error, 106 warnings | 1 | 0, 300 rows, 12.5 KB | **1**, 137 rows, 6.0 KB | **1**, 161 rows, 7.0 KB; 6 requests, 1.2 s, $0.0024 |
| type-errors: under budget | 90 | 90 (whole) | 90 | 90 (not asked) |
| failing-test: under budget, one 20,157-char row | 3 | 3, now 2.9 KB instead of 21 KB (row cap) | 3 | 3 |
| log-stats via a real program; purpose is a count | 904 | 43, 300 rows | 30, 296 rows | 43, 300 rows: whole judged 0.89, fixed cut and "filter or count" note; 8 requests, 0.5 s, $0.0058 |
| noisy-service (made up): cause names no problem, 327 unrelated `ERROR` lines | 2 | 0 | 0 | **1** of 2; 8 requests, 1.2 s, $0.0057 |
| long-run (made up): 2,393 passing tests, many named for errors, one failure marked with U+2715 | 2 | 0 | 0 | **2**; 6 requests, 0.4 s, $0.0018 |

Conclusions:
- **Keywords never lose a needed line the fixed cut keeps** on any capture with specific needed lines, and fix
  build-log. So **keywords are the default** (user decision 2). On log-stats, a count no selection serves, keywords keep
  30 sample lines against fixed's 43; neither helps, and the note points at the log.
- **Jev never loses a line keywords keep** and wins on both made-up captures, so the review's gate for phase 2 (a
  capture where keywords lose a needed line Jev keeps) is met, but only on made-up output. On noisy-service it ranked
  the cause window 0.93, below the windows full of the 503 symptom (0.95 to 0.97), and the routing line was kept only
  because the next window's widening reached it; "why did X start" favours symptoms. Phase 2 stays opt-in and is not
  recommended until the benchmark arms (section 13) show it on real runs.

## 4. Where it acts

`sink.render` lays out rows (`collect`), then `fit` cuts when over budget: with a decider and only plain rows, `shape`
asks the decider about the rows between the first 30 and the last 100 and `assemble` builds the result; otherwise
`fixedCut`. The decider runs under the MCP call's context (not the command's, which may have expired) with its own
deadline. Rows from nib's own commands (`[PATH#TAG]` runs) are never sent and never chosen (user decision 7): they are
file content the model asked for, what is shown of them counts as seen for `edit`, and keeping them out limits the
privacy question to program output. Output mixing both is cut the fixed way.

## 5. Log lines and long rows (both bugs found while measuring)

**Log lines (review B2).** Notes used to cite rendered rows, while the log holds raw lines; after collapsing, binary
output or records they differ (log-stats: "8496 rows; rows 101-8296" for 9,003 lines). Now every row carries the first
and last log line it stands for, computed while the log is written: a bytes entry's lines from the log's line count when
it is appended; a collapsed group's rows from `PlainLine.At`, a row standing for the lines up to the next row's
(Compress's note and echo rows keep `At`); a binary chunk from its own line breaks; a separator record writes a line and
no row; and where a record follows bytes that did not end their line (`printf foo; cat x`), the log gets a line break,
as the rows have one, so no log line holds two rows. When the log hits its 64 MiB cap the closing note says where it
stops. Tests: a sink with a binary chunk, a separator, an unterminated chunk before a record and a collapsed run, where
every log line belongs to exactly one row and `sed -n A,Bp` on each gap returns exactly the left-out lines.

**Long rows (review M6).** Plain rows had no length limit. A plain row over `engine.LineLimit` (2,000) is now cut in
read's bracketed non-anchor form, numbered with its log line (`[line 2, chars 1-2000 of 20157]|...`), with a footer
`[cut rows: N is the row's line in the whole output; read LOG:N with maxLineLength to see one whole]`; the log is saved
whenever a row is cut.

## 6. Assembly

Constants in `sink.go`: `decidedHead` 30, `decidedTail` 100, `pickContext` 3, next to `headRows` and `tailRows`.
1. Keep the first 30 and last 100 rows.
2. Take picks scoring at least `PickThreshold` (0.5), highest first (ties in the decider's order), each widened by 3
   rows; add each while the result stays within 300 rows and 30 KiB, else count it as not fitting.
3. If no pick fits or reaches the threshold: the fixed cut, saying the decider found nothing.
4. If the model judged that the purpose needs every line (`Whole` at least 0.7): the fixed cut, saying to filter or
   count the log instead.
5. Rows stay in output order, with a gap note at every gap.

## 7. Notes

```
[nibash: log lines 31-1179 left out]
[nibash: 161 rows shown of 3,013 log lines; typesafe/jev-1.14 and keywords chose the middle for your description; whole output: LOG]
[nibash: 137 rows shown of 3,013 log lines; keywords chose the middle; whole output: LOG]
[nibash: 300 rows shown of 5,000 log lines; log lines 101-4800 left out; whole output: LOG]
[nibash: 300 rows shown of 5,000 log lines; log lines 101-4800 left out; keywords found nothing in them; whole output: LOG]
[nibash: 300 rows shown of 9,003 log lines; log lines 101-8803 left out; typesafe/jev-1.14 and keywords judged that your description needs every line, so filter or count the whole output instead; whole output: LOG]
```
Optional clauses of the closing note: `the decision model failed (REASON), so keywords chose the middle`;
`2 more passages it chose did not fit (log lines 328-334, 628-634)`;
`log lines 731-755 were not judged (OpenRouter 502)` or `(over the 8-request limit)`;
`the log stops at its 64 MiB limit, in log line N`; `the whole output could not be saved`. The model's purpose is
`for your description`, or `for the failure` without one; keywords read no purpose, so their note names none. `REASON`
is one of `timeout after 2s`, `OpenRouter N`, `invalid answer`, `network error`, `cancelled`,
`the decision model is off for this session after OpenRouter 401`.

## 8. The decider package

`internal/decider` is the vocabulary's home (review M8): `Kind` (`fixed`, `keywords`, `openrouter`) with its description
clause as a fact on the member, `Parse(getenv)` for every `NIB_DECIDER_*` variable plus `NIB_SANDBOX`, and
`Config.New()`. `cmd/nib` parses at startup, prints an error and exits 1 on a bad configuration, and passes the decider
to `nibash.New(...).Deciding(d)`; a nil decider is the fixed cut.

```go
type Input struct { Command, Purpose string; Exit int; TimedOut bool; Rows []string }
type Pick struct { Range; Score float64 }
type Choice struct {
	Picks    []Pick
	Judged   bool    // whether Whole was answered (review: no NaN sentinel)
	Whole    float64
	Unjudged []Unjudged // ranges with the reason no answer covers them
	Who      string  // "keywords", or "typesafe/jev-1.14 and keywords" (the configured id)
	Failure  string  // why the model did not decide at all
	Model    bool    // a model read the purpose
	Usage    Usage
}
type Decider interface { Decide(context.Context, Input) Choice; Kind() Kind }
```

### 8.1 Description (review B1)

`Shell.Description()` builds the nibash description from the configured kind's clause: "long output cut to its start and
end" (fixed), "... its start, its end and the lines between that name errors or failures" (keywords), "... the parts
between that a decision model on OpenRouter judges your description needs (keywords when it fails)" (openrouter). A Go
test holds every variant under 2048 characters; the black-box suite checks the three as served.

### 8.2 Keywords

`Hit` in `keywords.go` is the vocabulary's only home: a problem word standing alone (`error(s)`,
`fail(s|ed|ure|ures|ing)`, `panic(ked)`, `exception`, `traceback`, `fatal`, not inside paths or file names such as
`src/errors.ts` or `error-handling.test.ts`), or a mark (pytest's `E `, `FAIL`, `not ok`, `Expected`/`Received`, Bun's
U+2717 cross, exception class names such as `TypeError`), after removing counts of none (`0 errors`, `no failures`).
`warning` is deliberately out (build-log has 106 warnings and one error). Hits score `KeywordScore` (0.5); the last hit
goes first, since runners print the reason last, then the rest in order, since compilers print causes first.

### 8.3 OpenRouter: what is asked

- **Gate (review M4):** the model is asked only with a purpose, a nonzero status, or a timeout (`TimedOut` is passed
  explicitly, since `Shell.Run` reports status 0 on a timeout). A successful run without a description is cut by
  keywords: its wording was never measured and it would pay ~0.5 s and a request on every long successful output.
- **Windows:** the candidate rows in windows of 25, each row cut to 400 characters for the question only, as named
  fields `output.w0`, `output.w1`... in output order, and a second set of windows starting half a window later (each
  straddling a boundary of the first, after them in the request order so the first set's batches and their cached
  answers stay as they were). Where windows start decided outcomes: git-log-stat's commit scored 0.56 in a window that
  started at the log's first line and under 0.3 in one that started after the kept head; with both sets it is kept by
  the decision model and the summary, for about twice the requests (the captures' recording went from about $0.12 to
  $0.24). Requests are packed to at most 30,000 bytes of JSON (under Jev's 32,000-byte long-context switch), at most 128
  per call in parallel and 16 in flight per process (128 requests of 30 KB is about 1.1M input tokens, some 5 cents at
  Jev's price; gradle-browser's 25,098 lines take 65 requests, $0.02 and 2 s). Past 128, windows with keyword hits go
  first, then those nearest either end; the rest are named in the note as not judged.
- **Questions:** one `noul` per window: with a purpose, "Does `output.wN` contain a line the developer needs for
  `purpose`?"; without one, "... needs to see to fix the failure of `command`?" (Test B's wording) or, after a timeout,
  "... to find out why `command` did not finish before its timeout?" (not measured). With a purpose, the first request
  also asks `whole`: "Does `purpose` need every line ... so that showing only some parts would mislead?". Changing the
  wording bumps `questionsVersion`.
- **The command is sent as written** (user decision 3); state is `{command, purpose, exit_status, timed_out, output}`;
  nothing else (no `user`, `session_id`, directory or environment).

### 8.4 OpenRouter: from answers to picks (reviews M3, M5)

The choice is the union of the model's windows (each its `noul`) and the keyword hits at 0.5, so confident model picks
rank first and a flood of hits cannot displace them, while a hit the model rated low is still kept if it fits: the
target is "never loses a needed line that keywords keep", and the offline check asserts it. A failed request leaves its
windows unjudged with its reason, and `Whole` unjudged if it carried that question; only when every request failed do
keywords decide alone, with the reason.

### 8.5 Failures, retries, session state

One retry for 429, 5xx and network errors, after `Retry-After` or 200 ms, only if it fits the deadline
(`NIB_DECIDER_TIMEOUT`, default 30 s for the whole decision; it was 2 s until the user set the bar at 30 s, since a
decision costs seconds the agent does not notice). With several models in `NIB_DECIDER_MODEL`, a model that gives no
answer at all hands the decision to the next within the same deadline, and `Who` names the one that chose and the
failures before it. No retry for other 4xx or invalid answers. 401, 402 and 403 turn the model off for the server
session; each later note says so. A call whose MCP context is already cancelled sends nothing. Recording took 0.4 to 1.2
s per decision with 6 to 8 parallel requests, so the 2 s deadline has some headroom but no tail-latency evidence: the
timeout fallback is tested as a common path.

### 8.6 Answers and the cache

Responses are decoded leniently: only `answers.*.noul` and `usage` are read, and every asked id must have a number in
[0, 1], else `invalid answer`. Answers are cached by SHA-256 of the question version, the endpoint URL and the whole
request body (model, state, questions): 256 in memory, and with `NIB_DECIDER_CACHE` one JSON file each on disk (`0700`
directory, `0600` files, written atomically). An entry holds only probabilities by window id, the snapshot, tokens and
cost, never output text (a test asserts it). Entries read from disk are validated like live answers, since a cache
directory under the roots is writable by sandboxed programs.

## 9. Configuration

| variable | default | meaning |
| --- | --- | --- |
| `NIB_DECIDER` | `keywords` | `fixed`, `keywords` or `openrouter`; anything else stops nib at startup |
| `NIB_DECIDER_KEY` | `$OPENROUTER_API_KEY` | (user decision 4); without either, `~/.config/nib/openrouter-key`, then OpenCode's `auth.json` `openrouter` entry; `openrouter` with none found stops nib at startup, naming where it looked. `nib setup --decider/--summary` remember `NIB_DECIDER`, `NIB_DECIDER_MODEL` and `NIB_DECIDER_SUMMARY` (`decider.Remembered`) in `setup.json`, read at startup unless the environment sets one |
| `NIB_DECIDER_MODEL` | `typesafe/jev-1.14` | Decisions models, comma separated, in order of preference |
| `NIB_DECIDER_URL` | `https://openrouter.ai/api/alpha/decisions` | tests point it at a fake |
| `NIB_DECIDER_TIMEOUT` | `30000` | ms for the whole decision |
| `NIB_DECIDER_SUMMARY`, `_URL`, `_TIMEOUT` | unset, OpenRouter's chat endpoint, `30000` | summarizers (section 16) |
| `NIB_DECIDER_CACHE` | unset | directory for answers on disk |
| `NIB_DECIDER_SANDBOX` | unset | `1` allows `openrouter` under `NIB_SANDBOX=1`; without it nib stops at startup (review M9) |

## 10. Privacy (review M7)

`NIB_DECIDER=openrouter` is the user's consent to send, for each cut output: the command as written, the description,
the exit status and the candidate rows. That is whatever the program printed, so a long `env`, `printenv` or `sort .env`
sends the secrets in it; the README says so plainly. There is no secret-pattern denylist (it would be a denylist gating
security). Never sent: nib's file rows, the kept start and end, the log path, the working directory. Every request
carries the ZDR and no-collection flags; OpenRouter's own prompt logging is an account setting the README tells users to
check.

## 11. Cost and expected saving (review M2)

The budget is unchanged, so a decider does not shrink what a cut output costs in the ordinary case; it can show fewer
rows (build-log: 137 rows and 6.0 KB with keywords against 300 rows and 12.5 KB), about 1,600 tokens less per such cut.
The main saving is the follow-up it avoids: when the needed line is cut, a model reads or greps the log, one more call
and turn, roughly 1,000 tokens of result plus the turn's context. Keywords cost nothing. The OpenRouter decider costs
$0.002 to $0.006 and 0.4 to 1.2 s per decided output on the captures; at a coding model's turn cost of a few cents (tens
of thousands of cached context tokens per turn), one avoided follow-up per 5 to 10 decided outputs breaks even, a rough
estimate to be replaced by the benchmark's measurement. Instant commands with a description (`seq 1 5000` described) pay
the latency too.

## 12. Testing

- `internal/decider`: `Parse` (every variable, key fallback, the three startup errors); every kind builds its decider;
  keyword hits and misses (file names, zero counts, warnings); against `httptest` fakes: named windows in output order,
  `whole` once, provider flags, request size, the 8-request cap and its unjudged ranges (keyword window kept), 429
  retried once, 502 retried once, 400 not retried, malformed and out-of-range answers, a partial failure, 401 turning
  the model off, the deadline, the gate (no request for exit 0 without a purpose, none when cancelled, a timeout judged
  as one), the cache (hit without a request, modes, no output text in entries, a tampered entry refused, the URL in the
  key), and a recorded real Jev response.
- `internal/nibash`: rows and log lines (section 5), gap notes checked with `sed`, assembly with a fake decider (head,
  tail, threshold, widening, budget, order), every note template, file rows never reaching the decider, keywords end to
  end, the row cap, the capped log, the description per kind; the existing cut test with the new note.
- Offline check (section 3), in `go test ./...`.
- Black-box (`test/decider.test.ts`): keywords as the default end to end with the log checked by line; the fixed cut and
  its description; the OpenRouter decider against a `Bun.serve` fake under `NIB_SANDBOX=1` with `NIB_DECIDER_SANDBOX=1`
  (no request for `cat` of a file, the description sent as the purpose, flags, the note); the three startup errors.

## 13. Benchmark changes still needed

Not made here (the benchmark is being moved to OpenCode v2 in parallel):
- **Identity:** `Identity` gains `decider?: string` (`"fixed"`, `"keywords"`, `"openrouter:typesafe/jev-1.14"`),
  appended to `identityKey` only when set. Since the default changed to keywords, a run without `NIB_DECIDER` is the
  keywords arm; the fixed arm must pass `NIB_DECIDER=fixed`.
- **Environment:** `nibEnvironment` passes `NIB_DECIDER`; for OpenRouter also `NIB_DECIDER_SANDBOX=1` (the bench runs
  `NIB_SANDBOX=1`), the key and `NIB_DECIDER_CACHE=bench/.cache/decider`; `--decider fixed,keywords,openrouter` selects
  arms for shell-series nib jobs on program tasks.
- **Metrics:** primary, follow-up reads of a cut output (nibash or `read` calls naming a path under the log directory,
  from transcripts); second, `toolChars` for nibash; then pass rate, turns, and the decider's own requests, cost and
  latency from the cache entries a run created.
- **Notes:** `NOTE_PATTERNS` has no nibash pattern. Add
  `{ kind: "cut", pattern: /^\[nibash: (?:[\d,]+ rows shown of [\d,]+ log lines|log lines? [\d-]+ left out)[^\n]*\]$/gm }`
  and `{ kind: "cut", pattern: /^\[cut rows: [^\n]*\]$/gm }`, with the note kind added to `Note["kind"]`, and extend the
  bench test that prints every note template with these.
- **Tasks** (from `generate.ts`), both `shell: true`:
  - `noisy-service`: prompt "POST /v1/payments starts answering 503 partway through bun scripts/serve.ts, which replays
    a recorded day of traffic and exits. Find out why and fix config/routes.json so payments keep working. Do not change
    the script." `before` writes `scripts/serve.ts`, which prints `noisyService()`'s log built from the actual
    `config/routes.json` (the reload lines name its `/v1/payments` upstream and that upstream's host count), and
    `config/routes.json` with `"/v1/payments": "payments-v2"` and upstreams `payments` (3 hosts) and `payments-v2`
    (none). Solve: route `/v1/payments` to `payments`. Check: the script unchanged; `/v1/payments` names an upstream
    with at least one host; everything else in the file unchanged.
  - `long-run`: prompt "bun scripts/check.ts runs the project's 2,394 checks and one fails. Find the failing check and
    fix the source so every check passes. Do not change the checks." `before` writes `src/duration.ts` whose
    `parseDuration` drops the sign (`Math.abs`), and `scripts/check.ts`, which prints `longRun()`'s output with the one
    real check (`parseDuration("-90s")` against -90) computed from `src/duration.ts`, printing
    `U+2715 ... got 90, want -90` and exiting 1 while it fails. Solve: keep the sign. Check: `scripts/check.ts`
    unchanged; `parseDuration` gives -90 for `"-90s"`, 90 for `"90s"` and the other fixture values.
  - A change to `bench/runners.ts` setup for these needs `SETUP` bumped; their prompts are new identities.

## 14. Known limitations

- The made-up captures are the only evidence that the model beats keywords; on noisy-service it preferred symptom
  windows to the cause window.
- With 300 rows of up to 2,000 characters, a fixed cut can still reach about 600 KB; only rows are capped, not the cut's
  total bytes. Making the fixed cut byte-aware would change what models see today on wide output and is left for a
  decision.
- Thresholds were set from single calls; Jev drifts by up to 0.08 between identical calls.
- The timeout question wording and the exit-0 behaviour with a purpose on very different outputs are not measured.

## 15. Review, point by point

| point | disposition |
| --- | --- |
| Bug 1, cut notes cite rows | done: log lines everywhere (section 5) |
| Bug 2, plain rows unbounded | done: row cap with log line (section 5); the total bytes of a fixed cut remain (section 14) |
| B1, description lies under fixed | done: built from the configured kind; every variant tested under 2048 |
| B2, false one-line-per-line invariant | done: first and last log line per row, from the log's line count and `PlainLine.At`; line break added where a record follows an unterminated chunk; capped log announced; the four cases tested with `sed` |
| M1, measurement cannot show the model's value | done: two made-up captures where keywords fail; phases split; phase 2's gate met on made-up output only, so it stays opt-in and unrecommended; bench tasks specified (section 13) |
| M2, saving unstated | done: section 11; follow-up reads as the primary bench metric |
| M3, model picks worse than keywords | done: union with keyword hits at 0.5; the offline check asserts the model never loses a line keywords keep |
| M4, exit 0 without purpose | done: keywords only; `TimedOut` passed explicitly |
| M5, all-or-nothing fallback | done: per-request unjudged ranges with reasons; `Whole` unjudged when its request failed |
| M6, row cap in this change | done |
| M7, secrets | done as overridden by the user: key falls back to `OPENROUTER_API_KEY`, command sent as written; cache test asserts no output text; README states that consent covers printed secrets; no denylist |
| M8, vocabulary home | done: `internal/decider` with `Kind`, `Parse`, `Config.New`; passed to nibash in `cmd/nib` (not through `server.Options`, since the shell is built there); bench arm names listed for the bench test |
| M9, silent sandbox downgrade | done: startup error |
| Minor: mixed units | done: "N rows shown of M log lines" |
| Minor: purpose echo | done: "for your description" |
| Minor: NOTE_PATTERNS | listed (section 13) |
| Minor: NaN sentinel | done: `Judged bool` |
| Minor: WHO from response model | done: the configured id |
| Minor: URL in cache key, validated entries | done |
| Minor: keyword false hits | done: paths, file names and zero counts excluded; measured on the captures |
| Minor: latency on instant commands | stated (section 11); the gate removes it without a description |
| Minor: thresholds from single calls | stated (section 2) |
| Minor: cancelled context | done: no request is sent |
| Minor: head/tail constants | done: in `sink.go` |
| Minor: sandboxed black-box fake, startup error tests | done |
| Minor: 2 s deadline evidence | recorded latencies stated; the fallback is tested |

## 16. Summaries

The research branch (`research/decider-eval/`, 27 cases, three rounds) measured chat models that read a cut output and
return a summary and line ranges. Their ranges covered every needed line in more cases than Jev's windows
(deepseek-v4.1-flash with reasoning off on the whole output: 19 of 19; gemini-3.1-flash-lite and haiku-5.5 on an excerpt
of Jev's picks: 18 of 19), in about 7 to 21 lines against Jev's 150 to 300 rows, and every line they quoted with its
number matched the log. Counts they gave were wrong, so the prompt leaves counts to the shell and no summary is asked
where the decider judged that every line counts.

`decider.Summarizer` reads only what nib hands it (the user's design: the decider picks, the summary model condenses the
picks). In `shape`, outside the gates above, an output of at most 600 log lines (`excerptLines`) goes to `Summarize`
whole and the decider is asked only when the summary fails; a longer one is decided first, and its excerpt (the first 30
and last 100 entries and the decider's picks rated 0.3 or more, widened by 10 lines, in `novelFirst` order, up to 600
entries) goes to `Summarize`, numbered by log line, each row cut to 300 characters. The answer is a summary and up to 5
ranges of line numbers, nothing quoted: the research's quoted lines all matched the log, so copying them only cost
tokens. The ranges are only the lines the agent must read as printed (an error, a stack frame, a value the summary
cites), and none when the summary answers the description on its own. `check` keeps the ranges inside the log, of at
most 60 lines, that touch a line the entry read; an answer whose ranges all miss fails (an empty list does not), and the
next entry is asked within the same deadline. A checked summary replaces the decider's cut with a `<summary>` element
(its text on one line, the model's name left out, summary tags in the text removed), the real rows of every range (1
line around each; the ranges share `summaryRows`, 30: by `shared`, one that fits an even split of what is left shows
whole and the longer ones split the rest, at least 3 rows each, dropping their surrounding lines first and then showing
their start with a named read of the rest), named gaps, and a closing note with nib's own counts; the agent sees fewer
rows than any decider shows, and nothing in it is the model's but the `<summary>` element. When every entry fails, the
decider's cut stands with `no summary` in its note.

Picks enter the excerpt best first, so the cap, not the 0.3 score, decides what a long log loses: at 400 lines the
captures lost go-build-x's error (its block ranked past the cap) and git-log-stat's commit (a 0.49 block ranked 14th),
and a lower score changed nothing. At 600 both are kept, for about 40% more input (median 10k tokens a request against
7k); 800 kept no more. pytest-v-fail, at 371 lines sent whole, keeps or loses its `FAILED` line from one recording to
the next: that is the summary model's own choice, not the excerpt's.

With a summarizer set, plain output past `summaryLines` (150) or `summaryBytes` (12 KB) goes to the summarizer even
within the cut's budget, since outputs of 10 to 23 KB reached the agent whole. On the captures (24 summarized of 33), 4
KB and 100 lines also summarized two reading tasks (a `--help` survey and a recursive grep) and lost their lines; 8 KB
lost the results of an end-to-end run (9.6 KB); 12 KB and 150 lines lost nothing but `type-errors`, a list whose 90
error lines the needs count one by one, where the summary shows 10. A prompt option letting the model answer that the
agent needs the output unchanged was tried and dropped: deepseek never used it, not even on the reading tasks. Output
within the budget whose summary fails stays whole, with no note. Output within the budget that `problemList` finds a
list of problems stays whole without a summary: 10 or more distinct problem lines (`decider.Hit`), 3 or more per kind
(`likeness`). On the captures that is `type-errors` alone (49 distinct error lines in 6 kinds), which keeps its 90 lines
again; `session-build` (202 failing tests, each naming its own, one cause) is summarized as before. The same prompt
option tried again, worded for lists ("separate problems the agent must each see"), was answered false on every capture,
`type-errors` included, so the rule is nib's own.

`claude-p:MODEL` runs Claude Code's `claude -p` on the user's login in an empty temporary directory with
`--tools "" --strict-mcp-config --setting-sources "" --no-session-persistence --disable-slash-commands`, so it loads
neither nib nor hooks; its process group is killed at the deadline. It costs about 1.8 s of start-up per call and no
key. Answers are cached by entry, endpoint, prompt and input (`summary-` files beside the decisions in
`NIB_DECIDER_CACHE`) and checked again whenever used.

`opencode:PROVIDER/MODEL` runs OpenCode 2's `opencode run --standalone --agent nib-summary --model PROVIDER/MODEL
--format json`, the output on stdin (`internal/decider/opencode.go`). OpenCode 2 has no flags for a system prompt, for no
tools or for no saved session, so nib isolates it by its environment instead: `HOME`, `TMPDIR` and every `XDG_*` folder
point into one temporary folder removed after, `OPENCODE_DB=:memory:` keeps its database in memory,
`OPENCODE_DISABLE_PROJECT_CONFIG=1` and the user's own `OPENCODE_*` variables left out keep every other config away, and
`OPENCODE_CONFIG_CONTENT` gives it the one config it reads: the agent `nib-summary` with the prompt and `permission`
`{"*": "deny"}`, and the provider's API key as `provider.ID.options.apiKey`. Denying every permission removes the tools
from the request: a probe asking for a file came back as text with 267 input tokens and no file, where the same agent
with permissions allowed sent 4,983 tokens and wrote it. `--standalone` keeps it off OpenCode 2's background service; a
run without it starts one. A fresh database is built by schema bootstrap, which skips the migration that imports
`auth.json`, so a login copied there would not be read: the key goes in through the config. It is read only from OpenCode
2's `credential` table (`opencode.Credential`) or the `auth.json` of OpenCode 1; OAuth logins are refused, since a run
that refreshed one could rotate the refresh token the user's own OpenCode holds. `--version` must say `opencode v2.`,
asked once per program while nib runs. The answer is the run's text parts, in order; an `error` event fails the entry by
its type (`provider.auth`, `provider.no-route`), and the process group is killed at the deadline.

The decision model and the summarizer read the output deduplicated (`dedupe` in `internal/nibash/dedup.go`, over
`engine.Repeats`, the one home of what counts as repeated output, which `engine.Compress` renders as the `[#N]` rows).
On the 34 captures: 190,254 log lines and 10.1 MB became 119,701 entries and 5.8 MB; build-log went from 3,013 lines to
235 and its decision from 116 windows to 10. Lines compare by `engine.MaskedKeys`: digits masked, except a number in a
place where otherwise equal lines take at most 8 values, 4 lines a value on average, so `POST /v1/payments 201` and
`... 503` stay apart while timestamps, ids and durations are masked. Only back-to-back runs collapse across masked
numbers (one entry under the first copy's text, marked `[numbers vary]`); a line or block elsewhere is named only where
it is the same text, and a copy with other numbers is spelled out. An earlier form wrote such a copy as its name
followed by its digits (`@L8 2026 10 13 09 28 08 751 1 86215 200 39`): numbers without their words, and with every
timestamp differing, most of a log. That, and an 8-request cap, cost noisy-service its cause: its 76 windows needed 10
requests, the window with the config reload was among those never judged, and the summary's excerpt never reached it;
with readable rows and 128 requests the decision model and the summary keep both lines. Lines under 24 characters keep
their text, since a name saves nothing there. `engine.Repeats` takes each caller's prices (`dedupe` counts an entry's
label and its legend line, `Compress` its marker rows), so a run or block collapses only where that costs fewer
characters than its lines one by one: three copies of `.` stay three entries.

Picks enter an excerpt, and the decider's cut, in `novelFirst` order: best first, except that a pick of which fewer than
a quarter of the kinds of row (`likeness`: digits and dotted names masked) are new waits until every pick with something
new has gone. gradle-browser shows why: judged whole, the decision model rates over a hundred windows 0.9 or more,
nearly all the same `ClassNotFoundException` and `Could not execute test class` for another test class, and they filled
the excerpt before the `LiveNamespaces` failure and the `NoSuchFileException` (rated 0.71 to 0.93) or Gradle's
`What went wrong` block. The excerpt's head and tail now count entries, not log lines, so a tail of 850 repeated
`NOT RUN` lines no longer pushes `What went wrong` (24228) out. With both, the decision model keeps 5 of its 13 needed
lines (was 1) and the summary names the `LiveNamespaces` and `NoSuchFileException` failures and points at their lines,
and every range is now shown within its share of 80 rows rather than only the first within 40; the summary still keeps
only 2 of the 13 needed lines, because its ranges point at the first report of the missing class (a
`NoClassDefFoundError` at 13394, which names `LiveNamespaces` with slashes) and Gradle's failure block, not at the
`ClassNotFoundException` lines the needs name. The prompt asks for a range for every error or cause the summary names.

git-log-stat's commit was lost after that change because of where windows started (see Windows above); the offset set of
windows keeps it. Sharing 80 rows among all ranges, against the first range plus what fits 40, was chosen on the
captures: 40 rows shared lost needed lines in the middle of long passages in four cases, 60 in two, 80 and 100 in none
but `pytest-v-fail` (whose `FAILED` progress line the summary leaves out while it shows the failure block), with 2,291
rows over all cases at 80 and 2,375 at 100. Over the 28 captures that are not counts or lists, the summary keeps every
needed line in 26 (was 25), the decision model alone in 27.

Summaries then got leaner (`summaryVersion` 4). In real sessions a summary that answered the description still came
with up to 80 rows of the stretches it pointed at, so the prompt now asks only for the lines the agent must read as
printed, or none, and the rows shrank to 30 shared, 1 line of context each. A passage longer than its share now drops
its context lines first and shows its start rather than its start and end: at 20 rows with start and end, gradle-browser
lost `What went wrong` (24228), whose range was wider than its share; showing the start lost go-test-panic's line 19 at
20 rows and nothing at 30. Over the captures the summary keeps what it kept before, pytest-v-fail's `FAILED` line now
too and javac-errors 6 of 16 needed lines (was 3), except git-log-p's `commit` line (429): the summary quotes its hash
but points at the commit's subject, the model's choice. Rows shown dropped from 22-80 per summarized capture to 5-30
(build-log 1,560 bytes to 908, git-diff-big 5,120 to 1,619, gradle-browser 8,859 to 3,562). Two real outputs from a
zenit-workspace session, three runs each: a 150-line `sqlite3` listing went from 4.7-8.2 KB to 2.8-3.4 KB with the same
findings (the 1970 timestamps, the largest lock waits); a 124-line task listing went from 1.9-2.1 KB to 1.2-2.1 KB, and
one of the three new summaries no longer said that no timing lines were shown.

The dedup ratio (log lines over entries) also moves when a summary starts: past 1.5 the 150-line and 12 KB limits are
divided by ratio/1.5, floored at 50 lines and 3 KB (`summaryDue`). The recorded set holds no small repetitive output
(every capture under the old limits shrinks 1.06 times or less: session-e2e 1.06, session-zemble-help 1.03, the rest
1.00), so the curve changes nothing there and is not measured below the old limits; gzip ratio was not used, since
failing-test compresses 23 times from one 20,000-character row while its lines do not repeat. Files that nib's own
commands read count as logs by name (`*.log`, `*.out`, `*.log.N`, or a `log`, `logs` or `test-logs` directory, when git
does not track them) or by content (100 lines or more shrinking 2 times or more); their rows go out as program output.
Ratios of nib's tracked files top out at 1.42 (`internal/patch/testdata/corpus.json`); the recorded logs run from 1.0
(git logs, listings) to 14 (build-log), gradle-browser 2.94, the Go test logs 1.9 to 2.4, so content alone catches the
test and build logs, and the name rule the rest.

## 17. Tails

An agent that pipes a long run into `tail -N` hands the cut only N rows, so neither the decider nor the summarizer ever
saw such output, and a cause printed before the tail cost another call. nibash's `tail` of a real program's output at
the model's end already kept everything it read in a log; it now hands the sink a `tailed` (the log's text, its saved
path, how many lines it read and showed), whose note is a fact, and `sink.render` calls `tailed.check` before the facts
are written.

`check` acts only with a decision model or a summarizer (the keyword decider alone would add keyword hits to output
the agent filtered on purpose), only when the lines the tail left out are past `summaryDue`'s limits, and only when the
call has a description, failed or timed out, as for a summary. It asks `choose`, the function `shape` uses for a long
output's middle, about the lines the tail left out (the decision model reads the whole log's deduplicated entries that
cover them), so the candidates are those lines and the tail plays the part of the kept end. Picks rated
`PickThreshold` or more go in `novelFirst` order, a pick mostly like the tail's own lines waiting, each widened by
`pickContext` lines. No pick leaves the plain note. Up to `tailAdded` (30) rows and `tailAddedBytes` (8 KB) of picks
follow the tail's output under one header naming the left-out lines, the log, who chose and for what; gaps between
them are named as everywhere, those before the first and after the last are left to the header. Past 30 rows with a
summarizer, the summarizer reads the whole log (all entries, or past `excerptLines` the excerpt of the picks, the last
100 entries, the tail included) and `summaryPassages` shows the passages it points at that the tail left out; one it
points at in the tail is not shown twice. A failed summary falls to the capped rows with `no summary`; a failed model
to the plain note with the failure. A judgement that every line counts says so instead of adding rows.

Two halves of the first idea were dropped. `head` is not checked: the program before it is closed once `head` has its
lines, as in bash, so the rest was never printed, and reading on would change what the command does. And the exit
status of the program before the pipe is not a trigger: the interpreter keeps only the pipeline's status, `tail`'s,
and matching a real program to the pipe a `tail` reads is not reliable, so the call's own status is what counts.

## 18. skim: answers from long files

An agent that wants what a long file says (a test or build log, a report, a data dump), rather than lines to edit,
reads it with the `skim` tool, whose required `expect` is the one-sentence purpose a decider and a summarizer need. The
server offers `skim` wherever it offers nibash and hands each call to `Shell.ReadLog`; `read` never takes `expect`, so a
read always gives rows to edit from. The tool's name is declared once, as `engine.Skim`; the registration, the notes
and the tests derive from it, so a rename is that one Go edit plus the docs.

`engine.ReadLog` resolves each path as read does (sandbox, links, named directories, selectors) and returns the file's
bytes from an offset on, at most `logLimit`, and the ranges a selector chose; an open range runs to the end. nibash
turns them into plain lines (`plainLines`, as for program output), collapses exact repeats (`plainItems`, the same
code `collect` uses) and calls `fit`, the function `render` calls, so the limits, `shape`, `choose`, the summary and
every note are nibash's own. Only three things differ, all carried by `saved`: notes say `skim` instead of `nibash`,
the log is the file itself (`whole file: PATH`, `read PATH:A-B plain:true`), and `base` shifts the numbers notes and cut
rows print. The decision model reads rows without numbers; the summarizer reads them under the file's own numbers
(`SummaryInput.Base`), its prompt saying which (`Output: lines 501-600 of a file`), and its ranges are taken back to the
window's before they are checked, so a summary that names a line names the one the agent sees; with no base the prompt
is the recorded one. The decision gives the models `cat PATH` as the command, `expect` as the purpose and exit status 0,
so the prompts stay those the answers were recorded with; notes say `for what you expect`.

Each agent's shell session remembers, per file read whole, its size, its first 4 KiB, the whole lines read and the
offset after them, and how much of a last line without its line break was shown. The next whole read starts at that
offset, so the window is what was added, numbered after the lines already read; nothing added, or the same unfinished
line, answers with one line. A smaller file or another start reads whole again and says so. A selector neither uses nor
moves the mark. The shell session is per agent, so a subagent's reads never move its parent's mark, and closing it
forgets the marks.

## 19. nib shape: logs other tools keep

A tool that runs builds or tests for agents and keeps their output in its own log (zenit-dev's MCP tools are the first)
can give the agent what nibash would have shown, with no second call: `nib shape --log PATH --purpose TEXT [--exit N]
[--command TEXT]` prints `nibash.ShapeLog` of the file. It is the same path as one `skim` window, `shapeLines`, with
a `saved` that is nibash's own rather than skim's: notes say `nibash`, the whole output is `PATH` itself (no copy), and
numbers start at 1. The decision gets `--command` (default `cat PATH`), `--purpose` as the call's description and
`--exit`, so the models read what they would for the run itself. No `[exit N]` line is printed, since the tool states its
own verdict; with no agent known, a recurring block's markers are always explained. The decider and summarizers come
from the same environment and remembered choice the server reads (`deciderChoice` in `cmd/nib`).

**The session's server.** Run in a nib server's Claude Code session or under one of its nibash programs, `nib shape`
hands the request to that server, so the session's state applies. Each stdio server listens on a socket in nib's
runtime folder (`server.RuntimeDirectory`, also nibash's log folder) named after Claude Code's session,
`session-claude-<CLAUDE_CODE_SESSION_ID>.sock`, which every program of that session sees in its environment, or else
after the server's own process; mode 0600 in a folder only the user may enter. The first server of a session keeps the
socket and later ones skip it, and a socket a dead server left is replaced. The server names its socket to the programs
nibash starts in `NIB_SHAPE_SOCKET`. `nib shape` asks the session's socket, then that one; a missing, dead or unreadable
socket, an older nib without one, or an answer it cannot read makes it shape the log itself. The request is one JSON line
(log, purpose, command, exit) and the answer one JSON object; a refusal (an unreadable log, or under the sandbox one
outside the roots the client's calls listed, none before any were) comes back with status 2 and is not shaped by the
caller.

The server shapes with `Shell.Shape`: the same `ShapeLog` with its own decider and its live summaries switch, a model
call of kind `shape` named after the log while it runs, which the mod's band shows (the mod polls nib while any other MCP
server's tool runs, since that tool may be the wrapper), and the result counted in the token report under `nib shape`.
What it showed of the whole log becomes a skim mark every agent's next `skim` of the file goes on from, as the agent that
asked is unknown; an agent's own later mark wins when it reaches further.
