package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemporalClaimsTrackValidityKnowledgeConflictsStalenessAndDeletion(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := Remember(ctx, db, "evidence", "project", "evidence version one", "manual", 1); err != nil {
		t.Fatal(err)
	}
	until := int64(20)
	input := Fact{Scope: "project", Subject: "service", Predicate: "uses", Object: "old", ValidFrom: 10, ValidTo: &until, EvidenceID: "evidence", Exclusive: true}
	first, err := AssertFact(ctx, db, input, 100)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := AssertFact(ctx, db, input, 101)
	if err != nil || repeated.ID != first.ID || repeated.RecordedAt != 100 {
		t.Fatalf("not idempotent: %+v %v", repeated, err)
	}
	conflict := input
	conflict.Object = "different"
	conflict.ValidFrom = 15
	if _, err := AssertFact(ctx, db, conflict, 102); err == nil {
		t.Fatal("accepted an overlapping exclusive value")
	}
	next := input
	next.Object = "new"
	next.ValidFrom = 20
	next.ValidTo = nil
	second, err := AssertFact(ctx, db, next, 110)
	if err != nil {
		t.Fatal(err)
	}
	query := func(at, known int64, stale bool) FactResults {
		t.Helper()
		result, err := QueryFacts(ctx, db, FactQuery{Scope: "project", Subject: "service", At: at, KnownAt: known, Limit: 100, IncludeStale: stale})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := query(19, 99, false); len(got.Facts) != 0 {
		t.Fatal("claim appeared before it was recorded")
	}
	if got := query(19, 100, false); len(got.Facts) != 1 || got.Facts[0].ID != first.ID || got.SourceVerified {
		t.Fatalf("past claim: %+v", got)
	}
	if got := query(20, 110, false); len(got.Facts) != 1 || got.Facts[0].ID != second.ID {
		t.Fatalf("half-open validity: %+v", got)
	}
	if err := RetractFact(ctx, db, first.ID, 150); err != nil {
		t.Fatal(err)
	}
	if got := query(19, 149, false); len(got.Facts) != 1 || got.Facts[0].RetractedAt != nil {
		t.Fatalf("historical query leaked later knowledge: %+v", got)
	}
	if got := query(19, 150, false); len(got.Facts) != 0 {
		t.Fatal("retraction was not applied")
	}
	if _, err := AssertFact(ctx, db, input, 160); err != nil {
		t.Fatal("could not reassert after explicit retraction:", err)
	}
	if err := Update(ctx, db, "evidence", "", "evidence version two", 200); err != nil {
		t.Fatal(err)
	}
	if got := query(25, 200, false); len(got.Facts) != 0 || got.StaleOmitted != 1 {
		t.Fatalf("obsolete evidence accepted: %+v", got)
	}
	if got := query(25, 200, true); len(got.Facts) != 1 || !got.Facts[0].EvidenceStale {
		t.Fatalf("stale evidence was not marked: %+v", got)
	}
	other := next
	other.Scope = "other"
	if _, err := AssertFact(ctx, db, other, 210); err != nil {
		t.Fatal(err)
	}
	if got := query(25, 220, false); len(got.Facts) != 0 {
		t.Fatal("another scope leaked")
	}
	if err := Forget(ctx, db, "evidence"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM facts").Scan(&n); err != nil || n != 0 {
		t.Fatalf("forget left fact content: %d %v", n, err)
	}
}

func TestFactFieldsAreRedactedAndEvidenceHashCannotIgnoreAnAppendedSuffix(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	content := strings.Repeat("x", 65536)
	if err := Remember(ctx, db, "e", "project", content, "manual", 1); err != nil {
		t.Fatal(err)
	}
	secret := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	fact, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "service", Predicate: "uses", Object: secret, EvidenceID: "e", ValidFrom: 0}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fact.Object, secret) || !strings.Contains(fact.Object, "redacted") {
		t.Fatal("secret fact field persisted")
	}
	if _, err := db.Exec("UPDATE memories SET content=? WHERE id='e'", content+"y"); err != nil {
		t.Fatal(err)
	}
	result, err := QueryFacts(ctx, db, FactQuery{Scope: "project", At: 2, KnownAt: 2, Limit: 1})
	if err != nil || len(result.Facts) != 0 || result.StaleOmitted != 1 {
		t.Fatalf("suffix change was ignored: %+v %v", result, err)
	}
	if _, err := AssertFact(ctx, db, Fact{Scope: "project", Subject: "service", Predicate: secret, Object: "value", EvidenceID: "e"}, 2); err == nil {
		t.Fatal("secret predicate accepted")
	}
}
