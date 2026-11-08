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
// msg reaches the client, so it must not reveal internals. err is kept as the
// cause, which connect doesn't send to the client. An err that already holds a
// connect.Error, such as a validation error returned from inside a transaction,
// comes back as that connect.Error, keeping its code and message, with err as
// a cause when it wraps more, such as a failed rollback.
func FromDBErr(err error, msg string) error {
	if connectErr, ok := errors.AsType[*connect.Error](err); ok {
		if connectErr == err {
			return connectErr
		}
		return connectErr.WithCause(err)
	}
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
