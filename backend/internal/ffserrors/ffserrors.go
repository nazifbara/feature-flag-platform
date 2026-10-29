// Package ffserrors maps errors to connect.Error
package ffserrors

import (
	"context"
	"database/sql"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/lib/pq"
)

func FromDBErr(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()).WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()).WithCause(err)
	case errors.Is(err, sql.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, connect.CodeNotFound.String()).WithCause(err)
	}
	if pqErr, ok := errors.AsType[*pq.Error](err); ok {
		switch pqErr.Code {
		case "23505": // unique_violation
			return connect.NewError(connect.CodeAlreadyExists, connect.CodeAlreadyExists.String()).WithCause(err)
		case "57014": // query_canceled
			return connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()).WithCause(err)
		}
	}
	return connect.NewError(connect.CodeInternal, connect.CodeInternal.String()).WithCause(err)
}
