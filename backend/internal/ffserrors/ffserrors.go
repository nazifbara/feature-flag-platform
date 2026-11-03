// Package ffserrors maps errors to connect.Error
package ffserrors

import (
	"context"
	"database/sql"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/lib/pq"
)

// FromDBErr maps a database error to the connect.Error with the matching code.
// msg is what the client sees, so it must not reveal internals; err is kept as
// the cause for logging.
func FromDBErr(err error, msg string) error {
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, msg).WithCause(err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, msg).WithCause(err)
	case errors.Is(err, sql.ErrNoRows):
		return connect.NewError(connect.CodeNotFound, msg).WithCause(err)
	}
	if pqErr, ok := errors.AsType[*pq.Error](err); ok {
		switch pqErr.Code {
		case "23505": // unique_violation
			return connect.NewError(connect.CodeAlreadyExists, msg).WithCause(err)
		case "23503": // foreign_key_violation
			return connect.NewError(connect.CodeNotFound, msg).WithCause(err)
		case "57014": // query_canceled
			return connect.NewError(connect.CodeCanceled, msg).WithCause(err)
		case "22021", "22P05": // character_not_in_repertoire, untranslatable_character
			return connect.NewError(connect.CodeInvalidArgument, msg).WithCause(err)
		}
	}
	return connect.NewError(connect.CodeInternal, msg).WithCause(err)
}
