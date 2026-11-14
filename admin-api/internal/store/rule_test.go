package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/lib/pq"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/testutil"
)

// parseClauses needs no database, so its tests run without TEST_DB_URL.

func TestParseClauses(t *testing.T) {
	type attrs = map[string][]any

	// The evaluator's clause types are private, so each parsed clause is
	// checked by how it behaves: match and noMatch are attributes it should
	// and shouldn't accept.
	tests := map[string]struct {
		raw     string
		match   attrs
		noMatch attrs
	}{
		"string": {
			raw:     `[{"attribute":"country","operator":"eq","kind":"string","values":["BJ"]}]`,
			match:   attrs{"country": {"BJ"}},
			noMatch: attrs{"country": {"FR"}},
		},
		"string with several values": {
			raw:     `[{"attribute":"country","operator":"il","kind":"string","values":["BJ","TG"]}]`,
			match:   attrs{"country": {"TG"}},
			noMatch: attrs{"country": {"FR"}},
		},
		"boolean": {
			raw:     `[{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}]`,
			match:   attrs{"isPro": {true}},
			noMatch: attrs{"isPro": {false}},
		},
		// JSON numbers decode to float64, the type number clauses need.
		"whole number": {
			raw:     `[{"attribute":"age","operator":"gte","kind":"number","values":[18]}]`,
			match:   attrs{"age": {18.0}},
			noMatch: attrs{"age": {17.0}},
		},
		"decimal number": {
			raw:     `[{"attribute":"score","operator":"gt","kind":"number","values":[0.5]}]`,
			match:   attrs{"score": {0.75}},
			noMatch: attrs{"score": {0.25}},
		},
		"surrounding whitespace": {
			raw:     " \n[{\"attribute\":\"country\",\"operator\":\"eq\",\"kind\":\"string\",\"values\":[\"BJ\"]}]\n ",
			match:   attrs{"country": {"BJ"}},
			noMatch: attrs{"country": {"FR"}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			clauses, err := parseClauses(json.RawMessage(test.raw))
			if err != nil {
				t.Fatalf("parseClauses(): %v", err)
			}
			if len(clauses) != 1 {
				t.Fatalf("parseClauses() returned %d clauses, want 1", len(clauses))
			}
			if got, err := clauses[0].Match(test.match); err != nil || !got {
				t.Errorf("Match(%v) = %v, %v, want true", test.match, got, err)
			}
			if got, err := clauses[0].Match(test.noMatch); err != nil || got {
				t.Errorf("Match(%v) = %v, %v, want false", test.noMatch, got, err)
			}
		})
	}

	// Each clause stays at its index, with its own attribute.
	t.Run("keeps clauses in order", func(t *testing.T) {
		clauses, err := parseClauses(json.RawMessage(`[
			{"attribute":"country","operator":"eq","kind":"string","values":["BJ"]},
			{"attribute":"age","operator":"gte","kind":"number","values":[18]},
			{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}
		]`))
		if err != nil {
			t.Fatalf("parseClauses(): %v", err)
		}
		if len(clauses) != 3 {
			t.Fatalf("parseClauses() returned %d clauses, want 3", len(clauses))
		}
		for i, input := range []attrs{{"country": {"BJ"}}, {"age": {18.0}}, {"isPro": {true}}} {
			if got, err := clauses[i].Match(input); err != nil || !got {
				t.Errorf("clause %d: Match(%v) = %v, %v, want true", i, input, got, err)
			}
		}
	})
}

func TestParseClausesRejectsInvalid(t *testing.T) {
	const good = `{"attribute":"country","operator":"eq","kind":"string","values":["BJ"]}`

	tests := map[string]struct {
		raw     string
		wantErr string
	}{
		// Not a list of clauses.
		"empty input":     {``, "invalid clauses: "},
		"malformed JSON":  {`[{`, "invalid clauses: "},
		"object":          {good, "invalid clauses: "},
		"string":          {`"clauses"`, "invalid clauses: "},
		"number":          {`3`, "invalid clauses: "},
		"element number":  {`[3]`, "invalid clauses: "},
		"null":            {`null`, "at least one clause is required"},
		"empty list":      {`[]`, "at least one clause is required"},
		"data after list": {`[` + good + `] []`, "unexpected data after the list"},
		"garbage after":   {`[` + good + `] }`, "unexpected data after the list"},

		// Fields of the wrong JSON type or name.
		"unknown field":      {`[{"attribute":"c","operator":"eq","kind":"string","values":["x"],"oops":1}]`, `unknown field "oops"`},
		"attribute a number": {`[{"attribute":1,"operator":"eq","kind":"string","values":["x"]}]`, "invalid clauses: "},
		"kind a number":      {`[{"attribute":"c","operator":"eq","kind":1,"values":["x"]}]`, "invalid clauses: "},
		"values not a list":  {`[{"attribute":"c","operator":"eq","kind":"string","values":"x"}]`, "invalid clauses: "},

		// Kinds, which parseClauses maps itself.
		"missing kind":    {`[{"attribute":"c","operator":"eq","values":["x"]}]`, `clause at index 0: unknown kind ""`},
		"unknown kind":    {`[{"attribute":"c","operator":"eq","kind":"date","values":["x"]}]`, `clause at index 0: unknown kind "date"`},
		"capitalized":     {`[{"attribute":"c","operator":"eq","kind":"String","values":["x"]}]`, `clause at index 0: unknown kind "String"`},
		"null element":    {`[null]`, `clause at index 0: unknown kind ""`},
		"empty element":   {`[{}]`, `clause at index 0: unknown kind ""`},
		"second is wrong": {`[` + good + `,{"attribute":"c","operator":"eq","kind":"date","values":["x"]}]`, `clause at index 1: unknown kind "date"`},

		// Rules left to evaluator.NewClause, which get the clause's index.
		"missing attribute":      {`[{"operator":"eq","kind":"string","values":["x"]}]`, "clause at index 0: can't create a clause with empty attribute"},
		"empty attribute":        {`[{"attribute":"","operator":"eq","kind":"string","values":["x"]}]`, "clause at index 0: can't create a clause with empty attribute"},
		"missing values":         {`[{"attribute":"c","operator":"eq","kind":"string"}]`, "clause at index 0: can't create a clause with empty values"},
		"empty values":           {`[{"attribute":"c","operator":"eq","kind":"string","values":[]}]`, "clause at index 0: can't create a clause with empty values"},
		"missing operator":       {`[{"attribute":"c","kind":"string","values":["x"]}]`, "clause at index 0: unknown operator: "},
		"unknown operator":       {`[{"attribute":"c","operator":"nope","kind":"string","values":["x"]}]`, "clause at index 0: unknown operator: nope"},
		"eq with several values": {`[{"attribute":"c","operator":"eq","kind":"string","values":["x","y"]}]`, "clause at index 0: eq operator on multi-value clause"},
		"operator wrong for kind": {`[{"attribute":"c","operator":"gt","kind":"boolean","values":[true]}]`,
			"clause at index 0: unsupported operator for BooleanClause: gt"},
		"number in string clause": {`[{"attribute":"c","operator":"eq","kind":"string","values":[1]}]`, "clause at index 0: invalid value for StringClause: "},
		"string in number clause": {`[{"attribute":"c","operator":"eq","kind":"number","values":["x"]}]`, "clause at index 0: invalid value for NumberClause: "},
		"string in bool clause":   {`[{"attribute":"c","operator":"eq","kind":"boolean","values":["true"]}]`, "clause at index 0: invalid value for BooleanClause: "},
		"null value":              {`[{"attribute":"c","operator":"eq","kind":"string","values":[null]}]`, "clause at index 0: invalid value for StringClause: "},
		"second invalid":          {`[` + good + `,{"attribute":"","operator":"eq","kind":"string","values":["x"]}]`, "clause at index 1: can't create a clause with empty attribute"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := parseClauses(json.RawMessage(test.raw))
			if err == nil {
				t.Fatalf("parseClauses(%s) = %v, want an error containing %q", test.raw, got, test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("parseClauses(%s) error = %q, want it to contain %q", test.raw, err, test.wantErr)
			}
			if got != nil {
				t.Fatalf("parseClauses(%s) = %v on error, want nil", test.raw, got)
			}
		})
	}
}

const validClauses = `[{"attribute":"country","operator":"eq","kind":"string","values":["BJ"]}]`

// otherClauses is valid too, and differs from validClauses.
const otherClauses = `[{"attribute":"age","operator":"gte","kind":"number","values":[18.5]},{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}]`

// ruleCount gives the number of committed rules.
func ruleCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM rules").Scan(&n); err != nil {
		t.Fatalf("counting rules: %v", err)
	}
	return n
}

// storedRule reads a rule straight from the table, so tests don't depend on a
// GetRule.
func storedRule(t *testing.T, store *Store, id uuid.UUID) database.Rule {
	t.Helper()
	var r database.Rule
	err := store.db.QueryRow("SELECT id, flag_setting_id, position, name, serve, clauses, created_at, updated_at FROM rules WHERE id = $1", id).
		Scan(&r.ID, &r.FlagSettingID, &r.Position, &r.Name, &r.Serve, &r.Clauses, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		t.Fatalf("reading rule %v: %v", id, err)
	}
	return r
}

// mustCreateRuleSetting resets the database and gives a flag setting for rules
// to belong to.
func mustCreateRuleSetting(t *testing.T, store *Store) database.FlagSetting {
	t.Helper()
	testutil.ResetDB(t, store.db)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s, err := store.GetFlagSetting(context.Background(), database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
	if err != nil {
		t.Fatalf("GetFlagSetting(): %v", err)
	}
	return s
}

// fullRuleUpdate gives every value of arg to UpdateRule, so the rule ends up
// with exactly them.
func fullRuleUpdate(arg database.UpdateRuleParams) RuleUpdate {
	return RuleUpdate{ID: arg.ID, Name: &arg.Name, Serve: &arg.Serve, Clauses: &arg.Clauses}
}

func mustCreateRule(t *testing.T, store *Store, arg database.CreateRuleParams) database.Rule {
	t.Helper()
	r, err := store.CreateRule(context.Background(), arg)
	if err != nil {
		t.Fatalf("CreateRule(%+v): %v", arg, err)
	}
	return r
}

// wantZeroRule fails the test unless r is the zero Rule. Clauses is a slice,
// so a Rule can't be compared with !=.
func wantZeroRule(t *testing.T, r database.Rule) {
	t.Helper()
	if diff := cmp.Diff(database.Rule{}, r); diff != "" {
		t.Fatalf("returned a rule on error (-want +got):\n%s", diff)
	}
}

// wantSameJSON compares JSON by value: JSONB changes spacing and key order.
func wantSameJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decoding %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decoding %s: %v", want, err)
	}
	if diff := cmp.Diff(wantValue, gotValue); diff != "" {
		t.Fatalf("clauses mismatch (-want +got):\n%s", diff)
	}
}

func TestCreateRule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("creates the rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		for position := range int32(3) {
			mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: position, Clauses: json.RawMessage(validClauses)})
		}
		before := time.Now()
		got, err := store.CreateRule(ctx, database.CreateRuleParams{
			FlagSettingID: setting.ID,
			Position:      3,
			Name:          "Beta users",
			Serve:         0.25,
			Clauses:       json.RawMessage(validClauses),
		})
		if err != nil {
			t.Fatalf("CreateRule(): %v", err)
		}
		if got.ID == uuid.Nil() {
			t.Error("CreateRule() returned a zero ID")
		}
		if got.FlagSettingID != setting.ID {
			t.Errorf("CreateRule() flag setting = %v, want %v", got.FlagSettingID, setting.ID)
		}
		if got.Position != 3 || got.Name != "Beta users" || got.Serve != 0.25 {
			t.Errorf("CreateRule() position, name, serve = %v, %q, %v, want 3, %q, 0.25", got.Position, got.Name, got.Serve, "Beta users")
		}
		wantSameJSON(t, got.Clauses, validClauses)
		// A new rule has never been updated.
		if !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Errorf("CreateRule() updated_at = %v, want it equal to created_at %v", got.UpdatedAt, got.CreatedAt)
		}
		// The database and the tests share a clock closely enough for a
		// minute's slack either way.
		if d := got.CreatedAt.Sub(before); d < -time.Minute || d > time.Minute {
			t.Errorf("CreateRule() created_at = %v, want close to %v", got.CreatedAt, before)
		}
		if diff := cmp.Diff(got, storedRule(t, store, got.ID)); diff != "" {
			t.Fatalf("stored rule mismatch (-returned +stored):\n%s", diff)
		}
	})

	t.Run("normalizes the name", func(t *testing.T) {
		tests := map[string]struct{ name, want string }{
			"kept as given":          {"Beta users", "Beta users"},
			"surrounding whitespace": {" \t Beta users \n", "Beta users"},
			"inner whitespace kept":  {"Beta  users", "Beta  users"},
			// A name is optional.
			"empty":             {"", ""},
			"only spaces":       {"   ", ""},
			"longest":           {strings.Repeat("a", 200), strings.Repeat("a", 200)},
			"longest non-ASCII": {strings.Repeat("é", 200), strings.Repeat("é", 200)},
			// The limit applies after trimming.
			"longest with whitespace": {"  " + strings.Repeat("a", 200) + "  ", strings.Repeat("a", 200)},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				got := mustCreateRule(t, store, database.CreateRuleParams{
					FlagSettingID: setting.ID,
					Name:          test.name,
					Clauses:       json.RawMessage(validClauses),
				})
				if got.Name != test.want {
					t.Errorf("CreateRule() name = %q, want %q", got.Name, test.want)
				}
				if s := storedRule(t, store, got.ID); s.Name != test.want {
					t.Errorf("stored name = %q, want %q", s.Name, test.want)
				}
			})
		}
	})

	t.Run("stores serve as given", func(t *testing.T) {
		tests := map[string]float32{
			"nobody":                  0,
			"everybody":               1,
			"half":                    0.5,
			"not exact in binary":     0.1,
			"smallest above 0":        math.SmallestNonzeroFloat32,
			"largest below 1":         math.Nextafter32(1, 0),
			"many significant digits": 0.123456789,
		}
		for name, serve := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				got := mustCreateRule(t, store, database.CreateRuleParams{
					FlagSettingID: setting.ID,
					Serve:         serve,
					Clauses:       json.RawMessage(validClauses),
				})
				if got.Serve != serve {
					t.Errorf("CreateRule() serve = %v, want %v", got.Serve, serve)
				}
				if s := storedRule(t, store, got.ID); s.Serve != serve {
					t.Errorf("stored serve = %v, want %v", s.Serve, serve)
				}
			})
		}
	})

	// A position is an index, so the last one allowed is right after the
	// setting's last rule.
	t.Run("a rule can go at the end", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		for position := range int32(4) {
			got := mustCreateRule(t, store, database.CreateRuleParams{
				FlagSettingID: setting.ID,
				Position:      position,
				Clauses:       json.RawMessage(validClauses),
			})
			if got.Position != position {
				t.Errorf("CreateRule() position = %v, want %v", got.Position, position)
			}
		}
	})

	// What's saved has to load: rules are parsed again when they're read.
	t.Run("saved clauses parse back", func(t *testing.T) {
		raw := `[
			{"attribute":"country","operator":"il","kind":"string","values":["BJ","TG"]},
			{"attribute":"age","operator":"gte","kind":"number","values":[18.5]},
			{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}
		]`
		setting := mustCreateRuleSetting(t, store)
		got := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(raw)})
		wantSameJSON(t, got.Clauses, raw)

		clauses, err := parseClauses(storedRule(t, store, got.ID).Clauses)
		if err != nil {
			t.Fatalf("parseClauses() on the stored clauses: %v", err)
		}
		if len(clauses) != 3 {
			t.Fatalf("parseClauses() returned %d clauses, want 3", len(clauses))
		}
		match := map[string][]any{"country": {"TG"}, "age": {20.0}, "isPro": {true}}
		for i, clause := range clauses {
			if ok, err := clause.Match(match); err != nil || !ok {
				t.Errorf("clause %d Match(%v) = %v, %v, want true", i, match, ok, err)
			}
		}
	})

	// Only the pair is unique: a setting has many rules, and a position can
	// be used once in each setting.
	t.Run("one rule per setting and position pair", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})

		tests := []struct {
			setting  uuid.UUID
			position int32
		}{
			{setting.ID, 0},
			{setting.ID, 1},
			{other.ID, 0},
			{other.ID, 1},
		}
		for _, test := range tests {
			mustCreateRule(t, store, database.CreateRuleParams{
				FlagSettingID: test.setting,
				Position:      test.position,
				Clauses:       json.RawMessage(validClauses),
			})
		}
		if n := ruleCount(t, store); n != len(tests) {
			t.Fatalf("%d rules, want %d", n, len(tests))
		}
	})

	t.Run("two rules get different IDs", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		a := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Clauses: json.RawMessage(validClauses)})
		b := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 1, Clauses: json.RawMessage(validClauses)})
		if a.ID == b.ID {
			t.Fatalf("both rules have ID %v", a.ID)
		}
	})
}

