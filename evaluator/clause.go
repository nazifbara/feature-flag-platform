package evaluator

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type ClauseKind int

const (
	StringClause ClauseKind = iota + 1
	BooleanClause
	NumberClause
)

// The operators a clause can have, as rules store them. Each kind allows only
// some of them, and EqOp and NeqOp take a single value.
const (
	EqOp  = "eq"
	NeqOp = "neq"
	GtOp  = "gt"
	LtOp  = "lt"
	GteOp = "gte"
	LteOp = "lte"
	StwOp = "stw"
	EdwOp = "edw"
	CntOp = "cnt"
	IlOp  = "il"
)

type Clause interface {
	Match(attributes map[string][]any) (bool, error)
}

type stringClause struct {
	attribute string
	operator  string
	values    []string
}

type numberClause struct {
	attribute string
	operator  string
	values    []float64
}

type booleanClause struct {
	attribute string
	operator  string
	values    []bool
}

var operators = []string{EqOp, NeqOp, GtOp, LtOp, GteOp, LteOp, StwOp, EdwOp, CntOp, IlOp}

var clauseOperatorMap = map[ClauseKind][]string{
	StringClause:  {EqOp, NeqOp, StwOp, EdwOp, CntOp, IlOp},
	BooleanClause: {EqOp, NeqOp},
	NumberClause:  {EqOp, NeqOp, GtOp, LtOp, GteOp, LteOp, CntOp, IlOp},
}

// NewClause returns a clause of the given kind, after checking its attribute,
// its operator and that its values have the kind's type. Number values must be
// float64.
func NewClause(attribute, operator string, kind ClauseKind, values []any) (Clause, error) {
	if attribute == "" {
		return nil, errors.New("can't create a clause with empty attribute")
	}
	if len(values) == 0 {
		return nil, errors.New("can't create a clause with empty values")
	}
	if !slices.Contains(operators, operator) {
		return nil, fmt.Errorf("unknown operator: %s", operator)
	}
	if slices.Contains([]string{EqOp, NeqOp}, operator) && len(values) > 1 {
		return nil, fmt.Errorf("%v operator on multi-value clause", operator)
	}

	switch kind {
	case StringClause:
		strs, err := convert[string](values)
		if err != nil {
			return nil, fmt.Errorf("invalid value for StringClause: %w", err)
		}
		if !slices.Contains(clauseOperatorMap[StringClause], operator) {
			return nil, fmt.Errorf("unsupported operator for StringClause: %s", operator)
		}
		return stringClause{attribute: attribute, operator: operator, values: strs}, nil
	case BooleanClause:
		bools, err := convert[bool](values)
		if err != nil {
			return nil, fmt.Errorf("invalid value for BooleanClause: %w", err)
		}
		if !slices.Contains(clauseOperatorMap[BooleanClause], operator) {
			return nil, fmt.Errorf("unsupported operator for BooleanClause: %s", operator)
		}
		return booleanClause{attribute: attribute, operator: operator, values: bools}, nil
	case NumberClause:
		numbers, err := convert[float64](values)
		if err != nil {
			return nil, fmt.Errorf("invalid value for NumberClause: %w", err)
		}
		if !slices.Contains(clauseOperatorMap[NumberClause], operator) {
			return nil, fmt.Errorf("unsupported operator for NumberClause: %s", operator)
		}
		return numberClause{attribute: attribute, operator: operator, values: numbers}, nil
	default:
		return nil, errors.New("unknown clause kind")
	}
}

// convert returns values as a []T, failing on the first value of another type.
func convert[T any](values []any) ([]T, error) {
	out := make([]T, len(values))
	for i, v := range values {
		t, ok := v.(T)
		if !ok {
			return nil, fmt.Errorf("invalid value %v", v)
		}
		out[i] = t
	}
	return out, nil
}

// Match reports whether the attribute satisfies the clause. A missing or empty
// attribute doesn't match, and only il and cnt accept several values.
func (c stringClause) Match(attributes map[string][]any) (bool, error) {
	values, ok := attributes[c.attribute]
	if !ok || len(values) == 0 {
		return false, nil
	}
	inputValues, err := convert[string](values)
	if err != nil {
		return false, fmt.Errorf("invalid value for attribute %q: %w", c.attribute, err)
	}
	if !slices.Contains([]string{IlOp, CntOp}, c.operator) && len(inputValues) > 1 {
		return false, fmt.Errorf("multi-value attribute not supported for %s operation in string clause", c.operator)
	}

	switch c.operator {
	case IlOp:
		return valueContains(c.values, inputValues), nil
	case CntOp:
		return valueContains(inputValues, c.values), nil
	case "eq":
		return inputValues[0] == c.values[0], nil
	case "neq":
		return inputValues[0] != c.values[0], nil
	case "stw":
		return strings.HasPrefix(inputValues[0], c.values[0]), nil
	case "edw":
		return strings.HasSuffix(inputValues[0], c.values[0]), nil
	default:
		return false, fmt.Errorf("string clause with invalid operator: %q", c.operator)
	}
}

// Match reports whether the attribute satisfies the clause. A missing or empty
// attribute doesn't match, and only il and cnt accept several values.
func (c numberClause) Match(attributes map[string][]any) (bool, error) {
	values, ok := attributes[c.attribute]
	if !ok || len(values) == 0 {
		return false, nil
	}
	inputValues, err := convert[float64](values)
	if err != nil {
		return false, fmt.Errorf("invalid value for attribute %q: %w", c.attribute, err)
	}
	if !slices.Contains([]string{IlOp, CntOp}, c.operator) && len(inputValues) > 1 {
		return false, fmt.Errorf("multi-value attribute not supported for %s operation in number clause", c.operator)
	}
	switch c.operator {
	case IlOp:
		return valueContains(c.values, inputValues), nil
	case CntOp:
		return valueContains(inputValues, c.values), nil
	case EqOp:
		return inputValues[0] == c.values[0], nil
	case NeqOp:
		return inputValues[0] != c.values[0], nil
	case GtOp:
		return inputValues[0] > c.values[0], nil
	case GteOp:
		return inputValues[0] >= c.values[0], nil
	case LtOp:
		return inputValues[0] < c.values[0], nil
	case LteOp:
		return inputValues[0] <= c.values[0], nil
	default:
		return false, fmt.Errorf("number clause with invalid operator: %q", c.operator)
	}
}

// Match reports whether the attribute satisfies the clause. A missing or empty
// attribute doesn't match, and several values are an error.
func (c booleanClause) Match(attributes map[string][]any) (bool, error) {
	values, ok := attributes[c.attribute]
	if !ok || len(values) == 0 {
		return false, nil
	}
	inputValues, err := convert[bool](values)
	if err != nil {
		return false, fmt.Errorf("invalid value for attribute %q: %w", c.attribute, err)
	}
	if len(inputValues) > 1 {
		return false, errors.New("multi-value attributes aren't supported in boolean clause")
	}

	input := inputValues[0]
	clauseValue := c.values[0]

	switch c.operator {
	case EqOp:
		return input == clauseValue, nil
	case NeqOp:
		return input != clauseValue, nil
	default:
		return false, fmt.Errorf("boolean clause with invalid operator: %q", c.operator)
	}
}

// valueContains reports whether a holds every value in b.
func valueContains[T comparable](a []T, b []T) bool {
	for _, v := range b {
		if !slices.Contains(a, v) {
			return false
		}
	}
	return true
}
