package evaluator

import (
	"math"
	"testing"
	"uuid"

	"github.com/google/go-cmp/cmp"
)

func TestNewFlagConfig(t *testing.T) {
	mustNewRule := func(serve float32, clauses ...Clause) Rule {
		r, err := NewRule(uuid.New(), clauses, serve)
		if err != nil {
			t.Fatalf("NewRule(): %v", err)
		}
		return r
	}
	country := mustNewRule(1, mustNewClause(t, "country", EqOp, StringClause, "BJ"))
	adults := mustNewRule(0.5, mustNewClause(t, "age", GteOp, NumberClause, 18.0))
	alice := "user-alice"
	bob := "user-bob"
	const salt = "s4lt"
	users := func(keys ...string) map[string]struct{} {
		m := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			m[key] = struct{}{}
		}
		return m
	}

	tests := map[string]struct {
		salt      string
		id        uuid.UUID
		enabled   bool
		allowlist map[string]struct{}
		rules     []Rule
		fall      float32
		want      FlagConfig
		errStr    string
	}{
		"empty salt rejected": {
			salt:    "",
			id:      testFlagID,
			enabled: true,
			errStr:  "the flagSalt is required",
		},
		"zero id rejected": {
			salt:    salt,
			enabled: true,
			errStr:  "the flagID is required",
		},
		"salt checked before id": {
			enabled: true,
			errStr:  "the flagSalt is required",
		},
		"id checked before fall": {
			salt:    salt,
			enabled: true,
			fall:    2,
			errStr:  "the flagID is required",
		},
		"id and salt kept as given": {
			salt:    " S4lt ",
			id:      testFlagID,
			enabled: true,
			want:    FlagConfig{id: testFlagID, salt: " S4lt ", enabled: true},
		},
		"negative fall rejected": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country},
			fall:    -0.1,
			errStr:  "invalid fallthrough value: -0.100000",
		},
		"fall above 1 rejected": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country},
			fall:    1.1,
			errStr:  "invalid fallthrough value: 1.100000",
		},
		"NaN fall rejected": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country},
			fall:    float32(math.NaN()),
			errStr:  "invalid fallthrough value: NaN",
		},
		// The rules below aren't reachable through NewRule, so they're built by
		// hand.
		"rule with nil id rejected": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country, {clauses: country.clauses, serve: 1}},
			fall:    0,
			errStr:  "rule with invalid id at index 1",
		},
		// A Rule with no clauses matches every user, so it would shadow the
		// rules after it.
		"rule with no clause rejected": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country, {id: uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-2a3b4c5d6e81"), serve: 1}, adults},
			fall:    0,
			errStr:  "rule with no clause at index 1",
		},
		"zero rule rejected on its id first": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country, {}, adults},
			fall:    0,
			errStr:  "rule with invalid id at index 1",
		},
		"empty allowlist key rejected": {
			salt:      salt,
			id:        testFlagID,
			enabled:   true,
			allowlist: users(alice, ""),
			fall:      0,
			errStr:    "empty key in the allowlist map",
		},
		"fall checked before rules": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{{}},
			fall:    2,
			errStr:  "invalid fallthrough value: 2.000000",
		},

		// Both bounds of fall are inclusive.
		"fall 0 accepted": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country},
			fall:    0,
			want:    FlagConfig{id: testFlagID, salt: salt, enabled: true, rules: []Rule{country}, fall: 0},
		},
		"fall 1 accepted": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			rules:   []Rule{country},
			fall:    1,
			want:    FlagConfig{id: testFlagID, salt: salt, enabled: true, rules: []Rule{country}, fall: 1},
		},
		"partial fall accepted": {
			salt:      salt,
			id:        testFlagID,
			enabled:   true,
			allowlist: users(alice),
			rules:     []Rule{country},
			fall:      0.1,
			want:      FlagConfig{id: testFlagID, salt: salt, enabled: true, allowlist: users(alice), rules: []Rule{country}, fall: 0.1},
		},
		"disabled flag keeps its settings": {
			salt:      salt,
			id:        testFlagID,
			enabled:   false,
			allowlist: users(alice),
			rules:     []Rule{country},
			fall:      0.1,
			want:      FlagConfig{id: testFlagID, salt: salt, enabled: false, allowlist: users(alice), rules: []Rule{country}, fall: 0.1},
		},
		// Rules are evaluated in order, so the order given must be kept.
		"rules keep their order": {
			salt:      salt,
			id:        testFlagID,
			enabled:   true,
			allowlist: users(alice, bob),
			rules:     []Rule{adults, country},
			fall:      0.5,
			want:      FlagConfig{id: testFlagID, salt: salt, enabled: true, allowlist: users(alice, bob), rules: []Rule{adults, country}, fall: 0.5},
		},
		"no allowlist or rules, only fall": {
			salt:    salt,
			id:      testFlagID,
			enabled: true,
			fall:    1,
			want:    FlagConfig{id: testFlagID, salt: salt, enabled: true, fall: 1},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := NewFlagConfig(tc.enabled, tc.salt, tc.id, tc.allowlist, tc.rules, tc.fall)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("NewFlagConfig() error = %v, want %q", err, tc.errStr)
				}
				opt := cmp.AllowUnexported(FlagConfig{})
				if diff := cmp.Diff(FlagConfig{}, got, opt); diff != "" {
					t.Fatalf("NewFlagConfig() = %#v on error, want zero FlagConfig", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewFlagConfig() unexpected error: %v", err)
			}

			opt := cmp.AllowUnexported(FlagConfig{}, Rule{}, stringClause{}, numberClause{})
			if diff := cmp.Diff(tc.want, got, opt); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestNewFlagConfigCopiesInputs(t *testing.T) {
	country, err := NewRule(uuid.New(), []Clause{mustNewClause(t, "country", EqOp, StringClause, "BJ")}, 1)
	if err != nil {
		t.Fatalf("NewRule(): %v", err)
	}
	adults, err := NewRule(uuid.New(), []Clause{mustNewClause(t, "age", GteOp, NumberClause, 18.0)}, 0.5)
	if err != nil {
		t.Fatalf("NewRule(): %v", err)
	}
	alice := "user-alice"
	bob := "user-bob"

	allowlist := map[string]struct{}{alice: {}}
	rules := []Rule{country}
	cfg, err := NewFlagConfig(true, "s4lt", testFlagID, allowlist, rules, 0)
	if err != nil {
		t.Fatalf("NewFlagConfig() unexpected error: %v", err)
	}
	allowlist[bob] = struct{}{}
	rules[0] = adults

	if diff := cmp.Diff(map[string]struct{}{alice: {}}, cfg.allowlist); diff != "" {
		t.Errorf("allowlist changed after caller modified its map:\n%s", diff)
	}
	opt := cmp.AllowUnexported(Rule{}, stringClause{}, numberClause{})
	if diff := cmp.Diff([]Rule{country}, cfg.rules, opt); diff != "" {
		t.Errorf("rules changed after caller modified its slice:\n%s", diff)
	}
}
