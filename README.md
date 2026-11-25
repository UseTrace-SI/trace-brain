# trace-brain

Continuous memory layer over Trace history.

## Quickstart (5 minutes)

```sh
go build -o trace-brain ./cmd/trace-brain   # or use the binary shipped in the Trace bundle

# In a project initialized with Trace (it has .trace/trace.db), memories live in
# .trace/brain/memory.db; elsewhere in .trace-brain/memory.db. --db overrides both.
./trace-brain setup                 # import Trace sessions and turns, extract facts
./trace-brain status                # memories: N

# Save and find notes. Search words are literal and stemmed: "decide" finds "decided".
./trace-brain remember "We decided to keep migrations in Go code"
./trace-brain recall decide
./trace-brain brief "fix the flaky tests"
./trace-brain context --query "flaky tests"   # protocol-1 JSON with provenance

# Give an agent the memory tools over MCP stdio (brain_search, brain_remember, ...).
# MCP clients may start servers in another directory, so pass an absolute --db:
./trace-brain --db "$PWD/.trace/brain/memory.db" mcp
```

An MCP client entry looks like
`{"command": "trace-brain", "args": ["--db", "/abs/project/.trace/brain/memory.db", "mcp"]}`.
Run `trace-brain <command> --help` for flags and examples.

Licensed under [Apache-2.0](LICENSE).

The coordinated Trace bundle supplies this executable beside the other two
local components, with one release version. With an approved personal connection,
CLI publishes bounded derived context to the hosted dashboard. Raw companion
SQLite files and manual/MCP-authored Brain notes remain local. Graph snapshots
describe the checkout when indexed, with their index timestamp.

## Commands
- `trace-brain version` (release version)
- `trace-brain status` (memory count)
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
- `trace-brain recall <query>` (keyword search, up to 20 results)
- `trace-brain query <query>` (alias of `recall`)
- `trace-brain overview`
- `trace-brain brief <task>` (up to 10 relevant memories)
- `trace-brain dash` (memory count and the 10 most recent memories)
- `trace-brain viz` (writes `viz.html` beside the database)
- `trace-brain setup [trace.db]` (import sessions from Trace)
- `trace-brain doctor` (opens the database, applying pending migrations)
- `trace-brain extract [--agent CMD] [--trace-db PATH]` (per-session evidence memories, summarized by the agent command when one is given)
- `trace-brain watch [--agent CMD] [--interval 60] [--daemon]` and `trace-brain stop` (periodic extraction)
- `trace-brain documents sync --root DIR [--scope S] [--dry-run] [--prune]`, `documents list --root DIR`, `documents forget-source --root DIR` (see Local document sources)
- `trace-brain mcp [--semantic-model EXACT_INSTALLED_MODEL] [--semantic-endpoint LOCAL_ORIGIN]` (MCP stdio server; learned search defaults off)
- `trace-brain refresh [--trace-db .trace/trace.db]` (import, extract claims and return protocol-1 JSON)
- `trace-brain facts [--subject S] [--at TIME] [--known-at TIME] [--history] [--json]` (current or historical claims)
- `trace-brain facts extract [--limit N] [--json]` (offline rule extraction)
- `trace-brain facts extract --model EXACT_INSTALLED_MODEL [--endpoint LOCAL_ORIGIN]` (opt-in learned extraction)
- `trace-brain facts assert|query|graph|evidence|retract|forget ...` (see Temporal claims)
- `trace-brain context [--query words] [--limit 20] [--facts N]` (bounded, read-only protocol-1 JSON)

## Build
`go build ./...`

## Reliability

In an initialized Trace project, new databases default to `.trace/brain/memory.db`. A pre-existing `.trace-brain/memory.db` is retained, and explicit `--db` overrides both. With the binary on the Trace daemon's `PATH`, CLI imports history after completed turns and sessions. `trace context` and MCP `get_context` combine these memories with history and Graph output. Manual memories remain local and survive refresh; another checkout rebuilds derived memories from recovered history. Context treats task words literally, caps results/content, and never creates a missing database or invokes extraction.

