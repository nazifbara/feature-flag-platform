package authservice

import (
	"context"

	"connectrpc.com/connect/v2"
	"github.com/alexedwards/argon2id"

	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/ffserrors"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	authv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/auth/v1"
)

type Service struct {
	store *store.Store
}

// New returns a Service that keeps its data in store.
func New(store *store.Store) *Service {
	return &Service{
		store: store,
	}
}

// SignUp creates the identity and the user in one transaction, so neither exists
// without the other.
func (s *Service) SignUp(
	ctx context.Context,
	req *authv1.SignUpRequest,
) (*authv1.SignUpResponse, error) {
	err := s.store.ExecTx(ctx, func(q *database.Queries) error {
		passwordHash, err := hashPassword(req.Password)
		if err != nil {
			return connect.NewError(connect.CodeInternal, "can't sign up").WithCause(err)
		}

		identity, err := q.CreateAuth(
			ctx,
			database.CreateAuthParams{
				Email:        req.Email,
				Passwordhash: passwordHash,
			},
		)
		if err != nil {
			return ffserrors.FromDBErr(err, "can't sign up")
		}

		_, err = q.CreateUser(
			ctx,
			database.CreateUserParams{
				ID:        identity.ID,
				Email:     req.Email,
				Firstname: req.FirstName,
				Lastname:  req.LastName,
			},
		)
		if err != nil {
			return ffserrors.FromDBErr(err, "can't sign up")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &authv1.SignUpResponse{}, nil
}

// hashPassword hashes password with argon2id's default parameters.
func hashPassword(password string) (string, error) {
	return argon2id.CreateHash(password, argon2id.DefaultParams)
}