func TestCreateRuleRejectsInvalid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	invalid := func(message string) *connect.Error {
		return connect.NewError(connect.CodeInvalidArgument, message)
	}
	tooLong := invalid("can't create a rule with a name over 200 characters")
	negative := invalid("can't create a rule with a negative position")
	pastEnd := func(rulesCount int) *connect.Error {
		return invalid(fmt.Sprintf("can't create a rule with a position outside [0, %d]", rulesCount))
	}
	outside := invalid("can't create a rule with a serve value outside [0, 1]")
	// Postgres finds these, so they carry no details.
	badText := invalid("can't create the rule")

	valid := func() database.CreateRuleParams {
		return database.CreateRuleParams{Name: "Beta users", Serve: 0.5, Clauses: json.RawMessage(validClauses)}
	}
	tests := map[string]struct {
		change func(*database.CreateRuleParams)
		want   *connect.Error
	}{
		"name too long":           {func(a *database.CreateRuleParams) { a.Name = strings.Repeat("a", 201) }, tooLong},
		"non-ASCII name too long": {func(a *database.CreateRuleParams) { a.Name = strings.Repeat("é", 201) }, tooLong},
		"NUL in name":             {func(a *database.CreateRuleParams) { a.Name = "a\x00b" }, badText},
		"invalid UTF-8 in name":   {func(a *database.CreateRuleParams) { a.Name = "a\xffb" }, badText},

		"position -1":           {func(a *database.CreateRuleParams) { a.Position = -1 }, negative},
		"smallest position":     {func(a *database.CreateRuleParams) { a.Position = math.MinInt32 }, negative},
		"position past the end": {func(a *database.CreateRuleParams) { a.Position = 1 }, pastEnd(0)},
		"largest position":      {func(a *database.CreateRuleParams) { a.Position = math.MaxInt32 }, pastEnd(0)},

		"serve negative":          {func(a *database.CreateRuleParams) { a.Serve = -0.5 }, outside},
		"serve just below 0":      {func(a *database.CreateRuleParams) { a.Serve = -math.SmallestNonzeroFloat32 }, outside},
		"serve just above 1":      {func(a *database.CreateRuleParams) { a.Serve = math.Nextafter32(1, 2) }, outside},
		"serve as a percentage":   {func(a *database.CreateRuleParams) { a.Serve = 50 }, outside},
		"serve NaN":               {func(a *database.CreateRuleParams) { a.Serve = float32(math.NaN()) }, outside},
		"serve positive infinity": {func(a *database.CreateRuleParams) { a.Serve = float32(math.Inf(1)) }, outside},
		"serve negative infinity": {func(a *database.CreateRuleParams) { a.Serve = float32(math.Inf(-1)) }, outside},

		// TestParseClausesRejectsInvalid covers every way clauses can be
		// wrong. These check the error reaches the caller as bad input.
		"no clauses": {func(a *database.CreateRuleParams) { a.Clauses = nil }, invalid("invalid clauses: EOF")},
		"empty clause list": {func(a *database.CreateRuleParams) { a.Clauses = json.RawMessage(`[]`) },
			invalid("invalid clauses: at least one clause is required")},
		"null clauses": {func(a *database.CreateRuleParams) { a.Clauses = json.RawMessage(`null`) },
			invalid("invalid clauses: at least one clause is required")},
		"unknown clause kind": {func(a *database.CreateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"date","values":["x"]}]`)
		}, invalid(`clause at index 0: unknown kind "date"`)},
		"second clause invalid": {func(a *database.CreateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"string","values":["x"]},{"attribute":"","operator":"eq","kind":"string","values":["x"]}]`)
		}, invalid("clause at index 1: can't create a clause with empty attribute")},
		// parseClauses accepts these, but JSONB can't hold them.
		"NUL in clause attribute": {func(a *database.CreateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"a\u0000b","operator":"eq","kind":"string","values":["x"]}]`)
		}, badText},
		"NUL in clause value": {func(a *database.CreateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"string","values":["\u0000"]}]`)
		}, badText},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			arg := valid()
			arg.FlagSettingID = setting.ID
			test.change(&arg)

			got, err := store.CreateRule(ctx, arg)
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			if n := ruleCount(t, store); n != 0 {
				t.Fatalf("%d rules after a rejected create, want 0", n)
			}
		})
	}

	t.Run("position past the setting's end", func(t *testing.T) {
		create := func(t *testing.T, settingID uuid.UUID, position int32) database.Rule {
			t.Helper()
			return mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: settingID, Position: position, Clauses: json.RawMessage(validClauses)})
		}
		wantRejected := func(t *testing.T, settingID uuid.UUID, position int32, want *connect.Error) {
			t.Helper()
			before := ruleCount(t, store)
			arg := valid()
			arg.FlagSettingID = settingID
			arg.Position = position
			got, err := store.CreateRule(ctx, arg)
			wantConnectErr(t, err, want)
			wantZeroRule(t, got)
			if n := ruleCount(t, store); n != before {
				t.Fatalf("%d rules after a rejected create, want %d", n, before)
			}
		}

		t.Run("one past the end", func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			create(t, setting.ID, 0)
			create(t, setting.ID, 1)
			wantRejected(t, setting.ID, 3, pastEnd(2))
		})

		// Only the setting's own rules count.
		t.Run("rules of another setting", func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			otherEnv := mustCreateEnv(t, store, "staging")
			other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
			create(t, other.ID, 0)
			create(t, other.ID, 1)
			wantRejected(t, setting.ID, 1, pastEnd(0))
		})

		t.Run("the end moves back after a delete", func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			create(t, setting.ID, 0)
			last := create(t, setting.ID, 1)
			if _, err := store.DeleteRule(ctx, last.ID); err != nil {
				t.Fatalf("DeleteRule(): %v", err)
			}
			wantRejected(t, setting.ID, 2, pastEnd(1))
			create(t, setting.ID, 1)
		})
	})

	// Validation comes first, so bad input is reported even when the flag
	// setting doesn't exist.
	t.Run("input checked before the flag setting", func(t *testing.T) {
		tests := map[string]struct {
			change func(*database.CreateRuleParams)
			want   *connect.Error
		}{
			"name":     {func(a *database.CreateRuleParams) { a.Name = strings.Repeat("a", 201) }, tooLong},
			"position": {func(a *database.CreateRuleParams) { a.Position = -1 }, negative},
			"serve":    {func(a *database.CreateRuleParams) { a.Serve = 2 }, outside},
			"clauses": {func(a *database.CreateRuleParams) { a.Clauses = json.RawMessage(`[]`) },
				invalid("invalid clauses: at least one clause is required")},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, store.db)
				arg := valid()
				arg.FlagSettingID = uuid.New()
				test.change(&arg)
				_, err := store.CreateRule(ctx, arg)
				wantConnectErr(t, err, test.want)
			})
		}
	})
}

// mustCreateNamedRules gives a setting one rule per name, at positions 0, 1, 2...
func mustCreateNamedRules(t *testing.T, store *Store, settingID uuid.UUID, names ...string) []database.Rule {
	t.Helper()
	rules := make([]database.Rule, 0, len(names))
	for i, name := range names {
		rules = append(rules, mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: settingID,
			Position:      int32(i),
			Name:          name,
			Serve:         0.25,
			Clauses:       json.RawMessage(validClauses),
		}))
	}
	return rules
}

func TestCreateRuleShiftsRules(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Rules from the new position on get a position one higher.
	t.Run("makes room at the position", func(t *testing.T) {
		tests := map[string]struct {
			position int32
			want     []string
		}{
			"at the start":  {0, []string{"new", "a", "b", "c"}},
			"in the middle": {1, []string{"a", "new", "b", "c"}},
			"before last":   {2, []string{"a", "b", "new", "c"}},
			"at the end":    {3, []string{"a", "b", "c", "new"}},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				before := map[string]database.Rule{}
				for _, r := range mustCreateNamedRules(t, store, setting.ID, "a", "b", "c") {
					before[r.Name] = r
				}

				created := mustCreateRule(t, store, database.CreateRuleParams{
					FlagSettingID: setting.ID,
					Position:      test.position,
					Name:          "new",
					Clauses:       json.RawMessage(validClauses),
				})
				if created.Position != test.position {
					t.Errorf("CreateRule() position = %d, want %d", created.Position, test.position)
				}

				got, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				if len(got) != len(test.want) {
					t.Fatalf("%d rules, want %d", len(got), len(test.want))
				}
				var want []database.Rule
				for i, name := range test.want {
					r := created
					if name != "new" {
						r = before[name]
						r.Position = int32(i)
						// Only a rule that moved has a new updated_at.
						if int32(i) > test.position {
							if !got[i].UpdatedAt.After(r.UpdatedAt) {
								t.Errorf("rule %q updated_at = %v, want it after %v", name, got[i].UpdatedAt, r.UpdatedAt)
							}
							r.UpdatedAt = got[i].UpdatedAt
						}
					}
					want = append(want, r)
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Fatalf("rules mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("leaves other settings' rules alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		mustCreateNamedRules(t, store, setting.ID, "a", "b")
		want := mustCreateNamedRules(t, store, other.ID, "a", "b")

		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Clauses: json.RawMessage(validClauses)})
		got, err := store.ListRulesBySetting(ctx, other.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("other setting's rules changed (-want +got):\n%s", diff)
		}
	})

	// The range is checked first, so nothing has moved.
	t.Run("position out of range moves nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		want := mustCreateNamedRules(t, store, setting.ID, "a", "b")

		_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 3, Clauses: json.RawMessage(validClauses)})
		wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, "can't create a rule with a position outside [0, 2]"))
		got, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("rules changed (-want +got):\n%s", diff)
		}
	})

	// Postgres only finds the bad name on the insert, after the shift, which
	// has to be undone.
	t.Run("a failed insert undoes the shift", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		want := mustCreateNamedRules(t, store, setting.ID, "a", "b")

		_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Name: "a\x00b", Clauses: json.RawMessage(validClauses)})
		wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, "can't create the rule"))
		got, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("rules changed (-want +got):\n%s", diff)
		}
	})
}

// CreateRule expects positions without gaps. These go through the query to make
// one: the shift then runs into the next rule, which the deferred unique check
// reports at commit. That error has to reach the caller converted too, as an
// internal one: under the lock only broken positions collide, never a request.
func TestCreateRuleRejectsCollision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)

	var want []database.Rule
	for _, position := range []int32{0, 2, 3} {
		r, err := store.Queries.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: position, Clauses: json.RawMessage(validClauses)})
		if err != nil {
			t.Fatalf("creating a rule at position %d: %v", position, err)
		}
		want = append(want, r)
	}

	got, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Clauses: json.RawMessage(validClauses)})
	wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the rule"))
	wantZeroRule(t, got)
	rules, err := store.ListRulesBySetting(ctx, setting.ID)
	if err != nil {
		t.Fatalf("ListRulesBySetting(): %v", err)
	}
	if diff := cmp.Diff(want, rules); diff != "" {
		t.Fatalf("rules changed (-want +got):\n%s", diff)
	}
}

// A rule must point at a flag setting that exists.
func TestCreateRuleNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)

	tests := map[string]uuid.UUID{
		"unknown flag setting": uuid.New(),
		"zero ID":              uuid.Nil(),
		// Easy to do, since both are UUIDs.
		"flag ID instead": setting.FlagID,
	}
	for name, id := range tests {
		t.Run(name, func(t *testing.T) {
			// The setting is looked for first, so even a position past the end
			// is reported as a missing setting.
			for _, position := range []int32{0, 5} {
				got, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: id, Position: position, Clauses: json.RawMessage(validClauses)})
				wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't create the rule"))
				wantZeroRule(t, got)
				if n := ruleCount(t, store); n != 0 {
					t.Fatalf("%d rules after a failed create, want 0", n)
				}
			}
		})
	}
}

func TestCreateRuleReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	arg := database.CreateRuleParams{FlagSettingID: setting.ID, Serve: 0.5, Clauses: json.RawMessage(validClauses)}
	const message = "can't create the rule"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.CreateRule(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			if n := ruleCount(t, store); n != 0 {
				t.Fatalf("%d rules after a failed create, want 0", n)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.CreateRule(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		wantZeroRule(t, got)

		// These are found without the database, so they're still bad input.
		tests := map[string]struct {
			change func(*database.CreateRuleParams)
			want   string
		}{
			"name":     {func(a *database.CreateRuleParams) { a.Name = strings.Repeat("a", 201) }, "can't create a rule with a name over 200 characters"},
			"position": {func(a *database.CreateRuleParams) { a.Position = -1 }, "can't create a rule with a negative position"},
			"serve":    {func(a *database.CreateRuleParams) { a.Serve = 2 }, "can't create a rule with a serve value outside [0, 1]"},
			"clauses":  {func(a *database.CreateRuleParams) { a.Clauses = json.RawMessage(`[]`) }, "invalid clauses: at least one clause is required"},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				bad := arg
				test.change(&bad)
				_, err := store.CreateRule(context.Background(), bad)
				wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, test.want))
			})
		}
	})
}

// Creates racing for one position wait their turn, and each pushes the others
// down, so all of them get in with no gaps.
func TestCreateRuleRacingForAPosition(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)

	const racers = 8
	errs := make(chan error, racers)
	for range racers {
		go func() {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
			errs <- err
		}()
	}
	for range racers {
		if err := <-errs; err != nil {
			t.Errorf("CreateRule(): %v", err)
		}
	}

	rules, err := store.ListRulesBySetting(ctx, setting.ID)
	if err != nil {
		t.Fatalf("ListRulesBySetting(): %v", err)
	}
	if len(rules) != racers {
		t.Fatalf("%d rules, want %d", len(rules), racers)
	}
	for i, r := range rules {
		if int(r.Position) != i {
			t.Fatalf("rule %d has position %d, want %d", i, r.Position, i)
		}
	}
}

// A setting belongs to one environment, so only that one changes.
func TestCreateRuleBumpsEnvVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the version and updated_at move, and only in the setting's own
	// environment.
	t.Run("its environment goes up by one", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		other := mustCreateEnv(t, store, "staging")
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		got := mustGetEnv(t, store, env.ID)
		if !got.UpdatedAt.After(env.UpdatedAt) {
			t.Errorf("updated_at = %v, want it after %v", got.UpdatedAt, env.UpdatedAt)
		}
		want := env
		want.Version++
		want.UpdatedAt = got.UpdatedAt
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("environment mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(other, mustGetEnv(t, store, other.ID)); diff != "" {
			t.Errorf("other environment changed (-want +got):\n%s", diff)
		}
	})

	t.Run("each create counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+3; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Creates racing each other don't overwrite each other's bump.
	t.Run("racing creates all count", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("CreateRule(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("rejected create leaves versions alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		tests := map[string]database.CreateRuleParams{
			"bad serve":            {FlagSettingID: setting.ID, Serve: 2, Clauses: json.RawMessage(validClauses)},
			"no clauses":           {FlagSettingID: setting.ID, Clauses: json.RawMessage(`[]`)},
			"position past end":    {FlagSettingID: setting.ID, Position: 1, Clauses: json.RawMessage(validClauses)},
			"unknown flag setting": {FlagSettingID: uuid.New(), Clauses: json.RawMessage(validClauses)},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.CreateRule(ctx, arg); err == nil {
					t.Fatal("CreateRule() = nil error, want it rejected")
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// The create, its shift and the bump land together or not at all, and the
	// caller is told which.
	t.Run("failed bump undoes the create", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a")
		rejectEnvBumps(t, store)
		got, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "new", Clauses: json.RawMessage(validClauses)})
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the rule"))
		wantZeroRule(t, got)
		wantRuleNames(t, store, setting.ID, "a")
	})

	// A reader never sees the new rule with the old version: while the bump
	// waits for the environment's row, the create hasn't happened yet.
	t.Run("create stays hidden until the bump commits", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx(): %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SELECT id FROM environments WHERE id = $1 FOR UPDATE", env.ID); err != nil {
			t.Fatalf("locking the environment: %v", err)
		}

		done := wantWaits(t, func() error {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
			return err
		})
		if n := ruleCount(t, store); n != 0 {
			t.Errorf("%d rules visible before the bump, want 0", n)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback(): %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("CreateRule(): %v", err)
		}
		if n := ruleCount(t, store); n != 1 {
			t.Errorf("%d rules, want 1", n)
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Errorf("version = %d, want %d", got, want)
		}
	})
}