The memory database records its schema in `PRAGMA user_version`. Opening a database applies any pending numbered migrations once, inside one IMMEDIATE transaction, and upgrades existing files in place without data loss; an up-to-date database is not rewritten on open. A database written by a newer release is refused rather than modified, by writable opens and by read-only commands alike: `context`, `get`, `history`, `verify`, fact reads and learned reads fail with an explicit error (Trace then reports the brain companion as failed), and `workspace query` reports that source as unavailable. Read-only commands never migrate; older schemas stay readable. An upgrade holds the IMMEDIATE write transaction for its whole duration (rebuilding the keyword index and re-fingerprinting every memory: roughly 0.5–2 s per 20,000 memories, about 7 s for 200,000), so concurrent writers and openers wait, up to SQLite's 30-second busy timeout, until the first open after an upgrade finishes.

Database writers reserve their SQLite write transaction before reading import fingerprints, so concurrent manual saves and daemon refreshes wait rather than fail with a stale-snapshot lock. Concurrent first opens retry transient WAL initialization contention.

`setup` imports sessions and turns transactionally. Repeating it updates the same source entities without creating new memories. Turn text is redacted before it is cut; if it exceeds the 64 KiB memory budget, the derived memory keeps its beginning and end around an explicit `bytes omitted` notice, and invalid UTF-8 is replaced, so one oversized turn never blocks the rest of the history. For turns over 512 KiB only the first and last 256 KiB (at line boundaries) are redacted and considered, which bounds the per-refresh cost while any secret overlapping the kept text is still redacted whole. `extract` skips unchanged sessions, including external agent calls, and reports failures. It re-checks the evidence fingerprint inside its write transaction after the agent runs, so a concurrent refresh cannot commit a stale extraction. Existing unlinked duplicates created by older versions are preserved. Refresh removes derived `trace-import` memories whose source entity was deleted; manual/MCP notes are retained.

`viz` escapes memory content as text, writes beside the selected `--db`, and renders at most the 1,000 most recent memories with explicit truncation reporting. `query` is an alias in behavior for `recall`.

`watch` holds a per-database lock and supports `--daemon`. `stop` sends a private, instance-specific request and waits for an acknowledgement; it never signals a PID. Legacy PID-only watcher records are rejected. Stop an old watcher through its original terminal before upgrading. Extraction commands have a 30-second timeout.

Run `go test -count=1 ./...` (which also enforces the absolute performance budgets), `go test -race -count=1 ./...` (liveness waits are scaled and absolute budgets skipped under the race detector) and `go test -race -count=100 -run 'TestRegression|TestCompanion' -timeout=30m ./...` to repeat the regression cases, matching `.github/workflows/ci.yml`. macOS and Linux are supported.

## Continuous integration

`.github/workflows/ci.yml` runs pure Go builds, vet, staticcheck, tests with and without the race detector, and 100 repetitions of the regression cases on GitHub-hosted macOS and Linux runners for pushes, pull requests, and manual runs. The workflow has read-only repository access. Hosted execution begins after these changes are pushed.

## Production bounds

Ordinary search text is interpreted as literal words, including punctuation. Context queries have a two-second deadline. Keyword reads fetch at most five times the requested limit (at most 250 candidates) and read content only for those candidates. The existing context `--semantic` flag uses hashed word projections; it does not invoke a learned model. The separate explicit `embed` and `semantic` commands support learned vectors through a local Ollama service.

The MCP server reports the binary's release version. Its preferred keyword tool is
the read-only `brain_search` (`query`, `scope`, `since` as an RFC3339 time or a
relative age such as `36h` or `7d`, `limit` 1–50). It searches the query's literal words
and echoes them after redacting the query as a whole (so no fragment of a secret is
returned, though a word redaction removes is still searched). It returns
ranked memories with `source`, `session_id`, `turn_id`, `created_at`, `score` and
`content_truncated`; an empty query lists recent memories. `brain_recall` and
`brain_query` (and `brain_status`/`brain_overview`) remain as backward-compatible
aliases with their original result shapes. Every tool has a description and
input-schema descriptions, and read-only tools carry the MCP `readOnlyHint`
annotation. Status/overview, like the other memory tools, have a two-second deadline.

