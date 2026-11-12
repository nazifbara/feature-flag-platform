package pingservice

import (
	"context"

	"github.com/nazifbara/feature-flag-platform/gen/go/flagsvc/ping/v1"
)

type PingService struct{}

// Ping answers with the message it was sent.
func (p PingService) Ping(ctx context.Context, req *ping.PingRequest) (*ping.PingResponse, error) {
	return &ping.PingResponse{
		Message: req.Message,
	}, nil
}
