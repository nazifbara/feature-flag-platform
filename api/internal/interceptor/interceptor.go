package interceptor

import (
	"context"
	"errors"
	"log/slog"

	connect "connectrpc.com/connect/v2"
)

// NewLoggingInterceptor logs every failed call with its cause. The client gets
// the error's message, but connect never sends it the cause.
func NewLoggingInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			info, _ := connect.CallInfoForServerContext(ctx)
			err := next(ctx, spec, stream)
			if err != nil {
				attrs := []any{slog.String("caller-address", info.PeerAddr), slog.String("procedure", info.Spec.Procedure)}
				// A connect error's text leaves out its cause, like the
				// database error behind it.
				if connectErr, ok := errors.AsType[*connect.Error](err); ok && connectErr.Unwrap() != nil {
					attrs = append(attrs, slog.String("cause", connectErr.Unwrap().Error()))
				}
				slog.Log(ctx, slog.LevelError, err.Error(), attrs...)
			}
			return err
		}
	}
}
