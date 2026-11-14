// Package flagsetting serves the admin API for flag settings: whether a flag is
// on in one environment, and what fraction of traffic it serves there.
package flagsetting

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/utils"
	flagsettingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the FlagSettingService RPCs on top of the store.
type Service struct {
	store *store.Store
}

// New returns a Service that keeps its data in store.
func New(store *store.Store) *Service {
	return &Service{store: store}
}

// settingToProto converts a stored flag setting into the API's FlagSetting.
func settingToProto(storedSetting database.FlagSetting) *flagsettingv1.FlagSetting {
	return &flagsettingv1.FlagSetting{
		Id:            storedSetting.ID.String(),
		FlagId:        storedSetting.FlagID.String(),
		EnvironmentId: storedSetting.EnvironmentID.String(),
		Enabled:       storedSetting.Enabled,
		Fallthrough:   storedSetting.Fallthrough,
		CreatedAt:     timestamppb.New(storedSetting.CreatedAt),
		UpdatedAt:     timestamppb.New(storedSetting.UpdatedAt),
	}
}

// storedOperators maps the API's clause operators to the names the
// rules.clauses column keeps.
var storedOperators = map[flagsettingv1.Operator]string{
	flagsettingv1.Operator_OPERATOR_EQ:  "eq",
	flagsettingv1.Operator_OPERATOR_NEQ: "neq",
	flagsettingv1.Operator_OPERATOR_GT:  "gt",
	flagsettingv1.Operator_OPERATOR_LT:  "lt",
	flagsettingv1.Operator_OPERATOR_GTE: "gte",
	flagsettingv1.Operator_OPERATOR_LTE: "lte",
	flagsettingv1.Operator_OPERATOR_STW: "stw",
	flagsettingv1.Operator_OPERATOR_EDW: "edw",
	flagsettingv1.Operator_OPERATOR_CNT: "cnt",
	flagsettingv1.Operator_OPERATOR_IL:  "il",
}

// toAny copies values into a []any, as store.StoredClause holds them. nil
// stays nil.
func toAny[T any](values []T) []any {
	if values == nil {
		return nil
	}
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// clausesToJSON turns a request's clauses into the list of store.StoredClause
// the rules.clauses column keeps, as JSON, with each kind taken from whichever
// values are set. The proto's rules reject an unspecified operator or no values;
// checking again here guards against a caller without them. NaN and infinite
// numbers have no JSON form, so they're rejected too. Everything else, such as
// the operators a kind allows, is left to the store. Errors are plain, for the
// caller to report as InvalidArgument.
func clausesToJSON(clauses []*flagsettingv1.Clause) (json.RawMessage, error) {
	// Not nil, so no clauses gives [] for the store to reject, not null.
	stored := make([]store.StoredClause, 0, len(clauses))
	for i, c := range clauses {
		op, ok := storedOperators[c.GetOperator()]
		if !ok {
			return nil, fmt.Errorf("clause at index %d: unknown operator %v", i, c.GetOperator())
		}
		clause := store.StoredClause{Attribute: c.GetAttribute(), Operator: op}
		switch v := c.GetValues().(type) {
		case *flagsettingv1.Clause_StringValues:
			clause.Kind, clause.Values = "string", toAny(v.StringValues.GetValues())
		case *flagsettingv1.Clause_NumberValues:
			for _, n := range v.NumberValues.GetValues() {
				if math.IsNaN(n) || math.IsInf(n, 0) {
					return nil, fmt.Errorf("clause at index %d: %v isn't a finite number", i, n)
				}
			}
			clause.Kind, clause.Values = "number", toAny(v.NumberValues.GetValues())
		case *flagsettingv1.Clause_BoolValues:
			clause.Kind, clause.Values = "boolean", toAny(v.BoolValues.GetValues())
		default:
			return nil, fmt.Errorf("clause at index %d: no values", i)
		}
		stored = append(stored, clause)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("invalid clauses: %w", err)
	}
	return raw, nil
}

// protoOperators maps the names the rules.clauses column keeps back to the
// API's clause operators.
var protoOperators = func() map[string]flagsettingv1.Operator {
	ops := make(map[string]flagsettingv1.Operator, len(storedOperators))
	for op, name := range storedOperators {
		ops[name] = op
	}
	return ops
}()

// fromAny copies values into a []T, failing on the first value that isn't a T.
func fromAny[T any](values []any) ([]T, error) {
	out := make([]T, len(values))
	for i, v := range values {
		t, ok := v.(T)
		if !ok {
			return nil, fmt.Errorf("value at index %d is %T, want %T", i, v, t)
		}
		out[i] = t
	}
	return out, nil
}

// clausesFromJSON turns the JSON list of store.StoredClause the rules.clauses
// column keeps back into the API's clauses, in the same order, with each kind
// giving which values are set. The store only saves clauses it can parse, so
// an error here means the stored data is corrupt: errors are plain, for the
// caller to report as Internal without their text.
func clausesFromJSON(raw json.RawMessage) ([]*flagsettingv1.Clause, error) {
	var stored []store.StoredClause
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, fmt.Errorf("invalid clauses: %w", err)
	}
	clauses := make([]*flagsettingv1.Clause, 0, len(stored))
	for i, s := range stored {
		op, ok := protoOperators[s.Operator]
		if !ok {
			return nil, fmt.Errorf("clause at index %d: unknown operator %q", i, s.Operator)
		}
		if len(s.Values) == 0 {
			return nil, fmt.Errorf("clause at index %d: no values", i)
		}
		clause := &flagsettingv1.Clause{Attribute: s.Attribute, Operator: op}
		switch s.Kind {
		case "string":
			values, err := fromAny[string](s.Values)
			if err != nil {
				return nil, fmt.Errorf("clause at index %d: %w", i, err)
			}
			clause.Values = &flagsettingv1.Clause_StringValues{StringValues: &flagsettingv1.StringValues{Values: values}}
		case "number":
			// JSON numbers decode as float64.
			values, err := fromAny[float64](s.Values)
			if err != nil {
				return nil, fmt.Errorf("clause at index %d: %w", i, err)
			}
			clause.Values = &flagsettingv1.Clause_NumberValues{NumberValues: &flagsettingv1.NumberValues{Values: values}}
		case "boolean":
			values, err := fromAny[bool](s.Values)
			if err != nil {
				return nil, fmt.Errorf("clause at index %d: %w", i, err)
			}
			clause.Values = &flagsettingv1.Clause_BoolValues{BoolValues: &flagsettingv1.BoolValues{Values: values}}
		default:
			return nil, fmt.Errorf("clause at index %d: unknown kind %q", i, s.Kind)
		}
		clauses = append(clauses, clause)
	}
	return clauses, nil
}

