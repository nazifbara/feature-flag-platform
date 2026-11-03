package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/nazifbara/flagsvc/backend/internal/utils"

	connect "connectrpc.com/connect/v2"
)

// NewLoggingInterceptor logs every failed call, and replaces the message of
// errors with a sensitive code with a generic one before it reaches the client.
func NewLoggingInterceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			sensitiveCodes := []connect.Code{connect.CodeInternal, connect.CodeAlreadyExists, connect.CodePermissionDenied}
			info, _ := connect.CallInfoForServerContext(ctx)
			err := next(ctx, spec, stream)
			if err != nil {
				slog.Log(ctx, slog.LevelError, err.Error(), slog.String("caller-address", info.PeerAddr), slog.String("procedure", info.Spec.Procedure))
			}
			if connectErr, ok := errors.AsType[*connect.Error](err); ok {
				if slices.Contains(sensitiveCodes, connectErr.Code()) {
					err = connect.NewError(connectErr.Code(), utils.CodeToText(connectErr.Code())).WithCause(err)
				}
			}
			return err
		}
	}
}
