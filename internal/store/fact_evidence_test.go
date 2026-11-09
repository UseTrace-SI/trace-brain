package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFactCaptureSurvivesSourceUpdatesWithoutCertifyingTruth(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	secret := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	if err := Remember(ctx, db, "m", "project", "original evidence "+secret, "manual", 10); err != nil {
		t.Fatal(err)
	}
	input := Fact{Scope: "project", Subject: "service", Predicate: "uses", Object: "sqlite", EvidenceID: "m"}
	fact, err := AssertFact(ctx, db, input, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !fact.EvidenceCaptured {
		t.Fatal("missing capture")
	}
	var before int
	if err := db.QueryRow("SELECT reserved_bytes FROM fact_evidence_usage").Scan(&before); err != nil {
		t.Fatal(err)
	}
	repeated, err := AssertFact(ctx, db, input, 101)
	if err != nil || repeated.ID != fact.ID || !repeated.EvidenceCaptured {
		t.Fatalf("non-idempotent capture: %+v %v", repeated, err)
	}
	var after int
	if err := db.QueryRow("SELECT reserved_bytes FROM fact_evidence_usage").Scan(&after); err != nil || before != after {
		t.Fatal("reassertion charged capacity twice", err)
	}
	if err := Update(ctx, db, "m", "", "later source content", 200); err != nil {
		t.Fatal(err)
	}
	current, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 250, KnownAt: 250, Limit: 10})
	if err != nil || len(current.Facts) != 0 || current.StaleOmitted != 1 {
		t.Fatalf("current check regressed: %+v %v", current, err)
	}
	captured, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 250, KnownAt: 100, Limit: 10, EvidenceMode: "captured"})
	if err != nil || len(captured.Facts) != 1 || !captured.Facts[0].EvidenceStale || !captured.Facts[0].EvidenceCaptured || captured.SourceVerified {
		t.Fatalf("captured query: %+v %v", captured, err)
	}
	evidence, err := GetFactEvidence(ctx, db, fact.ID, "project", 100)
	if err != nil || !strings.Contains(evidence.Content, "original evidence") || strings.Contains(evidence.Content, secret) || strings.Contains(evidence.Content, "later") || evidence.SourceVerified || !evidence.CurrentEvidenceStale {
		t.Fatalf("unsafe capture: %+v %v", evidence, err)
	}
	if _, err := GetFactEvidence(ctx, db, fact.ID, "project", 99); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("capture leaked before assertion", err)
	}
	if _, err := GetFactEvidence(ctx, db, fact.ID, "other", 250); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("scope leaked", err)
	}
	if err := RetractFact(ctx, db, fact.ID, 300); err != nil {
		t.Fatal(err)
	}
	evidence, err = GetFactEvidence(ctx, db, fact.ID, "project", 299)
	if err != nil || evidence.RetractedAt != nil {
		t.Fatal("future retraction leaked", err)
	}
	evidence, err = GetFactEvidence(ctx, db, fact.ID, "project", 300)
	if err != nil || evidence.RetractedAt == nil || *evidence.RetractedAt != 300 {
		t.Fatal("retraction hidden", err)
	}
	if err := Forget(ctx, db, "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFactEvidence(ctx, db, fact.ID, "project", 100); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("forgotten evidence resurrected", err)
	}
	if err := db.QueryRow("SELECT reserved_bytes FROM fact_evidence_usage").Scan(&after); err != nil || after != 0 {
		t.Fatal("cascade retained reservation", after, err)
	}
}

