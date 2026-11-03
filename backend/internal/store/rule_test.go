package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/lib/pq"
	"github.com/nazifbara/flagsvc/backend/internal/database"
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
	resetDB(t, store.db)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	return mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
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
		t.Fatalf("CreateRule() returned a rule on error (-want +got):\n%s", diff)
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

	// Gaps are allowed, so deleting a rule doesn't renumber the others.
	t.Run("stores position as given", func(t *testing.T) {
		for _, position := range []int32{0, 1, 1000, math.MaxInt32} {
			setting := mustCreateRuleSetting(t, store)
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
		other := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: setting.FlagID, EnvironmentID: otherEnv.ID})

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

		"position -1":       {func(a *database.CreateRuleParams) { a.Position = -1 }, negative},
		"smallest position": {func(a *database.CreateRuleParams) { a.Position = math.MinInt32 }, negative},

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
				resetDB(t, store.db)
				arg := valid()
				arg.FlagSettingID = uuid.New()
				test.change(&arg)
				_, err := store.CreateRule(ctx, arg)
				wantConnectErr(t, err, test.want)
			})
		}
	})
}

func TestCreateRuleRejectsDuplicate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	setting := mustCreateRuleSetting(t, store)
	first := mustCreateRule(t, store, database.CreateRuleParams{
		FlagSettingID: setting.ID,
		Position:      0,
		Name:          "first",
		Serve:         0.25,
		Clauses:       json.RawMessage(validClauses),
	})

	// The position is taken whatever the other values are. The unique check
	// is deferred, so this also shows it still fires outside a transaction.
	got, err := store.CreateRule(ctx, database.CreateRuleParams{
		FlagSettingID: setting.ID,
		Position:      0,
		Name:          "second",
		Serve:         0.75,
		Clauses:       json.RawMessage(`[{"attribute":"age","operator":"gte","kind":"number","values":[18]}]`),
	})
	wantConnectErr(t, err, connect.NewError(connect.CodeAlreadyExists, "can't create the rule"))
	wantZeroRule(t, got)
	if n := ruleCount(t, store); n != 1 {
		t.Fatalf("%d rules after a duplicate, want 1", n)
	}
	if diff := cmp.Diff(first, storedRule(t, store, first.ID)); diff != "" {
		t.Fatalf("first rule changed (-want +got):\n%s", diff)
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
			got, err := store.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: id, Clauses: json.RawMessage(validClauses)})
			wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't create the rule"))
			wantZeroRule(t, got)
			if n := ruleCount(t, store); n != 0 {
				t.Fatalf("%d rules after a failed create, want 0", n)
			}
		})
	}
}

func TestCreateRuleReportsErrors(t *testing.T) {
	store := newTestStore(t)
	setting := mustCreateRuleSetting(t, store)
	arg := database.CreateRuleParams{FlagSettingID: setting.ID, Serve: 0.5, Clauses: json.RawMessage(validClauses)}

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
			want: connect.NewError(connect.CodeCanceled, "can't create the rule"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't create the rule"),
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
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the rule"))
		wantZeroRule(t, got)
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
		other := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: otherFlag.ID, EnvironmentID: otherEnv.ID})
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
