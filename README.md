# statsv1

Claude Code token usage and cost on this machine, collected from the
transcripts Claude Code already writes, and reported as a matrix that
compares sessions by the QAM components they used: Quality Harness (Q),
AI Agent Memory (A) and MultiPath Read/Write (M).

The point is to answer "is the QAM stack cheaper or dearer?" from recorded
data. Every number in the report is a sum over requests that exist on disk,
priced from a list-price table that ships with its provenance, and every
assumption the numbers rest on is counted beside them.

## What it measures

`collect` walks every Claude Code config directory it can find
(`$CLAUDE_CONFIG_DIR`, `~/.claude`, and each `~/.sandboxes/*`), reads
`projects/<slug>/<session>.jsonl` plus the subagent transcripts stored beside
each session in `<session>/subagents/agent-*.jsonl`, and stores one row per
billed API attempt in a SQLite file.

* Claude Code writes an assistant message once per content block, each copy
  carrying the same usage. The collector counts a message once, keyed by its
  `message.id`.
* When the API refused an attempt and answered with a fallback model, the
  usage block lists both attempts under `iterations`. Both were billed, under
  different models, so both are stored (as iteration 0 and 1 of one message).
* Cache writes are priced per TTL when the transcript records the split
  between 5-minute and 1-hour entries. Older transcripts without the split
  are priced at the cheaper 5-minute rate and reported as
  `assumed_ttl_tokens`.
* Synthetic assistant records (`model: <synthetic>`) carry no usage and are
  skipped.

A session is labelled by what its own transcripts show it doing:

| letter | counted when a tool call is |
|---|---|
| A | any `mcp__agentsmemory__*` tool |
| M | a Bash command running `mrw read`, `mrw write`, `mrw check`, `mrw iter`, `mrw seen` or `mrw --version` (building mrw does not count) |
| Q | a `quality-harness:*` skill, a `quality-harness:*` agent, or a Bash command running one of the gate binaries `adr-lint`, `adr-verify`, `adr-judge`, `adr-next`, `adr-debt`, `adr-retire-check`, `arch-lint`, `postmortem-verify`, `qh-mcp`, `qh-root` |

The cohort is `QAM` when all three appear, `none` when none does, and
`partial:` plus the letters present otherwise. Independently of usage, each
config directory is labelled by what it has installed (`qam-installed`,
`agentsmemory-only`, `quality-harness-only`, `plain`), read from
`.claude.json` and `plugins/installed_plugins.json`.

Human turns are user records that carry a prompt to the model: typed text, a
pasted image, or a skill invocation (`/am ...`, `/M`, `/loop ...`). Tool
results, harness-injected context, compaction summaries, background task
notifications, interrupts and built-in slash commands (`/clear`, `/effort`,
`/model`, ...) are not turns.

## What it does not measure

The transcript records only the requests the model answered as assistant
messages. Claude Code also bills side calls it makes on its own behalf, for
example the summary it generates while the person is away, which leave a
`system` record but no usage. `verify` measures that gap against Claude
Code's own per-project figures: on 2026-09-03, across 17 reference sessions,
transcript-recorded cost was 96.1% of what Claude Code recorded, with the
difference almost entirely cache-read tokens. Every cohort is a lower bound
by the same mechanism, so the ratios between cohorts are comparable while the
absolute totals are a few percent low.

The cohorts compare different sessions doing different work. A cheaper cohort
is a fact about those sessions, not the result of a controlled experiment.
Use `usd_per_turn` and `usd_per_request` as the normalised views and read the
per-session listing before drawing a conclusion.

## Commands

```
go build -o statsv1 ./cmd/statsv1

./statsv1 collect                       # every config dir found; --config-dir DIR to choose
./statsv1 report                        # matrix by cohort, environment and model (table)
./statsv1 report --format csv           # every section as CSV; --format json for JSON
./statsv1 report --since 2026-08-01 --until 2026-09-01
./statsv1 sessions --limit 30           # newest sessions with cohort, tokens and cost
./statsv1 sessions --cohort QAM --format csv --limit 0
./statsv1 compare                       # before the QAM stack arrived vs after, month by month, what changed
./statsv1 compare --at 2026-08-01       # split at a date of your choosing (the plugin install date, a month start)
./statsv1 verify                        # our cost beside Claude Code's own lastCost per project
./statsv1 prices                        # the price table and where it came from
```

`compare` answers "what changed once QAM was in use": it splits the sessions
at a date (by default the day of the first session that used quality-harness,
the last of the three components to arrive), prints both periods with the
same columns as the matrix plus `days` (calendar days with a session) and
`usd_per_day`, a month-by-month table, the cohorts inside each period, and
the after-to-before ratios. It also prints when each component first appears
in the data and when the quality-harness plugin was installed in each config
directory, so the split can be checked against the install date.

The database defaults to `statsv1.db` in the working directory
(`--db PATH` or `STATSV1_DB` to move it). Re-running `collect` is idempotent:
rows are keyed by the identities Claude Code gave them, so new transcripts
are added and existing ones updated in place. Claude Code prunes transcripts
after its cleanup period, so collect regularly if history matters.

## Prices

`internal/pricing/prices.json` holds USD per million tokens for input, output,
cache read, 5-minute cache write and 1-hour cache write, with a `verified`
flag per row and the source and date of the table. Rows marked
`verified: false` were not in the documented table the file was copied from;
cost computed from them is reported separately as `unverified_usd`. A model
missing from the table is counted in tokens and reported as
`unpriced_requests`, never guessed.

The opus-5 row and the cost formula reproduce Claude Code's own `lastCost`
to the cent on sessions whose usage the transcript records completely
(`internal/pricing/pricing_test.go`). Edit the JSON to update prices and
run `go test ./...`.

## Development

```
go build ./... && go vet ./... && gofmt -l . && go test ./...
```

Packages: `internal/usage` (domain types and the cohort rules),
`internal/pricing` (the table and the cost formula), `internal/claudecode`
(transcript discovery and parsing, environment and cost references),
`internal/store` (SQLite through gorm, goose SQL migrations), `internal/report`
(aggregation and rendering), `cmd/statsv1` (the CLI).
