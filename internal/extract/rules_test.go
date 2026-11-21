package extract

import (
	"fmt"
	"strings"
	"testing"
)

func triples(claims []Claim) []string {
	out := []string{}
	for _, c := range claims {
		out = append(out, fmt.Sprintf("%s|%s|%s|%t", c.Subject, c.Predicate, c.Object, c.Exclusive))
	}
	return out
}

func TestRulesExtractExplicitStatements(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		want       []string
	}{
		{"role decision", "We use SQLite (pure Go) for storage.", []string{"storage|uses|sqlite|true"}},
		{"project decision needs a name", "We use PostgreSQL. We use a lot of goroutines.", []string{"project|uses|postgresql|false"}},
		{"imperative role decision", "Use Vitest for unit tests", []string{"unit tests|uses|vitest|true"}},
		{"our role", "Vitest is now our test runner.", []string{"test runner|uses|vitest|true"}},
		{"switch with role", "We switched the test runner from Jest to Vitest.", []string{"test runner|uses|vitest|true", "test runner|stopped_using|jest|false", "vitest|replaces|jest|false"}},
		{"switch with trailing role", "Migrated from MySQL to Postgres for storage", []string{"storage|uses|postgres|true", "storage|stopped_using|mysql|false", "postgres|replaces|mysql|false"}},
		{"switch without role", "We switched from REST to gRPC.", []string{"project|uses|grpc|false", "project|stopped_using|rest|false", "grpc|replaces|rest|false"}},
		{"team replacement", "We replaced Redux with Zustand for state", []string{"state|uses|zustand|true", "state|stopped_using|redux|false", "zustand|replaces|redux|false"}},
		{"stop using", "We no longer use Redis for caching.", []string{"caching|stopped_using|redis|false"}},
		{"always policy", "Always run gofmt before committing.", []string{"run gofmt before committing|policy|always|true"}},
		{"never policy", "You should never commit generated files", []string{"commit generated files|policy|never|true"}},
		{"forbid policy", "Do not edit the vendor directory.", []string{"edit the vendor directory|policy|never|true"}},
		{"preference pair", "I prefer tabs over spaces.", []string{"spaces vs tabs|prefers|tabs|true"}},
		{"install command", "Ran `npm install --save-dev vitest@1.6.0 zod`", []string{"project|depends_on|vitest|false", "project|depends_on|zod|false"}},
		{"go get", "go get github.com/spf13/cobra@v1.10.2", []string{"project|depends_on|github.com/spf13/cobra|false"}},
		{"pip requirement file is not a package", "pip install -r requirements.txt requests==2.31", []string{"project|depends_on|requests|false"}},
		{"remove command", "pnpm remove left-pad && pnpm test", []string{"project|dropped_dependency|left-pad|false"}},
		{"added dependency", "Added the dependency on zod.", []string{"project|depends_on|zod|false"}},
		{"removed dependency", "Removed the dependency on lodash.", []string{"project|dropped_dependency|lodash|false"}},
		{"component dependency", "The API depends on the auth service.", []string{"API|depends_on|auth service|false"}},
		{"defined in", "AssertFact is defined in internal/store/facts.go.", []string{"AssertFact|defined_in|internal/store/facts.go|true"}},
		{"moved symbol", "Moved `importTrace` from cli.go to internal/cli/import.go.", []string{"importTrace|defined_in|internal/cli/import.go|true"}},
		{"last exclusive value in a memory wins", "Use Jest for tests. Use Vitest for tests.", []string{"tests|uses|vitest|true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Rules(Input{Text: tc.text})
			if strings.Join(triples(got), "\n") != strings.Join(tc.want, "\n") {
				t.Fatalf("got\n%s\nwant\n%s", strings.Join(triples(got), "\n"), strings.Join(tc.want, "\n"))
			}
			for _, c := range got {
				if !strings.Contains(tc.text, c.Quote) || c.Quote == "" || len(c.Quote) > MaxQuoteBytes {
					t.Fatalf("quote %q is not a bounded exact substring", c.Quote)
				}
			}
		})
	}
}

func TestRulesSkipHedgedQuestionedNegatedAndConversationalText(t *testing.T) {
	for _, text := range []string{
		"Should we switch from Jest to Vitest?",
		"Maybe we use SQLite for storage.",
		"If we switched from MySQL to Postgres for storage, tests would break.",
		"We have not switched from REST to gRPC.",
		"We never migrated from MySQL to Postgres.",
		"We always used tabs here.",
		"We could use Redis for caching.",
		"Don't worry about the failing lint job.",
		"It always fails on Windows.",
		"For example, we use SQLite for storage.",
		"I replaced the loop with a map.",
		"We use it for everything.",
		"Success depends on careful review.",
		"See https://example.com/docs/setup.md for details.",
		"The bug is in handler.go",
		"We are considering whether to adopt Bazel for builds.",
	} {
		if got := Rules(Input{Text: text}); len(got) != 0 {
			t.Errorf("%q produced %v", text, triples(got))
		}
	}
}

func TestRulesHandleTurnLayoutReferencesAndBudgets(t *testing.T) {
	text := "Turn t1 (session s1, seq 4): Fix the parser in internal/extract/rules.go and keep `Rules` pure | reasoning: Thinking: maybe we use Jest for tests.\n\nAssistant response: We use Vitest for tests. Updated README.md."
	got := Rules(Input{Text: text, Reference: "turn:t1", TurnFormat: true})
	want := []string{"tests|uses|vitest|true", "internal/extract/rules.go|referenced_in|turn:t1|false", "Rules|referenced_in|turn:t1|false", "README.md|referenced_in|turn:t1|false"}
	if strings.Join(triples(got), "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(triples(got), "\n"), strings.Join(want, "\n"))
	}
	if got := Rules(Input{Text: "Turn t1 (session s1, seq 4): update session.go", TurnFormat: true}); len(got) != 0 {
		t.Fatalf("reference claims without a reference: %v", triples(got))
	}
	var many strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&many, "Always run check number%d before merging. Touch file%d.go now.\n", i, i)
	}
	bounded := Rules(Input{Text: many.String(), Reference: "memory:m"})
	references := 0
	for _, c := range bounded {
		if c.Predicate == ReferencedIn {
			references++
		}
	}
	if len(bounded) != MaxClaims || references != 0 {
		t.Fatalf("budget not applied: %d claims, %d references", len(bounded), references)
	}
	if got := Rules(Input{Text: "We use \xff SQLite for storage"}); got != nil {
		t.Fatalf("invalid UTF-8 accepted: %v", triples(got))
	}
	long := "We use SQLite for storage " + strings.Repeat("and more words ", 60)
	if got := Rules(Input{Text: long}); len(got) != 0 {
		t.Fatalf("sentence longer than the quote budget produced %v", triples(got))
	}
}
