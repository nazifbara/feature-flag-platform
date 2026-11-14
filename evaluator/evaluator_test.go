package evaluator

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"testing/quick"
	"uuid"
)

// testFlagID is the flag the tests evaluate. The pinned buckets depend on it.
var testFlagID = uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-0000000000f1")

func TestEvaluate(t *testing.T) {
	mustNewRule := func(id uuid.UUID, serve float32, clauses ...Clause) Rule {
		r, err := NewRule(id, clauses, serve)
		if err != nil {
			t.Fatalf("NewRule(): %v", err)
		}
		return r
	}
	mustNewFlagConfig := func(enabled bool, allowlist []string, fall float32, rules ...Rule) FlagConfig {
		users := make(map[string]struct{}, len(allowlist))
		for _, key := range allowlist {
			users[key] = struct{}{}
		}
		cfg, err := NewFlagConfig(enabled, "s4lt", testFlagID, users, rules, fall)
		if err != nil {
			t.Fatalf("NewFlagConfig(): %v", err)
		}
		return cfg
	}

	proID := uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-000000000001")
	countryID := uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-000000000002")
	offID := uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-000000000003")
	pro := mustNewRule(proID, 1, mustNewClause(t, "isPro", EqOp, BooleanClause, true))
	country := mustNewRule(countryID, 0.5, mustNewClause(t, "country", EqOp, StringClause, "BJ"))
	off := mustNewRule(offID, 0, mustNewClause(t, "country", EqOp, StringClause, "FR"))

	// Buckets for salt "s4lt" and testFlagID:
	// carol 0.0986, bob 0.5244, minBucket 0, maxBucket 0.9999.
	alice := "user-alice"
	bob := "user-bob-109"
	carol := "user-carol-8404"
	minBucket := "user-5830"
	maxBucket := "user-2214"

	tests := map[string]struct {
		config     FlagConfig
		attributes map[string][]any
		wantOn     bool
		wantReason string
		errStr     string
	}{
		// The master switch wins over everything, including bad input.
		"disabled flag is off": {
			config:     mustNewFlagConfig(false, []string{alice}, 1, pro),
			attributes: map[string][]any{"user_key": {alice}, "isPro": {true}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},
		"disabled flag ignores an invalid key": {
			config:     mustNewFlagConfig(false, nil, 1, pro),
			attributes: map[string][]any{"user_key": {42.0}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},
		"disabled flag ignores a missing key": {
			config:     mustNewFlagConfig(false, nil, 1, pro),
			attributes: map[string][]any{"isPro": {true}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},
		"disabled flag ignores an empty key": {
			config:     mustNewFlagConfig(false, nil, 1, pro),
			attributes: map[string][]any{"user_key": {""}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},
		"disabled flag ignores erroring rules": {
			config:     mustNewFlagConfig(false, nil, 1, pro),
			attributes: map[string][]any{"isPro": {"true"}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},

		"allowlisted key is on": {
			config:     mustNewFlagConfig(true, []string{bob, alice}, 0, off),
			attributes: map[string][]any{"user_key": {alice}},
			wantOn:     true,
			wantReason: AllowlistMatchReason,
		},
		// The allowlist bypasses the rules, even one that would serve 0.
		"allowlist checked before rules": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, off),
			attributes: map[string][]any{"user_key": {alice}, "country": {"FR"}},
			wantOn:     true,
			wantReason: AllowlistMatchReason,
		},
		"allowlist checked before erroring rules": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, pro),
			attributes: map[string][]any{"user_key": {alice}, "isPro": {"true"}},
			wantOn:     true,
			wantReason: AllowlistMatchReason,
		},
		"key not in allowlist goes on to rules": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, country),
			attributes: map[string][]any{"user_key": {carol}, "country": {"BJ"}},
			wantOn:     true,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"missing key rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, country),
			attributes: map[string][]any{"country": {"BJ"}},
			errStr:     "the user_key attribute is required",
		},
		// The key is required even when there is no allowlist to check it against.
		"missing key rejected without an allowlist": {
			config:     mustNewFlagConfig(true, nil, 1),
			attributes: map[string][]any{"country": {"BJ"}},
			errStr:     "the user_key attribute is required",
		},
		"missing key rejected before erroring rules": {
			config:     mustNewFlagConfig(true, nil, 0, pro),
			attributes: map[string][]any{"isPro": {"true"}},
			errStr:     "the user_key attribute is required",
		},
		// A flag's key is a different thing; "key" isn't read as the user's.
		"old key attribute name rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 1),
			attributes: map[string][]any{"key": {alice}},
			errStr:     "the user_key attribute is required",
		},
		"no attributes rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 0.3, pro),
			attributes: map[string][]any{},
			errStr:     "the user_key attribute is required",
		},
		"nil attributes rejected": {
			config:     mustNewFlagConfig(true, nil, 0.3),
			attributes: nil,
			errStr:     "the user_key attribute is required",
		},
		"empty key rejected": {
			config:     mustNewFlagConfig(true, nil, 1),
			attributes: map[string][]any{"user_key": {""}},
			errStr:     "the user_key attribute value is required",
		},
		"empty key rejected before erroring rules": {
			config:     mustNewFlagConfig(true, nil, 0, pro),
			attributes: map[string][]any{"user_key": {""}, "isPro": {"true"}},
			errStr:     "the user_key attribute value is required",
		},
		// NewFlagConfig rejects an empty allowlist key, so this config is
		// built by hand to show an empty key can't be allowlisted either.
		"empty key rejected before the allowlist": {
			config:     FlagConfig{enabled: true, salt: "s4lt", id: testFlagID, allowlist: map[string]struct{}{"": {}}},
			attributes: map[string][]any{"user_key": {""}},
			errStr:     "the user_key attribute value is required",
		},
		// Only an empty string is rejected; whitespace is a key like any other.
		"blank key accepted": {
			config:     mustNewFlagConfig(true, nil, 1),
			attributes: map[string][]any{"user_key": {" "}},
			wantOn:     true,
			wantReason: FallthroughReason,
		},
		"key with multiple values rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, country),
			attributes: map[string][]any{"user_key": {alice, bob}},
			errStr:     "the user_key attribute must have exactly one value, got: 2",
		},
		"non-string key rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, country),
			attributes: map[string][]any{"user_key": {true}},
			errStr:     "the user_key attribute must be a string",
		},
		"key with no value rejected": {
			config:     mustNewFlagConfig(true, []string{alice}, 0, country),
			attributes: map[string][]any{"user_key": {}},
			errStr:     "the user_key attribute must have exactly one value, got: 0",
		},
		// The key is validated even when there is no allowlist to check it against.
		"invalid key rejected without an allowlist": {
			config:     mustNewFlagConfig(true, nil, 0, country),
			attributes: map[string][]any{"user_key": {42.0}, "country": {"BJ"}},
			errStr:     "the user_key attribute must be a string",
		},

		"matching rule serves its value": {
			config:     mustNewFlagConfig(true, nil, 0, pro),
			attributes: map[string][]any{"user_key": {bob}, "isPro": {true}},
			wantOn:     true,
			wantReason: RuleMatchReason + proID.String(),
		},
		// A rule serving 0 is still a match, not a fallthrough.
		"matching rule serving 0": {
			config:     mustNewFlagConfig(true, nil, 1, off),
			attributes: map[string][]any{"user_key": {bob}, "country": {"FR"}},
			wantOn:     false,
			wantReason: RuleMatchReason + offID.String(),
		},
		"first matching rule wins": {
			config:     mustNewFlagConfig(true, nil, 0, pro, country),
			attributes: map[string][]any{"user_key": {bob}, "isPro": {true}, "country": {"BJ"}},
			wantOn:     true,
			wantReason: RuleMatchReason + proID.String(),
		},
		"later rule matches when earlier ones don't": {
			config:     mustNewFlagConfig(true, nil, 0, pro, off, country),
			attributes: map[string][]any{"user_key": {bob}, "isPro": {false}, "country": {"BJ"}},
			wantOn:     false,
			wantReason: RuleMatchReason + countryID.String(),
		},
		// Rules after the first match are not evaluated, so their errors
		// never surface.
		"match stops before a later erroring rule": {
			config:     mustNewFlagConfig(true, nil, 0, country, pro),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}, "isPro": {"true"}},
			wantOn:     false,
			wantReason: RuleMatchReason + countryID.String(),
		},

		"no rule matches serves fall": {
			config:     mustNewFlagConfig(true, []string{alice}, 0.2, pro, country),
			attributes: map[string][]any{"user_key": {carol}, "isPro": {false}, "country": {"US"}},
			wantOn:     true,
			wantReason: FallthroughReason,
		},
		"no rules serves fall": {
			config:     mustNewFlagConfig(true, nil, 0.7),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}},
			wantOn:     true,
			wantReason: FallthroughReason,
		},
		"only a key serves fall": {
			config:     mustNewFlagConfig(true, []string{alice}, 0.3, pro),
			attributes: map[string][]any{"user_key": {bob}},
			wantOn:     false,
			wantReason: FallthroughReason,
		},

		// On a rule error the flag is off, whatever its fallthrough value.
		"rule error is wrapped with its index and is off": {
			config:     mustNewFlagConfig(true, nil, 1, country, pro),
			attributes: map[string][]any{"user_key": {bob}, "country": {"FR"}, "isPro": {"true"}},
			errStr:     `rule at index 1 errors with: clause at index 0 errors: invalid value for attribute "isPro": invalid value true`,
		},

		// The user is on when their bucket is strictly below the rollout.
		"partial rule is on for a user below it": {
			config:     mustNewFlagConfig(true, nil, 0, country),
			attributes: map[string][]any{"user_key": {carol}, "country": {"BJ"}},
			wantOn:     true,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"partial rule is off for a user above it": {
			config:     mustNewFlagConfig(true, nil, 1, country),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}},
			wantOn:     false,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"partial fall is on for a user below it": {
			config:     mustNewFlagConfig(true, nil, 0.5),
			attributes: map[string][]any{"user_key": {carol}},
			wantOn:     true,
			wantReason: FallthroughReason,
		},
		"partial fall is off for a user above it": {
			config:     mustNewFlagConfig(true, nil, 0.5),
			attributes: map[string][]any{"user_key": {bob}},
			wantOn:     false,
			wantReason: FallthroughReason,
		},
		// A matched rule's serve decides, not fall, and the other way round.
		"matched rule ignores fall": {
			config:     mustNewFlagConfig(true, nil, 1, mustNewRule(countryID, 0.1, mustNewClause(t, "country", EqOp, StringClause, "BJ"))),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}},
			wantOn:     false,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"fallthrough ignores unmatched rule serve": {
			config:     mustNewFlagConfig(true, nil, 0, pro),
			attributes: map[string][]any{"user_key": {carol}, "isPro": {false}},
			wantOn:     false,
			wantReason: FallthroughReason,
		},
		// Boundaries: a user whose bucket equals the rollout is off, 0 is off
		// for everyone and 1 is on for everyone.
		"rule serve equal to the bucket is off": {
			config:     mustNewFlagConfig(true, nil, 1, mustNewRule(countryID, 0.5244, mustNewClause(t, "country", EqOp, StringClause, "BJ"))),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}},
			wantOn:     false,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"rule serve just above the bucket is on": {
			config:     mustNewFlagConfig(true, nil, 0, mustNewRule(countryID, 0.5245, mustNewClause(t, "country", EqOp, StringClause, "BJ"))),
			attributes: map[string][]any{"user_key": {bob}, "country": {"BJ"}},
			wantOn:     true,
			wantReason: RuleMatchReason + countryID.String(),
		},
		"fall equal to the bucket is off": {
			config:     mustNewFlagConfig(true, nil, 0.5244),
			attributes: map[string][]any{"user_key": {bob}},
			wantOn:     false,
			wantReason: FallthroughReason,
		},
		"rule serve 0 is off for the lowest bucket": {
			config:     mustNewFlagConfig(true, nil, 1, off),
			attributes: map[string][]any{"user_key": {minBucket}, "country": {"FR"}},
			wantOn:     false,
			wantReason: RuleMatchReason + offID.String(),
		},
		"fall 0 is off for the lowest bucket": {
			config:     mustNewFlagConfig(true, nil, 0),
			attributes: map[string][]any{"user_key": {minBucket}},
			wantOn:     false,
			wantReason: FallthroughReason,
		},
		"rule serve 1 is on for the highest bucket": {
			config:     mustNewFlagConfig(true, nil, 0, pro),
			attributes: map[string][]any{"user_key": {maxBucket}, "isPro": {true}},
			wantOn:     true,
			wantReason: RuleMatchReason + proID.String(),
		},
		"fall 1 is on for the highest bucket": {
			config:     mustNewFlagConfig(true, nil, 1),
			attributes: map[string][]any{"user_key": {maxBucket}},
			wantOn:     true,
			wantReason: FallthroughReason,
		},

		// Configs without a salt or ID aren't reachable through
		// NewFlagConfig, so they're built by hand.
		"config with no salt rejected": {
			config:     FlagConfig{enabled: true, id: testFlagID, fall: 1},
			attributes: map[string][]any{"user_key": {bob}},
			errStr:     "flag config with no salt",
		},
		"config with no id rejected": {
			config:     FlagConfig{enabled: true, salt: "s4lt", fall: 1},
			attributes: map[string][]any{"user_key": {bob}},
			errStr:     "flag config with no id",
		},
		"config checked before the allowlist": {
			config:     FlagConfig{enabled: true, allowlist: map[string]struct{}{alice: {}}},
			attributes: map[string][]any{"user_key": {alice}},
			errStr:     "flag config with no salt",
		},
		"config checked before the user_key attribute": {
			config:     FlagConfig{enabled: true, salt: "s4lt"},
			attributes: map[string][]any{},
			errStr:     "flag config with no id",
		},
		"disabled config with no salt or id is off": {
			config:     FlagConfig{},
			attributes: map[string][]any{"user_key": {bob}},
			wantOn:     false,
			wantReason: MasterSwitchReason,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			on, reason, err := Evaluate(tc.config, tc.attributes)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("Evaluate() error = %v, want %q", err, tc.errStr)
				}
				if on || reason != "" {
					t.Fatalf("Evaluate() = (%v, %q) on error, want (false, \"\")", on, reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("Evaluate() unexpected error: %v", err)
			}
			if on != tc.wantOn || reason != tc.wantReason {
				t.Fatalf("Evaluate() = (%v, %q), want (%v, %q)", on, reason, tc.wantOn, tc.wantReason)
			}
		})
	}
}

