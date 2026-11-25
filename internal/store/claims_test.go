package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UseTrace-SI/trace-brain/internal/extract"
)

func claimsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func extractAll(t *testing.T, db *sql.DB, at int64) ExtractionReport {
	t.Helper()
	report, err := ExtractRules(context.Background(), db, RulesOptions{Limit: 1000, At: at})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func current(t *testing.T, db *sql.DB, subject string, at, known int64) map[string]Fact {
	t.Helper()
	result, err := QueryFacts(context.Background(), db, FactQuery{Scope: "project", Subject: subject, At: at, KnownAt: known, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]Fact{}
	for _, f := range result.Facts {
		out[f.Predicate+":"+f.Object] = f
	}
	return out
}

func keys(m map[string]Fact) string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

func TestExtractedClaimsSupersedeOlderValuesAndKeepHistory(t *testing.T) {
	ctx := context.Background()
	for _, order := range [][]string{{"old", "new"}, {"new", "old"}} {
		t.Run(strings.Join(order, "-first-"), func(t *testing.T) {
			db := claimsDB(t)
			texts := map[string]string{"old": "We use SQLite for storage.", "new": "We switched from SQLite to Postgres for storage."}
			times := map[string]int64{"old": 100, "new": 200}
			for i, name := range order {
				if err := Remember(ctx, db, name, "project", texts[name], "manual", times[name]); err != nil {
					t.Fatal(err)
				}
				extractAll(t, db, int64(1000+i))
			}
			before := current(t, db, "storage", 150, 1001)
			sqlite, ok := before["uses:sqlite"]
			if len(before) != 1 || !ok || sqlite.Origin != OriginExtracted || sqlite.Quote != texts["old"] || sqlite.EvidenceID != "old" {
				t.Fatalf("older value at 150: %s %+v", keys(before), sqlite)
			}
			if sqlite.ValidTo == nil || *sqlite.ValidTo != 200 || sqlite.SupersededBy == nil || sqlite.SupersededBy.EvidenceID != "new" || sqlite.SupersededBy.At != 200 {
				t.Fatalf("supersession not recorded: %+v %+v", sqlite.ValidTo, sqlite.SupersededBy)
			}
			after := current(t, db, "storage", 250, 1001)
			if _, ok := after["uses:postgres"]; !ok || len(after) != 2 || after["stopped_using:sqlite"].ID == "" {
				t.Fatalf("newer values at 250: %s", keys(after))
			}
			var total int
			if err := db.QueryRow("SELECT count(*) FROM facts").Scan(&total); err != nil || total != 4 {
				t.Fatalf("history was not kept: %d %v", total, err)
			}
			if report := extractAll(t, db, 1100); report.Memories != 0 || report.Added != 0 || report.Pending != 0 {
				t.Fatalf("re-running was not idempotent: %+v", report)
			}
			timeline, err := FactTimeline(ctx, db, TimelineQuery{Scope: "project", Subject: "storage", KnownAt: 1100, Limit: 10})
			if err != nil || len(timeline.Facts) != 3 {
				t.Fatalf("timeline: %+v %v", timeline, err)
			}
		})
	}
}

func TestInvalidationIsBitemporalAndFollowsRetractionAndForget(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	for _, m := range []struct {
		id, text string
		at       int64
	}{{"a", "Use Jest for tests.", 100}, {"b", "Use Vitest for tests.", 200}, {"c", "Use Mocha for tests.", 300}} {
		if err := Remember(ctx, db, m.id, "project", m.text, "manual", m.at); err != nil {
			t.Fatal(err)
		}
	}
	extractAll(t, db, 1000)
	if got := current(t, db, "tests", 150, 1000); got["uses:jest"].ValidTo == nil || *got["uses:jest"].ValidTo != 200 {
		t.Fatalf("jest not closed by vitest: %+v", got)
	}
	vitest := current(t, db, "tests", 250, 1000)["uses:vitest"]
	if err := RetractFact(ctx, db, vitest.ID, 2000); err != nil {
		t.Fatal(err)
	}
	// Known before the retraction, jest still ends at 200; afterwards the next
	// contradicting claim (mocha at 300) closes it, without any rewrite.
	if got := current(t, db, "tests", 250, 1999); len(got) != 1 || got["uses:vitest"].ID == "" {
		t.Fatalf("historical knowledge changed: %s", keys(got))
	}
	jest := current(t, db, "tests", 250, 2000)["uses:jest"]
	if jest.ValidTo == nil || *jest.ValidTo != 300 || jest.SupersededBy == nil || jest.SupersededBy.EvidenceID != "c" {
		t.Fatalf("retraction did not reopen jest until mocha: %+v %+v", jest.ValidTo, jest.SupersededBy)
	}
	if err := Forget(ctx, db, "c"); err != nil {
		t.Fatal(err)
	}
	if got := current(t, db, "tests", 5000, 5000); len(got) != 1 || got["uses:jest"].ValidTo != nil {
		t.Fatalf("forgotten evidence still closes jest: %+v", got)
	}
}

func TestExtractionReconcilesChangedMemoriesWithoutAlteringThem(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	if err := Remember(ctx, db, "m", "project", "Always run gofmt before committing.", "manual", 100); err != nil {
		t.Fatal(err)
	}
	extractAll(t, db, 1000)
	if err := Update(ctx, db, "m", "", "Never run gofmt before committing. Use Ruff for linting.", 2000); err != nil {
		t.Fatal(err)
	}
	if pending, err := CountPendingExtraction(ctx, db, RulesExtractor); err != nil || pending != 1 {
		t.Fatalf("changed memory is not pending: %d %v", pending, err)
	}
	report := extractAll(t, db, 3000)
	if report.Retracted != 1 || report.Added != 2 {
		t.Fatalf("reconciliation: %+v", report)
	}
	now := current(t, db, "run gofmt before committing", 4000, 4000)
	if len(now) != 1 || now["policy:never"].ValidFrom != 2000 {
		t.Fatalf("current policy: %+v", now)
	}
	// The earlier claim's quote is gone from today's memory, so like a manual
	// claim it is stale under the default current-evidence check.
	past, err := QueryFacts(ctx, db, FactQuery{Scope: "project", Subject: "run gofmt before committing", At: 1500, KnownAt: 2999, Limit: 10, IncludeStale: true})
	if err != nil || len(past.Facts) != 1 || past.Facts[0].Object != "always" || !past.Facts[0].EvidenceStale {
		t.Fatalf("earlier knowledge lost: %+v %v", past, err)
	}
	if hidden := current(t, db, "run gofmt before committing", 1500, 2999); len(hidden) != 0 {
		t.Fatalf("stale extracted evidence was not omitted: %s", keys(hidden))
	}
	memory, err := Get(ctx, db, "m")
	if err != nil || memory.Content != "Never run gofmt before committing. Use Ruff for linting." {
		t.Fatalf("extraction changed the manual memory: %+v %v", memory, err)
	}
	revisions, err := History(ctx, db, "m")
	if err != nil || len(revisions) != 2 {
		t.Fatalf("extraction wrote revisions: %d %v", len(revisions), err)
	}
	if report := extractAll(t, db, 5000); report.Memories != 0 {
		t.Fatalf("unchanged memory re-extracted: %+v", report)
	}
}

func TestExtractionDefersToManualClaimsAndManualAssertionsCloseExtractedOnes(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	for _, m := range []struct {
		id, text string
		at       int64
	}{{"note", "Storage notes.", 1}, {"older", "We use Redis for storage.", 50}, {"newer", "We use SQLite for storage.", 200}, {"cache", "Use Redis for cache.", 200}} {
		if err := Remember(ctx, db, m.id, "project", m.text, "manual", m.at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "storage", Predicate: "uses", Object: "mysql", ValidFrom: 100, EvidenceID: "note", Exclusive: true}, 500); err != nil {
		t.Fatal(err)
	}
	report := extractAll(t, db, 1000)
	if report.Conflicts != 1 || report.Added != 2 {
		t.Fatalf("manual claim was not respected: %+v", report)
	}
	if got := current(t, db, "storage", 60, 1000); got["uses:redis"].ValidTo == nil || *got["uses:redis"].ValidTo != 100 {
		t.Fatalf("older extracted claim not closed by the manual claim: %+v", got)
	}
	if got := current(t, db, "storage", 250, 1000); len(got) != 1 || got["uses:mysql"].Origin != OriginManual || got["uses:mysql"].ValidTo != nil {
		t.Fatalf("manual claim altered or contradicted: %+v", got)
	}
	asserted, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "cache", Predicate: "uses", Object: "memcached", ValidFrom: 300, EvidenceID: "note", Exclusive: true}, 2000)
	if err != nil || len(asserted.Supersedes) != 1 {
		t.Fatalf("manual assertion did not supersede the extracted claim: %+v %v", asserted, err)
	}
	if got := current(t, db, "cache", 350, 2000); len(got) != 1 || got["uses:memcached"].ID != asserted.ID {
		t.Fatalf("cache at 350: %s", keys(got))
	}
	if got := current(t, db, "cache", 250, 2000); got["uses:redis"].ValidTo == nil || *got["uses:redis"].ValidTo != 300 {
		t.Fatalf("cache at 250: %+v", got)
	}
	if _, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "storage", Predicate: "uses", Object: "postgres", ValidFrom: 300, EvidenceID: "note", Exclusive: true}, 2001); err == nil {
		t.Fatal("manual claim was silently replaced by another manual claim")
	}
}

