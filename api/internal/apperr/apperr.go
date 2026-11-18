// Package apperr handles the application's errors, such as turning them into
// connect.Error values for clients.
package apperr

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"

	connect "connectrpc.com/connect/v2"
	"github.com/lib/pq"
)

// errWithAttr is an error carrying log attributes, made by WithAttrs.
type errWithAttr struct {
	error
	attrs []slog.Attr
}

// Unwrap returns the error the attributes were added to.
func (e *errWithAttr) Unwrap() error {
	return e.error
}

// Attrs returns the attributes added to this error alone, not those of the
// errors it wraps.
func (e *errWithAttr) Attrs() []slog.Attr {
	return e.attrs
}

// attrError is an error that carries log attributes.
type attrError interface {
	Attrs() []slog.Attr
}

// Attrs collects the log attributes of err and of every error it wraps, outer
// errors first. It follows single wrapping only, so it doesn't look inside
// errors.Join or fmt.Errorf with several %w.
func Attrs(err error) []slog.Attr {
	var attrs []slog.Attr
	for err != nil {
		if attrErr, ok := err.(attrError); ok {
			attrs = append(attrs, attrErr.Attrs()...)
		}
		err = errors.Unwrap(err)
	}
	return attrs
}

// WithAttrs wraps err with log attributes, given like slog's key-value
// arguments, for the logger to print next to it. The returned error has err's
// text and unwraps to err. A nil err gives nil, like pkg/errors' Wrap, so a
// result can be wrapped without checking it first.
func WithAttrs(err error, args ...any) error {
	if err == nil {
		return nil
	}
	return &errWithAttr{
		error: err,
		attrs: argsToAttr(args),
	}
}

// argsToAttr turns slog-style arguments into attributes: a slog.Attr is taken
// as is, a string is a key followed by its value, and anything else is logged
// under "!BADKEY", like slog does.
func argsToAttr(args []any) []slog.Attr {
	attrs := make([]slog.Attr, 0, len(args))
	for i := 0; i < len(args); {
		switch key := args[i].(type) {
		case slog.Attr:
			attrs = append(attrs, key)
			i++
		case string:
			if i+1 >= len(args) {
				attrs = append(attrs, slog.String("!BADKEY", key))
				i++
			} else {
				attrs = append(attrs, slog.Any(key, args[i+1]))
				i += 2
			}
		default:
			attrs = append(attrs, slog.Any("!BADKEY", args[i]))
			i++
		}
	}
	return attrs
}

// userFacingUniques are the unique constraints a client breaks by asking for a
// key, name or email that's taken. Any other unique violation means the data or
// the store is wrong, so it's internal. The names are the ones the schema gives
// the constraints.
var userFacingUniques = map[string]bool{
	"flags_key_unique":         true,
	"environments_name_unique": true,
	"identities_email_unique":  true,
}

// FromDB maps a database error to the connect.Error with the matching code.
// msg reaches the client, so it must not reveal internals. err is kept as the
// cause, which connect doesn't send to the client. An err that already holds a
// connect.Error, such as a validation error returned from inside a transaction,
// comes back as that connect.Error, keeping its code and message, with err as
// a cause when it wraps more, such as a failed rollback. Unique violations are
// only blamed on the client for the constraints in userFacingUniques; the rest
// are internal. Foreign key violations are always internal: every write looks
// its parent rows up under the lock first, which reports a missing one as not
// found, so only a bug reaches a foreign key.
func FromDB(err error, msg string) error {
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
