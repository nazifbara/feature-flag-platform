package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/ffserrors"
	"github.com/nazifbara/feature-flag-platform/evaluator"
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

// CreateRule adds a rule to a flag's settings in one environment, at a position
// from 0 up to the setting's rule count. Rules from that position on get a
// position one higher, which makes room. Clauses are checked by parseClauses, so
// a saved rule can always be loaded back. It bumps that environment's version and
// logs the new rule there.
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
	var rule database.Rule
	err = s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		setting, err := q.GetFlagSettingByID(ctx, args.FlagSettingID)
		if err != nil {
			return err
		}
		flag, err := q.GetFlag(ctx, setting.FlagID)
		if err != nil {
			return err
		}
		if flag.Archived {
			return connect.NewError(connect.CodeFailedPrecondition, "can't create rules for archived flag")
		}
		ruleCount, err := q.CountRulesBySetting(ctx, setting.ID)
		if err != nil {
			return err
		}
		if int(args.Position) > int(ruleCount) {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Sprintf("can't create a rule with a position outside [0, %d]", ruleCount))
		}
		err = q.ShiftRulePositionsBetween(ctx, database.ShiftRulePositionsBetweenParams{
			FlagSettingID: setting.ID,
			Delta:         1,
			After:         args.Position - 1,
			Before:        ruleCount,
		})
		if err != nil {
			return err
		}
		rule, err = q.CreateRule(ctx, args)
		if err != nil {
			return err
		}
		if err := q.IncrementEnvVersion(ctx, setting.EnvironmentID); err != nil {
			return err
		}
		return q.CreateChangeLogsForRuleCreate(ctx, rule.ID)
	})
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

// DeleteRule deletes the rule with the given id and returns it as it was. Rules
// after it get a position one lower, which fills the gap. It bumps the version of
// the rule's environment and logs the deletion there.
func (s *Store) DeleteRule(ctx context.Context, id uuid.UUID) (database.Rule, error) {
	var deletedRule database.Rule
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		rule, err := q.GetRule(ctx, id)
		if err != nil {
			return err
		}

		ruleCount, err := q.CountRulesBySetting(ctx, rule.FlagSettingID)
		if err != nil {
			return err
		}
		err = q.ShiftRulePositionsBetween(ctx, database.ShiftRulePositionsBetweenParams{
			FlagSettingID: rule.FlagSettingID,
			Delta:         -1,
			After:         rule.Position,
			Before:        ruleCount,
		})
		if err != nil {
			return err
		}
		deletedRule, err = q.DeleteRule(ctx, id)
		if err != nil {
			return err
		}
		setting, err := q.GetFlagSettingByID(ctx, rule.FlagSettingID)
		if err != nil {
			return err
		}
		flag, err := q.GetFlag(ctx, setting.FlagID)
		if err != nil {
			return err
		}
		if flag.Archived {
			return connect.NewError(connect.CodeFailedPrecondition, "can't delete rules of archived flag")
		}
		if err := q.IncrementEnvVersion(ctx, setting.EnvironmentID); err != nil {
			return err
		}
		return q.CreateChangeLogsForRuleDelete(ctx, database.CreateChangeLogsForRuleDeleteParams{
			FlagSettingID: deletedRule.FlagSettingID,
			Position:      deletedRule.Position,
		})
	})
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't delete the rule")
	}
	return deletedRule, nil
}

