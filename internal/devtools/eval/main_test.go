//go:build eval

package main

import "testing"

// The grader decides whether a run passes, so a grader that cannot fail an
// answer is a harness that measures nothing. These cases are the ones that
// were scored wrong before: a correct answer that names a component in order
// to rule it out.

func TestGradeStillCatchesInvention(t *testing.T) {
	q := Question{
		MustMention:    []string{"postgres"},
		MustNotMention: []string{"mongodb", "sqs"},
	}
	answer := "The service persists to postgres and publishes to mongodb."

	missing, invented := grade(answer, q)
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(invented) != 1 || invented[0] != "mongodb" {
		t.Errorf("invented = %v, want [mongodb] — an invented component was not caught", invented)
	}
}

func TestGradeCatchesAMissingFact(t *testing.T) {
	q := Question{MustMention: []string{"postgres", "redis|cache"}}

	missing, _ := grade("It only uses postgres.", q)
	if len(missing) != 1 || missing[0] != "redis|cache" {
		t.Errorf("missing = %v, want [redis|cache]", missing)
	}
}

func TestGradeAcceptsAnAlternative(t *testing.T) {
	q := Question{MustMention: []string{"redis|cache"}}

	if missing, _ := grade("It writes to the cache.", q); len(missing) != 0 {
		t.Errorf("missing = %v; either wording should satisfy the fact", missing)
	}
}

func TestGradeDoesNotMatchInsideAnotherWord(t *testing.T) {
	// The bug this replaces: "sqs" found inside an unrelated identifier, and
	// "no" found inside "cannot", each failing a correct answer.
	for _, tc := range []struct {
		name, answer, forbidden string
	}{
		{"substring of an identifier", "It publishes to orders-sqs-queue-name.", "qsq"},
		{"inside a longer word", "The scan cannot see application code.", "can"},
		{"inside a hyphenated name", "It reaches session-cache.", "session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, invented := grade(tc.answer, Question{MustNotMention: []string{tc.forbidden}})
			if len(invented) != 0 {
				t.Errorf("%q was flagged inside a longer token in %q", tc.forbidden, tc.answer)
			}
		})
	}
}

func TestGradeMatchesAWholeWord(t *testing.T) {
	// Boundaries must not make the check toothless in the other direction.
	for _, answer := range []string{
		"It uses mongodb.",
		"It uses MongoDB for sessions.",
		"Dependencies: mongodb, redis",
		"mongodb is the datastore",
	} {
		_, invented := grade(answer, Question{MustNotMention: []string{"mongodb"}})
		if len(invented) != 1 {
			t.Errorf("mongodb was not caught in %q", answer)
		}
	}
}

func TestShortTermsRemainUsable(t *testing.T) {
	// "sqs" is three characters and a perfectly good invention check; an
	// earlier length floor rejected it.
	_, invented := grade("The worker publishes to SQS.", Question{MustNotMention: []string{"sqs"}})
	if len(invented) != 1 {
		t.Error("a valid three-letter term was not matched")
	}
}

func TestPassedRequiresBoth(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Result
		want bool
	}{
		{"clean", Result{}, true},
		{"missing a fact", Result{Missing: []string{"postgres"}}, false},
		{"invented a component", Result{Invented: []string{"mongodb"}}, false},
		{"errored", Result{Err: errStub}, false},
	} {
		if got := tc.r.Passed(); got != tc.want {
			t.Errorf("%s: Passed() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

var errStub = stubError("the call failed")

type stubError string

func (e stubError) Error() string { return string(e) }
