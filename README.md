# trace-brain

Continuous memory layer over Trace history.

Licensed under [Apache-2.0](LICENSE).

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Commands
- `trace-brain version`
- `trace-brain status`
- `trace-brain remember <content> [--scope]`
- `trace-brain get <id>`
- `trace-brain update <id> <content> [--scope]`
- `trace-brain history <id>`
- `trace-brain forget <id>`
- `trace-brain verify <id>`
- `trace-brain workspace query <words> --databases PATH [--databases PATH]`
- `trace-brain embed --model EXACT_INSTALLED_MODEL [--endpoint LOCAL_ORIGIN]`
- `trace-brain semantic <query> --model EXACT_INSTALLED_MODEL`
- `trace-brain capabilities`
- `trace-brain recall <query>`
- `trace-brain query <query>`
- `trace-brain overview`
- `trace-brain brief <task>`
- `trace-brain setup [trace.db]` (import sessions from Trace)
- `trace-brain doctor`
- `trace-brain mcp [--semantic-model EXACT_INSTALLED_MODEL] [--semantic-endpoint LOCAL_ORIGIN]` (MCP stdio server; learned search defaults off)
- `trace-brain refresh [--trace-db .trace/trace.db]` (import and return protocol-1 JSON)
- `trace-brain context [--query words] [--limit 20]` (bounded, read-only protocol-1 JSON)

## Build
`go build ./...`

## Reliability

In an initialized Trace project, new databases default to `.trace/brain/memory.db`. A pre-existing `.trace-brain/memory.db` is retained, and explicit `--db` overrides both. With the binary on the Trace daemon's `PATH`, CLI imports history after completed turns and sessions. `trace context` and MCP `get_context` combine these memories with history and Graph output. Manual memories remain local and survive refresh; another checkout rebuilds derived memories from recovered history. Context treats task words literally, caps results/content, and never creates a missing database or invokes extraction.

Database writers reserve their SQLite write transaction before reading import fingerprints, so concurrent manual saves and daemon refreshes wait rather than fail with a stale-snapshot lock. Concurrent first opens retry transient WAL initialization contention.

`setup` imports sessions and turns transactionally. Repeating it updates the same source entities without creating new memories. `extract` skips unchanged sessions, including external agent calls, and reports failures. Existing unlinked duplicates created by older versions are preserved. Refresh removes derived `trace-import` memories whose source entity was deleted; manual/MCP notes are retained.

`viz` escapes memory content as text and writes beside the selected `--db`. `query` is an alias in behavior for `recall`.

`watch` holds a per-database lock and supports `--daemon`. `stop` sends a private, instance-specific request and waits for an acknowledgement; it never signals a PID. Legacy PID-only watcher records are rejected. Stop an old watcher through its original terminal before upgrading. Extraction commands have a 30-second timeout.

Run `go test -race ./...` and `go test -race -run TestRegression -count=100 ./internal/cli` to verify the regression cases. macOS and Linux are supported.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, race tests, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation. Context queries have a two-second deadline and keep at most 50 ranked candidates in memory. The existing context `--semantic` flag uses hashed word projections; it does not invoke a learned model. The separate explicit `embed` and `semantic` commands support learned vectors through a local Ollama service.

MCP recall/query return at most 50 memories with 2,048 characters of content each. New manual/MCP memories are limited to 64 KiB and save their embedding atomically. Extraction caps stdout at 64 KiB and stderr at 4 KiB, cancels noisy subprocesses, and holds no memory-database transaction while waiting on an external agent. Successful extracted memories commit independently; an error leaves previous successful extractions intact for idempotent retry.

Production builds select Go 1.27.3 to include the patched certificate verifier. CI pins this compiler and runs Govulncheck. The language version remains Go 1.26; release builds reject a different compiler. Existing runtime dependencies are pinned to their reviewed updates in go.mod/go.sum.

## Extraction and retrieval quality

Extraction now receives up to 32 recent turns and 50 file-change observations,
within 48 KiB of structured evidence. It includes recorded responses and turn
IDs, treats quoted text as untrusted data and fingerprints the sampled evidence,
so changed outcomes invalidate an earlier extraction. The extractor must not infer
a successful test merely from an intention to run it. All imported, extracted,
manual and MCP memory content passes through secret-pattern redaction before storage.