MCP recall/query return at most 50 memories with 2,048 characters of content each (the same cap applies to CLI recall/query/brief); `history` caps each revised version at 2,048 runes and marks truncation explicitly. New manual/MCP memories are limited to 64 KiB after redaction and save their embedding atomically. Learned reads use a small connection pool so a long semantic scan cannot starve other MCP tools; writes remain serialized by IMMEDIATE transactions. Extraction caps stdout at 64 KiB and stderr at 4 KiB, cancels noisy subprocesses, and holds no memory-database transaction while waiting on an external agent. Successful extracted memories commit independently; an error leaves previous successful extractions intact for idempotent retry.

Production builds select Go 1.27.4 to include the patched certificate verifier. CI pins this compiler and runs Govulncheck. The language version remains Go 1.26. Existing runtime dependencies are pinned to their reviewed updates in go.mod/go.sum.

## Extraction and retrieval quality

Extraction now receives up to 32 recent turns and 50 file-change observations,
within 48 KiB of structured evidence. It includes recorded responses and turn
IDs, treats quoted text as untrusted data and fingerprints the sampled evidence,
so changed outcomes invalidate an earlier extraction. The extractor must not infer
a successful test merely from an intention to run it. All imported, extracted,
manual and MCP memory content passes through secret-pattern redaction before storage.
Read paths (recall/query/brief, context, workspace retrieval, dash, viz, get,
history and every fact listing, query, graph and MCP fact tool) re-apply redaction
to stored text, so a legacy row cannot bypass the current policy.

Keyword search stems English words (SQLite FTS5 `porter unicode61`), so `test`
matches "tests" and `decide` matches "decided". Common stopwords ("the", "of",
"what", …) are ignored unless a query has nothing else; negations such as
"not" are kept. Memories containing every remaining word rank above memories
matching only some of them. Schema 2 rebuilds the keyword index from stored
memories on the first writable open; read-only commands against a database not
yet upgraded keep using its unstemmed index.

Every keyword surface (CLI `recall`/`query`/`brief`, `context`, `workspace query`
and the MCP `brain_search`/recall/query/brief tools) uses one search path. It fills a window of
five times the requested limit with all-word matches, then any-word matches,
each by SQLite FTS (BM25) relevance. Within each tier it ranks by a
blend of BM25 normalized against the best candidate (75%), recency with a
30-day half-life (15%) and a fixed boost for manual/MCP notes (10%), with
deterministic recency/ID ties. Near-duplicates (the same scope and the same text
ignoring only letter case and whitespace, compared over the 2,048-character snippet
and the last 256 characters) are collapsed to the best-ranked copy; punctuation and
symbols are significant, so "x > y" and "x < y" stay distinct.
The optional hashed-word ranker omits zero-overlap results; it remains a lexical
fallback. Quality regressions cover relevant older evidence outranking recent
weak matches, changed extraction inputs, output redaction and deleted-source
cleanup. Source/session/turn provenance remains present in context responses.

## Redaction markers

A redacted value is replaced by `⟨redacted:KIND:XXXXXXXX⟩`, where the digits are
an HMAC-SHA256 of the value under a per-install key. trace-brain shares the Trace
CLI's key file, `$TRACE_HOME/redaction.key` (an absolute `TRACE_HOME`) or
`~/.trace/redaction.key`, and its creation rules: a missing or damaged key is
created as a 0600 file in a 0700 directory and published with a hard link, so
concurrent first runs of any Trace component agree on one key; a damaged key is
replaced under an advisory lock (`redaction.key.lock`), moved aside to
`redaction.key.damaged` and never overwritten in place; a symlink or other
non-regular file is never used; a group/world-readable key is tightened to 0600.
A marker no longer confirms a guessed secret, and when trace-brain and the Trace CLI
detect the same span of text with the same rule kind they produce the same marker.
Their detection rules are not yet shared, however: the rule sets, minimum lengths and
what a rule includes in the hashed value (for example surrounding quotes or a label
name) differ, so the same secret can still be redacted with a different marker, or
redacted by only one component. Aligning the rules in a shared module is follow-up
work. `trace-brain doctor` reports which key is in use.