// A new rule is logged in its setting's environment, so SDKs can pick it up.
func TestCreateRuleLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The entry carries the flag's key, the rule's serve, clauses and position
	// and the version the environment moved to, and nothing else. Other flags
	// and environments are around, so an entry for any of them would show.
	t.Run("one entry for the rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateFlag(t, store, "dark-mode")
		mustCreateEnv(t, store, "staging")
		before := changeLogCount(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "benin", Serve: 0.25, Clauses: json.RawMessage(validClauses)})
		logs := storedChangeLogsFor(t, store, "rule-created")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		wantSameJSON(t, *logs[0].RuleClauses, validClauses)
		want := database.ChangeLog{
			TargetVersion:   mustGetEnv(t, store, setting.EnvironmentID).Version,
			EnvironmentID:   setting.EnvironmentID,
			FlagNewKey:      sql.NullString{String: "new-checkout", Valid: true},
			RuleID:          &rule.ID,
			RuleServe:       sql.NullFloat64{Float64: 0.25, Valid: true},
			RuleNewPosition: sql.NullInt32{Int32: 0, Valid: true},
			Action:          "rule-created",
		}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt", "RuleClauses")
		if diff := cmp.Diff(want, logs[0], ignore); diff != "" {
			t.Fatalf("change log mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// A rule put between others is logged at the position it gets, and the
	// rules it pushed down get no entry of their own.
	t.Run("rule in the middle logs its position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		before := changeLogCount(t, store)
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 1, Name: "new", Clauses: json.RawMessage(validClauses)})
		wantRuleNames(t, store, setting.ID, "a", "new", "b", "c")
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
		logs := storedChangeLogsFor(t, store, "rule-created")
		if got := logs[len(logs)-1].RuleNewPosition; got != (sql.NullInt32{Int32: 1, Valid: true}) {
			t.Errorf("logged position %+v, want 1", got)
		}
	})

	// An SDK inserting each rule at its logged position, pushing the rest down,
	// ends up with the stored rules in the stored order. 0.1 has no exact
	// binary form, so serves are compared as the float32 the rule holds.
	t.Run("replaying the entries gives the stored rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		creates := []struct {
			position int32
			serve    float32
			clauses  string
		}{{0, 0.1, validClauses}, {0, 0.2, otherClauses}, {2, 0.3, validClauses}, {1, 0.4, otherClauses}, {4, 0.5, validClauses}}
		for _, c := range creates {
			mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: c.position, Clauses: json.RawMessage(c.clauses), Serve: c.serve})
		}
		type replayed struct {
			serve   float32
			clauses json.RawMessage
		}
		var got []replayed
		for _, l := range storedChangeLogsFor(t, store, "rule-created") {
			got = slices.Insert(got, int(l.RuleNewPosition.Int32), replayed{float32(l.RuleServe.Float64), *l.RuleClauses})
		}
		rules, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if len(got) != len(rules) {
			t.Fatalf("replayed %d rules, want %d", len(got), len(rules))
		}
		for i, r := range rules {
			if got[i].serve != r.Serve {
				t.Errorf("replayed rule %d serves %v, want %v", i, got[i].serve, r.Serve)
			}
			wantSameJSON(t, got[i].clauses, string(r.Clauses))
		}
	})

	t.Run("each create counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		logs := storedChangeLogsFor(t, store, "rule-created")
		if len(logs) != 3 {
			t.Fatalf("%d entries, want 3", len(logs))
		}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if l.RuleNewPosition.Int32 != int32(i) {
				t.Errorf("entry %d logged position %d, want %d", i, l.RuleNewPosition.Int32, i)
			}
		}
	})

	// Creates racing each other each log the version they moved to, so no two
	// entries claim the same one. Each racer goes first, so each logs position 0.
	t.Run("racing creates log their own versions", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("CreateRule(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-created")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if l.RuleNewPosition.Int32 != 0 {
				t.Errorf("entry %d logged position %d, want 0", i, l.RuleNewPosition.Int32)
			}
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("rejected create logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		before := changeLogCount(t, store)
		tests := map[string]database.CreateRuleParams{
			"bad serve":            {FlagSettingID: setting.ID, Serve: 2, Clauses: json.RawMessage(validClauses)},
			"no clauses":           {FlagSettingID: setting.ID, Clauses: json.RawMessage(`[]`)},
			"position past end":    {FlagSettingID: setting.ID, Position: 1, Clauses: json.RawMessage(validClauses)},
			"unknown flag setting": {FlagSettingID: uuid.New(), Clauses: json.RawMessage(validClauses)},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.CreateRule(ctx, arg); err == nil {
					t.Fatal("CreateRule() = nil error, want it rejected")
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rejectEnvBumps(t, store)
		if _, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)}); err == nil {
			t.Fatal("CreateRule() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "rule-created")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The create, its shift, the bump and the log land together or not at all,
	// so SDKs never miss a rule.
	t.Run("failed log undoes the create", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateNamedRules(t, store, setting.ID, "a", "b")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		rejectNewChangeLogs(t, store)
		got, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "new", Clauses: json.RawMessage(validClauses)})
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the rule"))
		wantZeroRule(t, got)
		wantRuleNames(t, store, setting.ID, "a", "b")
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

// A create racing the delete of its flag or environment either gets in first or
// finds nothing, and is never aborted as a deadlock.
func TestCreateRuleWhileOwnerIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]func(setting database.FlagSetting) error{
		"DeleteFlag": func(setting database.FlagSetting) error {
			_, err := store.DeleteFlag(ctx, setting.FlagID)
			return err
		},
		"DeleteEnv": func(setting database.FlagSetting) error {
			_, err := store.DeleteEnv(ctx, setting.EnvironmentID)
			return err
		},
	}
	for name, del := range tests {
		t.Run(name, func(t *testing.T) {
			for range 20 {
				setting := mustCreateRuleSetting(t, store)
				mustCreateEnv(t, store, "staging")

				created, deleted := make(chan error, 1), make(chan error, 1)
				go func() {
					_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
					created <- err
				}()
				go func() { deleted <- del(setting) }()
				if err := <-deleted; err != nil {
					t.Fatalf("%s(): %v", name, err)
				}
				if err := <-created; err != nil {
					wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't create the rule"))
				}
			}
		})
	}
}

// A create racing another write to the same setting's rules gets in, and the
// positions stay contiguous, whichever goes first.
func TestCreateRuleWhileOthersChangeRules(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]struct {
		write func(rules []database.Rule) error
		want  int
	}{
		"DeleteRule": {
			write: func(rules []database.Rule) error {
				_, err := store.DeleteRule(ctx, rules[0].ID)
				return err
			},
			want: 3,
		},
		"ChangeRulePosition": {
			write: func(rules []database.Rule) error {
				_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 2})
				return err
			},
			want: 4,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			for range 30 {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")

				errs := make(chan error, 2)
				go func() {
					_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
					errs <- err
				}()
				go func() { errs <- test.write(rules) }()
				for range 2 {
					if err := <-errs; err != nil {
						t.Fatalf("write failed: %v", err)
					}
				}

				got, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				if len(got) != test.want {
					t.Fatalf("%d rules, want %d", len(got), test.want)
				}
				for i, r := range got {
					if int(r.Position) != i {
						t.Fatalf("rule %d has position %d, want %d", i, r.Position, i)
					}
				}
			}
		})
	}
}

// ruleWrite is a change to an existing rule, for tests that hold for all of them.
type ruleWrite struct {
	// write changes the rule, which must have a neighbour for a move to go to.
	write func(rule database.Rule) (database.Rule, error)
	// message is what the write's errors say.
	message string
}

func ruleWrites(store *Store) map[string]ruleWrite {
	ctx := context.Background()
	return map[string]ruleWrite{
		"DeleteRule": {
			write: func(rule database.Rule) (database.Rule, error) {
				return store.DeleteRule(ctx, rule.ID)
			},
			message: "can't delete the rule",
		},
		"UpdateRule": {
			write: func(rule database.Rule) (database.Rule, error) {
				return store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "changed", Serve: 0.75, Clauses: json.RawMessage(otherClauses)}))
			},
			message: "can't update the rule",
		},
		"ChangeRulePosition": {
			write: func(rule database.Rule) (database.Rule, error) {
				return store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rule.ID, Position: 1})
			},
			message: "can't update the rule",
		},
	}
}

// rejectRulePositions makes saving a rule at a position that fails check (an SQL
// condition on position) fail until the test ends, leaving the rows already
// there alone.
func rejectRulePositions(t *testing.T, store *Store, check string) {
	t.Helper()
	if _, err := store.db.Exec("ALTER TABLE rules ADD CONSTRAINT reject_position CHECK (" + check + ") NOT VALID"); err != nil {
		t.Fatalf("adding the constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec("ALTER TABLE rules DROP CONSTRAINT reject_position"); err != nil {
			t.Errorf("dropping the constraint: %v", err)
		}
	})
}

// A write whose shift of the other rules fails changes nothing, and the caller
// is told. Each check lets the rules a and b stay where they are but not where
// the shift moves b.
func TestRuleWritesFailedShift(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]struct {
		check   string
		write   func(rules []database.Rule) (database.Rule, error)
		message string
	}{
		// b moves from 1 to 2 to make room.
		"CreateRule": {"position < 2", func(rules []database.Rule) (database.Rule, error) {
			return store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: rules[0].FlagSettingID, Name: "new", Clauses: json.RawMessage(validClauses)})
		}, "can't create the rule"},
		// b moves from 1 to 0 to fill the gap.
		"DeleteRule": {"position <> 0", func(rules []database.Rule) (database.Rule, error) {
			return store.DeleteRule(ctx, rules[0].ID)
		}, "can't delete the rule"},
		// b moves from 1 to 0 to make room for a.
		"ChangeRulePosition": {"position <> 0", func(rules []database.Rule) (database.Rule, error) {
			return store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 1})
		}, "can't update the rule"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
			want := matrixState(t, store)
			rejectRulePositions(t, store, test.check)
			got, err := test.write(rules)
			wantConnectErr(t, err, connect.NewError(connect.CodeInternal, test.message))
			wantZeroRule(t, got)
			wantRuleNames(t, store, setting.ID, "a", "b")
			if got := matrixState(t, store); got != want {
				t.Fatalf("state = %s, want %s", got, want)
			}
		})
	}
}

// These wait for the lock before touching a row. They take settings and then
// environments, and DeleteEnv the other way round, so without it they can
// deadlock. They also read a setting's rules before writing, which the lock keeps
// from going stale.
func TestRuleWritesWaitForMatrixLock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	wantWaitsForLock := func(t *testing.T, fn func() error) {
		t.Helper()
		tx, _ := beginMatrixTx(t, store)
		defer tx.Rollback()
		done := wantWaits(t, fn)
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback(): %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("write after the lock was released: %v", err)
		}
	}

	t.Run("CreateRule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		wantWaitsForLock(t, func() error {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
			return err
		})
	})
	for name, test := range ruleWrites(store) {
		t.Run(name, func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
			wantWaitsForLock(t, func() error {
				_, err := test.write(rules[0])
				return err
			})
		})
	}
}

// A rule belongs to one environment, so only that one changes.
func TestRuleWritesBumpEnvVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for name, test := range ruleWrites(store) {
		t.Run(name, func(t *testing.T) {
			// Only the version and updated_at move, and only in the rule's own
			// environment, even with the flag's rules in another one.
			t.Run("its environment goes up by one", func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
				otherEnv := mustCreateEnv(t, store, "staging")
				otherSetting, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
				if err != nil {
					t.Fatalf("GetFlagSetting(): %v", err)
				}
				mustCreateNamedRules(t, store, otherSetting.ID, "a", "b")
				env, other := mustGetEnv(t, store, setting.EnvironmentID), mustGetEnv(t, store, otherEnv.ID)

				if _, err := test.write(rules[0]); err != nil {
					t.Fatalf("%s(): %v", name, err)
				}
				got := mustGetEnv(t, store, env.ID)
				if !got.UpdatedAt.After(env.UpdatedAt) {
					t.Errorf("updated_at = %v, want it after %v", got.UpdatedAt, env.UpdatedAt)
				}
				want := env
				want.Version++
				want.UpdatedAt = got.UpdatedAt
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("environment mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(other, mustGetEnv(t, store, other.ID)); diff != "" {
					t.Errorf("other environment changed (-want +got):\n%s", diff)
				}
			})

			// Nothing changed, so readers have nothing to pick up.
			t.Run("unknown rule leaves versions alone", func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				mustCreateNamedRules(t, store, setting.ID, "a", "b")
				env := mustGetEnv(t, store, setting.EnvironmentID)
				_, err := test.write(database.Rule{ID: uuid.New()})
				wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, test.message))
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})

			// The write, any shift and the bump land together or not at all, and
			// the caller is told which.
			t.Run("failed bump undoes the write", func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
				rejectEnvBumps(t, store)
				got, err := test.write(rules[0])
				wantConnectErr(t, err, connect.NewError(connect.CodeInternal, test.message))
				wantZeroRule(t, got)
				stored, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				if diff := cmp.Diff(rules, stored); diff != "" {
					t.Fatalf("rules changed after a failed bump (-want +got):\n%s", diff)
				}
			})

			// A reader never sees the new rules with the old version: while the
			// bump waits for the environment's row, the write hasn't happened yet.
			t.Run("write stays hidden until the bump commits", func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
				env := mustGetEnv(t, store, setting.EnvironmentID)
				tx, err := store.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatalf("BeginTx(): %v", err)
				}
				defer tx.Rollback()
				if _, err := tx.ExecContext(ctx, "SELECT id FROM environments WHERE id = $1 FOR UPDATE", env.ID); err != nil {
					t.Fatalf("locking the environment: %v", err)
				}

				done := wantWaits(t, func() error {
					_, err := test.write(rules[0])
					return err
				})
				stored, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				if diff := cmp.Diff(rules, stored); diff != "" {
					t.Errorf("write visible before the bump (-want +got):\n%s", diff)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatalf("Rollback(): %v", err)
				}
				if err := <-done; err != nil {
					t.Fatalf("%s(): %v", name, err)
				}
				if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
					t.Errorf("version = %d, want %d", got, want)
				}
			})
		})
	}
}

// A write racing the delete of its flag or environment either gets in first or
// finds nothing, and is never aborted as a deadlock.
func TestRuleWritesWhileOwnerIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	owners := map[string]func(setting database.FlagSetting) error{
		"DeleteFlag": func(setting database.FlagSetting) error {
			_, err := store.DeleteFlag(ctx, setting.FlagID)
			return err
		},
		"DeleteEnv": func(setting database.FlagSetting) error {
			_, err := store.DeleteEnv(ctx, setting.EnvironmentID)
			return err
		},
	}
	for name, test := range ruleWrites(store) {
		for owner, del := range owners {
			t.Run(name+"/"+owner, func(t *testing.T) {
				for range 20 {
					setting := mustCreateRuleSetting(t, store)
					mustCreateEnv(t, store, "staging")
					rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")

					written, deleted := make(chan error, 1), make(chan error, 1)
					go func() {
						_, err := test.write(rules[0])
						written <- err
					}()
					go func() { deleted <- del(setting) }()
					if err := <-deleted; err != nil {
						t.Fatalf("%s(): %v", owner, err)
					}
					if err := <-written; err != nil {
						wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, test.message))
					}
				}
			})
		}
	}
}

// Each delete changes something, and a rule can only go once.
func TestDeleteRuleBumpCounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("each delete counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		for _, r := range rules {
			if _, err := store.DeleteRule(ctx, r.ID); err != nil {
				t.Fatalf("DeleteRule(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+3; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	t.Run("racing deletes of one rule count once", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.DeleteRule(ctx, rules[0].ID)
				errs <- err
			}()
		}
		for range racers {
			<-errs
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})
}