Keyword retrieval ranks by SQLite FTS relevance with deterministic recency ties.
The optional hashed-word ranker omits zero-overlap results; it remains a lexical
fallback. Quality regressions cover relevant older evidence outranking recent
weak matches, changed extraction inputs, output redaction and deleted-source
cleanup. Source/session/turn provenance remains present in context responses.

## Memory lifecycle and workspace retrieval

Manual and MCP memories can be revised with `update`; the content, keyword index,
local vector and redacted revision commit together. Derived memories remain
source-owned. `history` returns at most 50 revisions. `forget` removes the memory,
its revision records, vectors, FTS entry and import links. It does not modify the
source Trace history or backups; a later refresh can reimport a derived memory.
`verify` checks whether linked source sessions/turns still exist and explicitly
does not certify the memory's conclusions. Read commands never create an index.

`workspace query` searches 1–16 explicitly selected local databases with an
optional exact `--scope` filter and a combined 1–50 result limit. It reports
unavailable sources as partial results, ranks by source rank and recency rather
than comparing unrelated BM25 magnitudes, and never copies or uploads memories.

## Explicit learned embeddings

Provide the exact name of a model already installed in Ollama. Trace never pulls
models automatically. `embed` processes at most 1,000 memories per invocation;
`--offset` continues a capped selection and `--force` recomputes existing vectors.
The model endpoint must be a loopback HTTP(S) origin. Redirects, remote endpoints,
credentials in URLs, oversized responses and silent model input truncation are
refused. Each inference batch is bounded to 16 texts and holds no SQLite transaction.

Cached vectors are tied to the endpoint, model name, observed model digest and
memory-content hash. Detected digest changes during inference abort that batch;
changed/deleted memories cannot receive an obsolete vector. Semantic queries
report missing/stale vectors and omit them from ranking. Updating or forgetting
a memory invalidates/removes its learned vectors. Ordinary context and MCP
recall/query reads do not contact a model. Protocol, cache and ranking paths have offline tests; live
Ollama acceptance and retrieval-quality benchmarks remain outstanding.

To expose learned retrieval to an agent, start
`trace-brain mcp --semantic-model EXACT_INSTALLED_MODEL`. The owner pins the exact
installed model and loopback endpoint at startup. This registers the read-only
`brain_semantic` tool; callers can select query, scope, limit and minimum score,
but cannot change the provider or model. Startup and tool discovery do not contact
Ollama. The tool embeds only its redacted query and reads existing vectors; it
never indexes memories, downloads models or changes caches. Use the explicit
`embed` command first to prepare vectors. There is no automatic keyword fallback
when learned search fails or needs embeddings.

CLI and MCP learned queries share scope filtering, identity checks and deterministic
score/recency/ID ranking. Both have a one-minute deadline, a 4 KiB UTF-8 query limit,
256-byte scope/provenance bounds, at most 100,000 scanned memories, 1–50 requested
results, UTF-8-safe redacted 2 KiB content snippets and a 64 KiB encoded result
budget. Content and result truncation are explicit. Oversized scans and malformed
provenance fail without returning a partial ranking. The learned read reports
`needs_embeddings` for a missing vector table without migrating it. Ordinary MCP
startup retains its existing schema initialization and memory-write tools. Invalid and
zero-norm stored vectors are stale. The MCP server admits one learned query at a
time and rejects overlapping inference; cancellation releases its slot. Private
backend error details are not forwarded through MCP. Returned memory content is
untrusted quoted evidence, not instructions or a truth certification. Learned
context aggregation and live installed-model quality/latency acceptance remain open.

## Local document sources

`documents sync --root /absolute/docs --scope docs --dry-run` previews an explicit
source. Omit `--dry-run` to import `.md`, `.markdown`, `.txt`, `.rst` and `.docx` files.
Sources are read-only, text is redacted before storage, and symlinks, protected
directories and Git-ignored paths are excluded before file content is read.
Plain-text files must contain valid UTF-8, without NUL, up to 128 KiB each. A scan is bounded
to 512 candidate files, 16 MiB each of input and extracted text, 10,000 entries and 4,096 memory chunks.
Oversized or failed scans leave the index unchanged.

