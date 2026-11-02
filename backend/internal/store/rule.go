package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nazifbara/flagsvc/backend/internal/evaluator"
)

// storedClause is how a clause is kept in the rules.clauses column.
type storedClause struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Kind      string `json:"kind"`
	Values    []any  `json:"values"`
}

// clauseKinds maps a stored clause's kind to the evaluator's.
var clauseKinds = map[string]evaluator.ClauseKind{
	"string":  evaluator.StringClause,
	"boolean": evaluator.BooleanClause,
	"number":  evaluator.NumberClause,
}

// parseClauses turns a rule's stored clauses into the evaluator's, leaving
// what makes a clause valid to evaluator.NewClause. It returns plain errors:
// bad input when saving a rule, but corrupt data when loading one, so the
// caller picks the code.
func parseClauses(raw json.RawMessage) ([]evaluator.Clause, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var stored []storedClause
	if err := dec.Decode(&stored); err != nil {
		return nil, fmt.Errorf("invalid clauses: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid clauses: unexpected data after the list")
	}
	// Covers JSON null too, which decodes to a nil slice.
	if len(stored) == 0 {
		return nil, errors.New("invalid clauses: at least one clause is required")
	}

	clauses := make([]evaluator.Clause, 0, len(stored))
	for i, c := range stored {
		kind, ok := clauseKinds[c.Kind]
		if !ok {
			return nil, fmt.Errorf("clause at index %d: unknown kind %q", i, c.Kind)
		}
		clause, err := evaluator.NewClause(c.Attribute, c.Operator, kind, c.Values)
		if err != nil {
			return nil, fmt.Errorf("clause at index %d: %w", i, err)
		}
		clauses = append(clauses, clause)
	}
	return clauses, nil
}
