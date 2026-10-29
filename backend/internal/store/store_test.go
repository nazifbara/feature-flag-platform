package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nazifbara/flagsvc/backend/internal/database"
)

// envCount gives the number of committed environments.
func envCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM environments").Scan(&n); err != nil {
		t.Fatalf("counting environments: %v", err)
	}
	return n
}

// waitTxDone blocks until q's transaction has been rolled back behind the
// scenes, which database/sql does once the transaction's context is canceled.
// Queries are run with a fresh context so they report ErrTxDone rather than
// the canceled context.
func waitTxDone(t *testing.T, q *database.Queries) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := q.GetEnv(context.Background(), uuid.Nil); errors.Is(err, sql.ErrTxDone) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("transaction still open 5s after its context was canceled")
}

func TestExecTx(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("commits when fn succeeds", func(t *testing.T) {
		resetDB(t, store.db)
		var created database.Environment
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			var err error
			created, err = q.CreateEnv(ctx, "production")
			return err
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		got, err := store.GetEnv(ctx, created.ID)
		if err != nil {
			t.Fatalf("GetEnv() after commit: %v", err)
		}
		if got != created {
			t.Fatalf("GetEnv() = %+v, want %+v", got, created)
		}
	})

	t.Run("commits every write in fn", func(t *testing.T) {
		resetDB(t, store.db)
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, name := range []string{"development", "staging", "production"} {
				if _, err := q.CreateEnv(ctx, name); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		if n := envCount(t, store); n != 3 {
			t.Fatalf("%d environments after commit, want 3", n)
		}
	})

	// fn's writes are visible inside the transaction but not outside it
	// until the commit.
	t.Run("writes are hidden until committed", func(t *testing.T) {
		resetDB(t, store.db)
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			env, err := q.CreateEnv(ctx, "production")
			if err != nil {
				return err
			}
			if _, err := q.GetEnv(ctx, env.ID); err != nil {
				t.Errorf("GetEnv() inside the transaction: %v", err)
			}
			if _, err := store.GetEnv(ctx, env.ID); err == nil {
				t.Error("GetEnv() outside the transaction found an uncommitted environment")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
	})

	t.Run("rolls back and returns fn's error", func(t *testing.T) {
		resetDB(t, store.db)
		errBoom := errors.New("boom")
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.CreateEnv(ctx, "production"); err != nil {
				return err
			}
			return errBoom
		})
		if err != errBoom {
			t.Fatalf("ExecTx() error = %v, want %v unchanged", err, errBoom)
		}
		if n := envCount(t, store); n != 0 {
			t.Fatalf("%d environments after rollback, want 0", n)
		}
	})

	// A failing write rolls back the writes before it too.
	t.Run("a failing write rolls back earlier ones", func(t *testing.T) {
		resetDB(t, store.db)
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			if _, err := q.CreateEnv(ctx, "production"); err != nil {
				return err
			}
			_, err := q.CreateEnv(ctx, "production")
			return err
		})
		if err == nil {
			t.Fatal("ExecTx() = nil, want the duplicate name error")
		}
		if n := envCount(t, store); n != 0 {
			t.Fatalf("%d environments after rollback, want 0", n)
		}
	})

	t.Run("canceled context fails before fn runs", func(t *testing.T) {
		resetDB(t, store.db)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		called := false
		err := store.ExecTx(canceled, func(q *database.Queries) error {
			called = true
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecTx() error = %v, want %v", err, context.Canceled)
		}
		if called {
			t.Fatal("ExecTx() called fn without a transaction")
		}
	})

	// When the transaction is already gone, the rollback fails too, and both
	// errors are reported.
	t.Run("reports a failed rollback with fn's error", func(t *testing.T) {
		resetDB(t, store.db)
		txCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		errBoom := errors.New("boom")
		err := store.ExecTx(txCtx, func(q *database.Queries) error {
			cancel()
			waitTxDone(t, q)
			return errBoom
		})
		if !errors.Is(err, errBoom) || !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("ExecTx() error = %v, want it to wrap %v and %v", err, errBoom, sql.ErrTxDone)
		}
		want := "tx error: boom, rollback error: " + sql.ErrTxDone.Error()
		if err.Error() != want {
			t.Fatalf("ExecTx() error = %q, want %q", err.Error(), want)
		}
	})

	t.Run("reports a failed commit", func(t *testing.T) {
		resetDB(t, store.db)
		txCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		err := store.ExecTx(txCtx, func(q *database.Queries) error {
			if _, err := q.CreateEnv(context.Background(), "production"); err != nil {
				return err
			}
			cancel()
			waitTxDone(t, q)
			return nil
		})
		if err == nil {
			t.Fatal("ExecTx() = nil after the commit failed, want an error")
		}
		if n := envCount(t, store); n != 0 {
			t.Fatalf("%d environments after a failed commit, want 0", n)
		}
	})

	// A panic in fn must still end the transaction, or its connection stays
	// checked out of the pool, and its locks held, for good. The panic itself
	// keeps going up to the caller.
	t.Run("rolls back when fn panics", func(t *testing.T) {
		resetDB(t, store.db)
		// Canceling txCtx makes database/sql end the transaction if ExecTx
		// didn't, so a failure here can't block the tests after it.
		txCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = store.ExecTx(txCtx, func(q *database.Queries) error {
				if _, err := q.CreateEnv(txCtx, "production"); err != nil {
					return err
				}
				panic("boom")
			})
		}()
		if recovered != "boom" {
			t.Fatalf("recovered %v, want the panic from fn to reach the caller", recovered)
		}
		if inUse := store.db.Stats().InUse; inUse != 0 {
			t.Fatalf("%d connections still in use after the panic, want 0: the transaction was not rolled back", inUse)
		}
	})
}
