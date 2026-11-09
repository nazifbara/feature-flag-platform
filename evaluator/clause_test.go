package evaluator

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestNewClause(t *testing.T) {
	type newClauseTest struct {
		attr   string
		op     string
		kind   ClauseKind
		values []any
		want   Clause
		errStr string
	}
	tests := map[string]newClauseTest{
		"unknown operator": {
			attr:   "abc",
			op:     "equal",
			kind:   BooleanClause,
			values: []any{true},
			errStr: "unknown operator: equal",
		},
		"unknown kind rejected": {
			attr:   "abc",
			op:     eqOp,
			kind:   10000,
			values: []any{"a"},
			errStr: "unknown clause kind",
		},
		"eq operator not supported on multi-value clause": {
			attr:   "abc",
			op:     eqOp,
			kind:   NumberClause,
			values: []any{1.0, 2.0},
			errStr: "eq operator on multi-value clause",
		},
		"neq operator not supported on multi-value clause": {
			attr:   "abc",
			op:     neqOp,
			kind:   StringClause,
			values: []any{"a", "b"},
			errStr: "neq operator on multi-value clause",
		},
		"il operator accepts multiple values": {
			attr:   "country",
			op:     ilOp,
			kind:   StringClause,
			values: []any{"BJ", "FR"},
			want:   stringClause{attribute: "country", operator: ilOp, values: []string{"BJ", "FR"}},
		},
		"cnt operator accepts multiple values": {
			attr:   "country",
			op:     cntOp,
			kind:   StringClause,
			values: []any{"BJ", "FR"},
			want:   stringClause{attribute: "country", operator: cntOp, values: []string{"BJ", "FR"}},
		},
		"StringClause: invalid value type": {
			attr:   "abc",
			op:     ilOp,
			kind:   StringClause,
			values: []any{"1", 2, 3},
			errStr: "invalid value for StringClause: invalid value 2",
		},
		"StringClause: invalid value checked before unsupported operator": {
			attr:   "role",
			op:     gtOp,
			kind:   StringClause,
			values: []any{1.5},
			errStr: "invalid value for StringClause: invalid value 1.5",
		},
		"BooleanClause: invalid value type": {
			attr:   "isPro",
			op:     eqOp,
			kind:   BooleanClause,
			values: []any{"true"},
			errStr: "invalid value for BooleanClause: invalid value true",
		},
		"NumberClause: int value rejected": {
			attr:   "age",
			op:     gtOp,
			kind:   NumberClause,
			values: []any{18},
			errStr: "invalid value for NumberClause: invalid value 18",
		},
		"empty values rejected": {
			attr:   "name",
			op:     ilOp,
			kind:   StringClause,
			values: []any{},
			errStr: "can't create a clause with empty values",
		},
		"empty attribute rejected": {
			attr:   "",
			op:     eqOp,
			kind:   StringClause,
			values: []any{"a"},
			errStr: "can't create a clause with empty attribute",
		},
	}

	// Supported operators per kind are spelled out here rather than read from
	// clauseOperatorMap, so the test catches unintended changes to that map.
	kinds := []struct {
		kind    ClauseKind
		name    string
		value   any
		allowed []string
		want    func(op string) Clause
	}{
		{
			StringClause, "StringClause", "Na",
			[]string{eqOp, neqOp, stwOp, edwOp, cntOp, ilOp},
			func(op string) Clause { return stringClause{attribute: "attr", operator: op, values: []string{"Na"}} },
		},
		{
			BooleanClause, "BooleanClause", true,
			[]string{eqOp, neqOp},
			func(op string) Clause { return booleanClause{attribute: "attr", operator: op, values: []bool{true}} },
		},
		{
			NumberClause, "NumberClause", 18.0,
			[]string{eqOp, neqOp, gtOp, ltOp, gteOp, lteOp, ilOp, cntOp},
			func(op string) Clause { return numberClause{attribute: "attr", operator: op, values: []float64{18.0}} },
		},
	}

	for _, k := range kinds {
		for _, op := range operators {
			tc := newClauseTest{attr: "attr", op: op, kind: k.kind, values: []any{k.value}}
			if slices.Contains(k.allowed, op) {
				tc.want = k.want(op)
				tests[fmt.Sprintf("%s: create with %s operator", k.name, op)] = tc
			} else {
				tc.errStr = fmt.Sprintf("unsupported operator for %s: %s", k.name, op)
				tests[fmt.Sprintf("%s: %s operator not supported", k.name, op)] = tc
			}
		}
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := NewClause(tc.attr, tc.op, tc.kind, tc.values)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("NewClause() error = %v, want %q", err, tc.errStr)
				}
				if got != nil {
					t.Fatalf("NewClause() = %#v on error, want nil", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewClause() unexpected error: %v", err)
			}

			opt := cmp.AllowUnexported(stringClause{}, booleanClause{}, numberClause{})
			if diff := cmp.Diff(tc.want, got, opt); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func mustNewClause(t *testing.T, attr, op string, kind ClauseKind, values ...any) Clause {
	t.Helper()
	c, err := NewClause(attr, op, kind, values)
	if err != nil {
		t.Fatalf("NewClause(%q, %q, %v, %v): %v", attr, op, kind, values, err)
	}
	return c
}

func TestStringClauseMatch(t *testing.T) {
	str := func(op string, values ...any) Clause {
		return mustNewClause(t, "country", op, StringClause, values...)
	}

	tests := map[string]struct {
		clause Clause
		input  map[string][]any
		want   bool
		errStr string
	}{
		"false when attribute is not found": {
			clause: str(eqOp, "BJ"),
			input:  map[string][]any{"language": {"FR"}},
			want:   false,
		},
		"neq is false when attribute is not found": {
			clause: str(neqOp, "US"),
			input:  map[string][]any{"language": {"FR"}},
			want:   false,
		},
		"false on empty attribute values": {
			clause: str(eqOp, "BJ"),
			input:  map[string][]any{"country": {}},
			want:   false,
		},
		"non-string attribute value rejected": {
			clause: str(eqOp, "BJ"),
			input:  map[string][]any{"country": {1.0}},
			errStr: `invalid value for attribute "country": invalid value 1`,
		},
		"eq rejects attribute with multiple values": {
			clause: str(eqOp, "US"),
			input:  map[string][]any{"country": {"BJ", "US"}},
			errStr: "multi-value attribute not supported for eq operation in string clause",
		},
		"neq rejects attribute with multiple values": {
			clause: str(neqOp, "US"),
			input:  map[string][]any{"country": {"BJ", "US"}},
			errStr: "multi-value attribute not supported for neq operation in string clause",
		},
		"stw rejects attribute with multiple values": {
			clause: str(stwOp, "B"),
			input:  map[string][]any{"country": {"BJ", "BE"}},
			errStr: "multi-value attribute not supported for stw operation in string clause",
		},
		"edw rejects attribute with multiple values": {
			clause: str(edwOp, "J"),
			input:  map[string][]any{"country": {"BJ", "NJ"}},
			errStr: "multi-value attribute not supported for edw operation in string clause",
		},
		"eq true when equal": {
			clause: str(eqOp, "BJ"),
			input:  map[string][]any{"country": {"BJ"}, "language": {"FR"}},
			want:   true,
		},
		"eq false when different": {
			clause: str(eqOp, "US"),
			input:  map[string][]any{"country": {"BJ"}, "language": {"FR"}},
			want:   false,
		},
		"eq is case-sensitive": {
			clause: str(eqOp, "bj"),
			input:  map[string][]any{"country": {"BJ"}},
			want:   false,
		},
		"neq true when different": {
			clause: str(neqOp, "US"),
			input:  map[string][]any{"country": {"BJ"}, "language": {"FR"}},
			want:   true,
		},
		"neq false when equal": {
			clause: str(neqOp, "BJ"),
			input:  map[string][]any{"country": {"BJ"}, "language": {"FR"}},
			want:   false,
		},
		"il true when every attribute value is in the list": {
			clause: str(ilOp, "BJ", "NG", "US"),
			input:  map[string][]any{"country": {"US", "BJ"}},
			want:   true,
		},
		"il false when no attribute value is in the list": {
			clause: str(ilOp, "BJ", "NG", "US"),
			input:  map[string][]any{"country": {"NZ", "FR"}},
			want:   false,
		},
		"il false when only some attribute values are in the list": {
			clause: str(ilOp, "BJ", "NG", "US"),
			input:  map[string][]any{"country": {"US", "NZ"}},
			want:   false,
		},
		"cnt true when attribute has every clause value": {
			clause: str(cntOp, "BJ", "NG"),
			input:  map[string][]any{"country": {"NZ", "BJ", "FR", "NG"}},
			want:   true,
		},
		"cnt false when attribute misses a clause value": {
			clause: str(cntOp, "BJ", "NG"),
			input:  map[string][]any{"country": {"NZ", "BJ", "FR"}},
			want:   false,
		},
		// Same clause values and input: il and cnt must disagree.
		"il true where cnt is false": {
			clause: str(ilOp, "BJ", "FR", "US"),
			input:  map[string][]any{"country": {"BJ", "FR"}},
			want:   true,
		},
		"cnt false where il is true": {
			clause: str(cntOp, "BJ", "FR", "US"),
			input:  map[string][]any{"country": {"BJ", "FR"}},
			want:   false,
		},
		"stw true on prefix": {
			clause: str(stwOp, "he"),
			input:  map[string][]any{"country": {"hello world"}},
			want:   true,
		},
		"stw true when prefix is the whole value": {
			clause: str(stwOp, "hello"),
			input:  map[string][]any{"country": {"hello"}},
			want:   true,
		},
		"stw false when not a prefix": {
			clause: str(stwOp, "world"),
			input:  map[string][]any{"country": {"hello world"}},
			want:   false,
		},
		"stw is case-sensitive": {
			clause: str(stwOp, "He"),
			input:  map[string][]any{"country": {"hello"}},
			want:   false,
		},
		"edw true on suffix": {
			clause: str(edwOp, "rld"),
			input:  map[string][]any{"country": {"hello world"}},
			want:   true,
		},
		"edw false when not a suffix": {
			clause: str(edwOp, "rld"),
			input:  map[string][]any{"country": {"let's go touch some grass ld"}},
			want:   false,
		},
		"unsupported operator on hand-built clause": {
			clause: stringClause{attribute: "country", operator: gtOp, values: []string{"BJ"}},
			input:  map[string][]any{"country": {"BJ"}},
			errStr: "string clause with invalid operator: \"gt\"",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tc.clause.Match(tc.input)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("Match() error = %v, want %q", err, tc.errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNumberClauseMatch(t *testing.T) {
	num := func(op string, values ...any) Clause {
		return mustNewClause(t, "age", op, NumberClause, values...)
	}
	age := func(values ...any) map[string][]any {
		return map[string][]any{"age": values, "score": {99.0}}
	}

	tests := map[string]struct {
		clause Clause
		input  map[string][]any
		want   bool
		errStr string
	}{
		"false when attribute is not found": {
			clause: num(eqOp, 18.0),
			input:  map[string][]any{"score": {18.0}},
			want:   false,
		},
		"neq is false when attribute is not found": {
			clause: num(neqOp, 18.0),
			input:  map[string][]any{"score": {18.0}},
			want:   false,
		},
		"false on empty attribute values": {
			clause: num(gtOp, 18.0),
			input:  age(),
			want:   false,
		},
		"non-number attribute value rejected": {
			clause: num(eqOp, 18.0),
			input:  age("18"),
			errStr: `invalid value for attribute "age": invalid value 18`,
		},
		"int attribute value rejected": {
			clause: num(eqOp, 18.0),
			input:  age(18),
			errStr: `invalid value for attribute "age": invalid value 18`,
		},
		"eq rejects attribute with multiple values": {
			clause: num(eqOp, 18.0),
			input:  age(18.0, 18.0),
			errStr: "multi-value attribute not supported for eq operation in number clause",
		},
		"neq rejects attribute with multiple values": {
			clause: num(neqOp, 18.0),
			input:  age(17.0, 19.0),
			errStr: "multi-value attribute not supported for neq operation in number clause",
		},
		"gt rejects attribute with multiple values": {
			clause: num(gtOp, 18.0),
			input:  age(20.0, 30.0),
			errStr: "multi-value attribute not supported for gt operation in number clause",
		},
		"gte rejects attribute with multiple values": {
			clause: num(gteOp, 18.0),
			input:  age(20.0, 30.0),
			errStr: "multi-value attribute not supported for gte operation in number clause",
		},
		"lt rejects attribute with multiple values": {
			clause: num(ltOp, 18.0),
			input:  age(10.0, 12.0),
			errStr: "multi-value attribute not supported for lt operation in number clause",
		},
		"lte rejects attribute with multiple values": {
			clause: num(lteOp, 18.0),
			input:  age(10.0, 12.0),
			errStr: "multi-value attribute not supported for lte operation in number clause",
		},

		"eq true when equal": {
			clause: num(eqOp, 18.0),
			input:  age(18.0),
			want:   true,
		},
		"eq false when different": {
			clause: num(eqOp, 18.0),
			input:  age(18.5),
			want:   false,
		},
		"eq treats 0 and -0 as equal": {
			clause: num(eqOp, 0.0),
			input:  age(math.Copysign(0, -1)),
			want:   true,
		},
		"neq true when different": {
			clause: num(neqOp, 18.0),
			input:  age(21.0),
			want:   true,
		},
		"neq false when equal": {
			clause: num(neqOp, 18.0),
			input:  age(18.0),
			want:   false,
		},

		// Comparisons read as "attribute <op> clause value". Each operator is
		// checked below, at and above the clause value so that swapped operands,
		// swapped operators and off-by-equality mistakes all fail.
		"gt false when attribute is below": {
			clause: num(gtOp, 18.0),
			input:  age(17.0),
			want:   false,
		},
		"gt false when attribute is equal": {
			clause: num(gtOp, 18.0),
			input:  age(18.0),
			want:   false,
		},
		"gt true when attribute is above": {
			clause: num(gtOp, 18.0),
			input:  age(18.001),
			want:   true,
		},
		"gte false when attribute is below": {
			clause: num(gteOp, 18.0),
			input:  age(17.999),
			want:   false,
		},
		"gte true when attribute is equal": {
			clause: num(gteOp, 18.0),
			input:  age(18.0),
			want:   true,
		},
		"gte true when attribute is above": {
			clause: num(gteOp, 18.0),
			input:  age(40.0),
			want:   true,
		},
		"lt true when attribute is below": {
			clause: num(ltOp, 18.0),
			input:  age(17.999),
			want:   true,
		},
		"lt false when attribute is equal": {
			clause: num(ltOp, 18.0),
			input:  age(18.0),
			want:   false,
		},
		"lt false when attribute is above": {
			clause: num(ltOp, 18.0),
			input:  age(40.0),
			want:   false,
		},
		"lte true when attribute is below": {
			clause: num(lteOp, 18.0),
			input:  age(3.0),
			want:   true,
		},
		"lte true when attribute is equal": {
			clause: num(lteOp, 18.0),
			input:  age(18.0),
			want:   true,
		},
		"lte false when attribute is above": {
			clause: num(lteOp, 18.0),
			input:  age(18.001),
			want:   false,
		},
		"gt handles negative numbers": {
			clause: num(gtOp, -5.0),
			input:  age(-2.0),
			want:   true,
		},
		"lt handles negative numbers": {
			clause: num(ltOp, -5.0),
			input:  age(-2.0),
			want:   false,
		},

		"il true when every attribute value is in the list": {
			clause: num(ilOp, 1.0, 2.0, 3.0),
			input:  age(3.0, 1.0),
			want:   true,
		},
		"il true on single attribute value in the list": {
			clause: num(ilOp, 1.0, 2.0, 3.0),
			input:  age(2.0),
			want:   true,
		},
		"il false when no attribute value is in the list": {
			clause: num(ilOp, 1.0, 2.0, 3.0),
			input:  age(4.0, 5.0),
			want:   false,
		},
		"il false when only some attribute values are in the list": {
			clause: num(ilOp, 1.0, 2.0, 3.0),
			input:  age(3.0, 4.0),
			want:   false,
		},
		"cnt true when attribute has every clause value": {
			clause: num(cntOp, 1.0, 2.0),
			input:  age(5.0, 2.0, 9.0, 1.0),
			want:   true,
		},
		"cnt false when attribute misses a clause value": {
			clause: num(cntOp, 1.0, 2.0),
			input:  age(5.0, 2.0, 9.0),
			want:   false,
		},
		// Same clause values and input: il and cnt must disagree.
		"il true where cnt is false": {
			clause: num(ilOp, 1.0, 2.0, 3.0),
			input:  age(1.0, 2.0),
			want:   true,
		},
		"cnt false where il is true": {
			clause: num(cntOp, 1.0, 2.0, 3.0),
			input:  age(1.0, 2.0),
			want:   false,
		},

		// NaN compares unequal to everything, including itself.
		"eq false on NaN attribute": {
			clause: num(eqOp, 18.0),
			input:  age(math.NaN()),
			want:   false,
		},
		"gte false on NaN attribute": {
			clause: num(gteOp, 18.0),
			input:  age(math.NaN()),
			want:   false,
		},
		"lte false on NaN attribute": {
			clause: num(lteOp, 18.0),
			input:  age(math.NaN()),
			want:   false,
		},

		// Not reachable through NewClause; guards the default branch.
		"unsupported operator on hand-built clause": {
			clause: numberClause{attribute: "age", operator: stwOp, values: []float64{18.0}},
			input:  age(18.0),
			errStr: `number clause with invalid operator: "stw"`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tc.clause.Match(tc.input)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("Match() error = %v, want %q", err, tc.errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBooleanClauseMatch(t *testing.T) {
	boolean := func(op string, value bool) Clause {
		return mustNewClause(t, "isPro", op, BooleanClause, value)
	}
	isPro := func(values ...any) map[string][]any {
		return map[string][]any{"isPro": values, "isBeta": {true}}
	}

	tests := map[string]struct {
		clause Clause
		input  map[string][]any
		want   bool
		errStr string
	}{
		"false when attribute is not found": {
			clause: boolean(eqOp, true),
			input:  map[string][]any{"isBeta": {true}},
			want:   false,
		},
		"neq is false when attribute is not found": {
			clause: boolean(neqOp, true),
			input:  map[string][]any{"isBeta": {false}},
			want:   false,
		},
		"false on empty attribute values": {
			clause: boolean(eqOp, false),
			input:  isPro(),
			want:   false,
		},
		// No coercion from strings or numbers, even when they look boolean.
		"string attribute value rejected": {
			clause: boolean(eqOp, true),
			input:  isPro("true"),
			errStr: `invalid value for attribute "isPro": invalid value true`,
		},
		"number attribute value rejected": {
			clause: boolean(eqOp, true),
			input:  isPro(1.0),
			errStr: `invalid value for attribute "isPro": invalid value 1`,
		},
		"eq rejects attribute with multiple values": {
			clause: boolean(eqOp, true),
			input:  isPro(true, true),
			errStr: "multi-value attributes aren't supported in boolean clause",
		},
		"neq rejects attribute with multiple values": {
			clause: boolean(neqOp, true),
			input:  isPro(false, true),
			errStr: "multi-value attributes aren't supported in boolean clause",
		},

		// Full truth tables, so a constant result, an inverted check or a
		// swapped operator each fail at least one case.
		"eq true: true == true": {
			clause: boolean(eqOp, true),
			input:  isPro(true),
			want:   true,
		},
		"eq true: false == false": {
			clause: boolean(eqOp, false),
			input:  isPro(false),
			want:   true,
		},
		"eq false: true vs false": {
			clause: boolean(eqOp, true),
			input:  isPro(false),
			want:   false,
		},
		"eq false: false vs true": {
			clause: boolean(eqOp, false),
			input:  isPro(true),
			want:   false,
		},
		"neq true: true vs false": {
			clause: boolean(neqOp, true),
			input:  isPro(false),
			want:   true,
		},
		"neq true: false vs true": {
			clause: boolean(neqOp, false),
			input:  isPro(true),
			want:   true,
		},
		"neq false: true == true": {
			clause: boolean(neqOp, true),
			input:  isPro(true),
			want:   false,
		},
		"neq false: false == false": {
			clause: boolean(neqOp, false),
			input:  isPro(false),
			want:   false,
		},

		// Not reachable through NewClause; guards the default branch.
		"unsupported operator on hand-built clause": {
			clause: booleanClause{attribute: "isPro", operator: gtOp, values: []bool{true}},
			input:  isPro(true),
			errStr: `boolean clause with invalid operator: "gt"`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := tc.clause.Match(tc.input)
			if tc.errStr != "" {
				if err == nil || err.Error() != tc.errStr {
					t.Fatalf("Match() error = %v, want %q", err, tc.errStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Match() unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Match() = %v, want %v", got, tc.want)
			}
		})
	}
}
