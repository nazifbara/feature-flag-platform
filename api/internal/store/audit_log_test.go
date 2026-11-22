package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lib/pq"
	"github.com/nazifbara/feature-flag-platform/api/internal/testutil"
)

// auditLogCount counts the audit log entries straight from the table.
func auditLogCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM audit_logs").Scan(&n); err != nil {
		t.Fatalf("counting audit logs: %v", err)
	}
	return n
}

// auditLogValues holds a valid value for each column an action may require or
// forbid.
var auditLogValues = map[string]any{
	"environment_id":          "9a3f6c2e-1b84-4d7a-8e5f-0c6d2b9a7e14",
	"environment_name":        "production",
	"environment_old_name":    "staging",
	"flag_id":                 "0b7e7f5e-8a51-4c3e-9d6a-2f1c5b8e4a90",
	"flag_key":                "new-checkout",
	"flag_old_key":            "checkout",
	"rule_id":                 "5d2c9a1e-3f47-4b8d-a6e0-7c19f2b4d385",
	"setting_old_enabled":     false,
	"setting_new_enabled":     true,
	"setting_old_fallthrough": 0,
	"setting_new_fallthrough": 0.5,
	"rule_old_name":           "beta",
	"rule_new_name":           "beta testers",
	"rule_old_serve":          0,
	"rule_new_serve":          1,
	"rule_old_clauses":        validClauses,
	"rule_new_clauses":        otherClauses,
	"rule_old_position":       0,
	"rule_new_position":       1,
}

// insertAuditLog inserts an entry by a user for action, with the columns given
// set to their auditLogValues and the others null.
func insertAuditLog(store *Store, actorType, action string, columns []string) error {
	names := []string{"actor_type", "action"}
	args := []any{actorType, action}
	for _, c := range columns {
		names = append(names, c)
		args = append(args, auditLogValues[c])
	}
	params := make([]string, len(args))
	for i := range args {
		params[i] = fmt.Sprintf("$%d", i+1)
	}
	_, err := store.db.Exec(fmt.Sprintf(`
		INSERT INTO audit_logs (id, actor_id, request_id, created_at, %s)
		VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), now(), %s)`,
		strings.Join(names, ", "), strings.Join(params, ", ")), args...)
	return err
}

// wantCheckViolation fails the test unless err is constraint rejecting the
// entry, and the table is left empty.
func wantCheckViolation(t *testing.T, store *Store, err error, constraint string) {
	t.Helper()
	const checkViolation = "23514"
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || string(pqErr.Code) != checkViolation || pqErr.Constraint != constraint {
		t.Fatalf("inserting the entry: error = %v, want %s to reject it", err, constraint)
	}
	if n := auditLogCount(t, store); n != 0 {
		t.Fatalf("%d audit logs after a rejected insert, want 0", n)
	}
}

// Readers rely on each action's entry having what it describes, and on the
// before and after sides being set only by the actions that have them, so each
// action takes its entry with exactly its columns and rejects it missing any of
// them or carrying any other.
func TestAuditLogsTableActionFields(t *testing.T) {
	store := newTestStore(t)
	env := []string{"environment_id", "environment_name"}
	flag := []string{"flag_id", "flag_key"}
	// Settings and rules belong to a flag in an environment; a flag's own
	// actions belong to no environment.
	setting := append(slices.Clone(env), flag...)
	rule := append(slices.Clone(setting), "rule_id")
	tests := []struct {
		action     string
		required   []string
		constraint string
	}{
		{"environment-created", env, "environment_named_fields"},
		{"environment-renamed", append(slices.Clone(env), "environment_old_name"), "environment_renamed_fields"},
		{"environment-deleted", env, "environment_named_fields"},
		{"flag-created", flag, "flag_named_fields"},
		{"flag-updated", append(slices.Clone(flag), "flag_old_key"), "flag_updated_fields"},
		{"flag-deleted", flag, "flag_named_fields"},
		{"flag-archived", flag, "flag_named_fields"},
		{"flag-unarchived", flag, "flag_named_fields"},
		{"setting-created", append(slices.Clone(setting), "setting_new_enabled", "setting_new_fallthrough"), "setting_created_fields"},
		{"setting-updated", append(slices.Clone(setting),
			"setting_old_enabled", "setting_new_enabled", "setting_old_fallthrough", "setting_new_fallthrough",
		), "setting_updated_fields"},
		{"rule-created", append(slices.Clone(rule),
			"rule_new_name", "rule_new_serve", "rule_new_clauses", "rule_new_position",
		), "rule_created_fields"},
		{"rule-updated", append(slices.Clone(rule),
			"rule_old_name", "rule_new_name", "rule_old_serve", "rule_new_serve", "rule_old_clauses", "rule_new_clauses",
		), "rule_updated_fields"},
		{"rule-deleted", append(slices.Clone(rule),
			"rule_old_name", "rule_old_serve", "rule_old_clauses", "rule_old_position",
		), "rule_deleted_fields"},
		{"rule-position-changed", append(slices.Clone(rule), "rule_old_position", "rule_new_position"), "rule_position_changed_fields"},
	}
	for _, test := range tests {
		t.Run(test.action+" with its columns", func(t *testing.T) {
			testutil.ResetDB(t, store.db)
			if err := insertAuditLog(store, "user", test.action, test.required); err != nil {
				t.Fatalf("inserting the entry: %v", err)
			}
		})
		for _, missing := range test.required {
			t.Run(test.action+" without "+missing, func(t *testing.T) {
				testutil.ResetDB(t, store.db)
				columns := slices.DeleteFunc(slices.Clone(test.required), func(c string) bool { return c == missing })
				wantCheckViolation(t, store, insertAuditLog(store, "user", test.action, columns), test.constraint)
			})
		}
		for extra := range auditLogValues {
			if slices.Contains(test.required, extra) {
				continue
			}
			t.Run(test.action+" with "+extra, func(t *testing.T) {
				testutil.ResetDB(t, store.db)
				columns := append(slices.Clone(test.required), extra)
				wantCheckViolation(t, store, insertAuditLog(store, "user", test.action, columns), test.constraint)
			})
		}
	}
}

// Only users make changes so far, so an entry naming another kind of actor or
// an action nothing logs is a bug, and the table rejects it.
func TestAuditLogsTableRejectsUnknownActorsAndActions(t *testing.T) {
	store := newTestStore(t)

	t.Run("unknown actor type", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		wantCheckViolation(t, store, insertAuditLog(store, "system", "flag-created", []string{"flag_id", "flag_key"}), "audit_logs_actor_type_check")
	})
	t.Run("unknown action", func(t *testing.T) {
		testutil.ResetDB(t, store.db)
		wantCheckViolation(t, store, insertAuditLog(store, "user", "flag-renamed", []string{"flag_id", "flag_key"}), "audit_logs_action_check")
	})
}