func TestExtractionBudgetTiesAndOversizedMemoriesStayBounded(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	for _, m := range []struct{ id, text string }{{"x", "Use Jest for tests."}, {"y", "Use Vitest for tests."}} {
		if err := Remember(ctx, db, m.id, "project", m.text, "manual", 100); err != nil {
			t.Fatal(err)
		}
	}
	if report := extractAll(t, db, 1000); report.Added != 1 || report.Conflicts != 1 {
		t.Fatalf("contradiction at one instant was not skipped: %+v", report)
	}
	if _, err := db.Exec("INSERT INTO memories(id,scope,content,source,created_at) VALUES('legacy','project',?,'manual',1)", strings.Repeat("Use Go for builds. ", 4000)); err != nil {
		t.Fatal(err)
	}
	if report := extractAll(t, db, 1100); report.Memories != 1 || report.Added != 0 || report.Pending != 0 {
		t.Fatalf("oversized legacy memory not settled: %+v", report)
	}
	original := extractionFactLimit
	extractionFactLimit = 1
	defer func() { extractionFactLimit = original }()
	if err := Remember(ctx, db, "z", "project", "Use Ruff for linting.", "manual", 300); err != nil {
		t.Fatal(err)
	}
	if report := extractAll(t, db, 1200); !report.BudgetExhausted || report.Added != 0 || report.Pending != 1 {
		t.Fatalf("budget not enforced: %+v", report)
	}
	extractionFactLimit = original
	if report := extractAll(t, db, 1300); report.Added != 1 || report.Pending != 0 {
		t.Fatalf("pending memory not completed after capacity returned: %+v", report)
	}
}