DOCX ingestion follows the package's main-document relationship and verifies its
non-macro Word content type. It extracts UTF-8 WordprocessingML main-body paragraphs
and table text, including inserted revisions. Paragraph boundaries, tabs and line
breaks are retained. The implementation follows the [WordprocessingML structure](https://learn.microsoft.com/en-us/office/open-xml/word/how-to-open-and-add-text-to-a-word-processing-document)
and [package relationship identity](https://learn.microsoft.com/en-us/previous-versions/windows/desktop/opc/parts-overview).
Comments, headers, footers, footnotes/endnotes, drawings, deleted/moved-from revisions
and field instructions/results are omitted. Layout, numbering and style resolution
are not rendered. Images, macros, embedded objects, external content and formulas
are not executed or fetched. Only the selected parts are decompressed in memory;
no archive member is written to disk.

DOCX limits are 2 MiB of ZIP input, 256 entries, 32 MiB of declared expanded data,
64 KiB per package metadata part, 4 MiB for main XML, 200,000 XML tokens, depth 128
and 128 KiB of extracted text. Encrypted packages, ambiguous identities, unsafe
part names, unsupported main types, malformed selected XML and selected-part CRC
failures stop the whole scan before database mutation. Fingerprints bind the raw
package to the extractor version. Extracted text is redacted and stays local under
the same refresh/prune/forget policy as ordinary document sources. PDF, other
Office formats and remote connectors remain unimplemented.

Refresh tracks source fingerprints and repairs changed or forgotten derived
chunks atomically with their lexical/hashed-word indexes. Changed source chunks
lose obsolete learned vectors. `documents list --root /absolute/docs` reports the
stored fingerprints and import times; it does not claim that an unrefreshed source
is current. Missing/excluded documents remain reported until `sync --prune`
explicitly removes their local records. `documents forget-source --root PATH`
removes an entire derived source, including one whose directory no longer exists.
Source files and backups are never changed. A later sync can reimport a source.

Documents are local-only and are excluded from automatic hosted context snapshots.
Use local recall, explicit workspace retrieval or learned search to query them.
Remote connectors, PDF/Office formats and document sharing remain unimplemented.

## Temporal claims

`facts assert SUBJECT PREDICATE OBJECT --evidence MEMORY_ID --from RFC3339`
records a claim and an immutable copy of the current redacted local evidence. `--until`
sets an exclusive validity end. Without `--from`, validity starts now. Scopes are
exact and default to `project`. Text fields are redacted; claims are never treated
as certified truth. `--exclusive` rejects overlapping different values for the
same scoped subject and predicate, including a new nonexclusive value that would
violate an existing exclusive claim. Resolve a conflict by reviewing and explicitly
retracting the previous claim; no conclusion is silently replaced.

`facts query --at RFC3339 --known-at RFC3339` separates the time a claim applies
from when it was recorded or retracted. `facts retract ID` preserves prior
recorded-time queries. Queries default to the **current** memory hash; changed
evidence is marked stale and excluded unless `--include-stale` is selected.

`facts query --evidence-mode captured` and the same flag on `facts graph` instead
require the original captured bytes to match the assertion hash. The current
staleness flag remains visible. `facts evidence ID [--known-at RFC3339]` reads the
original redacted copy, including after source edits or claim retraction, within
the exact `--scope`. Neither mode certifies the claim or its source as true.
Legacy assertions without a capture are never backfilled from today's content;
captured mode reports them as unavailable. This retains assertion evidence, not
an arbitrary historical file or external document revision.

Each capture is at most 64 KiB. Content bytes plus a 1 KiB reservation per capture
share a 32 MiB local application budget; this is not the physical SQLite file
size. Acceptance and reservation are atomic. At capacity, remove an unused claim
with `facts forget ID`; this deletes its captured copy and keeps the source memory.
Retraction preserves evidence and its reservation. Forgetting a source memory,
or removing its derived record during refresh, deletes its facts and captures
and releases their reservations. Assertions and captures stay local and are not
uploaded.

`facts graph ENTITY --depth 3 --direction both` traverses scoped relationships,
handles cycles and returns at most 100 assertions. Reads have a two-second deadline,
candidate/content budgets and explicit truncation. The read-only `brain_facts` MCP
tool accepts validity/recorded times as Unix milliseconds and an `evidence_mode`.
The read-only `brain_fact_evidence` tool caps content at 16 KiB, preserving valid
UTF-8 and reporting the full capture's byte count, hash and truncation. Evidence
content remains untrusted quoted data. Natural-language entity
extraction, automatic contradiction resolution and comparative graph-quality
benchmarks are still pending.