If the shared key cannot be read or created, markers use an ephemeral per-process
key (still unguessable, but not equal across processes). Nothing that is compared
over time depends on marker digests: learned-vector fingerprints (`content_hash`)
and Trace import change detection compare redacted text with marker digests
elided, so a missing key never makes fingerprints stale or re-imports history.
Document refresh compares exact chunks, so with an ephemeral key, document chunks
containing secrets are rewritten on each `documents sync`.

Schema 4 (redaction algorithm `redact-v3`) recomputes every stored fingerprint, so
learned vectors cached by earlier releases are reported stale until the next
`embed` run. Memories already stored keep the unkeyed markers written by earlier
releases: manual/MCP notes, revisions, captured fact evidence and document chunks
are not rewritten. Derived Trace memories that contain a secret are re-imported
once by the next refresh and then carry keyed markers; claims citing them as
current evidence are re-extracted or reported stale like any other source change.
Use `update` or `forget` to replace old markers in a manual note.

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
a fingerprint of the exact redacted text sent to the model plus a redaction
algorithm version, so changed redaction rules invalidate cached vectors. Detected digest changes during inference abort that batch;
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
budget. Content and result truncation are explicit. Oversized scans fail without
returning a partial ranking. Each memory stores the fingerprint of its redacted
content (schema 3, backfilled once on upgrade: about 0.5–2 s per 20,000 memories),
so cached vectors are matched in SQL; only rows without a stored fingerprint and
the rows about to be returned are read, re-verified and redacted. A content change
by an external writer clears the stored fingerprint; trace-brain's own writers set it
again even when only marker digests changed, and schema 5 restores fingerprints that
earlier builds of this branch left cleared. A top-ranked memory whose
provenance exceeds the output bounds or contains protected text is skipped and
counted in `skipped_malformed` with a `warnings` entry instead of failing the query. The learned read reports
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
violate an existing exclusive claim. Resolve a conflict with another manual claim
by reviewing and explicitly retracting the previous claim; no manual conclusion is
silently replaced. The one exception is an older automatically extracted claim:
a newer manual assertion closes it (see below) and lists it under `supersedes`.

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
content remains untrusted quoted data. Comparative graph and extraction quality
benchmarks are still pending.

## Automatic fact extraction and invalidation

After the import commits, `setup` and `refresh` run a deterministic, offline,
rule-based extractor. It reads Trace turn memories (the prompt, plus reasoning
paragraphs that start with `Assistant response:` or `Agent output:`; unlabelled
and `Thinking:` reasoning is skipped, which also drops later paragraphs of a
multi-paragraph response) and manual, MCP and agent-extract memories. Documents and session headers are not extracted, and
extraction never writes a memory, so manual notes are unchanged. The rules
recognize only explicit statements:

- decisions: "we use X for Y", "use X for Y", "X is our Y", "switched/migrated
  from A to B [for Y]", "we replaced A with B", "we no longer use X";
- constraints and preferences: "always …", "never …", "do not …", "prefer X over Y";
- dependencies: package-manager commands (`npm install`, `go get`, `pip install`,
  `cargo add`, … and their remove forms), "added/removed the dependency on X",
  "A depends on B";
- structure and references: "Sym is defined in path", "moved Sym to path", and file
  paths or backticked symbols mentioned in a turn (`referenced_in turn:ID`).

Questions, hedged or conditional sentences ("maybe", "if", "when", "could",
"should we", "for example"), negated statements and narrative past-tense policies
are skipped. Tool and role names are lowercased; paths and symbols keep their case.
A memory yields at most 16 claims (8 references). Within one memory only the last
statement counts for an exclusive subject/predicate and for a negation pair about
the same value ("npm install x … npm uninstall x" keeps only the removal).

Every claim links to its evidence memory, its source session/turn and the exact
redacted sentence it came from (a quote of at most 512 bytes that must occur
verbatim in the redacted memory). Subject, object and quote are redacted. A claim
is valid from the turn's recorded start, or the latest revision of a manual note.
Claims from Trace history use scope `project`; manual and MCP notes keep their
memory scope.