func TestRetractedClaimsAreNotRestoredAndScopeChangesReExtract(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	if err := Remember(ctx, db, "m", "project", "Use Jest for tests.", "manual", 100); err != nil {
		t.Fatal(err)
	}
	extractAll(t, db, 1000)
	jest := current(t, db, "tests", 200, 1000)["uses:jest"]
	if err := RetractFact(ctx, db, jest.ID, 1100); err != nil {
		t.Fatal(err)
	}
	// Editing the note away and back re-extracts it, but the retracted claim
	// stays retracted instead of being silently recreated.
	for i, text := range []string{"Use Jest for tests. Ship weekly.", "Use Jest for tests."} {
		if err := Update(ctx, db, "m", "", text, int64(1200+i)); err != nil {
			t.Fatal(err)
		}
		if report := extractAll(t, db, int64(2000+i)); report.Memories != 1 || report.Added != 0 || report.Suppressed != 1 {
			t.Fatalf("retracted claim restored: %+v", report)
		}
	}
	if got := current(t, db, "tests", 3000, 3000); len(got) != 0 {
		t.Fatalf("retracted claim is current again: %s", keys(got))
	}
	if err := Remember(ctx, db, "n", "project", "Use Ruff for linting.", "manual", 100); err != nil {
		t.Fatal(err)
	}
	extractAll(t, db, 3100)
	if err := Update(ctx, db, "n", "tooling", "Use Ruff for linting.", 3200); err != nil {
		t.Fatal(err)
	}
	if report := extractAll(t, db, 3300); report.Memories != 1 || report.Retracted != 1 || report.Added != 1 {
		t.Fatalf("scope change was not re-extracted: %+v", report)
	}
	moved, err := QueryFacts(ctx, db, FactQuery{Scope: "tooling", Subject: "linting", At: 3400, KnownAt: 3400, Limit: 10})
	if err != nil || len(moved.Facts) != 1 || moved.Facts[0].Object != "ruff" {
		t.Fatalf("claim not in the new scope: %+v %v", moved, err)
	}
	if old := current(t, db, "linting", 3400, 3400); len(old) != 0 {
		t.Fatalf("claim left in the old scope: %s", keys(old))
	}
}

