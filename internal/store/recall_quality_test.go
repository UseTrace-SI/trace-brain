package store

import (
	"context"
	"strings"
	"testing"
)

// These examples failed before stemming, stopword removal and all-words
// ranking: the default tokenizer did not stem, and every word was OR-ed.
func TestRecallQualityExamples(t *testing.T) {
	db := searchTestDB(t)
	insertMemory(t, db, "flaky", "project", "tests are flaky on CI", "manual", daysAgo(3))
	insertMemory(t, db, "decided", "project", "We decided to keep migrations in Go code", "manual", daysAgo(3))
	insertMemory(t, db, "cat", "project", "the cat sat on the mat", "manual", daysAgo(0))
	insertMemory(t, db, "backups", "project", "database backups run nightly", "trace-import", daysAgo(30))
	ctx := context.Background()
	for _, tc := range []struct{ query, want string }{
		{"test", "flaky"},
		{"testing", "flaky"},
		{"decide", "decided"},
		{"decisions", ""}, // Porter keeps "decis" and "decid" apart; no false match.
		{"the database", "backups"},
		{"what about the databases?", "backups"},
		{"the", "cat"}, // a stopword-only query still searches its words
	} {
		results, err := Search(ctx, db, SearchOpts{Query: tc.query, Now: searchNow})
		if err != nil {
			t.Fatalf("%q: %v", tc.query, err)
		}
		if got := strings.Join(resultIDs(results), ","); got != tc.want {
			t.Fatalf("%q: got %q want %q", tc.query, got, tc.want)
		}
	}
}

func TestRecallRanksAllWordMatchesAboveAnyWordMatches(t *testing.T) {
	db := searchTestDB(t)
	// The partial matches are newer, manual and have a far higher BM25 for
	// their single word; complete matches must still rank first.
	insertMemory(t, db, "both", "project", "a deploy hit a timeout once during a long and otherwise uneventful release window", "trace-import", daysAgo(300))
	insertMemory(t, db, "deploy-only", "project", "deploy deploy deploy", "manual", daysAgo(0))
	insertMemory(t, db, "timeout-only", "project", "timeout timeout", "manual", daysAgo(0))
	insertMemory(t, db, "three", "project", "deploy timeout rollback", "trace-import", daysAgo(400))
	results, err := Search(context.Background(), db, SearchOpts{Query: "deploy timeout", Now: searchNow})
	if err != nil {
		t.Fatal(err)
	}
	got := resultIDs(results)
	if len(got) != 4 || strings.Join(got[:2], ",") != "three,both" {
		t.Fatalf("partial matches outranked complete ones: %v", got)
	}
	// With three words only "three" contains all of them.
	results, err = Search(context.Background(), db, SearchOpts{Query: "deploy timeout rollback", Now: searchNow})
	if err != nil || len(results) != 4 || results[0].ID != "three" {
		t.Fatalf("complete match not first: %v %v", resultIDs(results), err)
	}
	// Filters apply to both tiers, and the tiers never return a row twice.
	results, err = Search(context.Background(), db, SearchOpts{Query: "deploy timeout", Since: daysAgo(350), Now: searchNow})
	if err != nil || strings.Join(resultIDs(results), ",") != "both,deploy-only,timeout-only" && strings.Join(resultIDs(results), ",") != "both,timeout-only,deploy-only" {
		t.Fatalf("filtered tiers: %v %v", resultIDs(results), err)
	}
}

func TestQueryTermsDropStopwordsAndDuplicates(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"the database", "database"},
		{"How do we deploy the API?", "deploy API"},
		{"Test test TESTS", "Test TESTS"},
		{"not the cache", "not cache"},
		{"the of and", "the of and"},
		{"", ""},
		{"!!!", ""},
	} {
		terms, err := QueryTerms(tc.in)
		if err != nil || strings.Join(terms, " ") != tc.want {
			t.Fatalf("%q: got %q want %q (%v)", tc.in, strings.Join(terms, " "), tc.want, err)
		}
	}
	many := strings.Repeat("word ", 3) + "a b c d e f g h j k l m n o p q r s"
	if terms, _ := QueryTerms(many); len(terms) != 16 {
		t.Fatalf("query terms not capped: %d", len(terms))
	}
}

// Echoed words must be words that were searched, and never fragments of a
// secret the redactor recognizes.
func TestDisplayTermsAreSearchedWordsWithoutSecretFragments(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("Ab3dE5fG7hJ9kL1mN2pQ", 3)
	sixteen := "w1 w2 w3 w4 w5 w6 w7 w8 w9 w10 w11 w12 w13 w14 w15 w16"
	for _, tc := range []struct{ query, want string }{
		// The secret's words take three of the 16 searched slots, so only
		// w1..w13 were searched and only those are echoed.
		{secret + " " + sixteen, "w1 w2 w3 w4 w5 w6 w7 w8 w9 w10 w11 w12 w13"},
		// Stopword fallback: the searched words are the secret's fragments,
		// none of which is echoed; "the" was not searched, so not echoed.
		{"the " + secret, ""},
		// Nothing here is a secret the redactor recognizes, so every searched
		// word is echoed; with a real token the redactor replaces the whole
		// header, so nothing is echoed.
		{"Authorization: Bearer X", "Authorization Bearer X"},
		{"Authorization: Bearer " + strings.Repeat("eyJhbGciOiJIUzI1NiJ9", 2), ""},
		{"deploy api_key=" + secret, "deploy api_key"},
		{"flaky tests", "flaky tests"},
	} {
		searched, err := QueryTerms(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		shown := DisplayTerms(tc.query)
		if got := strings.Join(shown, " "); got != tc.want {
			t.Fatalf("%q: echoed %q, want %q (searched %v)", tc.query, got, tc.want, searched)
		}
		inSearch := map[string]bool{}
		for _, term := range searched {
			inSearch[term] = true
		}
		for _, term := range shown {
			if !inSearch[term] || strings.Contains(secret, term) && len(term) > 2 {
				t.Fatalf("%q: echoed %q that was not searched or is part of the secret", tc.query, term)
			}
		}
	}
}