// UpdateRule replaces the name, serve and clauses of the rule with the given id
// and returns it as updated. The rule's position doesn't change. When the serve
// or clauses change, it bumps the version of the rule's environment and logs the
// update there. A new name alone is saved without either, since SDKs never see
// it, and values the rule already has change nothing. Clauses are compared as
// parsed, so JSON spacing and key order don't count, but the order of clauses
// and of their values does.
func (s *Store) UpdateRule(ctx context.Context, args database.UpdateRuleParams) (database.Rule, error) {
	name, err := normalizeRuleName("update", args.Name)
	if err != nil {
		return database.Rule{}, err
	}
	args.Name = name
	if err := checkServe("update", args.Serve); err != nil {
		return database.Rule{}, err
	}
	parsedClauses, err := parseClauses(args.Clauses)
	if err != nil {
		return database.Rule{}, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
	}
	var rule database.Rule
	err = s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		currentRule, err := q.GetRule(ctx, args.ID)
		if err != nil {
			return err
		}
		parsedCurrentClauses, _ := parseClauses(currentRule.Clauses)
		if currentRule.Name == args.Name &&
			reflect.DeepEqual(parsedCurrentClauses, parsedClauses) &&
			currentRule.Serve == args.Serve {
			rule = currentRule
			return nil
		}
		r, err := q.UpdateRule(ctx, args)
		if err != nil {
			return err
		}
		rule = r
		if currentRule.Name != args.Name &&
			reflect.DeepEqual(parsedCurrentClauses, parsedClauses) &&
			currentRule.Serve == args.Serve {
			return nil
		}
		setting, err := q.GetFlagSettingByID(ctx, rule.FlagSettingID)
		if err != nil {
			return err
		}
		flag, err := q.GetFlag(ctx, setting.FlagID)
		if err != nil {
			return err
		}
		if flag.Archived {
			return connect.NewError(connect.CodeFailedPrecondition, "can't update rules of archived flag")
		}
		if err := q.IncrementEnvVersion(ctx, setting.EnvironmentID); err != nil {
			return err
		}
		return q.CreateChangeLogsForRuleUpdate(ctx, r.ID)
	})
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

// ChangeRulePosition moves the rule with the given id to a position from 0 up to
// the setting's rule count minus one and returns it as updated. Rules between its
// old and new positions get a position one higher or lower, which keeps the
// positions contiguous. It bumps the version of the rule's environment and logs
// the move there, unless the rule is already at that position, which changes
// nothing.
func (s *Store) ChangeRulePosition(ctx context.Context, args database.ChangeRulePositionParams) (database.Rule, error) {
	if err := checkPosition("update", args.Position); err != nil {
		return database.Rule{}, err
	}
	var updatedRule database.Rule
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		rule, err := q.GetRule(ctx, args.ID)
		if err != nil {
			return err
		}
		ruleCount, err := q.CountRulesBySetting(ctx, rule.FlagSettingID)
		if err != nil {
			return err
		}
		if int64(args.Position) >= ruleCount {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("can't update a rule with a position outside [0, %d]", ruleCount-1))
		}
		if rule.Position == args.Position {
			updatedRule = rule
			return nil
		}
		shift := database.ShiftRulePositionsBetweenParams{
			FlagSettingID: rule.FlagSettingID,
			Delta:         1,
			After:         args.Position - 1,
			Before:        int64(rule.Position),
		}
		if args.Position > rule.Position {
			shift.Delta = -1
			shift.After = rule.Position
			shift.Before = int64(args.Position) + 1
		}
		if err := q.ShiftRulePositionsBetween(ctx, shift); err != nil {
			return err
		}
		updatedRule, err = q.ChangeRulePosition(ctx, args)
		if err != nil {
			return err
		}
		setting, err := q.GetFlagSettingByID(ctx, rule.FlagSettingID)
		if err != nil {
			return err
		}
		flag, err := q.GetFlag(ctx, setting.FlagID)
		if err != nil {
			return err
		}
		if flag.Archived {
			return connect.NewError(connect.CodeFailedPrecondition, "can't update rules of archived flag")
		}
		if err := q.IncrementEnvVersion(ctx, setting.EnvironmentID); err != nil {
			return err
		}
		return q.CreateChangeLogsForRulePositionChange(ctx, database.CreateChangeLogsForRulePositionChangeParams{
			OldPosition: rule.Position,
			RuleID:      rule.ID,
		})
	})
	if err != nil {
		return database.Rule{}, ffserrors.FromDBErr(err, "can't update the rule")
	}
	return updatedRule, nil
}