// Read-time invalidation used one correlated scan over every claim about the
// subject per candidate row, so reads at a few thousand claims on one subject
// (e.g. project-wide dependencies) exceeded the two-second fact deadline.
func TestClaimReadsStayWithinDeadlineAtTheExtractionBudget(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	if err := Remember(ctx, db, "m", "project", "We use many packages.", "manual", 1); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < ExtractionFactLimit; i++ {
		id := fmt.Sprintf("fact-%032x", i)
		predicate, subject, object, exclusive := []string{"depends_on", "uses", "stopped_using"}[i%3], "project", fmt.Sprintf("pkg%d", i/3), false
		if i%4 == 0 {
			subject, object, exclusive = "storage", []string{"sqlite", "postgres"}[i/4%2], true
			predicate = "uses"
		}
		if _, err := tx.Exec("INSERT INTO facts VALUES(?,?,?,?,?,?,NULL,?,NULL,?,?,?)", id, "project", subject, predicate, object, int64(10+i), 100000, "m", "h", exclusive); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO fact_sources VALUES(?,?,?,?)", id, "rules", "rules-v1", "We use many packages."); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// The two-second read budget is enforced only by the ordinary build; the
	// race build still checks that every query completes.
	deadline, cancel := context.WithCancel(ctx)
	if !raceEnabled {
		deadline, cancel = context.WithTimeout(ctx, 2*time.Second)
	}
	defer cancel()
	for _, subject := range []string{"project", "storage", ""} {
		if _, err := QueryFacts(deadline, db, FactQuery{Scope: "project", Subject: subject, At: 200000, KnownAt: 200000, Limit: 100, IncludeStale: true}); err != nil {
			t.Fatalf("query %q: %v", subject, err)
		}
	}
	if _, _, err := ContextFacts(deadline, db, []string{"pkg"}, MaxContextFacts, 200000, ""); err != nil {
		t.Fatalf("context facts: %v", err)
	}
	if _, err := FactTimeline(deadline, db, TimelineQuery{Scope: "project", Subject: "storage", KnownAt: 200000, Limit: 100}); err != nil {
		t.Fatalf("timeline: %v", err)
	}
}

func TestRestatedValuesAreHeldByBoundedClaims(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	for i := 0; i < 8; i++ {
		if err := Remember(ctx, db, fmt.Sprintf("m%d", i), "project", "Use SQLite for storage.", "manual", int64(100+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remember(ctx, db, "switch", "project", "Use Postgres for storage.", "manual", 200); err != nil {
		t.Fatal(err)
	}
	if err := Remember(ctx, db, "back", "project", "Use SQLite for storage.", "manual", 300); err != nil {
		t.Fatal(err)
	}
	report := extractAll(t, db, 1000)
	if report.Added != maxCoveringClaims+2 || report.Corroborated != 8-maxCoveringClaims {
		t.Fatalf("restated value not bounded: %+v", report)
	}
	if got := current(t, db, "storage", 250, 1000); len(got) != 1 || got["uses:postgres"].ID == "" {
		t.Fatalf("interval after the switch: %s", keys(got))
	}
	if got := current(t, db, "storage", 350, 1000); len(got) != 1 || got["uses:sqlite"].ValidFrom != 300 {
		t.Fatalf("value restated after a change was not recorded: %+v", got)
	}
}

func applyLearned(t *testing.T, db *sql.DB, memory ExtractionMemory, id ExtractorID, claims []extract.Claim, at int64, skip bool) ExtractionReport {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var report ExtractionReport
	if skip {
		report, err = RecordExtractionSkip(context.Background(), tx, memory, id, at)
	} else {
		report, err = ApplyExtraction(context.Background(), tx, memory, id, claims, at)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return report
}

func pendingFor(t *testing.T, db *sql.DB, id ExtractorID) []ExtractionMemory {
	t.Helper()
	memories, err := PendingExtraction(context.Background(), db, id, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	return memories
}

// Learned claims used to be reconciled across all models, so running another
// model retracted the first model's claims for good; skips recorded state
// without re-checking content and never reconciled old claims.
func TestLearnedClaimsAreModelScopedAndSkipsRecheckContent(t *testing.T) {
	ctx := context.Background()
	db := claimsDB(t)
	quote := "We switched the queue from RabbitMQ to NATS."
	if err := Remember(ctx, db, "m", "project", quote, "manual", 100); err != nil {
		t.Fatal(err)
	}
	claim := []extract.Claim{{Subject: "queue", Predicate: "uses", Object: "nats", Quote: quote}}
	a, b := LearnedExtractor("a:1", strings.Repeat("a", 64)), LearnedExtractor("b:1", strings.Repeat("b", 64))
	if report := applyLearned(t, db, pendingFor(t, db, a)[0], a, claim, 1000, false); report.Added != 1 {
		t.Fatalf("model a: %+v", report)
	}
	if report := applyLearned(t, db, pendingFor(t, db, b)[0], b, nil, 1100, false); report.Retracted != 0 {
		t.Fatalf("model b retracted model a's claim: %+v", report)
	}
	if got := current(t, db, "queue", 2000, 2000); got["uses:nats"].ExtractorVersion != a.Version {
		t.Fatalf("model a claim lost: %+v", got)
	}
	updated := LearnedExtractor("a:1", strings.Repeat("c", 64))
	if report := applyLearned(t, db, pendingFor(t, db, updated)[0], updated, claim, 1200, false); report.Unchanged != 1 || report.Added != 0 {
		t.Fatalf("a model update did not reconcile its own claims: %+v", report)
	}
	selected := pendingFor(t, db, LearnedExtractor("d:1", strings.Repeat("d", 64)))[0]
	if err := Update(ctx, db, "m", "", quote+" Ship weekly.", 1300); err != nil {
		t.Fatal(err)
	}
	if report := applyLearned(t, db, selected, LearnedExtractor("d:1", strings.Repeat("d", 64)), nil, 1400, true); report.Changed != 1 || report.Skipped != 0 {
		t.Fatalf("skip of changed content was recorded: %+v", report)
	}
	if len(pendingFor(t, db, LearnedExtractor("d:1", strings.Repeat("d", 64)))) != 1 {
		t.Fatal("changed memory is no longer pending")
	}
	if err := Update(ctx, db, "m", "", strings.Repeat("Unrelated text. ", 1200), 1500); err != nil {
		t.Fatal(err)
	}
	oversized := pendingFor(t, db, updated)[0]
	if report := applyLearned(t, db, oversized, updated, nil, 1600, true); report.Skipped != 1 || report.Retracted != 1 {
		t.Fatalf("skip kept a claim whose quote is gone: %+v", report)
	}
	if len(pendingFor(t, db, updated)) != 0 {
		t.Fatal("skipped memory stays pending")
	}
}
