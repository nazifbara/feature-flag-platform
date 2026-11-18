package interceptor

import (
	"context"
	"log/slog"

	connect "connectrpc.com/connect/v2"
)

// NewLoggingInterceptor logs every call with its procedure and the caller's
// address: at info when it succeeds, and at error with the error when it
// fails. The client gets the error's message, but connect never sends it the
// cause.
func NewLoggingInterceptor(logger *slog.Logger) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			attrs := []any{slog.String("caller-address", info.PeerAddr), slog.String("procedure", info.Spec.Procedure)}
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