func TestCapturedEvidenceQuotaIsAtomicImmutableAndReleasedByExplicitForget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "m", "project", "evidence", "manual", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE fact_evidence_usage SET limit_bytes=?", len("evidence")+1024); err != nil {
		t.Fatal(err)
	}
	other, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var group sync.WaitGroup
	results := make(chan Fact, 2)
	failures := make(chan error, 2)
	for index, connection := range []*sql.DB{db, other} {
		group.Add(1)
		go func(index int, connection *sql.DB) {
			defer group.Done()
			fact, err := AssertFact(ctx, connection, Fact{Scope: "project", Subject: []string{"first", "second"}[index], Predicate: "uses", Object: "sqlite", EvidenceID: "m"}, 2)
			if err != nil {
				failures <- err
			} else {
				results <- fact
			}
		}(index, connection)
	}
	group.Wait()
	close(results)
	close(failures)
	if len(results) != 1 || len(failures) != 1 {
		t.Fatalf("quota race: %d accepted %d rejected", len(results), len(failures))
	}
	for failure := range failures {
		if !strings.Contains(failure.Error(), "fact_evidence_budget_exceeded") {
			t.Fatalf("unexpected concurrent failure: %v", failure)
		}
	}
	fact := <-results
	var count, bytes int
	if err := db.QueryRow("SELECT count(*) FROM facts").Scan(&count); err != nil || count != 1 {
		t.Fatal("failed insertion left a claim", err)
	}
	if _, err := db.Exec("UPDATE fact_evidence SET content='tampered'"); err == nil {
		t.Fatal("capture was mutable")
	}
	if _, err := db.Exec("DELETE FROM fact_evidence_usage"); err == nil {
		t.Fatal("capacity ledger was removable")
	}
	if err := ForgetFact(ctx, db, fact.ID, "other"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("wrong scope deleted claim", err)
	}
	if err := RetractFact(ctx, db, fact.ID, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT reserved_bytes FROM fact_evidence_usage").Scan(&bytes); err != nil || bytes != len("evidence")+1024 {
		t.Fatal("retraction forgot retained evidence", err)
	}
	db.SetMaxIdleConns(0)
	if err := ForgetFact(ctx, db, fact.ID, "project"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT reserved_bytes FROM fact_evidence_usage").Scan(&bytes); err != nil || bytes != 0 {
		t.Fatal("forget did not release capacity", err)
	}
	if _, err := Get(ctx, db, "m"); err != nil {
		t.Fatal("forget removed source memory", err)
	}
	if _, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "replacement", Predicate: "uses", Object: "sqlite", EvidenceID: "m"}, 4); err != nil {
		t.Fatal("released capacity unavailable", err)
	}
}

func TestLegacyClaimsAreNotInventedOrBackfilledAndTamperedCapturesFailClosed(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "m", "project", "original", "manual", 1); err != nil {
		t.Fatal(err)
	}
	input := Fact{Scope: "project", Subject: "A", Predicate: "calls", Object: "B", EvidenceID: "m"}
	fact, err := AssertFact(ctx, db, input, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM fact_evidence WHERE fact_id=?", fact.ID); err != nil {
		t.Fatal(err)
	}
	repeated, err := AssertFact(ctx, db, input, 3)
	if err != nil || repeated.EvidenceCaptured {
		t.Fatal("invented capture for legacy claim", err)
	}
	captured, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 3, KnownAt: 3, Limit: 10, EvidenceMode: "captured"})
	if err != nil || len(captured.Facts) != 0 || captured.EvidenceUnavailableOmitted != 1 {
		t.Fatalf("missing capture: %+v %v", captured, err)
	}
	if _, err := db.Exec("DROP TABLE fact_evidence"); err != nil {
		t.Fatal(err)
	}
	current, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 3, KnownAt: 3, Limit: 10})
	if err != nil || len(current.Facts) != 1 {
		t.Fatalf("legacy read failed: %+v %v", current, err)
	}
	captured, err = QueryFacts(ctx, db, FactQuery{Scope: "project", At: 3, KnownAt: 3, Limit: 10, EvidenceMode: "captured"})
	if err != nil || captured.EvidenceUnavailableOmitted != 1 {
		t.Fatal("legacy capture invented", err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	// Reopening/upgrading must leave legacy assertions without an invented copy.
	if _, err := GetFactEvidence(ctx, db, fact.ID, "project", 3); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("migration backfilled legacy evidence", err)
	}
	next, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "C", Predicate: "calls", Object: "D", EvidenceID: "m"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TRIGGER fact_evidence_immutable; UPDATE fact_evidence SET content='tampered',reserved_bytes=1032"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetFactEvidence(ctx, db, next.ID, "project", 4); err == nil {
		t.Fatal("tampered evidence returned")
	}
	captured, err = QueryFacts(ctx, db, FactQuery{Scope: "project", At: 4, KnownAt: 4, Limit: 10, EvidenceMode: "captured"})
	if err != nil || len(captured.Facts) != 0 || captured.EvidenceUnavailableOmitted != 2 {
		t.Fatalf("tampered hash accepted: %+v %v", captured, err)
	}
}

func TestFactQueriesOnPreFeatureDatabasesStayReadOnly(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DROP TABLE fact_evidence; DROP TABLE facts"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"current", "captured"} {
		result, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 1, KnownAt: 1, Limit: 10, EvidenceMode: mode})
		if err != nil || len(result.Facts) != 0 || result.EvidenceMode != mode {
			t.Fatalf("old database read: %+v %v", result, err)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('facts','fact_evidence')").Scan(&count); err != nil || count != 0 {
		t.Fatal("read migrated old database", err)
	}
}