// A deleted rule is logged in its setting's environment, so SDKs can drop it.
func TestDeleteRuleLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The entry carries the flag's key, the position the rule had and the
	// version the environment moved to, and nothing else. Other rules, flags
	// and environments are around, so an entry for any of them would show.
	t.Run("one entry for the rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateFlag(t, store, "dark-mode")
		mustCreateEnv(t, store, "staging")
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		before := changeLogCount(t, store)
		if _, err := store.DeleteRule(ctx, rules[1].ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		want := []database.ChangeLog{{
			TargetVersion:   mustGetEnv(t, store, setting.EnvironmentID).Version,
			EnvironmentID:   setting.EnvironmentID,
			FlagNewKey:      sql.NullString{String: "new-checkout", Valid: true},
			RuleNewPosition: sql.NullInt32{Int32: 1, Valid: true},
			Action:          "rule-deleted",
		}}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, storedChangeLogsFor(t, store, "rule-deleted"), ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// An SDK replaying the entries in version order, inserting each created
	// rule at its position and removing each deleted one, the rest moving to
	// close the gap, ends up with the stored rules in the stored order.
	t.Run("replaying the entries gives the stored rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := make([]database.Rule, 5)
		for i := range rules {
			rules[i] = mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: int32(i), Serve: float32(i+1) / 10, Clauses: json.RawMessage(validClauses)})
		}
		for _, i := range []int{3, 0, 4} {
			if _, err := store.DeleteRule(ctx, rules[i].ID); err != nil {
				t.Fatalf("DeleteRule(): %v", err)
			}
		}
		var got []float32
		for _, l := range queryChangeLogs(t, store, "action IN ('rule-created', 'rule-deleted')") {
			p := int(l.RuleNewPosition.Int32)
			if l.Action == "rule-created" {
				got = slices.Insert(got, p, float32(l.RuleServe.Float64))
				continue
			}
			if p >= len(got) {
				t.Fatalf("entry at version %d deletes position %d of %d rules", l.TargetVersion, p, len(got))
			}
			got = slices.Delete(got, p, p+1)
		}
		stored, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		var want []float32
		for _, r := range stored {
			want = append(want, r.Serve)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("replayed rules mismatch (-want +got):\n%s", diff)
		}
	})

	// The key and position are read when the rule goes, so a renamed flag and
	// a moved rule are logged as they were then.
	t.Run("logs the current key and position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		if _, err := store.UpdateFlag(ctx, setting.FlagID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[1].ID, Position: 0}); err != nil {
			t.Fatalf("ChangeRulePosition(): %v", err)
		}
		if _, err := store.DeleteRule(ctx, rules[1].ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "rule-deleted")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		if got := logs[0].FlagNewKey.String; got != "checkout-v2" {
			t.Errorf("logged key %q, want %q", got, "checkout-v2")
		}
		if got := logs[0].RuleNewPosition.Int32; got != 0 {
			t.Errorf("logged position %d, want 0", got)
		}
	})

	// Each delete gets its own entry with its own version. Deleting from the
	// top each time, every rule is first when it goes.
	t.Run("each delete counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		for _, r := range rules {
			if _, err := store.DeleteRule(ctx, r.ID); err != nil {
				t.Fatalf("DeleteRule(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-deleted")
		if len(logs) != len(rules) {
			t.Fatalf("%d entries, want %d", len(logs), len(rules))
		}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if l.RuleNewPosition.Int32 != 0 {
				t.Errorf("entry %d logged position %d, want 0", i, l.RuleNewPosition.Int32)
			}
		}
	})

	// Deletes of different rules racing each other each log the version they
	// moved to and the position the rule had by then, so replaying them leaves
	// no rules.
	t.Run("racing deletes log their own versions", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		const racers = 8
		names := make([]string, racers)
		for i := range names {
			names[i] = fmt.Sprintf("rule-%d", i)
		}
		rules := mustCreateNamedRules(t, store, setting.ID, names...)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		errs := make(chan error, racers)
		for _, r := range rules {
			go func() {
				_, err := store.DeleteRule(ctx, r.ID)
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("DeleteRule(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-deleted")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		left := racers
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if p := int(l.RuleNewPosition.Int32); p >= left {
				t.Errorf("entry %d deletes position %d of %d rules", i, p, left)
			}
			left--
		}
	})

	// Only the first of several racing deletes of one rule finds it, so it is
	// logged once.
	t.Run("racing deletes of one rule log once", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.DeleteRule(ctx, rule.ID)
				errs <- err
			}()
		}
		for range racers {
			<-errs
		}
		if n := len(storedChangeLogsFor(t, store, "rule-deleted")); n != 1 {
			t.Fatalf("%d entries, want 1", n)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("not found logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		before := changeLogCount(t, store)
		for _, id := range []uuid.UUID{rule.ID, uuid.New(), uuid.Nil()} {
			_, err := store.DeleteRule(ctx, id)
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't delete the rule"))
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries after deleting %v, want %d", got, id, before)
			}
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		rejectEnvBumps(t, store)
		if _, err := store.DeleteRule(ctx, rule.ID); err == nil {
			t.Fatal("DeleteRule() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "rule-deleted")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The delete, its shift, the bump and the log land together or not at
	// all, so SDKs never keep serving a rule that's gone.
	t.Run("failed log undoes the delete", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		rejectNewChangeLogs(t, store)
		got, err := store.DeleteRule(ctx, rules[0].ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't delete the rule"))
		wantZeroRule(t, got)
		wantRuleNames(t, store, setting.ID, "a", "b")
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

func TestUpdateRuleBumpCounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// A change to the serve or the clauses alone is enough.
	t.Run("each change counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.25, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		changes := []database.UpdateRuleParams{
			{ID: rule.ID, Name: "a", Serve: 0.5, Clauses: rule.Clauses},
			{ID: rule.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(otherClauses)},
			{ID: rule.ID, Name: "b", Serve: 0.75, Clauses: json.RawMessage(otherClauses)},
		}
		for i, arg := range changes {
			if _, err := store.UpdateRule(ctx, fullRuleUpdate(arg)); err != nil {
				t.Fatalf("UpdateRule(): %v", err)
			}
			if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+int64(i)+1; got != want {
				t.Fatalf("after change %d: version = %d, want %d", i, got, want)
			}
		}
	})

	// Nothing changed, so readers have nothing to pick up. The clauses are the
	// stored ones, spelled the way the database gives them back. The name gets
	// normalized first, so one that only differs by its spaces is the same too.
	t.Run("same values leave the version alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.1, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		for _, n := range []string{rule.Name, "  " + rule.Name + "  "} {
			if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: n, Serve: rule.Serve, Clauses: rule.Clauses})); err != nil {
				t.Fatalf("UpdateRule(): %v", err)
			}
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		}
	})

	// JSONB keeps its own spelling of the clauses, and callers send theirs, so
	// clauses are compared by what they mean, not by their bytes.
	t.Run("clauses spelled differently are the same", func(t *testing.T) {
		tests := map[string]struct{ stored, sent string }{
			"as first sent": {
				stored: validClauses,
				sent:   validClauses,
			},
			"extra spaces": {
				stored: validClauses,
				sent:   `[ {"attribute": "country", "operator": "eq", "kind": "string", "values": ["BJ"]} ]`,
			},
			"keys reordered": {
				stored: validClauses,
				sent:   `[{"values":["BJ"],"kind":"string","operator":"eq","attribute":"country"}]`,
			},
			"number written another way": {
				stored: `[{"attribute":"age","operator":"gte","kind":"number","values":[18.5]}]`,
				sent:   `[{"attribute":"age","operator":"gte","kind":"number","values":[18.50]}]`,
			},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(test.stored)})
				env := mustGetEnv(t, store, setting.EnvironmentID)
				got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: json.RawMessage(test.sent)}))
				if err != nil {
					t.Fatalf("UpdateRule(): %v", err)
				}
				if diff := cmp.Diff(rule, got); diff != "" {
					t.Errorf("UpdateRule() mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Errorf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Every part of a clause matters, and so does how many there are.
	t.Run("changed clauses count", func(t *testing.T) {
		const stored = `[{"attribute":"country","operator":"il","kind":"string","values":["BJ","FR"]}]`
		tests := map[string]string{
			"attribute":  `[{"attribute":"region","operator":"il","kind":"string","values":["BJ","FR"]}]`,
			"operator":   `[{"attribute":"country","operator":"cnt","kind":"string","values":["BJ","FR"]}]`,
			"value":      `[{"attribute":"country","operator":"il","kind":"string","values":["BJ","US"]}]`,
			"value gone": `[{"attribute":"country","operator":"il","kind":"string","values":["BJ"]}]`,
			"kind":       `[{"attribute":"country","operator":"il","kind":"number","values":[1]}]`,
			"new clause": `[{"attribute":"country","operator":"il","kind":"string","values":["BJ","FR"]},{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}]`,
		}
		for name, clauses := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(stored)})
				env := mustGetEnv(t, store, setting.EnvironmentID)
				got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: json.RawMessage(clauses)}))
				if err != nil {
					t.Fatalf("UpdateRule(): %v", err)
				}
				wantSameJSON(t, got.Clauses, clauses)
				if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
					t.Fatalf("version = %d, want %d", got, want)
				}
			})
		}
	})

	// Clauses are the same only when they're written the same, so a new order
	// counts as a change and gets saved, even if it would match the same users.
	t.Run("a new order counts", func(t *testing.T) {
		tests := map[string]struct{ stored, sent string }{
			"values reordered": {
				stored: `[{"attribute":"country","operator":"il","kind":"string","values":["BJ","FR"]}]`,
				sent:   `[{"attribute":"country","operator":"il","kind":"string","values":["FR","BJ"]}]`,
			},
			"clauses swapped": {
				stored: `[{"attribute":"age","operator":"gte","kind":"number","values":[18]},{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}]`,
				sent:   `[{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]},{"attribute":"age","operator":"gte","kind":"number","values":[18]}]`,
			},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(test.stored)})
				env := mustGetEnv(t, store, setting.EnvironmentID)
				if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: json.RawMessage(test.sent)})); err != nil {
					t.Fatalf("UpdateRule(): %v", err)
				}
				wantSameJSON(t, storedRule(t, store, rule.ID).Clauses, test.sent)
				if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
					t.Fatalf("version = %d, want %d", got, want)
				}
			})
		}
	})

	// Stored clauses that no longer parse don't stop an update replacing them.
	t.Run("bad stored clauses can be replaced", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		if _, err := store.db.Exec(`UPDATE rules SET clauses = '[{"attribute":"country","operator":"nope","kind":"string","values":["BJ"]}]' WHERE id = $1`, rule.ID); err != nil {
			t.Fatalf("corrupting the clauses: %v", err)
		}
		got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: json.RawMessage(validClauses)}))
		if err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		wantSameJSON(t, got.Clauses, validClauses)
		wantSameJSON(t, storedRule(t, store, rule.ID).Clauses, validClauses)
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("rejected update leaves versions alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		tests := map[string]database.UpdateRuleParams{
			"bad serve":  {ID: rule.ID, Serve: 2, Clauses: json.RawMessage(validClauses)},
			"no clauses": {ID: rule.ID, Clauses: json.RawMessage(`[]`)},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.UpdateRule(ctx, fullRuleUpdate(arg)); err == nil {
					t.Fatal("UpdateRule() = nil error, want it rejected")
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Updates racing each other don't overwrite each other's bump. Every value
	// is different, so each racer has something to change.
	t.Run("racing changes all count", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Serve: float32(i+1) / racers, Clauses: rule.Clauses}))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateRule(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Racers writing the same values each compare before they write, so only
	// the first one finds something to change.
	t.Run("racing same values count once", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: 0.75, Clauses: rule.Clauses}))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateRule(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// SDKs never see a rule's name, so a new name alone gives them nothing to
	// pick up. The name is still saved.
	t.Run("a name-only change leaves the version alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.25, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "b", Serve: rule.Serve, Clauses: rule.Clauses}))
		if err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		if got.Name != "b" {
			t.Errorf("UpdateRule() name = %q, want %q", got.Name, "b")
		}
		if stored := storedRule(t, store, rule.ID); stored.Name != "b" {
			t.Errorf("stored name = %q, want %q", stored.Name, "b")
		}
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Fatalf("environment changed (-want +got):\n%s", diff)
		}
	})
}

// An updated rule is logged in its setting's environment, so SDKs can pick it
// up.
func TestUpdateRuleLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The entry carries the flag's key, the rule's new serve and clauses, the
	// position it stays at and the version the environment moved to, and
	// nothing else. Other rules, flags and environments are around, so an
	// entry for any of them would show.
	t.Run("one entry for the rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateFlag(t, store, "dark-mode")
		mustCreateEnv(t, store, "staging")
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		before := changeLogCount(t, store)
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rules[1].ID, Name: "b", Serve: 0.75, Clauses: json.RawMessage(otherClauses)})); err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "rule-updated")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		wantSameJSON(t, *logs[0].RuleClauses, otherClauses)
		want := database.ChangeLog{
			TargetVersion:   mustGetEnv(t, store, setting.EnvironmentID).Version,
			EnvironmentID:   setting.EnvironmentID,
			FlagNewKey:      sql.NullString{String: "new-checkout", Valid: true},
			RuleServe:       sql.NullFloat64{Float64: 0.75, Valid: true},
			RuleNewPosition: sql.NullInt32{Int32: 1, Valid: true},
			Action:          "rule-updated",
		}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt", "RuleClauses")
		if diff := cmp.Diff(want, logs[0], ignore); diff != "" {
			t.Fatalf("change log mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// An SDK replacing the rule at the logged position with the logged values
	// ends up with the stored rule. 0.1 has no exact binary form, so the entry
	// has to hold the same float32.
	t.Run("entry matches the stored rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "a", Serve: 0.1, Clauses: json.RawMessage(otherClauses)})); err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "rule-updated")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		stored := storedRule(t, store, rule.ID)
		if got := float32(logs[0].RuleServe.Float64); got != stored.Serve {
			t.Errorf("logged serve %v, want %v", got, stored.Serve)
		}
		wantSameJSON(t, *logs[0].RuleClauses, string(stored.Clauses))
	})

	// The key and position are read when the rule changes, so a renamed flag
	// and a moved rule are logged as they are now.
	t.Run("logs the current key and position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		if _, err := store.UpdateFlag(ctx, setting.FlagID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[1].ID, Position: 0}); err != nil {
			t.Fatalf("ChangeRulePosition(): %v", err)
		}
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rules[1].ID, Name: "b", Serve: 0.75, Clauses: rules[1].Clauses})); err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		logs := storedChangeLogsFor(t, store, "rule-updated")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		if got := logs[0].FlagNewKey.String; got != "checkout-v2" {
			t.Errorf("logged key %q, want %q", got, "checkout-v2")
		}
		if got := logs[0].RuleNewPosition.Int32; got != 0 {
			t.Errorf("logged position %d, want 0", got)
		}
	})

	// Each change gets its own entry with its own version, and earlier ones
	// stay.
	t.Run("each change counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.25, Clauses: json.RawMessage(validClauses)})
		env := mustGetEnv(t, store, setting.EnvironmentID)
		changes := []struct {
			serve   float32
			clauses string
		}{{0.5, validClauses}, {0.5, otherClauses}, {0.75, validClauses}}
		for _, c := range changes {
			if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "a", Serve: c.serve, Clauses: json.RawMessage(c.clauses)})); err != nil {
				t.Fatalf("UpdateRule(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-updated")
		if len(logs) != len(changes) {
			t.Fatalf("%d entries, want %d", len(logs), len(changes))
		}
		for i, c := range changes {
			l := logs[i]
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if float32(l.RuleServe.Float64) != c.serve {
				t.Errorf("entry %d logged serve %v, want %v", i, l.RuleServe.Float64, c.serve)
			}
			wantSameJSON(t, *l.RuleClauses, c.clauses)
		}
	})

	// Nothing changed, so there is nothing to log. The name is normalized and
	// the clauses compared by meaning, so these are all the same rule.
	t.Run("same values log nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.1, Clauses: json.RawMessage(validClauses)})
		before := changeLogCount(t, store)
		for _, arg := range []database.UpdateRuleParams{
			{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: rule.Clauses},
			{ID: rule.ID, Name: "  " + rule.Name + "  ", Serve: rule.Serve, Clauses: rule.Clauses},
			{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: json.RawMessage(`[{"values":["BJ"],"kind":"string","operator":"eq","attribute":"country"}]`)},
		} {
			if _, err := store.UpdateRule(ctx, fullRuleUpdate(arg)); err != nil {
				t.Fatalf("UpdateRule(): %v", err)
			}
			if got := changeLogCount(t, store); got != before {
				t.Fatalf("%d entries after UpdateRule(%+v), want %d", got, arg, before)
			}
		}
	})

	// SDKs never see a rule's name, so a new name alone has nothing to log.
	t.Run("a name-only change logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		before := changeLogCount(t, store)
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "b", Serve: rule.Serve, Clauses: rule.Clauses})); err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		if got := changeLogCount(t, store); got != before {
			t.Fatalf("%d entries, want %d", got, before)
		}
	})

	// Updates racing each other each log the version they moved to, so no two
	// entries claim the same one. Every serve is different, and none is the 0.25
	// the rule starts with, so each racer has something to change.
	t.Run("racing changes log their own versions", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "a", Serve: float32(i+1) / (racers + 1), Clauses: rule.Clauses}))
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateRule(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-updated")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		serves := map[float64]bool{}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if serves[l.RuleServe.Float64] {
				t.Errorf("serve %v logged twice", l.RuleServe.Float64)
			}
			serves[l.RuleServe.Float64] = true
		}
		// The last entry is what an SDK ends up with, so it has to be what's
		// stored.
		if got, want := float32(logs[racers-1].RuleServe.Float64), storedRule(t, store, rule.ID).Serve; got != want {
			t.Errorf("last entry logged serve %v, want the stored %v", got, want)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("rejected update logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		before := changeLogCount(t, store)
		tests := map[string]database.UpdateRuleParams{
			"bad serve":    {ID: rule.ID, Name: "a", Serve: 2, Clauses: rule.Clauses},
			"no clauses":   {ID: rule.ID, Name: "a", Serve: 0.5, Clauses: json.RawMessage(`[]`)},
			"unknown rule": {ID: uuid.New(), Name: "a", Serve: 0.5, Clauses: rule.Clauses},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.UpdateRule(ctx, fullRuleUpdate(arg)); err == nil {
					t.Fatal("UpdateRule() = nil error, want it rejected")
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		rejectEnvBumps(t, store)
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "a", Serve: 0.75, Clauses: rule.Clauses})); err == nil {
			t.Fatal("UpdateRule() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "rule-updated")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The update, the bump and the log land together or not at all, so SDKs
	// never miss a change.
	t.Run("failed log undoes the update", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateNamedRules(t, store, setting.ID, "a")[0]
		env := mustGetEnv(t, store, setting.EnvironmentID)
		rejectNewChangeLogs(t, store)
		got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "b", Serve: 0.75, Clauses: json.RawMessage(otherClauses)}))
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the rule"))
		wantZeroRule(t, got)
		if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
			t.Errorf("rule changed after a failed log (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

func TestChangeRulePositionBumpCounts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("each move counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		for i, position := range []int32{1, 2, 0} {
			if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: position}); err != nil {
				t.Fatalf("ChangeRulePosition(): %v", err)
			}
			if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+int64(i)+1; got != want {
				t.Fatalf("after move %d: version = %d, want %d", i, got, want)
			}
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("own position leaves the version alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[1].ID, Position: 1}); err != nil {
			t.Fatalf("ChangeRulePosition(): %v", err)
		}
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Fatalf("environment changed (-want +got):\n%s", diff)
		}
	})

	t.Run("rejected move leaves versions alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		for _, position := range []int32{-1, 2} {
			if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: position}); err == nil {
				t.Fatalf("ChangeRulePosition() to %d = nil error, want it rejected", position)
			}
			if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
				t.Fatalf("environment changed (-want +got):\n%s", diff)
			}
		}
	})

	// The first racer moves the rule, and the rest find it already there.
	t.Run("racing moves to one position count once", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 2})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("ChangeRulePosition(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})
}

// replayRuleLogs replays the setting's rule entries the way an SDK would, in
// version order, and gives the serve of each rule it ends up with, in order.
// A created rule is inserted at its position, a deleted one removed from its
// position and a moved one taken from its old position and put at its new one,
// the rest moving to make room or close the gap.
func replayRuleLogs(t *testing.T, store *Store) []float32 {
	t.Helper()
	var rules []float32
	for _, l := range queryChangeLogs(t, store, "action IN ('rule-created', 'rule-deleted', 'rule-position-changed')") {
		p := int(l.RuleNewPosition.Int32)
		switch l.Action {
		case "rule-created":
			rules = slices.Insert(rules, p, float32(l.RuleServe.Float64))
		case "rule-deleted":
			if p >= len(rules) {
				t.Fatalf("entry at version %d deletes position %d of %d rules", l.TargetVersion, p, len(rules))
			}
			rules = slices.Delete(rules, p, p+1)
		case "rule-position-changed":
			old := int(l.RuleOldPosition.Int32)
			if old >= len(rules) || p >= len(rules) {
				t.Fatalf("entry at version %d moves position %d to %d of %d rules", l.TargetVersion, old, p, len(rules))
			}
			serve := rules[old]
			rules = slices.Insert(slices.Delete(rules, old, old+1), p, serve)
		}
	}
	return rules
}

