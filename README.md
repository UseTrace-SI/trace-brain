# trace-brain

Continuous memory layer over Trace history.

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Commands
- `trace-brain version`
- `trace-brain status`
- `trace-brain remember <content> [--scope]`
- `trace-brain recall <query>`
- `trace-brain query <query>`
- `trace-brain overview`
- `trace-brain brief <task>`
- `trace-brain setup [trace.db]` (import sessions from Trace)
- `trace-brain doctor`
- `trace-brain mcp` (MCP stdio server)
- `trace-brain refresh [--trace-db .trace/trace.db]` (import and return protocol-1 JSON)
- `trace-brain context [--query words] [--limit 20]` (bounded, read-only protocol-1 JSON)

## Build
`go build ./...`

## Reliability

In an initialized Trace project, new databases default to `.trace/brain/memory.db`. A pre-existing `.trace-brain/memory.db` is retained, and explicit `--db` overrides both. With the binary on the Trace daemon's `PATH`, CLI imports history after completed turns and sessions. `trace context` and MCP `get_context` combine these memories with history and Graph output. Manual memories remain local and survive refresh; another checkout rebuilds derived memories from recovered history. Context treats task words literally, caps results/content, and never creates a missing database or invokes extraction.

Database writers reserve their SQLite write transaction before reading import fingerprints, so concurrent manual saves and daemon refreshes wait rather than fail with a stale-snapshot lock. Concurrent first opens retry transient WAL initialization contention.

`setup` imports sessions and turns transactionally. Repeating it updates the same source entities without creating new memories. `extract` skips unchanged sessions, including external agent calls, and reports failures. Existing duplicates created by older versions are preserved; no historical memories are deleted automatically.

`viz` escapes memory content as text and writes beside the selected `--db`. `query` is an alias in behavior for `recall`.

`watch` holds a per-database lock and supports `--daemon`. `stop` sends a private, instance-specific request and waits for an acknowledgement; it never signals a PID. Legacy PID-only watcher records are rejected. Stop an old watcher through its original terminal before upgrading. Extraction commands have a 30-second timeout.

Run `go test -race ./...` and `go test -race -run TestRegression -count=100 ./internal/cli` to verify the regression cases. macOS and Linux are supported.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation. Context queries have a two-second deadline and keep at most 50 ranked candidates in memory. Local embeddings are hashed word projections rather than a learned semantic model.

MCP recall/query return at most 50 memories with 2,048 characters of content each. New manual/MCP memories are limited to 64 KiB and save their embedding atomically. Extraction caps stdout at 64 KiB and stderr at 4 KiB, cancels noisy subprocesses, and holds no memory-database transaction while waiting on an external agent. Successful extracted memories commit independently; an error leaves previous successful extractions intact for idempotent retry.

Production builds select Go 1.27.3 to include the patched certificate verifier. CI pins this compiler and runs Govulncheck. The language version remains Go 1.26; release builds reject a different compiler. Existing runtime dependencies are pinned to their reviewed updates in go.mod/go.sum.
