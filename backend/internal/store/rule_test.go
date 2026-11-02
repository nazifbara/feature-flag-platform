package store

import (
	"encoding/json"
	"strings"
	"testing"
)

// parseClauses needs no database, so these tests run without TEST_DB_URL.

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