// storedServes gives the serve of each of the setting's rules, in order.
func storedServes(t *testing.T, store *Store, settingID uuid.UUID) []float32 {
	t.Helper()
	rules, err := store.ListRulesBySetting(context.Background(), settingID)
	if err != nil {
		t.Fatalf("ListRulesBySetting(): %v", err)
	}
	var serves []float32
	for _, r := range rules {
		serves = append(serves, r.Serve)
	}
	return serves
}

// A moved rule is logged in its setting's environment, so SDKs can reorder
// their rules.
func TestChangeRulePositionLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// mustCreateServedRules gives the setting n rules, the i-th serving
	// (i+1)/10, so a replay can tell them apart.
	mustCreateServedRules := func(t *testing.T, settingID uuid.UUID, n int) []database.Rule {
		t.Helper()
		rules := make([]database.Rule, n)
		for i := range rules {
			rules[i] = mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: settingID, Position: int32(i), Serve: float32(i+1) / 10, Clauses: json.RawMessage(validClauses)})
		}
		return rules
	}
	move := func(t *testing.T, id uuid.UUID, position int32) {
		t.Helper()
		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: id, Position: position}); err != nil {
			t.Fatalf("ChangeRulePosition(): %v", err)
		}
	}

	// The entry carries the flag's key, both positions and the version the
	// environment moved to, and nothing else. The rules the move shifts get no
	// entry of their own, and other flags and environments are around, so an
	// entry for any of them would show.
	t.Run("one entry for the rule", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		mustCreateFlag(t, store, "dark-mode")
		mustCreateEnv(t, store, "staging")
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		before := changeLogCount(t, store)
		move(t, rules[0].ID, 2)
		wantRuleNames(t, store, setting.ID, "b", "c", "a", "d")
		want := []database.ChangeLog{{
			TargetVersion:   mustGetEnv(t, store, setting.EnvironmentID).Version,
			EnvironmentID:   setting.EnvironmentID,
			FlagNewKey:      sql.NullString{String: "new-checkout", Valid: true},
			RuleOldPosition: sql.NullInt32{Int32: 0, Valid: true},
			RuleNewPosition: sql.NullInt32{Int32: 2, Valid: true},
			Action:          "rule-position-changed",
		}}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, storedChangeLogsFor(t, store, "rule-position-changed"), ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// Moves in both directions, mixed with creates and deletes, replay to the
	// stored rules in the stored order.
	t.Run("replaying the entries gives the stored rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateServedRules(t, setting.ID, 5)
		move(t, rules[0].ID, 4)
		move(t, rules[3].ID, 0)
		if _, err := store.DeleteRule(ctx, rules[2].ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		move(t, rules[4].ID, 2)
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 1, Serve: 0.9, Clauses: json.RawMessage(validClauses)})
		move(t, rules[1].ID, 4)
		if diff := cmp.Diff(storedServes(t, store, setting.ID), replayRuleLogs(t, store)); diff != "" {
			t.Fatalf("replayed rules mismatch (-stored +replayed):\n%s", diff)
		}
	})

	// The key and old position are read when the rule moves, so a renamed flag
	// and a rule pushed by an earlier write are logged as they are then.
	t.Run("logs the current key and position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		if _, err := store.UpdateFlag(ctx, setting.FlagID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		// b moves from 1 to 2 to make room.
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Name: "new", Clauses: json.RawMessage(validClauses)})
		move(t, rules[1].ID, 0)
		logs := storedChangeLogsFor(t, store, "rule-position-changed")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		if got := logs[0].FlagNewKey.String; got != "checkout-v2" {
			t.Errorf("logged key %q, want %q", got, "checkout-v2")
		}
		if got := logs[0].RuleOldPosition.Int32; got != 2 {
			t.Errorf("logged old position %d, want 2", got)
		}
		if got := logs[0].RuleNewPosition.Int32; got != 0 {
			t.Errorf("logged new position %d, want 0", got)
		}
	})

	// Each move gets its own entry with its own version, and earlier ones stay.
	t.Run("each move counts", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		moves := []struct{ from, to int32 }{{0, 2}, {2, 1}, {1, 0}}
		for _, m := range moves {
			move(t, rules[0].ID, m.to)
		}
		logs := storedChangeLogsFor(t, store, "rule-position-changed")
		if len(logs) != len(moves) {
			t.Fatalf("%d entries, want %d", len(logs), len(moves))
		}
		for i, m := range moves {
			l := logs[i]
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if l.RuleOldPosition.Int32 != m.from || l.RuleNewPosition.Int32 != m.to {
				t.Errorf("entry %d logged a move from %d to %d, want %d to %d", i, l.RuleOldPosition.Int32, l.RuleNewPosition.Int32, m.from, m.to)
			}
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("own position logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		before := changeLogCount(t, store)
		move(t, rules[1].ID, 1)
		if got := changeLogCount(t, store); got != before {
			t.Fatalf("%d entries, want %d", got, before)
		}
	})

	// Moves racing each other each log the version they moved to and the
	// positions they saw then, so replaying them gives the stored order.
	t.Run("racing moves log their own versions", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		const racers = 8
		rules := mustCreateServedRules(t, setting.ID, racers)
		env := mustGetEnv(t, store, setting.EnvironmentID)
		errs := make(chan error, racers)
		for i, r := range rules {
			go func() {
				// Each rule heads for the other end, so most of them still
				// have somewhere to go when their turn comes.
				_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: r.ID, Position: int32(racers - 1 - i)})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("ChangeRulePosition(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "rule-position-changed")
		if len(logs) == 0 {
			t.Fatal("no entries, want at least one")
		}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+int64(len(logs)); got != want {
			t.Errorf("version = %d, want one bump per entry, %d", got, want)
		}
		if diff := cmp.Diff(storedServes(t, store, setting.ID), replayRuleLogs(t, store)); diff != "" {
			t.Fatalf("replayed rules mismatch (-stored +replayed):\n%s", diff)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("rejected move logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		before := changeLogCount(t, store)
		tests := map[string]database.ChangeRulePositionParams{
			"negative position": {ID: rules[0].ID, Position: -1},
			"position past end": {ID: rules[0].ID, Position: 2},
			"unknown rule":      {ID: uuid.New(), Position: 1},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.ChangeRulePosition(ctx, arg); err == nil {
					t.Fatal("ChangeRulePosition() = nil error, want it rejected")
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		rejectEnvBumps(t, store)
		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 1}); err == nil {
			t.Fatal("ChangeRulePosition() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "rule-position-changed")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The move, its shift, the bump and the log land together or not at all,
	// so SDKs never evaluate rules in an order that's gone.
	t.Run("failed log undoes the move", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		env := mustGetEnv(t, store, setting.EnvironmentID)
		rejectNewChangeLogs(t, store)
		got, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 2})
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the rule"))
		wantZeroRule(t, got)
		wantRuleNames(t, store, setting.ID, "a", "b", "c")
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

// Rules belong to a flag setting, and go when its flag or environment does.
func TestRulesDeletedWithOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two settings with a rule each; each case deletes an owner of one and
	// expects only its rule to go.
	setup := func(t *testing.T) (setting database.FlagSetting, kept database.Rule) {
		t.Helper()
		setting = mustCreateRuleSetting(t, store)
		otherFlag := mustCreateFlag(t, store, "dark-mode")
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: otherFlag.ID, EnvironmentID: otherEnv.ID})
		mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		kept = mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: other.ID, Clauses: json.RawMessage(validClauses)})
		return setting, kept
	}
	wantOnly := func(t *testing.T, kept database.Rule) {
		t.Helper()
		if n := ruleCount(t, store); n != 1 {
			t.Fatalf("%d rules left, want 1", n)
		}
		if diff := cmp.Diff(kept, storedRule(t, store, kept.ID)); diff != "" {
			t.Fatalf("other rule changed (-want +got):\n%s", diff)
		}
	}

	t.Run("deleting a flag", func(t *testing.T) {
		setting, kept := setup(t)
		if _, err := store.DeleteFlag(ctx, setting.FlagID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		wantOnly(t, kept)
	})

	t.Run("deleting an environment", func(t *testing.T) {
		setting, kept := setup(t)
		if _, err := store.DeleteEnv(ctx, setting.EnvironmentID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		wantOnly(t, kept)
	})
}

// The unique check on a rule's position runs at commit.
func TestRulePositionsInTransaction(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("two rules can swap positions", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		a := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Clauses: json.RawMessage(validClauses)})
		b := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 1, Clauses: json.RawMessage(validClauses)})

		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx(): %v", err)
		}
		defer tx.Rollback()
		for _, step := range []struct {
			id       uuid.UUID
			position int32
		}{{a.ID, 1}, {b.ID, 0}} {
			if _, err := tx.ExecContext(ctx, "UPDATE rules SET position = $1 WHERE id = $2", step.position, step.id); err != nil {
				t.Fatalf("moving rule %v to %d: %v", step.id, step.position, err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit(): %v", err)
		}
		if p := storedRule(t, store, a.ID).Position; p != 1 {
			t.Errorf("first rule position = %d, want 1", p)
		}
		if p := storedRule(t, store, b.ID).Position; p != 0 {
			t.Errorf("second rule position = %d, want 0", p)
		}
	})

	t.Run("a duplicate fails the commit", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		arg := database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)}
		mustCreateRule(t, store, arg)

		err := store.ExecTx(ctx, func(q *database.Queries) error {
			_, err := q.CreateRule(ctx, arg)
			return err
		})
		var pqErr *pq.Error
		if !errors.As(err, &pqErr) || pqErr.Code != "23505" {
			t.Fatalf("ExecTx() error = %v, want a unique violation", err)
		}
		if n := ruleCount(t, store); n != 1 {
			t.Fatalf("%d rules after a failed commit, want 1", n)
		}
	})
}

// The table enforces what it can on its own, for writes that skip the wrapper.
func TestRulesTableRejectsInvalid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)

	const checkViolation, notNullViolation = "23514", "23502"
	tests := map[string]struct {
		position int32
		serve    float32
		clauses  json.RawMessage
		want     string
	}{
		"negative position":  {position: -1, clauses: json.RawMessage(validClauses), want: checkViolation},
		"serve below 0":      {serve: -0.5, clauses: json.RawMessage(validClauses), want: checkViolation},
		"serve above 1":      {serve: 1.5, clauses: json.RawMessage(validClauses), want: checkViolation},
		"serve NaN":          {serve: float32(math.NaN()), clauses: json.RawMessage(validClauses), want: checkViolation},
		"clauses not a list": {clauses: json.RawMessage(`{}`), want: checkViolation},
		"empty clause list":  {clauses: json.RawMessage(`[]`), want: checkViolation},
		"JSON null clauses":  {clauses: json.RawMessage(`null`), want: checkViolation},
		"SQL null clauses":   {clauses: nil, want: notNullViolation},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := store.Queries.CreateRule(ctx, database.CreateRuleParams{
				FlagSettingID: setting.ID,
				Position:      test.position,
				Serve:         test.serve,
				Clauses:       test.clauses,
			})
			var pqErr *pq.Error
			if !errors.As(err, &pqErr) || string(pqErr.Code) != test.want {
				t.Fatalf("CreateRule() error = %v, want code %s", err, test.want)
			}
			if n := ruleCount(t, store); n != 0 {
				t.Fatalf("%d rules after a rejected insert, want 0", n)
			}
		})
	}
}

// GetRule passes one message for every failure, so each error below has it.
const getRuleMessage = "can't get the rule"

func TestGetRule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	otherEnv := mustCreateEnv(t, store, "staging")
	other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})

	// Rules with their own values in two settings, including the same position
	// in both, so a lookup that ignores the ID is caught.
	created := []database.Rule{
		mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: setting.ID,
			Position:      0,
			Name:          "Beta users",
			Serve:         0.25,
			Clauses:       json.RawMessage(validClauses),
		}),
		mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: setting.ID,
			Position:      1,
			Serve:         1,
			Clauses:       json.RawMessage(`[{"attribute":"age","operator":"gte","kind":"number","values":[18.5]},{"attribute":"isPro","operator":"eq","kind":"boolean","values":[true]}]`),
		}),
		mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: other.ID,
			Position:      0,
			Name:          "Everyone in Benin",
			Serve:         0,
			Clauses:       json.RawMessage(`[{"attribute":"country","operator":"il","kind":"string","values":["BJ","TG"]}]`),
		}),
	}

	for _, want := range created {
		t.Run("finds "+want.ID.String(), func(t *testing.T) {
			got, err := store.GetRule(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetRule(): %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("GetRule() mismatch (-want +got):\n%s", diff)
			}
			// What comes back has to be usable by the evaluator.
			if _, err := parseClauses(got.Clauses); err != nil {
				t.Fatalf("parseClauses() on the rule's clauses: %v", err)
			}
		})
	}

	// Reading a rule changes nothing.
	t.Run("leaves the rule as it was", func(t *testing.T) {
		want := created[0]
		if _, err := store.GetRule(ctx, want.ID); err != nil {
			t.Fatalf("GetRule(): %v", err)
		}
		if diff := cmp.Diff(want, storedRule(t, store, want.ID)); diff != "" {
			t.Fatalf("stored rule changed (-want +got):\n%s", diff)
		}
	})

	notFound := connect.NewError(connect.CodeNotFound, getRuleMessage)
	t.Run("not found", func(t *testing.T) {
		tests := map[string]uuid.UUID{
			"unknown ID": uuid.New(),
			"zero ID":    uuid.Nil(),
			// Easy to do, since these are UUIDs too.
			"flag setting ID": setting.ID,
			"flag ID":         setting.FlagID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := store.GetRule(ctx, id)
				wantConnectErr(t, err, notFound)
				wantZeroRule(t, got)
			})
		}
	})

	// A rule goes with its flag setting, so looking it up afterwards fails.
	t.Run("rule of a deleted flag", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		if _, err := store.DeleteFlag(ctx, setting.FlagID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		_, err := store.GetRule(ctx, rule.ID)
		wantConnectErr(t, err, notFound)
	})

	t.Run("rule of a deleted environment", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
		if _, err := store.DeleteEnv(ctx, setting.EnvironmentID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		_, err := store.GetRule(ctx, rule.ID)
		wantConnectErr(t, err, notFound)
	})
}

func TestGetRuleReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, getRuleMessage),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, getRuleMessage),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.GetRule(test.ctx, rule.ID)
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.GetRule(context.Background(), rule.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, getRuleMessage))
		wantZeroRule(t, got)
	})
}

// newTestStoreKeepingData is newTestStore without the reset, for a second store
// that looks at what a test has already made.
func newTestStoreKeepingData(t *testing.T) *Store {
	t.Helper()
	store, err := New(os.Getenv("TEST_DB_URL"))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	t.Cleanup(func() { store.db.Close() })
	return store
}

func TestListRulesBySetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	list := func(t *testing.T, settingID uuid.UUID) []database.Rule {
		t.Helper()
		got, err := store.ListRulesBySetting(ctx, settingID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(%v): %v", settingID, err)
		}
		return got
	}
	create := func(t *testing.T, settingID uuid.UUID, position int32) database.Rule {
		t.Helper()
		return mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: settingID, Position: position, Clauses: json.RawMessage(validClauses)})
	}
	wantPositions := func(t *testing.T, rules []database.Rule, want []int32) {
		t.Helper()
		var got []int32
		for _, r := range rules {
			got = append(got, r.Position)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("positions mismatch (-want +got):\n%s", diff)
		}
	}

	t.Run("returns the rules as stored", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		want := []database.Rule{
			mustCreateRule(t, store, database.CreateRuleParams{
				FlagSettingID: setting.ID,
				Position:      0,
				Name:          "Beta users",
				Serve:         0.25,
				Clauses:       json.RawMessage(validClauses),
			}),
			mustCreateRule(t, store, database.CreateRuleParams{
				FlagSettingID: setting.ID,
				Position:      1,
				Serve:         1,
				Clauses:       json.RawMessage(`[{"attribute":"age","operator":"gte","kind":"number","values":[18.5]}]`),
			}),
		}
		if diff := cmp.Diff(want, list(t, setting.ID)); diff != "" {
			t.Fatalf("ListRulesBySetting() mismatch (-want +got):\n%s", diff)
		}
	})

	// The first matching rule wins, so the order decides what users get. These
	// are made out of order, which CreateRule doesn't allow, so they go through
	// the query. 10 sorts before 2 if compared as text.
	t.Run("ordered by position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		for _, position := range []int32{10, 2, 0} {
			_, err := store.Queries.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: position, Clauses: json.RawMessage(validClauses)})
			if err != nil {
				t.Fatalf("creating a rule at position %d: %v", position, err)
			}
		}
		wantPositions(t, list(t, setting.ID), []int32{0, 2, 10})

		// Reading through the unique index gives this order even without an
		// ORDER BY. Without the index, rows come back in the order they were
		// made. SET only lasts for one connection, so the pool is limited to one.
		noIndex := newTestStoreKeepingData(t)
		noIndex.db.SetMaxOpenConns(1)
		for _, scan := range []string{"enable_indexscan", "enable_indexonlyscan", "enable_bitmapscan"} {
			if _, err := noIndex.db.ExecContext(ctx, "SET "+scan+" = off"); err != nil {
				t.Fatalf("turning off %s: %v", scan, err)
			}
		}
		got, err := noIndex.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting() without indexes: %v", err)
		}
		wantPositions(t, got, []int32{0, 2, 10})
	})

	t.Run("follows a change of position", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		a := create(t, setting.ID, 0)
		b := create(t, setting.ID, 1)
		if _, err := store.db.ExecContext(ctx, "UPDATE rules SET position = 5 WHERE id = $1", a.ID); err != nil {
			t.Fatalf("moving the rule: %v", err)
		}
		got := list(t, setting.ID)
		wantPositions(t, got, []int32{1, 5})
		if got[0].ID != b.ID || got[1].ID != a.ID {
			t.Fatalf("rules = %v, %v, want %v, %v", got[0].ID, got[1].ID, b.ID, a.ID)
		}
	})

	// The same flag in another environment has its own rules, at the same
	// positions, and so does another flag in the same environment.
	t.Run("only the setting's rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		otherFlag := mustCreateFlag(t, store, "dark-mode")
		sameFlag := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		sameEnv := mustUpdateFlagSetting(t, store, pairSetting{FlagID: otherFlag.ID, EnvironmentID: setting.EnvironmentID})

		want := map[uuid.UUID][]database.Rule{}
		for _, id := range []uuid.UUID{setting.ID, sameFlag.ID, sameEnv.ID} {
			want[id] = []database.Rule{create(t, id, 0), create(t, id, 1)}
		}
		for id, rules := range want {
			if diff := cmp.Diff(rules, list(t, id)); diff != "" {
				t.Errorf("ListRulesBySetting(%v) mismatch (-want +got):\n%s", id, diff)
			}
		}
	})

	// None of these is an error: a setting without rules just falls through,
	// and an unknown setting looks the same.
	t.Run("nothing to list", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		other := mustUpdateFlagSetting(t, store, pairSetting{
			FlagID:        mustCreateFlag(t, store, "dark-mode").ID,
			EnvironmentID: setting.EnvironmentID,
		})
		create(t, other.ID, 0)

		tests := map[string]uuid.UUID{
			"setting without rules": setting.ID,
			"unknown setting":       uuid.New(),
			"zero ID":               uuid.Nil(),
			// Easy to do, since these are UUIDs too.
			"flag ID": other.FlagID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				if got := list(t, id); len(got) != 0 {
					t.Fatalf("ListRulesBySetting() = %+v, want no rules", got)
				}
			})
		}
	})

	t.Run("empty once the flag is deleted", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		create(t, setting.ID, 0)
		if _, err := store.DeleteFlag(ctx, setting.FlagID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if got := list(t, setting.ID); len(got) != 0 {
			t.Fatalf("ListRulesBySetting() = %+v, want no rules", got)
		}
	})
}

func TestListRulesBySettingReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
	const message = "can't list the flag setting's rules"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ListRulesBySetting(test.ctx, setting.ID)
			wantConnectErr(t, err, test.want)
			if got != nil {
				t.Fatalf("ListRulesBySetting() = %+v on error, want nil", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.ListRulesBySetting(context.Background(), setting.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		if got != nil {
			t.Fatalf("ListRulesBySetting() = %+v on error, want nil", got)
		}
	})
}

func TestDeleteRule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't delete the rule")

	create := func(t *testing.T, settingID uuid.UUID, position int32) database.Rule {
		t.Helper()
		return mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: settingID, Position: position, Clauses: json.RawMessage(validClauses)})
	}

	t.Run("deletes and returns the rule as it was", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		create(t, setting.ID, 0)
		rule := mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: setting.ID,
			Position:      1,
			Name:          "Beta users",
			Serve:         0.25,
			Clauses:       json.RawMessage(validClauses),
		})
		got, err := store.DeleteRule(ctx, rule.ID)
		if err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		if diff := cmp.Diff(rule, got); diff != "" {
			t.Fatalf("DeleteRule() mismatch (-want +got):\n%s", diff)
		}
		_, err = store.GetRule(ctx, rule.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, getRuleMessage))
		if n := ruleCount(t, store); n != 1 {
			t.Fatalf("%d rules after the delete, want 1", n)
		}
	})

	// Rules in another setting can have the same position.
	t.Run("leaves other settings' rules alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		rule := create(t, setting.ID, 0)
		kept := create(t, other.ID, 0)

		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		if n := ruleCount(t, store); n != 1 {
			t.Fatalf("%d rules after the delete, want 1", n)
		}
		if diff := cmp.Diff(kept, storedRule(t, store, kept.ID)); diff != "" {
			t.Fatalf("other setting's rule changed (-want +got):\n%s", diff)
		}
	})

	// Rules after the deleted one get a position one lower, so there's no gap.
	t.Run("fills the gap", func(t *testing.T) {
		for deleted := range 4 {
			t.Run(fmt.Sprintf("deleting position %d", deleted), func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")

				got, err := store.DeleteRule(ctx, rules[deleted].ID)
				if err != nil {
					t.Fatalf("DeleteRule(): %v", err)
				}
				if diff := cmp.Diff(rules[deleted], got); diff != "" {
					t.Fatalf("DeleteRule() mismatch (-want +got):\n%s", diff)
				}

				remaining, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				if len(remaining) != 3 {
					t.Fatalf("%d rules left, want 3", len(remaining))
				}
				want := slices.Delete(slices.Clone(rules), deleted, deleted+1)
				for i := range want {
					// Only a rule that moved has a new updated_at.
					if i >= deleted {
						if !remaining[i].UpdatedAt.After(want[i].UpdatedAt) {
							t.Errorf("rule %q updated_at = %v, want it after %v", want[i].Name, remaining[i].UpdatedAt, want[i].UpdatedAt)
						}
						want[i].UpdatedAt = remaining[i].UpdatedAt
					}
					want[i].Position = int32(i)
				}
				if diff := cmp.Diff(want, remaining); diff != "" {
					t.Fatalf("remaining rules mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	// With no gap left, a new rule can go anywhere again.
	t.Run("a new rule fits after a delete", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		if _, err := store.DeleteRule(ctx, rules[1].ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		create(t, setting.ID, 0)
		got, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		for i, r := range got {
			if int(r.Position) != i {
				t.Fatalf("rule %d has position %d, want %d", i, r.Position, i)
			}
		}
	})

	// Rules in another setting can have the same positions.
	t.Run("other settings' rules don't move", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		want := mustCreateNamedRules(t, store, other.ID, "a", "b")

		if _, err := store.DeleteRule(ctx, rules[0].ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		got, err := store.ListRulesBySetting(ctx, other.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("other setting's rules changed (-want +got):\n%s", diff)
		}
	})

	// A missing rule moves nothing.
	t.Run("not found moves nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		want := mustCreateNamedRules(t, store, setting.ID, "a", "b")
		_, err := store.DeleteRule(ctx, uuid.New())
		wantConnectErr(t, err, notFound)
		got, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Fatalf("rules changed (-want +got):\n%s", diff)
		}
	})

	t.Run("leaves the flag setting alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		if diff := cmp.Diff(setting, storedFlagSetting(t, store, setting.ID)); diff != "" {
			t.Fatalf("flag setting changed (-want +got):\n%s", diff)
		}
	})

	t.Run("the position can be reused", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		again := create(t, setting.ID, 0)
		if again.ID == rule.ID {
			t.Fatalf("new rule reused the deleted rule's ID %v", rule.ID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		kept := create(t, setting.ID, 0)

		tests := map[string]uuid.UUID{
			"unknown ID": uuid.New(),
			"zero ID":    uuid.Nil(),
			// Easy to do, since these are UUIDs too. Passing the setting's ID
			// must not delete its rules.
			"flag setting ID": setting.ID,
			"flag ID":         setting.FlagID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := store.DeleteRule(ctx, id)
				wantConnectErr(t, err, notFound)
				wantZeroRule(t, got)
				if diff := cmp.Diff(kept, storedRule(t, store, kept.ID)); diff != "" {
					t.Fatalf("rule changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("deleting twice", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("first DeleteRule(): %v", err)
		}
		got, err := store.DeleteRule(ctx, rule.ID)
		wantConnectErr(t, err, notFound)
		wantZeroRule(t, got)
	})

	// The rule already went with its flag.
	t.Run("rule of a deleted flag", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		if _, err := store.DeleteFlag(ctx, setting.FlagID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		_, err := store.DeleteRule(ctx, rule.ID)
		wantConnectErr(t, err, notFound)
	})

	t.Run("rolled back with its transaction", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		boom := errors.New("boom")
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.DeleteRule(ctx, rule.ID); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("ExecTx() error = %v, want %v", err, boom)
		}
		if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
			t.Fatalf("rule changed after the rollback (-want +got):\n%s", diff)
		}
	})
}

// The lock comes before the delete: otherwise a create holding the lock could
// count a rule that's on its way out.
func TestDeleteRuleLeavesRuleUntilLocked(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})

	tx, _ := beginMatrixTx(t, store)
	defer tx.Rollback()

	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteRule(ctx, rule.ID)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)

	// NOWAIT fails at once if the delete already holds the rule's row.
	if _, err := tx.ExecContext(ctx, "SELECT id FROM rules WHERE id = $1 FOR UPDATE NOWAIT", rule.ID); err != nil {
		t.Errorf("rule's row is taken while the delete waits for the lock: %v", err)
	}
	tx.Rollback()
	if err := <-done; err != nil {
		t.Fatalf("DeleteRule(): %v", err)
	}
}

// Deletes racing for one rule have one winner, and the others find it gone. The
// lock holds them all back so they really do race.
func TestDeleteRuleRacing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})

	tx, _ := beginMatrixTx(t, store)
	defer tx.Rollback()

	const racers = 8
	errs := make(chan error, racers)
	for range racers {
		go func() {
			_, err := store.DeleteRule(ctx, rule.ID)
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(): %v", err)
	}

	wins := 0
	for range racers {
		if err := <-errs; err == nil {
			wins++
		} else {
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't delete the rule"))
		}
	}
	if wins != 1 {
		t.Fatalf("%d deletes succeeded, want 1", wins)
	}
}

// queueForMatrixLock makes ops queue up for the flag/env matrix lock in order,
// then releases them all and waits for them.
func queueForMatrixLock(t *testing.T, store *Store, ops ...func() error) {
	t.Helper()
	tx, _ := beginMatrixTx(t, store)
	defer tx.Rollback()
	errs := make(chan error, len(ops))
	for _, op := range ops {
		go func() { errs <- op() }()
		time.Sleep(100 * time.Millisecond)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(): %v", err)
	}
	for range ops {
		if err := <-errs; err != nil {
			t.Errorf("operation failed: %v", err)
		}
	}
}

// wantRuleNames checks a flag setting's rules are the named ones, in order, with
// no gaps in their positions.
func wantRuleNames(t *testing.T, store *Store, settingID uuid.UUID, want ...string) {
	t.Helper()
	rules, err := store.ListRulesBySetting(context.Background(), settingID)
	if err != nil {
		t.Fatalf("ListRulesBySetting(): %v", err)
	}
	var got []string
	for i, r := range rules {
		got = append(got, r.Name)
		if int(r.Position) != i {
			t.Errorf("rule %q has position %d, want %d", r.Name, r.Position, i)
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("rules mismatch (-want +got):\n%s", diff)
	}
}

// A rule's position can change while its delete waits for the lock, so the gap
// has to be filled from where the rule is by then.
func TestDeleteRuleWhileOthersMoveIt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	remove := func(id uuid.UUID) func() error {
		return func() error {
			_, err := store.DeleteRule(ctx, id)
			return err
		}
	}

	t.Run("an earlier rule is deleted first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d", "e")
		queueForMatrixLock(t, store, remove(rules[0].ID), remove(rules[1].ID), remove(rules[2].ID))
		wantRuleNames(t, store, setting.ID, "d", "e")
	})

	t.Run("a rule is created before it first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		queueForMatrixLock(t, store, func() error {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: 0, Name: "new", Clauses: json.RawMessage(validClauses)})
			return err
		}, remove(rules[2].ID))
		wantRuleNames(t, store, setting.ID, "new", "a", "b", "d")
	})
}

// The rule can go with its flag while the delete waits for the lock.
func TestDeleteRuleWhileItsFlagIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx(): %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM flags WHERE id = $1", setting.FlagID); err != nil {
		t.Fatalf("deleting the flag: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteRule(ctx, rule.ID)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	wantConnectErr(t, <-done, connect.NewError(connect.CodeNotFound, "can't delete the rule"))
}

func TestDeleteRuleReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
	const message = "can't delete the rule"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.DeleteRule(test.ctx, rule.ID)
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
				t.Fatalf("rule changed after a failed delete (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.DeleteRule(context.Background(), rule.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		wantZeroRule(t, got)
	})
}

func TestUpdateRule(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	create := func(t *testing.T, settingID uuid.UUID, position int32) database.Rule {
		t.Helper()
		return mustCreateRule(t, store, database.CreateRuleParams{
			FlagSettingID: settingID,
			Position:      position,
			Name:          "Beta users",
			Serve:         0.25,
			Clauses:       json.RawMessage(validClauses),
		})
	}
	update := func(t *testing.T, arg database.UpdateRuleParams) database.Rule {
		t.Helper()
		got, err := store.UpdateRule(ctx, fullRuleUpdate(arg))
		if err != nil {
			t.Fatalf("UpdateRule(%+v): %v", arg, err)
		}
		return got
	}

	t.Run("replaces the values and keeps the rest", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		create(t, setting.ID, 0)
		rule := create(t, setting.ID, 1)

		got := update(t, database.UpdateRuleParams{
			ID:      rule.ID,
			Name:    "Everyone in Benin",
			Serve:   0.75,
			Clauses: json.RawMessage(otherClauses),
		})
		if got.Name != "Everyone in Benin" || got.Serve != 0.75 {
			t.Errorf("UpdateRule() name, serve = %q, %v, want %q, 0.75", got.Name, got.Serve, "Everyone in Benin")
		}
		wantSameJSON(t, got.Clauses, otherClauses)
		if got.ID != rule.ID || got.FlagSettingID != rule.FlagSettingID || got.Position != rule.Position {
			t.Errorf("UpdateRule() id, setting, position = %v, %v, %v, want %v, %v, %v",
				got.ID, got.FlagSettingID, got.Position, rule.ID, rule.FlagSettingID, rule.Position)
		}
		if !got.CreatedAt.Equal(rule.CreatedAt) {
			t.Errorf("UpdateRule() created_at = %v, want %v", got.CreatedAt, rule.CreatedAt)
		}
		if !got.UpdatedAt.After(rule.UpdatedAt) {
			t.Errorf("UpdateRule() updated_at = %v, want it after %v", got.UpdatedAt, rule.UpdatedAt)
		}
		if diff := cmp.Diff(got, storedRule(t, store, rule.ID)); diff != "" {
			t.Fatalf("stored rule mismatch (-returned +stored):\n%s", diff)
		}
	})

	t.Run("each change moves updated_at", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		arg := database.UpdateRuleParams{ID: rule.ID, Name: "first", Serve: rule.Serve, Clauses: rule.Clauses}

		first := update(t, arg)
		arg.Name = "second"
		second := update(t, arg)
		if !first.UpdatedAt.After(rule.UpdatedAt) {
			t.Errorf("first updated_at = %v, want it after %v", first.UpdatedAt, rule.UpdatedAt)
		}
		if !second.UpdatedAt.After(first.UpdatedAt) {
			t.Errorf("second updated_at = %v, want it after %v", second.UpdatedAt, first.UpdatedAt)
		}
	})

	// Writing the values the rule already has isn't an update, so updated_at
	// stays put and the caller still gets the rule back. The clauses are the
	// stored ones, spelled the way the database gives them back.
	t.Run("same values change nothing", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		got := update(t, database.UpdateRuleParams{ID: rule.ID, Name: rule.Name, Serve: rule.Serve, Clauses: rule.Clauses})
		if diff := cmp.Diff(rule, got); diff != "" {
			t.Errorf("UpdateRule() mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
			t.Errorf("stored rule changed (-want +got):\n%s", diff)
		}
	})

	t.Run("normalizes the name", func(t *testing.T) {
		tests := map[string]struct{ name, want string }{
			"kept as given":          {"Everyone", "Everyone"},
			"surrounding whitespace": {" \t Everyone \n", "Everyone"},
			"inner whitespace kept":  {"Every  one", "Every  one"},
			// A name is optional, so it can be cleared.
			"empty":             {"", ""},
			"only spaces":       {"   ", ""},
			"longest":           {strings.Repeat("a", 200), strings.Repeat("a", 200)},
			"longest non-ASCII": {strings.Repeat("é", 200), strings.Repeat("é", 200)},
			// The limit applies after trimming.
			"longest with whitespace": {"  " + strings.Repeat("a", 200) + "  ", strings.Repeat("a", 200)},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rule := create(t, setting.ID, 0)
				got := update(t, database.UpdateRuleParams{ID: rule.ID, Name: test.name, Clauses: json.RawMessage(validClauses)})
				if got.Name != test.want {
					t.Errorf("UpdateRule() name = %q, want %q", got.Name, test.want)
				}
				if s := storedRule(t, store, rule.ID); s.Name != test.want {
					t.Errorf("stored name = %q, want %q", s.Name, test.want)
				}
			})
		}
	})

	t.Run("stores serve as given", func(t *testing.T) {
		tests := map[string]float32{
			"nobody":              0,
			"everybody":           1,
			"not exact in binary": 0.1,
			"smallest above 0":    math.SmallestNonzeroFloat32,
			"largest below 1":     math.Nextafter32(1, 0),
		}
		for name, serve := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rule := create(t, setting.ID, 0)
				got := update(t, database.UpdateRuleParams{ID: rule.ID, Serve: serve, Clauses: json.RawMessage(validClauses)})
				if got.Serve != serve {
					t.Errorf("UpdateRule() serve = %v, want %v", got.Serve, serve)
				}
				if s := storedRule(t, store, rule.ID); s.Serve != serve {
					t.Errorf("stored serve = %v, want %v", s.Serve, serve)
				}
			})
		}
	})

	// What's saved has to load: rules are parsed again when they're read.
	t.Run("saved clauses parse back", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		update(t, database.UpdateRuleParams{ID: rule.ID, Clauses: json.RawMessage(otherClauses)})

		clauses, err := parseClauses(storedRule(t, store, rule.ID).Clauses)
		if err != nil {
			t.Fatalf("parseClauses() on the stored clauses: %v", err)
		}
		if len(clauses) != 2 {
			t.Fatalf("parseClauses() returned %d clauses, want 2", len(clauses))
		}
		match := map[string][]any{"age": {20.0}, "isPro": {true}}
		for i, clause := range clauses {
			if ok, err := clause.Match(match); err != nil || !ok {
				t.Errorf("clause %d Match(%v) = %v, %v, want true", i, match, ok, err)
			}
		}
	})

	// A rule keeps its place in the evaluation order.
	t.Run("the setting's order stays the same", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		first := create(t, setting.ID, 0)
		second := create(t, setting.ID, 1)

		updated := update(t, database.UpdateRuleParams{ID: second.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)})
		got, err := store.ListRulesBySetting(ctx, setting.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff([]database.Rule{first, updated}, got); diff != "" {
			t.Fatalf("rules mismatch (-want +got):\n%s", diff)
		}
	})

	// Rules in another setting can have the same position.
	t.Run("leaves other rules alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		rule := create(t, setting.ID, 0)
		kept := []database.Rule{create(t, setting.ID, 1), create(t, other.ID, 0)}

		update(t, database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)})
		for _, want := range kept {
			if diff := cmp.Diff(want, storedRule(t, store, want.ID)); diff != "" {
				t.Errorf("other rule changed (-want +got):\n%s", diff)
			}
		}
	})

	t.Run("leaves the flag setting alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		update(t, database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)})
		if diff := cmp.Diff(setting, storedFlagSetting(t, store, setting.ID)); diff != "" {
			t.Fatalf("flag setting changed (-want +got):\n%s", diff)
		}
	})

	t.Run("not found", func(t *testing.T) {
		notFound := connect.NewError(connect.CodeNotFound, "can't update the rule")
		setting := mustCreateRuleSetting(t, store)
		kept := create(t, setting.ID, 0)

		tests := map[string]uuid.UUID{
			"unknown ID": uuid.New(),
			"zero ID":    uuid.Nil(),
			// Easy to do, since these are UUIDs too. Passing the setting's ID
			// must not change its rules.
			"flag setting ID": setting.ID,
			"flag ID":         setting.FlagID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: id, Name: "Everyone", Clauses: json.RawMessage(validClauses)}))
				wantConnectErr(t, err, notFound)
				wantZeroRule(t, got)
				if diff := cmp.Diff(kept, storedRule(t, store, kept.ID)); diff != "" {
					t.Fatalf("rule changed (-want +got):\n%s", diff)
				}
			})
		}

		t.Run("deleted rule", func(t *testing.T) {
			rule := create(t, setting.ID, 1)
			if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
				t.Fatalf("DeleteRule(): %v", err)
			}
			got, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)}))
			wantConnectErr(t, err, notFound)
			wantZeroRule(t, got)
		})
	})

	t.Run("rolled back with its transaction", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		boom := errors.New("boom")
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.UpdateRule(ctx, database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("ExecTx() error = %v, want %v", err, boom)
		}
		if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
			t.Fatalf("rule changed after the rollback (-want +got):\n%s", diff)
		}
	})
}

