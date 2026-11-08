package ffserrors

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		"unique violation":         {&pq.Error{Code: "23505"}, connect.CodeAlreadyExists},
		"wrapped unique violation": {fmt.Errorf("insert: %w", &pq.Error{Code: "23505"}), connect.CodeAlreadyExists},
		"foreign key violation":    {&pq.Error{Code: "23503"}, connect.CodeNotFound},
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