// ruleToProto converts a stored rule into the API's Rule, with its clauses read
// by clausesFromJSON. Errors come from there, so they mean corrupt stored data
// and are for the caller to report as Internal without their text.
func ruleToProto(storedRule database.Rule) (*flagsettingv1.Rule, error) {
	clauses, err := clausesFromJSON(storedRule.Clauses)
	if err != nil {
		return nil, fmt.Errorf("rule %s: %w", storedRule.ID, err)
	}
	return &flagsettingv1.Rule{
		Id:            storedRule.ID.String(),
		FlagSettingId: storedRule.FlagSettingID.String(),
		Position:      storedRule.Position,
		Name:          storedRule.Name,
		Serve:         float64(storedRule.Serve),
		Clauses:       clauses,
		CreatedAt:     timestamppb.New(storedRule.CreatedAt),
		UpdatedAt:     timestamppb.New(storedRule.UpdatedAt),
	}, nil
}

// Get returns the flag setting with the requested id. The proto's rules reject
// an id that isn't a UUID; parsing it again here only guards against a caller
// without them. An id no setting has gives NotFound.
func (s *Service) Get(ctx context.Context, req *flagsettingv1.GetRequest) (*flagsettingv1.GetResponse, error) {
	settingID, err := utils.ParseID(req.Id, "flag setting")
	if err != nil {
		return nil, err
	}
	setting, err := s.store.GetFlagSettingByID(ctx, settingID)
	if err != nil {
		return nil, err
	}
	return &flagsettingv1.GetResponse{FlagSetting: settingToProto(setting)}, nil
}

// Update gives the flag setting with the requested id the values its update
// mask names, checked by the store, keeping the others whatever the request
// holds for them, and returns the setting as stored. An empty mask changes
// nothing. The proto's rules reject an id that isn't a UUID; parsing it again
// here only guards against a caller without them. Store errors reach the client
// as they are.
func (s *Service) Update(ctx context.Context, req *flagsettingv1.UpdateRequest) (*flagsettingv1.UpdateResponse, error) {
	settingID, err := utils.ParseID(req.Id, "flag setting")
	if err != nil {
		return nil, err
	}
	update := store.FlagSettingUpdate{ID: settingID}
	// The proto's rules reject other paths, but also take sub-paths such as
	// enabled.x, so each path is matched exactly here.
	for _, path := range req.GetUpdateMask().GetPaths() {
		switch path {
		case "enabled":
			update.Enabled = &req.Enabled
		case "fallthrough":
			update.Fallthrough = &req.Fallthrough
		default:
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Sprintf("invalid update mask path %q", path))
		}
	}
	setting, err := s.store.UpdateFlagSetting(ctx, update)
	if err != nil {
		return nil, err
	}
	return &flagsettingv1.UpdateResponse{FlagSetting: settingToProto(setting)}, nil
}

// CreateRule adds the requested rule to the flag setting with the requested id,
// checked by the store, and returns the rule as stored. The proto's rules reject
// an id that isn't a UUID and clauses with no operator or values; checking again
// here only guards against a caller without them. Store errors reach the client
// as they are, and a stored rule that can't be read back gives Internal.
func (s *Service) CreateRule(ctx context.Context, req *flagsettingv1.CreateRuleRequest) (*flagsettingv1.CreateRuleResponse, error) {
	settingID, err := utils.ParseID(req.FlagSettingId, "flag setting")
	if err != nil {
		return nil, err
	}
	clausesJSON, err := clausesToJSON(req.Clauses)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "invalid request clauses").WithCause(err)
	}
	rule, err := s.store.CreateRule(ctx, database.CreateRuleParams{
		FlagSettingID: settingID,
		Name:          req.Name,
		Position:      req.Position,
		Serve:         req.Serve,
		Clauses:       clausesJSON,
	})
	if err != nil {
		return nil, err
	}
	protoRule, err := ruleToProto(rule)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, connect.CodeInternal.String())
	}
	return &flagsettingv1.CreateRuleResponse{Rule: protoRule}, nil
}