func TestEvaluateUnwrapsRuleError(t *testing.T) {
	errBoom := errors.New("boom")
	rule, err := NewRule(uuid.New(), []Clause{stubClause{err: errBoom}}, 1)
	if err != nil {
		t.Fatalf("NewRule(): %v", err)
	}
	cfg, err := NewFlagConfig(true, "s4lt", testFlagID, nil, []Rule{rule}, 0)
	if err != nil {
		t.Fatalf("NewFlagConfig(): %v", err)
	}

	_, _, err = Evaluate(cfg, map[string][]any{"user_key": {"user-alice"}})
	if !errors.Is(err, errBoom) {
		t.Fatalf("Evaluate() error = %v, want it to wrap %v", err, errBoom)
	}
}

// Changing the hash reshuffles every user of every partial rollout, so these
// values are pinned on purpose.
func TestBucketIsStable(t *testing.T) {
	tests := []struct {
		salt    string
		flagID  uuid.UUID
		userKey string
		want    float32
	}{
		{"s4lt", testFlagID, "user-bob-109", 0.5244},
		{"s4lt", testFlagID, "user-carol-8404", 0.0986},
		{"s4lt", testFlagID, "user-5830", 0},
		{"s4lt", testFlagID, "user-2214", 0.9999},
	}
	for _, tc := range tests {
		if got := bucket(tc.salt, tc.flagID, tc.userKey); got != tc.want {
			t.Errorf("bucket(%q, %v, %q) = %v, want %v", tc.salt, tc.flagID, tc.userKey, got, tc.want)
		}
	}
}

