package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/semantic"
	"github.com/UseTrace-SI/trace-brain/internal/store"
)

func runBrain(t *testing.T, path string, args ...string) []byte {
	t.Helper()
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--db", path}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v (%s)", args, err, &out)
	}
	return out.Bytes()
}

type listedFacts struct {
	Result struct {
		Facts     []store.Fact `json:"facts"`
		Truncated bool         `json:"truncated"`
	} `json:"result"`
}

func listFacts(t *testing.T, path string, args ...string) map[string]store.Fact {
	t.Helper()
	var listed listedFacts
	if err := json.Unmarshal(runBrain(t, path, append([]string{"facts", "--json"}, args...)...), &listed); err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Fact{}
	for _, f := range listed.Result.Facts {
		out[f.Subject+" "+f.Predicate+" "+f.Object] = f
	}
	return out
}

func traceHistory(t *testing.T, path string) *sql.DB {
	t.Helper()
	src, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT,started_at INTEGER);
INSERT INTO sessions VALUES('s','codex','repo',1000);
INSERT INTO turns VALUES('t1','s',1,'We use SQLite for storage.','',1000),
 ('t2','s',2,'Ship it','Assistant response: We switched from SQLite to Postgres for storage.',2000),
 ('t3','s',3,'Always run gofmt before committing.','',3000);`); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestCompanionRefreshExtractsClaimsIdempotentlyAndKeepsManualNotes(t *testing.T) {
	dir := t.TempDir()
	path, source := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db")
	src := traceHistory(t, source)
	manualID := strings.Fields(string(runBrain(t, path, "remember", "We use Vitest for unit tests.")))[1]
	type refreshReply struct {
		Protocol int                    `json:"protocol"`
		Status   string                 `json:"status"`
		Facts    store.ExtractionReport `json:"facts"`
	}
	var first, second refreshReply
	if err := json.Unmarshal(runBrain(t, path, "refresh", "--trace-db", source), &first); err != nil || first.Protocol != 1 || first.Status != "ready" || first.Facts.Added == 0 || first.Facts.Pending != 0 {
		t.Fatalf("first refresh: %+v %v", first, err)
	}
	if err := json.Unmarshal(runBrain(t, path, "refresh", "--trace-db", source), &second); err != nil || second.Facts.Memories != 0 || second.Facts.Added != 0 {
		t.Fatalf("second refresh was not idempotent: %+v %v", second, err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	if err := db.QueryRow("SELECT count(*) FROM facts").Scan(&total); err != nil || total != first.Facts.Added {
		t.Fatalf("duplicate claims: %d vs %d %v", total, first.Facts.Added, err)
	}
	manual, err := store.Get(context.Background(), db, manualID)
	if err != nil || manual.Content != "We use Vitest for unit tests." {
		t.Fatalf("manual note altered: %+v %v", manual, err)
	}
	db.Close()
	now := listFacts(t, path, "--subject", "storage")
	postgres := now["storage uses postgres"]
	if postgres.ValidFrom != 2000 || postgres.Origin != store.OriginExtracted || postgres.EvidenceTurnID != "t2" || !strings.Contains(postgres.Quote, "Postgres") {
		t.Fatalf("current storage claim: %+v", now)
	}
	earlier := listFacts(t, path, "--subject", "storage", "--at", time.UnixMilli(1500).UTC().Format(time.RFC3339Nano))
	sqlite := earlier["storage uses sqlite"]
	if sqlite.ValidTo == nil || *sqlite.ValidTo != 2000 || sqlite.SupersededBy == nil || sqlite.SupersededBy.FactID != postgres.ID {
		t.Fatalf("superseded claim: %+v", earlier)
	}
	if manualClaim := listFacts(t, path, "--subject", "unit tests")["unit tests uses vitest"]; manualClaim.EvidenceID != manualID {
		t.Fatalf("manual memory was not extracted: %+v", manualClaim)
	}
	// A changed source turn retracts its unsupported claim and keeps history;
	// a deleted source turn removes its derived memory and claims.
	if _, err := src.Exec("UPDATE turns SET prompt='Run gofmt when convenient.' WHERE id='t3'"); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec("DELETE FROM turns WHERE id='t1'"); err != nil {
		t.Fatal(err)
	}
	var third refreshReply
	if err := json.Unmarshal(runBrain(t, path, "refresh", "--trace-db", source), &third); err != nil || third.Facts.Retracted != 1 {
		t.Fatalf("changed source not reconciled: %+v %v", third, err)
	}
	if policy := listFacts(t, path, "--subject", "run gofmt before committing"); len(policy) != 0 {
		t.Fatalf("unsupported claim still current: %+v", policy)
	}
	history := listFacts(t, path, "--subject", "run gofmt before committing", "--history", "--known-at", time.UnixMilli(postgres.RecordedAt).UTC().Format(time.RFC3339Nano))
	if _, ok := history["run gofmt before committing policy always"]; !ok {
		t.Fatalf("retracted claim missing from earlier knowledge: %+v", history)
	}
	if old := listFacts(t, path, "--subject", "storage", "--history"); len(old) != 2 {
		t.Fatalf("deleted source turn left claims: %+v", old)
	}
	text := string(runBrain(t, path, "facts", "--subject", "storage"))
	if !strings.Contains(text, "storage uses postgres") || !strings.Contains(text, "extracted/rules") {
		t.Fatalf("text listing: %s", text)
	}
}

func TestSetupReportsExtractionAndFactsExtractCoversManualMemories(t *testing.T) {
	dir := t.TempDir()
	path, source := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db")
	traceHistory(t, source)
	out := string(runBrain(t, path, "setup", source))
	if !strings.Contains(out, "imported 3 turns") || !strings.Contains(out, "facts: ") || !strings.Contains(out, "(0 pending)") {
		t.Fatalf("setup output: %s", out)
	}
	runBrain(t, path, "remember", "We no longer use Redis for caching.")
	var reply struct {
		Facts store.ExtractionReport `json:"facts"`
	}
	if err := json.Unmarshal(runBrain(t, path, "facts", "extract", "--json"), &reply); err != nil || reply.Facts.Added != 1 || reply.Facts.Memories != 1 {
		t.Fatalf("facts extract: %+v %v", reply, err)
	}
	if again := string(runBrain(t, path, "facts", "extract")); !strings.Contains(again, "0 added") {
		t.Fatalf("explicit extraction not idempotent: %s", again)
	}
	var capabilities struct {
		Facts map[string]any `json:"facts"`
	}
	if err := json.Unmarshal(runBrain(t, path, "capabilities"), &capabilities); err != nil || capabilities.Facts["automatic_extraction"] != "offline_rules_on_setup_and_refresh" {
		t.Fatalf("capabilities: %+v %v", capabilities, err)
	}
}

type fakeGenerator struct {
	origin  string
	digests []string
	output  func(prompt string) string
	failure func(prompt string) error
	during  func()
	prompts []string
}

func (g *fakeGenerator) Origin() string { return g.origin }
func (g *fakeGenerator) Close()         {}
func (g *fakeGenerator) Digest(context.Context, string) (string, error) {
	digest := g.digests[0]
	if len(g.digests) > 1 {
		g.digests = g.digests[1:]
	}
	return digest, nil
}
func (g *fakeGenerator) Generate(_ context.Context, _ string, prompt string, _ json.RawMessage) (string, error) {
	g.prompts = append(g.prompts, prompt)
	if g.during != nil {
		g.during()
	}
	if g.failure != nil {
		if err := g.failure(prompt); err != nil {
			return "", err
		}
	}
	return g.output(prompt), nil
}

func TestLearnedExtractionSkipsTruncatedCompletionsWithoutStopping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	runBrain(t, path, "remember", "We use Bazel for builds.")
	runBrain(t, path, "remember", "We use Ruff for linting.")
	original := generatorFactory
	defer func() { generatorFactory = original }()
	fake := &fakeGenerator{origin: "http://127.0.0.1:11434", digests: []string{strings.Repeat("c", 64)}}
	fake.failure = func(prompt string) error {
		if strings.Contains(prompt, "Bazel") {
			return semantic.ErrCompletionTruncated
		}
		return nil
	}
	fake.output = func(string) string {
		return `{"claims":[{"subject":"linting","predicate":"uses","object":"ruff","quote":"We use Ruff for linting."}]}`
	}
	generatorFactory = func(string) (semantic.Generator, error) { return fake, nil }
	var report learnedExtractionReport
	if err := json.Unmarshal(runBrain(t, path, "facts", "extract", "--model", "m:1", "--json"), &report); err != nil {
		t.Fatal(err)
	}
	if report.InvalidOutput != 1 || report.Facts.Added != 1 || report.Facts.Pending != 0 || len(fake.prompts) != 2 {
		t.Fatalf("truncated completion handling: %+v (%d prompts)", report, len(fake.prompts))
	}
}

func TestLearnedExtractionIsExplicitGroundedAndHoldsNoTransaction(t *testing.T) {
	dir := t.TempDir()
	path, source := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db")
	traceHistory(t, source)
	original := generatorFactory
	defer func() { generatorFactory = original }()
	generatorFactory = func(string) (semantic.Generator, error) {
		t.Fatal("refresh or setup contacted a model")
		return nil, nil
	}
	runBrain(t, path, "setup", source)
	runBrain(t, path, "refresh", "--trace-db", source)
	runBrain(t, path, "remember", "We switched the queue from RabbitMQ to NATS.")
	runBrain(t, path, "remember", strings.Repeat("Large memory text. ", 1000))
	digest := strings.Repeat("a", 64)
	fake := &fakeGenerator{origin: "http://127.0.0.1:11434", digests: []string{digest}}
	fake.output = func(prompt string) string {
		return `{"claims":[{"subject":"queue","predicate":"uses","object":"NATS","quote":"We switched the queue from RabbitMQ to NATS."},
			{"subject":"queue","predicate":"uses","object":"kafka","quote":"invented sentence"}]}`
	}
	var generatedAt int64
	fake.during = func() {
		time.Sleep(5 * time.Millisecond)
		generatedAt = time.Now().UnixMilli()
		// No SQLite transaction is held while the model runs.
		db, err := store.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := store.Remember(ctx, db, fmt.Sprintf("during-%d", len(fake.prompts)), "project", "written during inference", "manual", 1); err != nil {
			t.Fatalf("inference blocked a writer: %v", err)
		}
	}
	generatorFactory = func(endpoint string) (semantic.Generator, error) { return fake, nil }
	var report learnedExtractionReport
	if err := json.Unmarshal(runBrain(t, path, "facts", "extract", "--model", "m:1", "--limit", "100", "--json"), &report); err != nil {
		t.Fatal(err)
	}
	if report.ModelDigest != digest || report.SkippedOversized != 1 || report.Facts.Rejected == 0 || report.Facts.Extractor != "ollama" {
		t.Fatalf("learned report: %+v", report)
	}
	for _, prompt := range fake.prompts {
		if strings.Contains(prompt, "Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text. Large memory text.") {
			t.Fatal("oversized memory was sent to the model")
		}
	}
	queue := listFacts(t, path, "--subject", "queue")["queue uses nats"]
	if queue.Extractor != "ollama" || queue.ExtractorVersion != "m:1@"+digest {
		t.Fatalf("learned claim: %+v", queue)
	}
	// The claim is recorded at commit, after inference, not before it.
	if queue.RecordedAt < generatedAt {
		t.Fatalf("claim recorded at %d before inference finished at %d", queue.RecordedAt, generatedAt)
	}
	fake.digests = []string{digest, strings.Repeat("b", 64)}
	runBrain(t, path, "remember", "We use Bazel for builds.")
	var out bytes.Buffer
	cmd := newRoot()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--db", path, "facts", "extract", "--model", "m:1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "digest changed") {
		t.Fatalf("model change during extraction accepted: %v", err)
	}
	for _, args := range [][]string{{"facts", "extract", "--model", "m:1", "--endpoint", "http://example.com:11434"}, {"facts", "extract", "--endpoint", "http://127.0.0.1:11434"}, {"facts", "extract", "--model", "m:1", "--limit", "101"}} {
		cmd := newRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestCompanionContextFactsAreOptInBoundedAndCurrent(t *testing.T) {
	dir := t.TempDir()
	path, source := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db")
	traceHistory(t, source)
	runBrain(t, path, "refresh", "--trace-db", source)
	var plain map[string]json.RawMessage
	if err := json.Unmarshal(runBrain(t, path, "context", "--trace-db", source, "--query", "storage"), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["facts"]; ok || string(plain["status"]) != `"ready"` {
		t.Fatalf("default context output changed: %s", plain)
	}
	var withFacts memoryContext
	if err := json.Unmarshal(runBrain(t, path, "context", "--trace-db", source, "--query", "storage", "--facts", "5"), &withFacts); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, f := range withFacts.Facts {
		found[f.Subject+" "+f.Predicate+" "+f.Object] = true
		related := strings.Contains(f.Subject+f.Object+strings.ToLower(f.Quote), "storage")
		if !related || len(f.Quote) > maxContextQuoteBytes || f.EvidenceID == "" || f.ValidTo != nil {
			t.Fatalf("unbounded, unrelated or closed context fact: %+v", f)
		}
	}
	if !found["storage uses postgres"] || found["storage uses sqlite"] || len(withFacts.Facts) > 5 {
		t.Fatalf("context facts are not the current claims: %v", found)
	}
	var one memoryContext
	if err := json.Unmarshal(runBrain(t, path, "context", "--trace-db", source, "--query", "storage", "--facts", "1"), &one); err != nil || len(one.Facts) != 1 || !one.FactsTruncated {
		t.Fatalf("context facts limit: %+v %v", one, err)
	}
	for _, args := range [][]string{{"context", "--facts", "21"}, {"context", "--facts", "2", "--learned-model", "m:1", "--query", "x"}} {
		cmd := newRoot()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--db", path}, args...))
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// Extraction after a slow import must not push refresh past Trace's one-minute
// limit: once the import used the deadline, extraction is deferred and the
// backlog is reported as pending.
func TestExtractionAfterSlowImportIsDeferredAndReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	runBrain(t, path, "remember", "We use Ruff for linting.")
	old := dbPath
	dbPath = path
	defer func() { dbPath = old }()
	if summary := extractAfterImport(context.Background(), time.Now().Add(-importDeadline)); summary.Error != "" || summary.Memories != 0 || summary.Pending != 1 {
		t.Fatalf("extraction ran past the import deadline: %+v", summary)
	}
	if summary := extractAfterImport(context.Background(), time.Now()); summary.Error != "" || summary.Added != 1 || summary.Pending != 0 {
		t.Fatalf("extraction within the deadline: %+v", summary)
	}
}

// context --facts ignored --trace-db, so claims derived from another Trace
// source sharing the database were returned beside correctly scoped memories.
func TestRegressionContextFactsFollowTheSelectedTraceSource(t *testing.T) {
	dir := t.TempDir()
	path, mine, other := filepath.Join(dir, "memory.db"), filepath.Join(dir, "trace.db"), filepath.Join(dir, "other.db")
	traceHistory(t, mine)
	src, err := sql.Open("sqlite", other)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if _, err := src.Exec(`CREATE TABLE sessions(id TEXT,agent TEXT,worktree TEXT,started_at INTEGER);
