package evaluator

import (
	"errors"
	"math"
	"testing"
	"uuid"

	"github.com/google/go-cmp/cmp"
)

func TestNewRule(t *testing.T) {
	ruleID := uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-2a3b4c5d6e7f")
	country := mustNewClause(t, "country", eqOp, StringClause, "BJ")
	age := mustNewClause(t, "age", gteOp, NumberClause, 18.0)
	isPro := mustNewClause(t, "isPro", eqOp, BooleanClause, true)

	tests := map[string]struct {
		id      uuid.UUID
		clauses []Clause
		serve   float32
		want    Rule
		errStr  string
	}{
		"nil id rejected": {
			id:      uuid.Nil(),
			clauses: []Clause{country},
			serve:   1,
			errStr:  "invalid rule id: 00000000-0000-0000-0000-000000000000",
		},
		"id checked before clauses and serve": {
			id:      uuid.Nil(),
			clauses: []Clause{},
			serve:   2,
			errStr:  "invalid rule id: 00000000-0000-0000-0000-000000000000",
		},
		"nil clauses rejected": {
			id:      ruleID,
			clauses: nil,
			serve:   1,
			errStr:  "can't build a rule with no clause",
		},
		"empty clauses rejected": {
			id:      ruleID,
			clauses: []Clause{},
			serve:   1,
			errStr:  "can't build a rule with no clause",
		},
		"nil entry rejected": {
			id:      ruleID,
			clauses: []Clause{country, nil, age},
			serve:   1,
			errStr:  "nil clause at index 1",
		},
		"nil entry checked before serve": {
			id:      ruleID,
			clauses: []Clause{nil},
			serve:   2,
			errStr:  "nil clause at index 0",
		},
		"negative serve rejected": {
			id:      ruleID,
			clauses: []Clause{country},
			serve:   -0.1,
			errStr:  "invalid serve value: -0.100000",
		},
		"serve above 1 rejected": {
			id:      ruleID,
			clauses: []Clause{country},
			serve:   1.1,
			errStr:  "invalid serve value: 1.100000",
		},
		// NaN fails every comparison, so a plain range check lets it through.
		"NaN serve rejected": {
			id:      ruleID,
			clauses: []Clause{country},
			serve:   float32(math.NaN()),
			errStr:  "invalid serve value: NaN",
		},
		"empty clauses checked before serve": {
			id:      ruleID,
			clauses: []Clause{},
			serve:   2,
			errStr:  "can't build a rule with no clause",
		},

		// Both bounds are inclusive.
		"serve 0 accepted": {
			id:      ruleID,
			clauses: []Clause{age},
			serve:   0,
			want:    Rule{id: ruleID, clauses: []Clause{age}, serve: 0},
		},
		"serve 1 accepted": {
			id:      ruleID,
			clauses: []Clause{age},
			serve:   1,
			want:    Rule{id: ruleID, clauses: []Clause{age}, serve: 1},
		},
		"partial serve accepted": {
			id:      ruleID,
			clauses: []Clause{country},
			serve:   0.25,
			want:    Rule{id: ruleID, clauses: []Clause{country}, serve: 0.25},
		},
		"multiple clauses keep their order": {
			id:      ruleID,
			clauses: []Clause{isPro, country, age},
			serve:   0.5,
			want:    Rule{id: ruleID, clauses: []Clause{isPro, country, age}, serve: 0.5},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := NewRule(tc.id, tc.clauses, tc.serve)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("NewRule() error = %v, want %q", err, tc.errStr)
				}
				opt := cmp.AllowUnexported(Rule{})
				if diff := cmp.Diff(Rule{}, got, opt); diff != "" {
					t.Fatalf("NewRule() = %#v on error, want zero Rule", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewRule() unexpected error: %v", err)
			}

			opt := cmp.AllowUnexported(Rule{}, stringClause{}, booleanClause{}, numberClause{})
			if diff := cmp.Diff(tc.want, got, opt); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestNewRuleCopiesClauses(t *testing.T) {
	country := mustNewClause(t, "country", eqOp, StringClause, "BJ")
	age := mustNewClause(t, "age", gteOp, NumberClause, 18.0)

	clauses := []Clause{country}
	rule, err := NewRule(uuid.New(), clauses, 1)
	if err != nil {
		t.Fatalf("NewRule() unexpected error: %v", err)
	}
	clauses[0] = age

	opt := cmp.AllowUnexported(stringClause{}, numberClause{})
	if diff := cmp.Diff([]Clause{country}, rule.clauses, opt); diff != "" {
		t.Fatalf("rule changed after caller modified its slice:\n%s", diff)
	}
}

func TestRuleMatch(t *testing.T) {
	country := mustNewClause(t, "country", eqOp, StringClause, "BJ")
	age := mustNewClause(t, "age", gteOp, NumberClause, 18.0)
	isPro := mustNewClause(t, "isPro", eqOp, BooleanClause, true)
	mustNewRule := func(serve float32, clauses ...Clause) Rule {
		r, err := NewRule(uuid.New(), clauses, serve)
		if err != nil {
			t.Fatalf("NewRule(): %v", err)
		}
		return r
	}
	user := map[string][]any{"country": {"BJ"}, "age": {20.0}, "isPro": {true}}

	tests := map[string]struct {
		rule       Rule
		attributes map[string][]any
		wantServe  float32
		wantMatch  bool
		errStr     string
	}{
		"single clause match returns serve": {
			rule:       mustNewRule(0.5, country),
			attributes: user,
			wantServe:  0.5,
			wantMatch:  true,
		},
		"all clauses match returns serve": {
			rule:       mustNewRule(0.75, country, age, isPro),
			attributes: user,
			wantServe:  0.75,
			wantMatch:  true,
		},
		// match must be reported separately from a zero serve.
		"match with serve 0": {
			rule:       mustNewRule(0, country),
			attributes: user,
			wantServe:  0,
			wantMatch:  true,
		},
		"no match when first clause fails": {
			rule:       mustNewRule(1, country, age),
			attributes: map[string][]any{"country": {"FR"}, "age": {20.0}},
			wantServe:  0,
			wantMatch:  false,
		},
		"no match when last clause fails": {
			rule:       mustNewRule(1, country, age, isPro),
			attributes: map[string][]any{"country": {"BJ"}, "age": {20.0}, "isPro": {false}},
			wantServe:  0,
			wantMatch:  false,
		},
		"no match when attribute is missing": {
			rule:       mustNewRule(1, country, age),
			attributes: map[string][]any{"country": {"BJ"}},
			wantServe:  0,
			wantMatch:  false,
		},
		"no match on empty attributes": {
			rule:       mustNewRule(1, country),
			attributes: map[string][]any{},
			wantServe:  0,
			wantMatch:  false,
		},
		"clause error is wrapped with its index": {
			rule:       mustNewRule(1, country, age),
			attributes: map[string][]any{"country": {"BJ"}, "age": {"20"}},
			errStr:     `clause at index 1 errors: invalid value for attribute "age": invalid value 20`,
		},
		"error from an earlier clause wins over a later mismatch": {
			rule:       mustNewRule(1, age, country),
			attributes: map[string][]any{"country": {"FR"}, "age": {"20"}},
			errStr:     `clause at index 0 errors: invalid value for attribute "age": invalid value 20`,
		},
		// Clauses after the first mismatch are not evaluated, so their errors
		// never surface.
		"mismatch stops before a later erroring clause": {
			rule:       mustNewRule(1, country, age),
			attributes: map[string][]any{"country": {"FR"}, "age": {"20"}},
			wantServe:  0,
			wantMatch:  false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			serve, match, err := tc.rule.Match(tc.attributes)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("Match() error = %v, want %q", err, tc.errStr)
				}
				if serve != 0 || match {
					t.Fatalf("Match() = (%v, %v) on error, want (0, false)", serve, match)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match() unexpected error: %v", err)
			}
			if serve != tc.wantServe || match != tc.wantMatch {
				t.Fatalf("Match() = (%v, %v), want (%v, %v)", serve, match, tc.wantServe, tc.wantMatch)
			}
		})
	}
}

// stubClause returns a fixed result and counts how often it is called.
type stubClause struct {
	match bool
	err   error
	calls *int
}

func (c stubClause) Match(map[string][]any) (bool, error) {
	if c.calls != nil {
		*c.calls++
	}
	return c.match, c.err
}

func TestRuleMatchUnwrapsClauseError(t *testing.T) {
	errBoom := errors.New("boom")
	rule, err := NewRule(uuid.New(), []Clause{stubClause{match: true}, stubClause{err: errBoom}}, 1)
	if err != nil {
		t.Fatalf("NewRule(): %v", err)
	}

	_, _, err = rule.Match(nil)
	if !errors.Is(err, errBoom) {
		t.Fatalf("Match() error = %v, want it to wrap %v", err, errBoom)
	}
}

func TestRuleMatchStopsAtFirstMismatch(t *testing.T) {
	var first, second, third int
	rule, err := NewRule(uuid.New(), []Clause{
		stubClause{match: true, calls: &first},
		stubClause{match: false, calls: &second},
		stubClause{match: true, calls: &third},
	}, 1)
	if err != nil {
		t.Fatalf("NewRule(): %v", err)
	}

	if _, match, err := rule.Match(nil); err != nil || match {
		t.Fatalf("Match() = (_, %v, %v), want (_, false, nil)", match, err)
	}
	if first != 1 || second != 1 || third != 0 {
		t.Fatalf("clause calls = %d, %d, %d, want 1, 1, 0", first, second, third)
	}
}