func TestBucketDependsOnEveryInput(t *testing.T) {
	base := bucket("s4lt", testFlagID, "user-bob")
	tests := map[string]float32{
		"salt":     bucket("pepper", testFlagID, "user-bob"),
		"flag ID":  bucket("s4lt", uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-0000000000f2"), "user-bob"),
		"user key": bucket("s4lt", testFlagID, "user-dave"),
	}
	for input, got := range tests {
		if got == base {
			t.Errorf("bucket unchanged (%v) when the %s changes", got, input)
		}
	}
}

func TestEvaluateRollout(t *testing.T) {
	// The keys user-0 … user-19999 are the same on every run, so each share
	// below is a fixed number and the test can't flake.
	const users = 20000
	// on reports, for each user, whether a flag with only a fallthrough is on.
	on := func(salt string, flagID uuid.UUID, fall float32) []bool {
		cfg, err := NewFlagConfig(true, salt, flagID, nil, nil, fall)
		if err != nil {
			t.Fatalf("NewFlagConfig(): %v", err)
		}
		out := make([]bool, users)
		for i := range out {
			out[i], _, err = Evaluate(cfg, map[string][]any{"user_key": {fmt.Sprintf("user-%d", i)}})
			if err != nil {
				t.Fatalf("Evaluate(): %v", err)
			}
		}
		return out
	}
	// share is the fraction of results equal to want.
	share := func(xs []bool, want bool) float64 {
		n := 0
		for _, x := range xs {
			if x == want {
				n++
			}
		}
		return float64(n) / float64(len(xs))
	}
	// If the hash spreads users evenly, each user lands below a target share
	// p like a coin weighted p, so over n users the measured share drifts
	// from p by about sqrt(p(1-p)/n): 0.35 points at p = 0.5, 0.07 at 0.01.
	// Allowing 5 times that drift means an even hash essentially never fails
	// (well under 1 in a million), while one that's off by a few points does.
	// The allowance scales with p so that 2% passing as 1% is caught too.
	near := func(t *testing.T, what string, got, want float64) {
		t.Helper()
		tol := 5 * math.Sqrt(want*(1-want)/users)
		if math.Abs(got-want) > tol {
			t.Errorf("%s = %.4f, want %.4f ± %.4f", what, got, want, tol)
		}
	}

	t.Run("share of each result follows the rollout", func(t *testing.T) {
		for _, fall := range []float32{0.01, 0.1, 0.5, 0.9, 0.99} {
			results := on("s4lt", testFlagID, fall)
			near(t, fmt.Sprintf("share on at %v", fall), share(results, true), float64(fall))
			near(t, fmt.Sprintf("share off at %v", fall), share(results, false), 1-float64(fall))
		}
	})

	// Raising a rollout only adds users; nobody who had the feature loses it.
	t.Run("raising the rollout keeps users on", func(t *testing.T) {
		low := on("s4lt", testFlagID, 0.2)
		high := on("s4lt", testFlagID, 0.3)
		for i := range low {
			if low[i] && !high[i] {
				t.Fatalf("user-%d on at 0.2 but off at 0.3", i)
			}
		}
	})

	// Two flags at 50% shouldn't give the feature to the same half of users.
	// The ID keeps them apart even if they ever share a salt.
	t.Run("flags roll out independently", func(t *testing.T) {
		a := on("s4lt", testFlagID, 0.5)
		b := on("s4lt", uuid.MustParse("0192e4a0-7b3c-7d4e-9f10-0000000000f2"), 0.5)
		both := make([]bool, users)
		for i := range both {
			both[i] = a[i] && b[i]
		}
		near(t, "share on in both flags", share(both, true), 0.25)
	})

	// Changing the salt is how a rollout gets reshuffled.
	t.Run("new salt reshuffles users", func(t *testing.T) {
		a := on("s4lt", testFlagID, 0.5)
		b := on("pepper", testFlagID, 0.5)
		moved := make([]bool, users)
		for i := range moved {
			moved[i] = a[i] != b[i]
		}
		near(t, "share of users whose result changed", share(moved, true), 0.5)
	})

	t.Run("same salt and ID keep every user's result", func(t *testing.T) {
		a := on("s4lt", testFlagID, 0.5)
		b := on("s4lt", testFlagID, 0.5)
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("user-%d got %v then %v with the same salt and ID", i, a[i], b[i])
			}
		}
	})
}

// Same property as "raising the rollout keeps users on", over random salts
// and user keys rather than the fixed set.
func TestEvaluateRaisingRolloutKeepsUsersOnProperty(t *testing.T) {
	property := func(salt, userKey string) bool {
		if salt == "" || userKey == "" {
			return true
		}
		attributes := map[string][]any{"user_key": {userKey}}
		at := func(fall float32) bool {
			cfg, err := NewFlagConfig(true, salt, testFlagID, nil, nil, fall)
			if err != nil {
				t.Fatalf("NewFlagConfig(): %v", err)
			}
			on, _, err := Evaluate(cfg, attributes)
			if err != nil {
				t.Fatalf("Evaluate(): %v", err)
			}
			return on
		}
		return !at(0.2) || at(0.3)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 10000}); err != nil {
		t.Fatal(err)
	}
}