CREATE TABLE turns(id TEXT,session_id TEXT,seq INTEGER,prompt TEXT,reasoning TEXT,started_at INTEGER);
INSERT INTO sessions VALUES('o','codex','other',1000);
INSERT INTO turns VALUES('o1','o',1,'We use Oracle for billing storage.','',1000);`); err != nil {
		t.Fatal(err)
	}
	runBrain(t, path, "refresh", "--trace-db", mine)
	runBrain(t, path, "refresh", "--trace-db", other)
	var got memoryContext
	if err := json.Unmarshal(runBrain(t, path, "context", "--trace-db", mine, "--query", "storage", "--facts", "20"), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) == 0 {
		t.Fatal("no context facts for the selected source")
	}
	for _, f := range got.Facts {
		if f.Object == "oracle" || f.Subject == "billing storage" || f.EvidenceSource != "trace-import" {
			t.Fatalf("context fact crossed Trace sources or lacks provenance: %+v", f)
		}
	}
}

func TestRegressionFactReadPathsReapplyRedaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	secret := runtimeCredential()
	// Direct inserts emulate a legacy claim stored before current redaction.
	if _, err := db.Exec(`INSERT INTO memories(id,scope,content,source,created_at) VALUES('m','project','evidence','manual',1);
INSERT INTO facts VALUES('fact-00000000000000000000000000000001','project','service','uses',?,1,NULL,1,NULL,'m','legacy',0)`, "key "+secret); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"facts", "query", "--subject", "service", "--include-stale"}, {"facts", "graph", "service", "--include-stale"}, {"facts", "--subject", "service", "--include-stale", "--json"}} {
		if got := string(runBrain(t, path, args...)); strings.Contains(got, secret) || !strings.Contains(got, "redacted:") {
			t.Fatalf("%v leaked unredacted claim text: %s", args, got)
		}
	}
}
