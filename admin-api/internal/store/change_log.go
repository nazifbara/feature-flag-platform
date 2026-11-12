package store

import (
	"context"

	"connectrpc.com/connect/v2"
	"github.com/nazifbara/flagsvc/admin-api/internal/database"
	"github.com/nazifbara/flagsvc/admin-api/internal/ffserrors"
)

// maxChangesBehind is the furthest an SDK can be behind and still catch up from
// the change log; further behind, it has to sync from scratch.
const maxChangesBehind = 40

// ChangeSince gives an environment's change log entries past an SDK's version,
// oldest first, for it to catch up. An SDK already at the environment's version
// gets none, one past it is rejected, and one more than maxChangesBehind
// versions behind gets a FailedPrecondition error, telling it to sync from
// scratch. It reads under the matrix lock, so it never sees a version bumped
// but not logged yet.
func (s *Store) ChangeSince(ctx context.Context, args database.ChangeSinceParams) ([]database.ChangeLog, error) {
	var changes []database.ChangeLog
	err := s.ExecTx(ctx, func(q *database.Queries) error {
		if err := q.LockFlagEnvMatrix(ctx); err != nil {
			return err
		}
		env, err := q.GetEnv(ctx, args.EnvironmentID)
		if err != nil {
			return err
		}
		if env.Version == args.Version {
			return nil
		}
		if env.Version-args.Version < 0 {
			return connect.NewError(connect.CodeInvalidArgument, "current environment version can't exceed lastest vsersion")
		}
		if env.Version-args.Version > maxChangesBehind {
			return connect.NewError(connect.CodeFailedPrecondition, "too old")
		}
		c, err := q.ChangeSince(ctx, args)
		if err != nil {
			return err
		}
		changes = c
		return nil
	})
	if err != nil {
		return nil, ffserrors.FromDBErr(err, "can't get the change logs")
	}
	return changes, nil
}
