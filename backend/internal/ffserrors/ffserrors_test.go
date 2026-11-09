package ffserrors

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	connect "connectrpc.com/connect/v2"
	"github.com/lib/pq"
)

func TestFromDBErr(t *testing.T) {
	tests := map[string]struct {
		err  error
		want connect.Code
	}{
		"canceled context":         {context.Canceled, connect.CodeCanceled},
		"wrapped canceled context": {fmt.Errorf("query: %w", context.Canceled), connect.CodeCanceled},
		"deadline exceeded":        {context.DeadlineExceeded, connect.CodeDeadlineExceeded},
		"wrapped deadline":         {fmt.Errorf("query: %w", context.DeadlineExceeded), connect.CodeDeadlineExceeded},
		"no rows":                  {sql.ErrNoRows, connect.CodeNotFound},
		"wrapped no rows":          {fmt.Errorf("get: %w", sql.ErrNoRows), connect.CodeNotFound},
		"taken flag key":           {&pq.Error{Code: "23505", Constraint: "flags_key_unique"}, connect.CodeAlreadyExists},
		"wrapped taken flag key":   {fmt.Errorf("insert: %w", &pq.Error{Code: "23505", Constraint: "flags_key_unique"}), connect.CodeAlreadyExists},
		"taken environment name":   {&pq.Error{Code: "23505", Constraint: "environments_name_unique"}, connect.CodeAlreadyExists},
		"taken email":              {&pq.Error{Code: "23505", Constraint: "identities_email_unique"}, connect.CodeAlreadyExists},
		// Only a bug breaks these, so the client isn't told it asked for
		// something taken.
		"version logged twice":     {&pq.Error{Code: "23505", Constraint: "change_logs_env_version_unique"}, connect.CodeInternal},
		"rule position collision":  {&pq.Error{Code: "23505", Constraint: "rules_setting_position_unique"}, connect.CodeInternal},
		"unnamed unique violation": {&pq.Error{Code: "23505"}, connect.CodeInternal},
		// Writes look parent rows up first, so only a bug reaches a foreign key.
		"rule without its setting": {&pq.Error{Code: "23503", Constraint: "rules_setting_fk"}, connect.CodeInternal},
		"unnamed foreign key":      {&pq.Error{Code: "23503"}, connect.CodeInternal},
		"query canceled":           {&pq.Error{Code: "57014"}, connect.CodeCanceled},
		"invalid text":             {&pq.Error{Code: "22021"}, connect.CodeInvalidArgument},
		"NUL in JSON":              {&pq.Error{Code: "22P05"}, connect.CodeInvalidArgument},
		// Anything unrecognized is internal, never nil.
		"other Postgres error": {&pq.Error{Code: "22001"}, connect.CodeInternal},
		"closed database":      {sql.ErrConnDone, connect.CodeInternal},
		"plain error":          {errors.New("boom"), connect.CodeInternal},
	}
	const message = "can't save the thing"
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := FromDBErr(test.err, message)
			if got == nil {
				t.Fatalf("FromDBErr(%v) = nil, want code %v", test.err, test.want)
			}
			connectErr, ok := got.(*connect.Error)
			if !ok {
				t.Fatalf("FromDBErr(%v) = %T, want *connect.Error", test.err, got)
			}
			if connectErr.Code() != test.want {
				t.Errorf("FromDBErr(%v) code = %v, want %v", test.err, connectErr.Code(), test.want)
			}
			// The caller's message replaces the database's.
			if want := connect.NewError(test.want, message).Error(); connectErr.Error() != want {
				t.Errorf("FromDBErr(%v) = %q, want %q", test.err, connectErr.Error(), want)
			}
			// The original error is kept as the cause for logging.
			if !errors.Is(got, test.err) {
				t.Errorf("FromDBErr(%v) = %v, want it to wrap the original error", test.err, got)
			}
		})
	}
}

