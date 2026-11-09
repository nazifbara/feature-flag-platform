package evaluator

import (
	"errors"
	"fmt"
	"slices"
	"uuid"
)

// Rule represents a group of ordered clauses and what
// it serve what those clause checkout
type Rule struct {
	id uuid.UUID

	// clauses is the list of checks to decide if to serve or not
	clauses []Clause

	// serve is the feature rollout with 0 meaning off and
	// 1.0 a full rollout. Value in between denote partial rollout
	serve float32
}

// NewRule build a proper Rule out of existing clauses
func NewRule(id uuid.UUID, clauses []Clause, serve float32) (Rule, error) {
	if id == uuid.Nil() {
		return Rule{}, fmt.Errorf("invalid rule id: %v", id)
	}
	clausesCopy := slices.Clone(clauses)
	if len(clausesCopy) == 0 {
		return Rule{}, errors.New("can't build a rule with no clause")
	}
	for i, clause := range clausesCopy {
		if clause == nil {
			return Rule{}, fmt.Errorf("nil clause at index %d", i)
		}
	}
	if !(0 <= serve && serve <= 1) {
		return Rule{}, fmt.Errorf("invalid serve value: %f", serve)
	}

	return Rule{
		id:      id,
		clauses: clausesCopy,
		serve:   serve,
	}, nil
}

// Match checks if the given attributes meet the rule.
// The rule is met when every clause matches with the attributes
func (r Rule) Match(attributes map[string][]any) (serve float32, match bool, err error) {
	for i, clause := range r.clauses {
		match, err := clause.Match(attributes)
		if err != nil {
			return 0, false, fmt.Errorf("clause at index %d errors: %w", i, err)
		}
		if !match {
			return 0, false, nil
		}
	}
	return r.serve, true, nil
}
