package interceptor

import (
	"context"
	"log/slog"
	"uuid"

	connect "connectrpc.com/connect/v2"
)

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

// NewLoggingInterceptor logs every call with its request ID, procedure and the
// caller's address: at info when it succeeds, and at error with the error when
// it fails. The request ID is the client's X-Request-Id header, replaced with a
// new version 7 UUID, which sorts by creation time, when it's missing or isn't a
// UUID, and is sent back in the response's
// X-Request-Id header. The client gets the error's message,
// but connect never sends it the cause.
func NewLoggingInterceptor(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			attrs := []any{
				slog.String("caller-address", info.PeerAddr),
				slog.String("procedure", info.Spec.Procedure),
			}
			id := info.RequestHeader().Get("X-Request-Id")
			id = validateRequestID(id)
			attrs = append(attrs, slog.String("request-id", id))
			info.ResponseHeader().Set("X-Request-Id", id)

			err := next(ctx, spec, stream)

			if err != nil {
				attrs = append(attrs, slog.Any("error", err))
				logger.Error("Served Request", attrs...)
			} else {
				logger.Info("Served Request", attrs...)
			}
			return err
		}
	}
}
