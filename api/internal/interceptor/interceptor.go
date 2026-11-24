package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"uuid"

	connect "connectrpc.com/connect/v2"
)

// contextKey keeps the package's context keys apart from other packages'.
type contextKey string

// requestIDContextKey is the context key NewRequestIDInterceptor stores the
// call's request ID under, as a uuid.UUID.
const requestIDContextKey contextKey = "request-id"

// validateRequestID returns id, the client's X-Request-Id header, in the
// standard UUID form when it parses as one, or a new version 7 UUID when it's
// empty or doesn't.
func validateRequestID(id string) string {
	newID := uuid.NewV7().String()
	if id == "" {
		return newID
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return newID
	}
	return parsedID.String()
}

// GetRequestID returns the request ID NewRequestIDInterceptor put in ctx, or
// an error when ctx doesn't come from a call it handled.
func GetRequestID(ctx context.Context) (uuid.UUID, error) {
	if v, ok := ctx.Value(requestIDContextKey).(uuid.UUID); ok {
		return v, nil
	}

	return uuid.Nil(), errors.New("request id not set")
}

// NewRequestIDInterceptor gives every call a request ID and puts it in the
// call's context, for GetRequestID. The ID is the client's X-Request-Id header
// in the standard UUID form, replaced with a new version 7 UUID, which sorts by
// creation time, when it's missing or isn't a UUID.
func NewRequestIDInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			id := info.RequestHeader().Get("X-Request-Id")
			id = validateRequestID(id)
			ctx = context.WithValue(ctx, requestIDContextKey, uuid.MustParse(id))
			return next(ctx, spec, stream)
		}
	}
}

// NewLoggingInterceptor logs every call with its request ID, procedure and the
// caller's address: at info when it succeeds, and at error with the error when
// it fails. The request ID is the one NewRequestIDInterceptor put in the
// context, so that interceptor must run first; it's sent back in the
// response's X-Request-Id header. Without one, the call is logged at error as
// one that couldn't be processed and fails as internal without reaching the
// handler. The client gets the error's message, but connect never sends it the
// cause.
func NewLoggingInterceptor(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			attrs := []any{
				slog.String("caller-address", info.PeerAddr),
				slog.String("procedure", info.Spec.Procedure),
			}
			id, err := GetRequestID(ctx)
			if err != nil {
				attrs = append(attrs, slog.Any("error", err))
				logger.Error("request couldn't be processed", attrs...)
				return connect.NewError(connect.CodeInternal, connect.CodeInternal.String()).WithCause(err)
			}
			attrs = append(attrs, slog.String("request-id", id.String()))
			info.ResponseHeader().Set("X-Request-Id", id.String())

			err = next(ctx, spec, stream)

			if err != nil {
				attrs = append(attrs, slog.Any("error", err))
				logger.Error("served request", attrs...)
			} else {
				logger.Info("served request", attrs...)
			}
			return err
		}
	}
}
