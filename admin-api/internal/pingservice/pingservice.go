package pingservice

import (
	"context"

	pingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/ping/v1"
)

type PingService struct{}

// Ping answers with the message it was sent.
func (p PingService) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	return &pingv1.PingResponse{
		Message: req.Message,
	}, nil
}
