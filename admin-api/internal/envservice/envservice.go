// Package envservice serves the admin API for environments.
package envservice

import (
	"context"

	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/utils"
	environmentv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/environment/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements the EnvironmentService RPCs on top of the store.
type Service struct {
	store *store.Store
}

// New returns a Service that keeps its data in store.
func New(store *store.Store) *Service {
	return &Service{
		store: store,
	}
}

// envToProto converts a stored environment into the API's Environment,
// which every RPC returning environments wraps in its own response.
func envToProto(storedEnv database.Environment) *environmentv1.Environment {
	return &environmentv1.Environment{
		Id:        storedEnv.ID.String(),
		Name:      storedEnv.Name,
		Version:   storedEnv.Version,
		CreatedAt: timestamppb.New(storedEnv.CreatedAt),
		UpdatedAt: timestamppb.New(storedEnv.UpdatedAt),
	}
}

// Create adds an environment with the requested name, normalized and checked
// by the store, which also gives every existing flag a disabled setting in it.
// It returns the environment as stored, with its new id. Store errors reach
// the client as they are, since they're already connect errors meant for it.
func (s *Service) Create(ctx context.Context, req *environmentv1.CreateRequest) (*environmentv1.CreateResponse, error) {
	env, err := s.store.CreateEnv(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	return &environmentv1.CreateResponse{Environment: envToProto(env)}, nil
}

// Get returns the environment with the requested id, at its current version.
// The proto's rules reject an id that isn't a UUID; parsing it again here only
// guards against a caller without them. An id no environment has gives
// NotFound.
func (s *Service) Get(ctx context.Context, req *environmentv1.GetRequest) (*environmentv1.GetResponse, error) {
	envID, err := utils.ParseID(req.Id, "environment")
	if err != nil {
		return nil, err
	}
	env, err := s.store.GetEnv(ctx, envID)
	if err != nil {
		return nil, err
	}
	return &environmentv1.GetResponse{Environment: envToProto(env)}, nil
}

// Update gives the environment with the requested id the requested name,
// normalized and checked by the store as in Create, and returns the
// environment as stored. As in Get, the proto's rules reject an id that isn't
// a UUID, and parsing it again guards against a caller without them. An id no
// environment has gives NotFound.
func (s *Service) Update(ctx context.Context, req *environmentv1.UpdateRequest) (*environmentv1.UpdateResponse, error) {
	envID, err := utils.ParseID(req.Id, "environment")
	if err != nil {
		return nil, err
	}
	env, err := s.store.UpdateEnv(ctx, envID, req.Name)
	if err != nil {
		return nil, err
	}
	return &environmentv1.UpdateResponse{Environment: envToProto(env)}, nil
}
