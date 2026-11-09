// Package ffserrors maps errors to connect.Error
package ffserrors

import (
	"context"
	"database/sql"
	"errors"

	connect "connectrpc.com/connect/v2"
	"github.com/lib/pq"
)

// userFacingUniques are the unique constraints a client breaks by asking for a
// key, name or email that's taken. Any other unique violation means the data or
// the store is wrong, so it's internal. The names are the ones the schema gives
// the constraints.
var userFacingUniques = map[string]bool{
	"flags_key_unique":         true,
	"environments_name_unique": true,
	"identities_email_unique":  true,
}

// FromDBErr maps a database error to the connect.Error with the matching code.
// msg reaches the client, so it must not reveal internals. err is kept as the
// cause, which connect doesn't send to the client. An err that already holds a
// connect.Error, such as a validation error returned from inside a transaction,
// comes back as that connect.Error, keeping its code and message, with err as
// a cause when it wraps more, such as a failed rollback. Unique violations are
// only blamed on the client for the constraints in userFacingUniques; the rest
// are internal. Foreign key violations are always internal: every write looks
// its parent rows up under the lock first, which reports a missing one as not
// found, so only a bug reaches a foreign key.
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
			if userFacingUniques[pqErr.Constraint] {
				return connect.NewError(connect.CodeAlreadyExists, msg).WithCause(err)
			}
			return connect.NewError(connect.CodeInternal, msg).WithCause(err)
		case "57014": // query_canceled
			return connect.NewError(connect.CodeCanceled, msg).WithCause(err)
		case "22021", "22P05": // character_not_in_repertoire, untranslatable_character
			return connect.NewError(connect.CodeInvalidArgument, msg).WithCause(err)
		}
	}
	return connect.NewError(connect.CodeInternal, msg).WithCause(err)
}
