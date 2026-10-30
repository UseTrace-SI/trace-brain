# trace-brain

Continuous memory layer over Trace history.

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

## Build
`go build ./...`

## Reliability

`setup` imports sessions and turns transactionally. Repeating it updates the same source entities without creating new memories. `extract` skips unchanged sessions, including external agent calls, and reports failures. Existing duplicates created by older versions are preserved; no historical memories are deleted automatically.

`viz` escapes memory content as text and writes beside the selected `--db`. `query` is an alias in behavior for `recall`.

`watch` holds a per-database lock and supports `--daemon`. `stop` sends a private, instance-specific request and waits for an acknowledgement; it never signals a PID. Legacy PID-only watcher records are rejected. Stop an old watcher through its original terminal before upgrading. Extraction commands have a 30-second timeout.

Run `go test -race ./...` and `go test -race -run TestRegression -count=100 ./internal/cli` to verify the regression cases. macOS and Linux are supported.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.
