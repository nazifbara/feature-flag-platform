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
		"query canceled":           {&pq.Error{Code: "57014"}, connect.CodeCanceled},
		// Anything unrecognized is internal, never nil.
		"other Postgres error": {&pq.Error{Code: "22001"}, connect.CodeInternal},
		"closed database":      {sql.ErrConnDone, connect.CodeInternal},
		"plain error":          {errors.New("boom"), connect.CodeInternal},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := FromDBErr(test.err)
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
			// The original error is kept as the cause for logging.
			if !errors.Is(got, test.err) {
				t.Errorf("FromDBErr(%v) = %v, want it to wrap the original error", test.err, got)
			}
		})
	}
}
