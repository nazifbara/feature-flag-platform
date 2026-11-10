// Package flagservice serves the admin API for flags.
package flagservice

import (
	"context"
	"uuid"

	"connectrpc.com/connect/v2"
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

// Update gives the flag with the requested id the requested key, normalized and
// checked by the store, which also logs the rename in every environment. It
// returns the flag as stored. The proto's rules reject an id that isn't a UUID;
// parsing it again here only guards against a caller without them. An id no
// flag has gives NotFound.
func (s *Service) Update(ctx context.Context, req *flag.UpdateRequest) (*flag.UpdateResponse, error) {
	flagID, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "invalid flag id").WithCause(err)
	}
	updatedFlag, err := s.store.UpdateFlag(ctx, flagID, req.Key)
	if err != nil {
		return nil, err
	}
	return &flag.UpdateResponse{Flag: flagToProto(updatedFlag)}, nil
}

// Get returns the flag with the requested id. As in Update, the proto's rules
// reject an id that isn't a UUID, and parsing it again guards against a caller
// without them. An id no flag has gives NotFound.
func (s *Service) Get(ctx context.Context, req *flag.GetRequest) (*flag.GetResponse, error) {
	flagID, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "invalid flag id").WithCause(err)
	}
	storedFlag, err := s.store.GetFlag(ctx, flagID)
	if err != nil {
		return nil, err
	}
	return &flag.GetResponse{Flag: flagToProto(storedFlag)}, nil
}

// Delete removes the flag with the requested id, which the store does with its
// settings in every environment, logging the deletion there, and returns the
// flag as it was. As in Update, the proto's rules reject an id that isn't a
// UUID, and parsing it again guards against a caller without them. An id no
// flag has gives NotFound.
func (s *Service) Delete(ctx context.Context, req *flag.DeleteRequest) (*flag.DeleteResponse, error) {
	flagID, err := uuid.Parse(req.Id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, "invalid flag id").WithCause(err)
	}
	deletedFlag, err := s.store.DeleteFlag(ctx, flagID)
	if err != nil {
		return nil, err
	}
	return &flag.DeleteResponse{Flag: flagToProto(deletedFlag)}, nil
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