`uses` with a role subject, `policy`, `prefers` between two values and `defined_in`
are exclusive; `uses`/`stopped_using` and `depends_on`/`dropped_dependency` are
negation pairs. An extracted claim's validity ends where the earliest later claim
that contradicts it begins — a different value of an exclusive subject/predicate or
the negated predicate for the same value — counting only claims recorded and not
retracted at `--known-at`. This closure is derived at read time from the recorded
claims: nothing is deleted or rewritten, a query at an earlier `--known-at` still
sees the earlier interval, the result does not depend on import order, and
retracting or forgetting the newer claim reopens the older one up to the next
contradiction. Results report the effective `valid_to` and `superseded_by` (the
newer claim, its evidence memory, its start and when it was recorded).

Manual claims are never closed automatically. An extracted claim that would close
an active manual claim, or that contradicts another claim at the identical
instant, is skipped and counted in `conflicts_skipped`. A value restated by more
memories while three active claims already hold it is not recorded again
(`claims_already_held`). Extraction state is kept per memory for the rules and for
each learned model, with their version, and a trigger marks a memory pending when
its content or scope changes, so re-running adds no duplicates. Claims are recorded
at the time their write transaction begins. On re-extraction
identical claims are kept, unsupported ones are retracted (still visible at an
earlier `--known-at`), and a claim once retracted — by the extractor or with
`facts retract` — is not restored. Forgetting a memory, or refresh removing a
derived memory whose source turn was deleted, deletes its claims as before.

Each setup/refresh processes at most 500 pending memories in batches of 32 IMMEDIATE
transactions and starts no new batch after 15 seconds of extraction or 40 seconds
after the command began (Trace stops refresh at one minute); the remainder is
reported as `pending` in the refresh JSON `facts` object, and `facts extract
--limit N` (1–10,000, no new batch after 45 seconds) processes it explicitly. Extraction stops adding claims at
8,000 claims in total (of the shared 10,000) and reports `budget_exhausted`.
Extraction errors are reported as `facts.error` and never fail the import.
Extracted claims cite a quote rather than a captured copy: in the default current
mode a claim is stale once its memory no longer contains its quote, while
`--evidence-mode captured` omits extracted claims and `facts evidence` has none.

`trace-brain facts` lists claims valid at `--at` and known at `--known-at` (both
default to now) with `--subject`, `--predicate`, `--limit` (1–100),
`--include-stale` and `--json`; `--history --subject S` lists every claim about S,
including superseded values. The read-only MCP tool `brain_fact_history` returns
the same timeline (at most 50 claims, 64 KiB), and `brain_facts` results now
include origin, quote and supersession. `context --facts N` (0–20, default off)
adds up to N current, non-stale `project` claims whose subject, object or quote
contains a query word, with quotes capped at 256 bytes, the evidence memory's
`evidence_source` and `facts_truncated` reported. Like the memory list, a Trace
source limits them to claims backed by manual/MCP notes or by memories imported
from that source. Without the flag the context output is unchanged, and it cannot
be combined with `--learned-model`.

`facts extract --model EXACT_INSTALLED_MODEL [--endpoint http://127.0.0.1:11434]
[--limit 1–100]` adds opt-in learned extraction with the same local Ollama policy
as `embed`: exact installed model, loopback origin without redirects, digest pinned
before and re-checked after every request (a change aborts), bounded responses, a
five-minute deadline and no SQLite transaction while the model runs. Setup,
refresh, context and MCP never invoke it. Each memory of up to 16 KiB of redacted
text is sent once with a JSON schema at temperature 0; larger memories are skipped
and counted rather than truncated. A learned claim must use the same predicates,
quote a sentence that occurs verbatim in the memory and name its value inside that
quote; other output is rejected. Learned claims record their model and digest and
take part in the same invalidation. Each model's claims are reconciled only against
that model's earlier claims (a new digest of the same model re-checks them), so
running another model never retracts them. An oversized memory or invalid or
token-limited output is recorded as skipped only if the memory is unchanged; that
model's claims whose quote has left the memory are then retracted. Only offline protocol tests exist; live model
quality and latency are unmeasured.