// userFacingUniques names constraints, and a name that no longer exists would
// quietly turn a client's mistake into an internal error, so each must be in
// the schema as a unique constraint.
func TestUserFacingConstraintsExist(t *testing.T) {
	dbURL := os.Getenv("TEST_DB_URL")
	if dbURL == "" {
		t.Fatal("TEST_DB_URL not set")
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("opening the database: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// The list can only be kept right when every constraint it could need is
	// named in the schema, not left for Postgres to name.
	t.Run("no generated names", func(t *testing.T) {
		rows, err := db.Query(`
			SELECT conname FROM pg_constraint
			WHERE connamespace = 'public'::regnamespace AND contype IN ('u', 'f')
			  AND (conname LIKE '%\_key' OR conname LIKE '%\_fkey')`)
		if err != nil {
			t.Fatalf("listing constraints: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatalf("reading a constraint: %v", err)
			}
			t.Errorf("%s has the name Postgres made up, want one the schema gives", name)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("listing constraints: %v", err)
		}
	})

	for name := range userFacingUniques {
		t.Run(name, func(t *testing.T) {
			var kind string
			err := db.QueryRow("SELECT contype FROM pg_constraint WHERE conname = $1", name).Scan(&kind)
			if errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("no constraint named %s in the schema", name)
			}
			if err != nil {
				t.Fatalf("looking up %s: %v", name, err)
			}
			if kind != "u" {
				t.Fatalf("%s is a constraint of type %q, want a unique one (\"u\")", name, kind)
			}
		})
	}
}

// A connect.Error already says what the client should see, so FromDBErr keeps
// its code and message instead of replacing them. Whatever wrapped it is kept as
// a cause, so it can still be logged.
func TestFromDBErrKeepsConnectErrors(t *testing.T) {
	invalid := connect.NewError(connect.CodeInvalidArgument, "can't create a rule with a position outside [0, 2]")
	rollbackErr := errors.New("rollback failed")
	tests := map[string]struct {
		err  error
		want *connect.Error
		// causes are what errors.Is must still find in the result.
		causes []error
	}{
		"wrapped": {
			err:    fmt.Errorf("query: %w", invalid),
			want:   invalid,
			causes: []error{invalid},
		},
		// ExecTx wraps the error this way when the rollback fails too.
		"next to a rollback error": {
			err:    fmt.Errorf("tx error: %w, rollback error: %w", invalid, rollbackErr),
			want:   invalid,
			causes: []error{invalid, rollbackErr},
		},
		// A connect.Error wrapping a database error keeps its own code, not
		// the one the database error would map to.
		"with a database cause": {
			err:    fmt.Errorf("move: %w", connect.NewError(connect.CodeFailedPrecondition, "can't move the rule").WithCause(sql.ErrNoRows)),
			want:   connect.NewError(connect.CodeFailedPrecondition, "can't move the rule"),
			causes: []error{sql.ErrNoRows},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := FromDBErr(test.err, "can't save the thing")
			connectErr, ok := got.(*connect.Error)
			if !ok {
				t.Fatalf("FromDBErr(%v) = %T, want *connect.Error", test.err, got)
			}
			if connectErr.Code() != test.want.Code() || connectErr.Message() != test.want.Message() {
				t.Errorf("FromDBErr(%v) = %v, want %v", test.err, got, test.want)
			}
			if !errors.Is(got, test.err) {
				t.Errorf("FromDBErr(%v) = %v, want it to wrap the original error", test.err, got)
			}
			for _, cause := range test.causes {
				if !errors.Is(got, cause) {
					t.Errorf("FromDBErr(%v) lost the cause %v", test.err, cause)
				}
			}
		})
	}

	// Nothing wraps it, so there is nothing to keep and it comes back as it
	// is, not as its own cause.
	t.Run("as is", func(t *testing.T) {
		if got := FromDBErr(invalid, "can't save the thing"); got != invalid {
			t.Fatalf("FromDBErr(%v) = %v, want the same error back", invalid, got)
		}
	})
}
