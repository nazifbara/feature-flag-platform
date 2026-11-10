// Package flagservice serves the admin API for flags.
package flagservice

import (
	"context"

	"github.com/nazifbara/flagsvc/api/internal/database"
	"github.com/nazifbara/flagsvc/api/internal/store"
	flag "github.com/nazifbara/flagsvc/gen/go/flagsvc/flag/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the FlagService RPCs on top of the store.
type Service struct {
	store *store.Store
}

// New returns a Service that keeps its data in store.
func New(store *store.Store) *Service {
	return &Service{
		store: store,
	}
}

// Create adds a flag with the requested key, normalized and checked by the
// store, which also gives it a disabled setting in every environment. It
// returns the flag as stored, with its new id and salt. Store errors reach the
// client as they are, since they're already connect errors meant for it.
func (s *Service) Create(ctx context.Context, req *flag.CreateRequest) (*flag.CreateResponse, error) {
	storedFlag, err := s.store.CreateFlag(ctx, req.Key)
	if err != nil {
		return nil, err
	}
	return &flag.CreateResponse{Flag: flagToProto(storedFlag)}, nil
}

// flagToProto converts a stored flag into the API's Flag, which every RPC
// returning flags wraps in its own response.
func flagToProto(f database.Flag) *flag.Flag {
	return &flag.Flag{
		Id:        f.ID.String(),
		Key:       f.Key,
		Salt:      f.Salt,
		CreatedAt: timestamppb.New(f.CreatedAt),
		UpdatedAt: timestamppb.New(f.UpdatedAt),
	}
}