func TestUpdateRuleRejectsInvalid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	invalid := func(message string) *connect.Error {
		return connect.NewError(connect.CodeInvalidArgument, message)
	}
	tooLong := invalid("can't update a rule with a name over 200 characters")
	outside := invalid("can't update a rule with a serve value outside [0, 1]")
	// Postgres finds these, so they carry no details.
	badText := invalid("can't update the rule")

	valid := func() database.UpdateRuleParams {
		return database.UpdateRuleParams{Name: "Everyone", Serve: 0.5, Clauses: json.RawMessage(validClauses)}
	}
	tests := map[string]struct {
		change func(*database.UpdateRuleParams)
		want   *connect.Error
	}{
		"name too long":           {func(a *database.UpdateRuleParams) { a.Name = strings.Repeat("a", 201) }, tooLong},
		"non-ASCII name too long": {func(a *database.UpdateRuleParams) { a.Name = strings.Repeat("é", 201) }, tooLong},
		"NUL in name":             {func(a *database.UpdateRuleParams) { a.Name = "a\x00b" }, badText},
		"invalid UTF-8 in name":   {func(a *database.UpdateRuleParams) { a.Name = "a\xffb" }, badText},

		"serve negative":          {func(a *database.UpdateRuleParams) { a.Serve = -0.5 }, outside},
		"serve just below 0":      {func(a *database.UpdateRuleParams) { a.Serve = -math.SmallestNonzeroFloat32 }, outside},
		"serve just above 1":      {func(a *database.UpdateRuleParams) { a.Serve = math.Nextafter32(1, 2) }, outside},
		"serve as a percentage":   {func(a *database.UpdateRuleParams) { a.Serve = 50 }, outside},
		"serve NaN":               {func(a *database.UpdateRuleParams) { a.Serve = float32(math.NaN()) }, outside},
		"serve positive infinity": {func(a *database.UpdateRuleParams) { a.Serve = float32(math.Inf(1)) }, outside},
		"serve negative infinity": {func(a *database.UpdateRuleParams) { a.Serve = float32(math.Inf(-1)) }, outside},

		// TestParseClausesRejectsInvalid covers every way clauses can be
		// wrong. These check the error reaches the caller as bad input.
		"no clauses": {func(a *database.UpdateRuleParams) { a.Clauses = nil }, invalid("invalid clauses: EOF")},
		"empty clause list": {func(a *database.UpdateRuleParams) { a.Clauses = json.RawMessage(`[]`) },
			invalid("invalid clauses: at least one clause is required")},
		"null clauses": {func(a *database.UpdateRuleParams) { a.Clauses = json.RawMessage(`null`) },
			invalid("invalid clauses: at least one clause is required")},
		"unknown clause kind": {func(a *database.UpdateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"date","values":["x"]}]`)
		}, invalid(`clause at index 0: unknown kind "date"`)},
		"second clause invalid": {func(a *database.UpdateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"string","values":["x"]},{"attribute":"","operator":"eq","kind":"string","values":["x"]}]`)
		}, invalid("clause at index 1: can't create a clause with empty attribute")},
		// parseClauses accepts these, but JSONB can't hold them.
		"NUL in clause attribute": {func(a *database.UpdateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"a\u0000b","operator":"eq","kind":"string","values":["x"]}]`)
		}, badText},
		"NUL in clause value": {func(a *database.UpdateRuleParams) {
			a.Clauses = json.RawMessage(`[{"attribute":"c","operator":"eq","kind":"string","values":["\u0000"]}]`)
		}, badText},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			rule := mustCreateRule(t, store, database.CreateRuleParams{
				FlagSettingID: setting.ID,
				Name:          "Beta users",
				Serve:         0.25,
				Clauses:       json.RawMessage(validClauses),
			})
			arg := valid()
			arg.ID = rule.ID
			test.change(&arg)

			got, err := store.UpdateRule(ctx, fullRuleUpdate(arg))
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
				t.Fatalf("rule changed after a rejected update (-want +got):\n%s", diff)
			}
		})
	}

	// Validation comes first, so bad input is reported even when the rule
	// doesn't exist.
	t.Run("input checked before the rule", func(t *testing.T) {
		tests := map[string]struct {
			change func(*database.UpdateRuleParams)
			want   *connect.Error
		}{
			"name":  {func(a *database.UpdateRuleParams) { a.Name = strings.Repeat("a", 201) }, tooLong},
			"serve": {func(a *database.UpdateRuleParams) { a.Serve = 2 }, outside},
			"clauses": {func(a *database.UpdateRuleParams) { a.Clauses = json.RawMessage(`[]`) },
				invalid("invalid clauses: at least one clause is required")},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				arg := valid()
				arg.ID = uuid.New()
				test.change(&arg)
				_, err := store.UpdateRule(ctx, fullRuleUpdate(arg))
				wantConnectErr(t, err, test.want)
			})
		}
	})
}

func TestUpdateRuleReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "Beta users", Clauses: json.RawMessage(validClauses)})
	arg := database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Serve: 0.5, Clauses: json.RawMessage(validClauses)}
	const message = "can't update the rule"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.UpdateRule(test.ctx, fullRuleUpdate(arg))
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
				t.Fatalf("rule changed after a failed update (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.UpdateRule(context.Background(), fullRuleUpdate(arg))
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		wantZeroRule(t, got)
	})
}

func TestCountRulesBySetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	create := func(t *testing.T, settingID uuid.UUID, position int32) database.Rule {
		t.Helper()
		return mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: settingID, Position: position, Clauses: json.RawMessage(validClauses)})
	}
	wantCount := func(t *testing.T, settingID uuid.UUID, want int) {
		t.Helper()
		got, err := store.CountRulesBySetting(ctx, settingID)
		if err != nil {
			t.Fatalf("CountRulesBySetting(%v): %v", settingID, err)
		}
		if got != want {
			t.Fatalf("CountRulesBySetting(%v) = %d, want %d", settingID, got, want)
		}
	}

	t.Run("counts the rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		for position := range int32(3) {
			create(t, setting.ID, position)
			wantCount(t, setting.ID, int(position)+1)
		}
	})

	// The count comes from the rows, not the positions: these go through the
	// query, as CreateRule doesn't allow gaps.
	t.Run("positions with gaps", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		for _, position := range []int32{10, 2, 0} {
			_, err := store.Queries.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Position: position, Clauses: json.RawMessage(validClauses)})
			if err != nil {
				t.Fatalf("creating a rule at position %d: %v", position, err)
			}
		}
		wantCount(t, setting.ID, 3)
	})

	// The same flag in another environment has its own rules, and so does
	// another flag in the same environment.
	t.Run("only the setting's rules", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		otherFlag := mustCreateFlag(t, store, "dark-mode")
		sameFlag := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		sameEnv := mustUpdateFlagSetting(t, store, pairSetting{FlagID: otherFlag.ID, EnvironmentID: setting.EnvironmentID})

		want := map[uuid.UUID]int{setting.ID: 1, sameFlag.ID: 2, sameEnv.ID: 3}
		for id, n := range want {
			for position := range n {
				create(t, id, int32(position))
			}
		}
		for id, n := range want {
			wantCount(t, id, n)
		}
	})

	// None of these is an error, as with ListRulesBySetting.
	t.Run("nothing to count", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		other := mustUpdateFlagSetting(t, store, pairSetting{
			FlagID:        mustCreateFlag(t, store, "dark-mode").ID,
			EnvironmentID: setting.EnvironmentID,
		})
		rule := create(t, other.ID, 0)

		tests := map[string]uuid.UUID{
			"setting without rules": setting.ID,
			"unknown setting":       uuid.New(),
			"zero ID":               uuid.Nil(),
			// Easy to do, since these are UUIDs too.
			"flag ID": other.FlagID,
			"rule ID": rule.ID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				wantCount(t, id, 0)
			})
		}
	})

	t.Run("goes down when a rule is deleted", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		create(t, setting.ID, 1)
		if _, err := store.DeleteRule(ctx, rule.ID); err != nil {
			t.Fatalf("DeleteRule(): %v", err)
		}
		wantCount(t, setting.ID, 1)
	})

	t.Run("stays the same when a rule is updated", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rule := create(t, setting.ID, 0)
		if _, err := store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: rule.ID, Name: "Everyone", Clauses: json.RawMessage(validClauses)})); err != nil {
			t.Fatalf("UpdateRule(): %v", err)
		}
		wantCount(t, setting.ID, 1)
	})

	t.Run("zero once the flag is deleted", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		create(t, setting.ID, 0)
		if _, err := store.DeleteFlag(ctx, setting.FlagID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		wantCount(t, setting.ID, 0)
	})

	// Counting inside a transaction sees its own changes, and they go if it
	// rolls back.
	t.Run("follows a transaction", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		boom := errors.New("boom")
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)}); err != nil {
				return err
			}
			n, err := q.CountRulesBySetting(ctx, setting.ID)
			if err != nil {
				return err
			}
			if n != 1 {
				t.Errorf("CountRulesBySetting() in the transaction = %d, want 1", n)
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("ExecTx() error = %v, want %v", err, boom)
		}
		wantCount(t, setting.ID, 0)
	})
}

func TestCountRulesBySettingReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Clauses: json.RawMessage(validClauses)})
	const message = "can't count the flag setting's rules"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.CountRulesBySetting(test.ctx, setting.ID)
			wantConnectErr(t, err, test.want)
			if got != 0 {
				t.Fatalf("CountRulesBySetting() = %d on error, want 0", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.CountRulesBySetting(context.Background(), setting.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		if got != 0 {
			t.Fatalf("CountRulesBySetting() = %d on error, want 0", got)
		}
	})
}

func TestChangeRulePosition(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Rules between the old and new positions get a new position, and rules
	// outside them stay exactly as they were.
	t.Run("moves the rule", func(t *testing.T) {
		tests := map[string]struct {
			from, to int32
			want     []string
		}{
			"one later":            {0, 1, []string{"b", "a", "c", "d"}},
			"first to last":        {0, 3, []string{"b", "c", "d", "a"}},
			"later from middle":    {1, 2, []string{"a", "c", "b", "d"}},
			"to last from middle":  {1, 3, []string{"a", "c", "d", "b"}},
			"one earlier":          {3, 2, []string{"a", "b", "d", "c"}},
			"last to first":        {3, 0, []string{"d", "a", "b", "c"}},
			"earlier from middle":  {2, 1, []string{"a", "c", "b", "d"}},
			"to first from middle": {2, 0, []string{"c", "a", "b", "d"}},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				setting := mustCreateRuleSetting(t, store)
				rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
				before := map[string]database.Rule{}
				for _, r := range rules {
					before[r.Name] = r
				}

				moved, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[test.from].ID, Position: test.to})
				if err != nil {
					t.Fatalf("ChangeRulePosition(): %v", err)
				}

				wantRuleNames(t, store, setting.ID, test.want...)
				got, err := store.ListRulesBySetting(ctx, setting.ID)
				if err != nil {
					t.Fatalf("ListRulesBySetting(): %v", err)
				}
				for i, r := range got {
					want := before[r.Name]
					want.Position = int32(i)
					// Only a rule that moved has a new updated_at.
					if before[r.Name].Position != int32(i) {
						if !r.UpdatedAt.After(want.UpdatedAt) {
							t.Errorf("rule %q updated_at = %v, want it after %v", r.Name, r.UpdatedAt, want.UpdatedAt)
						}
						want.UpdatedAt = r.UpdatedAt
					}
					if diff := cmp.Diff(want, r); diff != "" {
						t.Errorf("rule %q mismatch (-want +got):\n%s", r.Name, diff)
					}
				}
				if diff := cmp.Diff(got[test.to], moved); diff != "" {
					t.Errorf("returned rule mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("to its own position changes nothing", func(t *testing.T) {
		for _, position := range []int32{0, 1, 2} {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")

			got, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[position].ID, Position: position})
			if err != nil {
				t.Fatalf("ChangeRulePosition(): %v", err)
			}
			if diff := cmp.Diff(rules[position], got); diff != "" {
				t.Errorf("returned rule mismatch (-want +got):\n%s", diff)
			}
			stored, err := store.ListRulesBySetting(ctx, setting.ID)
			if err != nil {
				t.Fatalf("ListRulesBySetting(): %v", err)
			}
			if diff := cmp.Diff(rules, stored); diff != "" {
				t.Errorf("rules changed (-want +got):\n%s", diff)
			}
		}
	})

	t.Run("leaves other settings' rules alone", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		otherEnv := mustCreateEnv(t, store, "staging")
		other := mustUpdateFlagSetting(t, store, pairSetting{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		otherRules := mustCreateNamedRules(t, store, other.ID, "x", "y", "z")

		if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 2}); err != nil {
			t.Fatalf("ChangeRulePosition(): %v", err)
		}
		got, err := store.ListRulesBySetting(ctx, other.ID)
		if err != nil {
			t.Fatalf("ListRulesBySetting(): %v", err)
		}
		if diff := cmp.Diff(otherRules, got); diff != "" {
			t.Errorf("other setting's rules changed (-want +got):\n%s", diff)
		}
	})

	t.Run("several moves in a row", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		for _, move := range []struct {
			rule database.Rule
			to   int32
		}{{rules[0], 2}, {rules[2], 0}, {rules[1], 1}} {
			if _, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: move.rule.ID, Position: move.to}); err != nil {
				t.Fatalf("ChangeRulePosition(): %v", err)
			}
		}
		wantRuleNames(t, store, setting.ID, "c", "b", "a")
	})
}

func TestChangeRulePositionRejectsInvalid(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
	const negative = "can't update a rule with a negative position"
	const outside = "can't update a rule with a position outside [0, 2]"

	tests := map[string]struct {
		position int32
		message  string
	}{
		"negative":       {-1, negative},
		"smallest int32": {math.MinInt32, negative},
		"the rule count": {3, outside},
		"past the end":   {99, outside},
		"largest int32":  {math.MaxInt32, outside},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[1].ID, Position: test.position})
			wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, test.message))
			wantZeroRule(t, got)
			stored, err := store.ListRulesBySetting(ctx, setting.ID)
			if err != nil {
				t.Fatalf("ListRulesBySetting(): %v", err)
			}
			if diff := cmp.Diff(rules, stored); diff != "" {
				t.Fatalf("rules changed after a rejected move (-want +got):\n%s", diff)
			}
		})
	}

	t.Run("a lone rule can't move anywhere else", func(t *testing.T) {
		lone := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: lone.ID, Clauses: json.RawMessage(validClauses)})
		got, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rule.ID, Position: 1})
		wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, "can't update a rule with a position outside [0, 0]"))
		wantZeroRule(t, got)
	})
}

func TestChangeRulePositionNotFound(t *testing.T) {
	store := newTestStore(t)
	got, err := store.ChangeRulePosition(context.Background(), database.ChangeRulePositionParams{ID: uuid.New(), Position: 0})
	wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't update the rule"))
	wantZeroRule(t, got)
}

// The rule's position and the rule count can both change while a move waits for
// the lock, so it has to read them after.
func TestChangeRulePositionWhileOthersChangeRules(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	move := func(id uuid.UUID, position int32) func() error {
		return func() error {
			_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: id, Position: position})
			return err
		}
	}
	remove := func(id uuid.UUID) func() error {
		return func() error {
			_, err := store.DeleteRule(ctx, id)
			return err
		}
	}
	create := func(settingID uuid.UUID, position int32) func() error {
		return func() error {
			_, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: settingID, Position: position, Name: "new", Clauses: json.RawMessage(validClauses)})
			return err
		}
	}

	t.Run("a rule is created before it first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		queueForMatrixLock(t, store, create(setting.ID, 0), move(rules[2].ID, 0))
		wantRuleNames(t, store, setting.ID, "c", "new", "a", "b", "d")
	})

	t.Run("an earlier rule is deleted first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		queueForMatrixLock(t, store, remove(rules[0].ID), move(rules[1].ID, 2))
		wantRuleNames(t, store, setting.ID, "c", "d", "b")
	})

	t.Run("another move goes first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d")
		queueForMatrixLock(t, store, move(rules[3].ID, 0), move(rules[1].ID, 3))
		wantRuleNames(t, store, setting.ID, "d", "a", "c", "b")
	})

	t.Run("the range grows first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		queueForMatrixLock(t, store, create(setting.ID, 3), move(rules[0].ID, 3))
		wantRuleNames(t, store, setting.ID, "b", "c", "new", "a")
	})

	t.Run("the range shrinks first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		var moveErr error
		queueForMatrixLock(t, store, remove(rules[0].ID), func() error {
			moveErr = move(rules[2].ID, 2)()
			return nil
		})
		wantConnectErr(t, moveErr, connect.NewError(connect.CodeInvalidArgument, "can't update a rule with a position outside [0, 1]"))
		wantRuleNames(t, store, setting.ID, "b", "c")
	})

	t.Run("the rule is deleted first", func(t *testing.T) {
		setting := mustCreateRuleSetting(t, store)
		rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c")
		var moveErr error
		queueForMatrixLock(t, store, remove(rules[0].ID), func() error {
			moveErr = move(rules[0].ID, 1)()
			return nil
		})
		wantConnectErr(t, moveErr, connect.NewError(connect.CodeNotFound, "can't update the rule"))
		wantRuleNames(t, store, setting.ID, "b", "c")
	})
}

// Moves wait their turn, so however they land the positions have no gaps or
// duplicates.
func TestChangeRulePositionRacing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rules := mustCreateNamedRules(t, store, setting.ID, "a", "b", "c", "d", "e", "f")

	tx, _ := beginMatrixTx(t, store)
	defer tx.Rollback()

	const racers = 12
	errs := make(chan error, racers)
	for i := range racers {
		go func() {
			_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{
				ID:       rules[i%len(rules)].ID,
				Position: int32(i * 5 % len(rules)),
			})
			errs <- err
		}()
	}
	time.Sleep(200 * time.Millisecond)
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback(): %v", err)
	}
	for range racers {
		if err := <-errs; err != nil {
			t.Errorf("ChangeRulePosition(): %v", err)
		}
	}

	got, err := store.ListRulesBySetting(ctx, setting.ID)
	if err != nil {
		t.Fatalf("ListRulesBySetting(): %v", err)
	}
	var names []string
	for i, r := range got {
		names = append(names, r.Name)
		if int(r.Position) != i {
			t.Errorf("rule %q has position %d, want %d", r.Name, r.Position, i)
		}
	}
	slices.Sort(names)
	if diff := cmp.Diff([]string{"a", "b", "c", "d", "e", "f"}, names); diff != "" {
		t.Fatalf("rules mismatch (-want +got):\n%s", diff)
	}
}

// The rule can go with its flag while the move waits for the lock.
func TestChangeRulePositionWhileItsFlagIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx(): %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "DELETE FROM flags WHERE id = $1", setting.FlagID); err != nil {
		t.Fatalf("deleting the flag: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: rules[0].ID, Position: 1})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	wantConnectErr(t, <-done, connect.NewError(connect.CodeNotFound, "can't update the rule"))
}

func TestChangeRulePositionReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	rules := mustCreateNamedRules(t, store, setting.ID, "a", "b")
	arg := database.ChangeRulePositionParams{ID: rules[0].ID, Position: 1}
	const message = "can't update the rule"

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, message),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, message),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ChangeRulePosition(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			wantZeroRule(t, got)
			stored, err := store.ListRulesBySetting(context.Background(), setting.ID)
			if err != nil {
				t.Fatalf("ListRulesBySetting(): %v", err)
			}
			if diff := cmp.Diff(rules, stored); diff != "" {
				t.Fatalf("rules changed after a failed move (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.ChangeRulePosition(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, message))
		wantZeroRule(t, got)
	})
}

// An archived flag's rules are frozen until it's unarchived: none is created,
// changed, moved or deleted, so there's nothing to bump or log.
func TestRulesOfArchivedFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	ops := map[string]struct {
		call    func(setting database.FlagSetting, rule database.Rule) (database.Rule, error)
		message string
	}{
		"CreateRule": {
			call: func(s database.FlagSetting, _ database.Rule) (database.Rule, error) {
				return store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: s.ID, Position: 0, Name: "new", Serve: 1, Clauses: json.RawMessage(validClauses)})
			},
			message: "can't create rules for archived flag",
		},
		"UpdateRule": {
			call: func(_ database.FlagSetting, r database.Rule) (database.Rule, error) {
				return store.UpdateRule(ctx, fullRuleUpdate(database.UpdateRuleParams{ID: r.ID, Name: "renamed", Serve: 0.5, Clauses: json.RawMessage(otherClauses)}))
			},
			message: "can't update rules of archived flag",
		},
		"ChangeRulePosition": {
			call: func(_ database.FlagSetting, r database.Rule) (database.Rule, error) {
				return store.ChangeRulePosition(ctx, database.ChangeRulePositionParams{ID: r.ID, Position: 1})
			},
			message: "can't update rules of archived flag",
		},
		"DeleteRule": {
			call: func(_ database.FlagSetting, r database.Rule) (database.Rule, error) {
				return store.DeleteRule(ctx, r.ID)
			},
			message: "can't delete rules of archived flag",
		},
	}

	for name, op := range ops {
		t.Run(name+" is rejected", func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "first", "second")
			mustArchiveFlag(t, store, setting.FlagID)
			envBefore := mustGetEnv(t, store, setting.EnvironmentID)
			logsBefore := changeLogCount(t, store)

			got, err := op.call(setting, rules[0])
			wantConnectErr(t, err, connect.NewError(connect.CodeFailedPrecondition, op.message))
			wantZeroRule(t, got)
			for _, r := range rules {
				if diff := cmp.Diff(r, storedRule(t, store, r.ID)); diff != "" {
					t.Errorf("rule %s changed (-want +got):\n%s", r.Name, diff)
				}
			}
			if n := len(storedServes(t, store, setting.ID)); n != len(rules) {
				t.Errorf("%d rules, want %d", n, len(rules))
			}
			if diff := cmp.Diff(envBefore, mustGetEnv(t, store, setting.EnvironmentID)); diff != "" {
				t.Errorf("environment changed (-want +got):\n%s", diff)
			}
			if got := changeLogCount(t, store); got != logsBefore {
				t.Errorf("%d entries, want %d", got, logsBefore)
			}
		})

		t.Run(name+" works again once unarchived", func(t *testing.T) {
			setting := mustCreateRuleSetting(t, store)
			rules := mustCreateNamedRules(t, store, setting.ID, "first", "second")
			mustArchiveFlag(t, store, setting.FlagID)
			if _, err := store.UnarchiveFlag(ctx, setting.FlagID); err != nil {
				t.Fatalf("UnarchiveFlag(): %v", err)
			}
			if _, err := op.call(setting, rules[0]); err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
		})
	}
}

// A RuleUpdate changes only the values it sets; the others keep the rule's,
// read in the same transaction.
func TestUpdateRuleSetsOnlyGivenValues(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	create := func(t *testing.T) (database.FlagSetting, database.Rule) {
		t.Helper()
		setting := mustCreateRuleSetting(t, store)
		rule := mustCreateRule(t, store, database.CreateRuleParams{FlagSettingID: setting.ID, Name: "a", Serve: 0.25, Clauses: json.RawMessage(validClauses)})
		return setting, rule
	}
	name, serve, clauses := "b", float32(0.75), json.RawMessage(otherClauses)

	// Every mix of values, each to a new one. Only a new serve or new clauses
	// change how the rule evaluates, so only they bump and log; a new name is
	// stored but SDKs have nothing to pick up.
	tests := map[string]struct {
		update    RuleUpdate
		evaluated bool
	}{
		"nothing":                  {RuleUpdate{}, false},
		"only the name":            {RuleUpdate{Name: &name}, false},
		"only the serve":           {RuleUpdate{Serve: &serve}, true},
		"only the clauses":         {RuleUpdate{Clauses: &clauses}, true},
		"the name and serve":       {RuleUpdate{Name: &name, Serve: &serve}, true},
		"the name and clauses":     {RuleUpdate{Name: &name, Clauses: &clauses}, true},
		"the serve and clauses":    {RuleUpdate{Serve: &serve, Clauses: &clauses}, true},
		"the name, serve, clauses": {RuleUpdate{Name: &name, Serve: &serve, Clauses: &clauses}, true},
	}
	for testName, test := range tests {
		t.Run(testName, func(t *testing.T) {
			setting, rule := create(t)
			envBefore := mustGetEnv(t, store, setting.EnvironmentID)
			logsBefore := changeLogCount(t, store)
			arg := test.update
			arg.ID = rule.ID

			got, err := store.UpdateRule(ctx, arg)
			if err != nil {
				t.Fatalf("UpdateRule(): %v", err)
			}
			want := rule
			if arg.Name != nil {
				want.Name = name
			}
			if arg.Serve != nil {
				want.Serve = serve
			}
			if arg.Name == nil && arg.Serve == nil && arg.Clauses == nil {
				if diff := cmp.Diff(rule, got); diff != "" {
					t.Errorf("UpdateRule() with nothing set changed the rule (-want +got):\n%s", diff)
				}
			}
			if got.Name != want.Name || got.Serve != want.Serve {
				t.Errorf("UpdateRule() name, serve = %q, %v, want %q, %v", got.Name, got.Serve, want.Name, want.Serve)
			}
			if arg.Clauses != nil {
				wantSameJSON(t, got.Clauses, otherClauses)
			} else {
				wantSameJSON(t, got.Clauses, string(rule.Clauses))
			}
			if diff := cmp.Diff(got, storedRule(t, store, rule.ID)); diff != "" {
				t.Errorf("stored rule mismatch (-returned +stored):\n%s", diff)
			}

			wantVersion, wantLogs := envBefore.Version, logsBefore
			if test.evaluated {
				wantVersion, wantLogs = wantVersion+1, wantLogs+1
			}
			if got := mustGetEnv(t, store, setting.EnvironmentID).Version; got != wantVersion {
				t.Errorf("version = %d, want %d", got, wantVersion)
			}
			if got := changeLogCount(t, store); got != wantLogs {
				t.Errorf("%d entries, want %d", got, wantLogs)
			}
		})
	}

	// Setting a value the rule already has is no change, whatever else is
	// left unset: the rule comes back as it is, and nothing is bumped or
	// logged.
	t.Run("values the rule already has change nothing", func(t *testing.T) {
		for testName, update := range map[string]func(r database.Rule) RuleUpdate{
			"only the name":    func(r database.Rule) RuleUpdate { return RuleUpdate{Name: &r.Name} },
			"only the serve":   func(r database.Rule) RuleUpdate { return RuleUpdate{Serve: &r.Serve} },
			"only the clauses": func(r database.Rule) RuleUpdate { return RuleUpdate{Clauses: &r.Clauses} },
		} {
			t.Run(testName, func(t *testing.T) {
				setting, rule := create(t)
				envBefore := mustGetEnv(t, store, setting.EnvironmentID)
				logsBefore := changeLogCount(t, store)
				arg := update(rule)
				arg.ID = rule.ID
				got, err := store.UpdateRule(ctx, arg)
				if err != nil {
					t.Fatalf("UpdateRule(): %v", err)
				}
				if diff := cmp.Diff(rule, got); diff != "" {
					t.Errorf("UpdateRule() mismatch (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
					t.Errorf("stored rule changed (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(envBefore, mustGetEnv(t, store, setting.EnvironmentID)); diff != "" {
					t.Errorf("environment changed (-want +got):\n%s", diff)
				}
				if got := changeLogCount(t, store); got != logsBefore {
					t.Errorf("%d entries, want %d", got, logsBefore)
				}
			})
		}
	})

	// A value that's set is checked even when the others are left unset.
	t.Run("checks the values it sets", func(t *testing.T) {
		long, outside, none := strings.Repeat("a", 201), float32(1.5), json.RawMessage(`[]`)
		tests := map[string]struct {
			update RuleUpdate
			want   string
		}{
			"a name over 200 characters": {RuleUpdate{Name: &long}, "can't update a rule with a name over 200 characters"},
			"a serve outside [0, 1]":     {RuleUpdate{Serve: &outside}, "can't update a rule with a serve value outside [0, 1]"},
			"no clauses":                 {RuleUpdate{Clauses: &none}, "invalid clauses: at least one clause is required"},
		}
		for testName, test := range tests {
			t.Run(testName, func(t *testing.T) {
				setting, rule := create(t)
				envBefore := mustGetEnv(t, store, setting.EnvironmentID)
				arg := test.update
				arg.ID = rule.ID
				got, err := store.UpdateRule(ctx, arg)
				wantConnectErr(t, err, connect.NewError(connect.CodeInvalidArgument, test.want))
				wantZeroRule(t, got)
				if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
					t.Errorf("rule changed (-want +got):\n%s", diff)
				}
				if diff := cmp.Diff(envBefore, mustGetEnv(t, store, setting.EnvironmentID)); diff != "" {
					t.Errorf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// An update that sets nothing still has to name a rule.
	t.Run("nothing set for an id no rule has is not found", func(t *testing.T) {
		got, err := store.UpdateRule(ctx, RuleUpdate{ID: uuid.New()})
		wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't update the rule"))
		wantZeroRule(t, got)
	})

	// An archived flag's rules are frozen, whichever value an update sets.
	t.Run("rejects a setting of an archived flag", func(t *testing.T) {
		for testName, update := range map[string]RuleUpdate{
			"only the name":    {Name: &name},
			"only the serve":   {Serve: &serve},
			"only the clauses": {Clauses: &clauses},
		} {
			t.Run(testName, func(t *testing.T) {
				setting, rule := create(t)
				mustArchiveFlag(t, store, setting.FlagID)
				arg := update
				arg.ID = rule.ID
				got, err := store.UpdateRule(ctx, arg)
				wantConnectErr(t, err, connect.NewError(connect.CodeFailedPrecondition, "can't update rules of archived flag"))
				wantZeroRule(t, got)
				if diff := cmp.Diff(rule, storedRule(t, store, rule.ID)); diff != "" {
					t.Errorf("rule changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Two updates racing on different values both land: each reads the value
	// it leaves unset under the lock, so neither writes back a stale one.
	t.Run("racing updates of different values both land", func(t *testing.T) {
		for round := range 20 {
			_, rule := create(t)
			newName, newServe := fmt.Sprintf("renamed %d", round), float32(0.5)
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i, arg := range []RuleUpdate{{ID: rule.ID, Name: &newName}, {ID: rule.ID, Serve: &newServe}} {
				wg.Go(func() {
					_, errs[i] = store.UpdateRule(ctx, arg)
				})
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil {
					t.Fatalf("UpdateRule(): %v", err)
				}
			}
			if got := storedRule(t, store, rule.ID); got.Name != newName || got.Serve != newServe {
				t.Fatalf("round %d: stored name, serve = %q, %v, want %q, %v", round, got.Name, got.Serve, newName, newServe)
			}
		}
	})
}
