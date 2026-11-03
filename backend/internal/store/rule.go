package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/backend/internal/database"
	"github.com/nazifbara/flagsvc/backend/internal/evaluator"
	"github.com/nazifbara/flagsvc/backend/internal/ffserrors"
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

// checkServe checks f is a fraction of traffic, in [0, 1]. action ("create",
// "update") goes into the error message.
func checkServe(action string, f float32) error {
	// NaN fails every comparison, so it is rejected too.
	if !(f >= 0 && f <= 1) {
		return connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a rule with a serve value outside [0, 1]")
	}
	return nil
}

// checkPosition checks p isn't negative. action ("create", "update") goes into
// the error message.
func checkPosition(action string, p int32) error {
	if p < 0 {
		return connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a rule with a negative position")
	}
	return nil
}

// normalizeRuleName trims name and checks it fits the column. A name is
// optional, so it can end up empty. action ("create", "update") goes into the
// error message.
func normalizeRuleName(action, name string) (string, error) {
	name = strings.TrimSpace(name)

	// The column is VARCHAR(200), which counts characters, not bytes.
	if utf8.RuneCountInString(name) > 200 {
		return "", connect.NewError(connect.CodeInvalidArgument, "can't "+action+" a rule with a name over 200 characters")
	}
	return name, nil
}

// CreateRule adds a rule to a flag's settings in one environment. Clauses are
// checked by parseClauses, so a saved rule can always be loaded back.
func (s *Store) CreateRule(ctx context.Context, args database.CreateRuleParams) (database.Rule, error) {
	name, err := normalizeRuleName("create", args.Name)
	if err != nil {
		return database.Rule{}, err
	}
	args.Name = name
	if err := checkPosition("create", args.Position); err != nil {
		return database.Rule{}, err
	}
	if err := checkServe("create", args.Serve); err != nil {
		return database.Rule{}, err
	}
	if _, err := parseClauses(args.Clauses); err != nil {
		return database.Rule{}, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	rule, err := s.Queries.CreateRule(ctx, args)
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't create the rule")
	}
	return rule, nil
}

// GetRule gets the rule with the given id.
func (s *Store) GetRule(ctx context.Context, id uuid.UUID) (database.Rule, error) {
	rule, err := s.Queries.GetRule(ctx, id)
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't get the rule")
	}
	return rule, nil
}

// ListRulesBySetting gets a flag setting's rules, ordered by position, which is
// the order they're evaluated in. An unknown setting has no rules.
func (s *Store) ListRulesBySetting(ctx context.Context, settingID uuid.UUID) ([]database.Rule, error) {
	rules, err := s.Queries.ListRulesBySetting(ctx, settingID)
	if err != nil {
		return nil, ffserrors.FromDBErr(err, "can't list the flag setting's rules")
	}
	return rules, nil
}

// DeleteRule deletes the rule with the given id and returns it as it was. The
// other rules keep their positions, so this leaves a gap.
func (s *Store) DeleteRule(ctx context.Context, id uuid.UUID) (database.Rule, error) {
	rule, err := s.Queries.DeleteRule(ctx, id)
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't delete the rule")
	}
	return rule, nil
}

// UpdateRule replaces the name, serve and clauses of the rule with the given id
// and returns it as updated. The rule's position doesn't change.
func (s *Store) UpdateRule(ctx context.Context, args database.UpdateRuleParams) (database.Rule, error) {
	name, err := normalizeRuleName("update", args.Name)
	if err != nil {
		return database.Rule{}, err
	}
	args.Name = name
	if err := checkServe("update", args.Serve); err != nil {
		return database.Rule{}, err
	}
	if _, err := parseClauses(args.Clauses); err != nil {
		return database.Rule{}, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	rule, err := s.Queries.UpdateRule(ctx, args)
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't update the rule")
	}
	return rule, nil
}

// CountRulesBySetting gives the number of rules a flag setting has. An unknown
// setting has none.
func (s *Store) CountRulesBySetting(ctx context.Context, flagSettingID uuid.UUID) (int, error) {
	count, err := s.Queries.CountRulesBySetting(ctx, flagSettingID)
	if err != nil {
		return 0, ffserrors.FromDBErr(err, "can't count the flag setting's rules")
	}
	return int(count), nil
}
