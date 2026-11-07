package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
	"uuid"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/nazifbara/flagsvc/backend/internal/database"
)

// flagSettingCount gives the number of committed flag settings.
func flagSettingCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow("SELECT count(*) FROM flag_settings").Scan(&n); err != nil {
		t.Fatalf("counting flag settings: %v", err)
	}
	return n
}

// storedFlagSetting reads a flag setting straight from the table, so tests
// don't depend on a GetFlagSetting.
func storedFlagSetting(t *testing.T, store *Store, id uuid.UUID) database.FlagSetting {
	t.Helper()
	var s database.FlagSetting
	err := store.db.QueryRow("SELECT id, flag_id, environment_id, enabled, fallthrough, created_at, updated_at FROM flag_settings WHERE id = $1", id).
		Scan(&s.ID, &s.FlagID, &s.EnvironmentID, &s.Enabled, &s.Fallthrough, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		t.Fatalf("reading flag setting %v: %v", id, err)
	}
	return s
}

// mustUpdateFlagSetting gives the setting CreateFlag or CreateEnv made for the
// pair the values in arg.
func mustUpdateFlagSetting(t *testing.T, store *Store, arg database.UpdateFlagSettingParams) database.FlagSetting {
	t.Helper()
	s, err := store.UpdateFlagSetting(context.Background(), arg)
	if err != nil {
		t.Fatalf("UpdateFlagSetting(%+v): %v", arg, err)
	}
	return s
}

// mustRemoveFlagSetting deletes a pair's setting straight from the table, as no
// store method leaves a flag and an environment without one.
func mustRemoveFlagSetting(t *testing.T, store *Store, flagID, environmentID uuid.UUID) {
	t.Helper()
	if _, err := store.db.Exec("DELETE FROM flag_settings WHERE flag_id = $1 AND environment_id = $2", flagID, environmentID); err != nil {
		t.Fatalf("removing flag setting: %v", err)
	}
}

// Settings belong to their flag and environment, and go when either does.
func TestFlagSettingsDeletedWithOwner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two flags in two environments; each case deletes one owner and
	// expects only the settings it owned to go.
	setup := func(t *testing.T) (flag, otherFlag database.Flag, env, otherEnv database.Environment) {
		t.Helper()
		resetDB(t, store.db)
		flag = mustCreateFlag(t, store, "new-checkout")
		otherFlag = mustCreateFlag(t, store, "dark-mode")
		env = mustCreateEnv(t, store, "production")
		otherEnv = mustCreateEnv(t, store, "staging")
		return flag, otherFlag, env, otherEnv
	}
	countWhere := func(t *testing.T, column string, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := store.db.QueryRow("SELECT count(*) FROM flag_settings WHERE "+column+" = $1", id).Scan(&n); err != nil {
			t.Fatalf("counting settings by %s: %v", column, err)
		}
		return n
	}

	t.Run("deleting an environment", func(t *testing.T) {
		_, _, env, otherEnv := setup(t)
		if _, err := store.DeleteEnv(ctx, env.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		if n := countWhere(t, "environment_id", env.ID); n != 0 {
			t.Errorf("%d settings left in the deleted environment, want 0", n)
		}
		if n := countWhere(t, "environment_id", otherEnv.ID); n != 2 {
			t.Errorf("%d settings in the other environment, want 2", n)
		}
	})

	t.Run("deleting a flag", func(t *testing.T) {
		flag, otherFlag, _, _ := setup(t)
		if _, err := store.DeleteFlag(ctx, flag.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if n := countWhere(t, "flag_id", flag.ID); n != 0 {
			t.Errorf("%d settings left for the deleted flag, want 0", n)
		}
		if n := countWhere(t, "flag_id", otherFlag.ID); n != 2 {
			t.Errorf("%d settings for the other flag, want 2", n)
		}
	})
}

func TestGetFlagSetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Two flags in two environments, each setting with its own values, so a
	// lookup that matches on only one ID, or ignores both, is caught.
	flags := []database.Flag{mustCreateFlag(t, store, "new-checkout"), mustCreateFlag(t, store, "dark-mode")}
	envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
	var created []database.FlagSetting
	for i, f := range flags {
		for j, e := range envs {
			created = append(created, mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{
				FlagID:        f.ID,
				EnvironmentID: e.ID,
				Enabled:       (i+j)%2 == 0,
				Fallthrough:   float32(2*i+j) / 4,
			}))
		}
	}

	for _, want := range created {
		t.Run("finds the setting for its flag and environment", func(t *testing.T) {
			got, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: want.FlagID, EnvironmentID: want.EnvironmentID})
			if err != nil {
				t.Fatalf("GetFlagSetting(): %v", err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("GetFlagSetting() mismatch (-want +got):\n%s", diff)
			}
		})
	}

	notFound := connect.NewError(connect.CodeNotFound, "can't get the flag setting")
	t.Run("not found", func(t *testing.T) {
		flag, env := flags[0], envs[0]
		tests := map[string]database.GetFlagSettingParams{
			"unknown flag":        {FlagID: uuid.New(), EnvironmentID: env.ID},
			"unknown environment": {FlagID: flag.ID, EnvironmentID: uuid.New()},
			"zero IDs":            {},
			// The setting's own ID is not a flag or environment ID.
			"setting ID as flag":        {FlagID: created[0].ID, EnvironmentID: env.ID},
			"setting ID as environment": {FlagID: flag.ID, EnvironmentID: created[0].ID},
			// Easy to do with two fields of the same type.
			"IDs swapped": {FlagID: env.ID, EnvironmentID: flag.ID},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				got, err := store.GetFlagSetting(ctx, arg)
				wantConnectErr(t, err, notFound)
				if got != (database.FlagSetting{}) {
					t.Fatalf("GetFlagSetting() = %+v on error, want the zero FlagSetting", got)
				}
			})
		}
	})

	// Both exist, but the pair has lost its setting.
	t.Run("flag without a setting in the environment", func(t *testing.T) {
		flag := mustCreateFlag(t, store, "beta-banner")
		mustRemoveFlagSetting(t, store, flag.ID, envs[0].ID)
		_, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: envs[0].ID})
		wantConnectErr(t, err, notFound)
	})

	t.Run("deleted environment", func(t *testing.T) {
		env := mustCreateEnv(t, store, "qa")
		if _, err := store.DeleteEnv(ctx, env.ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		_, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flags[0].ID, EnvironmentID: env.ID})
		wantConnectErr(t, err, notFound)
	})
}

func TestGetFlagSettingReportsErrors(t *testing.T) {
	store := newTestStore(t)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	arg := database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, "can't get the flag setting"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't get the flag setting"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.GetFlagSetting(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			if got != (database.FlagSetting{}) {
				t.Fatalf("GetFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.GetFlagSetting(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't get the flag setting"))
		if got != (database.FlagSetting{}) {
			t.Fatalf("GetFlagSetting() = %+v on error, want the zero FlagSetting", got)
		}
	})
}

func TestUpdateFlagSetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// enabled, fallthrough and updated_at change; the ID, the pair and
	// created_at stay, and the change is saved, not just returned.
	t.Run("replaces the setting", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.25})
		got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{
			FlagID:        flag.ID,
			EnvironmentID: env.ID,
			Enabled:       true,
			Fallthrough:   0.75,
		})
		if err != nil {
			t.Fatalf("UpdateFlagSetting(): %v", err)
		}
		if got.ID != s.ID || got.FlagID != flag.ID || got.EnvironmentID != env.ID || !got.CreatedAt.Equal(s.CreatedAt) {
			t.Errorf("UpdateFlagSetting() = %+v, want the ID, flag, environment and created_at of %+v", got, s)
		}
		if !got.Enabled || got.Fallthrough != 0.75 {
			t.Errorf("UpdateFlagSetting() enabled, fallthrough = %v, %v, want true, 0.75", got.Enabled, got.Fallthrough)
		}
		if !got.UpdatedAt.After(s.UpdatedAt) {
			t.Errorf("UpdateFlagSetting() updated_at = %v, want it after %v", got.UpdatedAt, s.UpdatedAt)
		}
		if diff := cmp.Diff(got, storedFlagSetting(t, store, s.ID)); diff != "" {
			t.Fatalf("stored setting mismatch (-returned +stored):\n%s", diff)
		}
	})

	// Every value in [0, 1] is kept as given, with either enabled state,
	// including turning a setting off and back to nobody.
	t.Run("stores enabled and fallthrough as given", func(t *testing.T) {
		tests := map[string]struct {
			enabled  bool
			fraction float32
		}{
			"disabled, nobody":    {false, 0},
			"enabled, everybody":  {true, 1},
			"half":                {true, 0.5},
			"not exact in binary": {true, 0.1},
			"smallest above 0":    {true, math.SmallestNonzeroFloat32},
			"largest below 1":     {true, math.Nextafter32(1, 0)},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				resetDB(t, store.db)
				flag := mustCreateFlag(t, store, "new-checkout")
				env := mustCreateEnv(t, store, "production")
				s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: !test.enabled, Fallthrough: 0.3})
				got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{
					FlagID:        flag.ID,
					EnvironmentID: env.ID,
					Enabled:       test.enabled,
					Fallthrough:   test.fraction,
				})
				if err != nil {
					t.Fatalf("UpdateFlagSetting(): %v", err)
				}
				for _, s := range []database.FlagSetting{got, storedFlagSetting(t, store, s.ID)} {
					if s.Enabled != test.enabled || s.Fallthrough != test.fraction {
						t.Errorf("enabled, fallthrough = %v, %v, want %v, %v", s.Enabled, s.Fallthrough, test.enabled, test.fraction)
					}
				}
			})
		}
	})

	// Only the setting for the given pair changes, not the flag's settings
	// in other environments or other flags' settings in this one.
	t.Run("leaves other settings alone", func(t *testing.T) {
		resetDB(t, store.db)
		flags := []database.Flag{mustCreateFlag(t, store, "a"), mustCreateFlag(t, store, "b")}
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		var others []database.FlagSetting
		for i, f := range flags {
			for j, e := range envs {
				s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID, Fallthrough: 0.5})
				if i != 0 || j != 0 {
					others = append(others, s)
				}
			}
		}
		if _, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flags[0].ID, EnvironmentID: envs[0].ID, Enabled: true, Fallthrough: 1}); err != nil {
			t.Fatalf("UpdateFlagSetting(): %v", err)
		}
		for _, want := range others {
			if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
				t.Fatalf("other setting changed (-want +got):\n%s", diff)
			}
		}
	})

	// Writing the same values again isn't an update, so updated_at stays put
	// and the caller still gets the setting back.
	t.Run("same values change nothing", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		if err != nil {
			t.Fatalf("UpdateFlagSetting(): %v", err)
		}
		if diff := cmp.Diff(s, got); diff != "" {
			t.Errorf("UpdateFlagSetting() mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
			t.Errorf("stored setting changed (-want +got):\n%s", diff)
		}
	})

	// The clock is read per statement, not per transaction, so each update
	// in one transaction gets a later time.
	t.Run("updates in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		var updates []database.FlagSetting
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, f := range []float32{0.25, 0.5, 0.75} {
				u, err := q.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: f})
				if err != nil {
					return err
				}
				updates = append(updates, u)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		for i := 1; i < len(updates); i++ {
			if !updates[i].UpdatedAt.After(updates[i-1].UpdatedAt) {
				t.Errorf("update %d updated_at %v not after update %d updated_at %v", i, updates[i].UpdatedAt, i-1, updates[i-1].UpdatedAt)
			}
		}
	})
}

func TestUpdateFlagSettingRejectsInvalidFallthrough(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	outside := connect.NewError(connect.CodeInvalidArgument, "can't update a flag setting with a fallthrough outside [0, 1]")

	tests := map[string]float32{
		"negative":          -0.5,
		"just below 0":      -math.SmallestNonzeroFloat32,
		"just above 1":      math.Nextafter32(1, 2),
		"a percentage":      50,
		"NaN":               float32(math.NaN()),
		"positive infinity": float32(math.Inf(1)),
		"negative infinity": float32(math.Inf(-1)),
	}
	for name, fraction := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			flag := mustCreateFlag(t, store, "new-checkout")
			env := mustCreateEnv(t, store, "production")
			s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
			got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{
				FlagID:        flag.ID,
				EnvironmentID: env.ID,
				Enabled:       true,
				Fallthrough:   fraction,
			})
			wantConnectErr(t, err, outside)
			if got != (database.FlagSetting{}) {
				t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			// The rejected values were never written.
			if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
				t.Fatalf("setting changed (-want +got):\n%s", diff)
			}
		})
	}

	// Validation comes first, so a bad fallthrough is reported even for a
	// pair with no setting.
	t.Run("fallthrough checked before the IDs", func(t *testing.T) {
		resetDB(t, store.db)
		_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: uuid.New(), EnvironmentID: uuid.New(), Fallthrough: 2})
		wantConnectErr(t, err, outside)
	})
}

func TestUpdateFlagSettingNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, "can't update the flag setting")

	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
	// Both exist, but have no setting together.
	lonely := mustCreateFlag(t, store, "beta-banner")
	mustRemoveFlagSetting(t, store, lonely.ID, env.ID)

	tests := map[string]database.UpdateFlagSettingParams{
		"unknown flag":        {FlagID: uuid.New(), EnvironmentID: env.ID},
		"unknown environment": {FlagID: flag.ID, EnvironmentID: uuid.New()},
		"zero IDs":            {},
		"no setting yet":      {FlagID: lonely.ID, EnvironmentID: env.ID},
		"setting ID as flag":  {FlagID: s.ID, EnvironmentID: env.ID},
		// Easy to do with two fields of the same type.
		"IDs swapped": {FlagID: env.ID, EnvironmentID: flag.ID},
	}
	for name, arg := range tests {
		t.Run(name, func(t *testing.T) {
			arg.Enabled, arg.Fallthrough = true, 1
			got, err := store.UpdateFlagSetting(ctx, arg)
			wantConnectErr(t, err, notFound)
			if got != (database.FlagSetting{}) {
				t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			// No other setting was updated instead, and none was created.
			if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
				t.Fatalf("setting changed (-want +got):\n%s", diff)
			}
			if n := flagSettingCount(t, store); n != 1 {
				t.Fatalf("%d flag settings, want 1", n)
			}
		})
	}
}

// An update made through ExecTx is undone when the transaction rolls back.
func TestUpdateFlagSettingRolledBack(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})

	errBoom := errors.New("boom")
	err := store.ExecTx(ctx, func(q *database.Queries) error {
		if _, err := q.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1}); err != nil {
			return err
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("ExecTx() error = %v, want %v", err, errBoom)
	}
	if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
		t.Fatalf("setting changed after rollback (-want +got):\n%s", diff)
	}
}

func TestUpdateFlagSettingReportsErrors(t *testing.T) {
	store := newTestStore(t)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
	arg := database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, "can't update the flag setting"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't update the flag setting"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.UpdateFlagSetting(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			if got != (database.FlagSetting{}) {
				t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			if diff := cmp.Diff(s, storedFlagSetting(t, store, s.ID)); diff != "" {
				t.Fatalf("setting changed (-want +got):\n%s", diff)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.UpdateFlagSetting(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag setting"))
		if got != (database.FlagSetting{}) {
			t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
		}
	})
}

func TestListSettingsByFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// envNames gives the environment name of each setting, in order.
	envNames := func(t *testing.T, settings []database.FlagSetting, envs []database.Environment) []string {
		t.Helper()
		names := make(map[uuid.UUID]string)
		for _, e := range envs {
			names[e.ID] = e.Name
		}
		var out []string
		for _, s := range settings {
			out = append(out, names[s.EnvironmentID])
		}
		return out
	}
	list := func(t *testing.T, flagID uuid.UUID) []database.FlagSetting {
		t.Helper()
		got, err := store.ListSettingsByFlag(ctx, flagID)
		if err != nil {
			t.Fatalf("ListSettingsByFlag(%v): %v", flagID, err)
		}
		return got
	}

	t.Run("returns the flag's settings as stored", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		staging := mustCreateEnv(t, store, "staging")
		prod := mustCreateEnv(t, store, "production")
		want := []database.FlagSetting{
			mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: staging.ID, Enabled: true, Fallthrough: 1}),
			mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: prod.ID, Fallthrough: 0.25}),
		}
		if diff := cmp.Diff(want, list(t, flag.ID)); diff != "" {
			t.Fatalf("ListSettingsByFlag() mismatch (-want +got):\n%s", diff)
		}
	})

	// Settings follow the environments' order, not the order the settings
	// were last updated in.
	t.Run("ordered by environment creation", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{
			mustCreateEnv(t, store, "development"),
			mustCreateEnv(t, store, "staging"),
			mustCreateEnv(t, store, "production"),
		}
		for _, i := range []int{2, 0, 1} {
			mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: envs[i].ID})
		}
		if diff := cmp.Diff([]string{"development", "staging", "production"}, envNames(t, list(t, flag.ID), envs)); diff != "" {
			t.Fatalf("ListSettingsByFlag() order mismatch (-want +got):\n%s", diff)
		}
	})

	// The order comes from the environments' created_at, so changing it
	// reorders the list.
	t.Run("follows environment created_at", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{mustCreateEnv(t, store, "a"), mustCreateEnv(t, store, "b"), mustCreateEnv(t, store, "c")}
		if _, err := store.db.Exec("UPDATE environments SET created_at = created_at - interval '1 day' WHERE id = $1", envs[2].ID); err != nil {
			t.Fatalf("backdating environment c: %v", err)
		}
		if diff := cmp.Diff([]string{"c", "a", "b"}, envNames(t, list(t, flag.ID), envs)); diff != "" {
			t.Fatalf("ListSettingsByFlag() order mismatch (-want +got):\n%s", diff)
		}
	})

	// Environments created at the same moment are ordered by ID, so the
	// order is the same every time.
	t.Run("ties broken by environment ID", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{mustCreateEnv(t, store, "a"), mustCreateEnv(t, store, "b"), mustCreateEnv(t, store, "c")}
		if _, err := store.db.Exec("UPDATE environments SET created_at = '2026-01-01T00:00:00Z'"); err != nil {
			t.Fatalf("equalizing created_at: %v", err)
		}
		got := list(t, flag.ID)
		for i := 1; i < len(got); i++ {
			if got[i-1].EnvironmentID.String() >= got[i].EnvironmentID.String() {
				t.Fatalf("environment IDs not ascending: %v then %v", got[i-1].EnvironmentID, got[i].EnvironmentID)
			}
		}
		if len(got) != len(envs) {
			t.Fatalf("ListSettingsByFlag() returned %d settings, want %d", len(got), len(envs))
		}
	})

	// Other flags' settings in the same environments are left out.
	t.Run("only the given flag", func(t *testing.T) {
		resetDB(t, store.db)
		flags := []database.Flag{mustCreateFlag(t, store, "a"), mustCreateFlag(t, store, "b")}
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		for _, f := range flags {
			got := list(t, f.ID)
			if len(got) != len(envs) {
				t.Fatalf("ListSettingsByFlag(%s) returned %d settings, want %d", f.Key, len(got), len(envs))
			}
			for _, s := range got {
				if s.FlagID != f.ID {
					t.Fatalf("ListSettingsByFlag(%s) returned a setting of flag %v", f.Key, s.FlagID)
				}
			}
		}
	})

	// An environment's settings go with it, so it drops out of the list.
	t.Run("deleted environment is left out", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		if _, err := store.DeleteEnv(ctx, envs[0].ID); err != nil {
			t.Fatalf("DeleteEnv(): %v", err)
		}
		if diff := cmp.Diff([]string{"production"}, envNames(t, list(t, flag.ID), envs)); diff != "" {
			t.Fatalf("ListSettingsByFlag() mismatch (-want +got):\n%s", diff)
		}
	})

	// No settings is not an error, whether or not the flag exists.
	t.Run("empty", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		other := mustCreateFlag(t, store, "dark-mode")
		env := mustCreateEnv(t, store, "production")
		mustRemoveFlagSetting(t, store, flag.ID, env.ID)
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: other.ID, EnvironmentID: env.ID})
		tests := map[string]uuid.UUID{
			"flag without settings": flag.ID,
			"unknown flag":          uuid.New(),
			"zero ID":               uuid.Nil(),
			// Neither is a flag ID.
			"environment ID": env.ID,
			"setting ID":     s.ID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				if got := list(t, id); len(got) != 0 {
					t.Fatalf("ListSettingsByFlag(%v) = %+v, want none", id, got)
				}
			})
		}
	})
}

func TestListSettingsByFlagReportsErrors(t *testing.T) {
	store := newTestStore(t)
	flag := mustCreateFlag(t, store, "new-checkout")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, "can't list the flag's settings"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't list the flag's settings"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ListSettingsByFlag(test.ctx, flag.ID)
			wantConnectErr(t, err, test.want)
			if got != nil {
				t.Fatalf("ListSettingsByFlag() = %+v on error, want nil", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.ListSettingsByFlag(context.Background(), flag.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't list the flag's settings"))
		if got != nil {
			t.Fatalf("ListSettingsByFlag() = %+v on error, want nil", got)
		}
	})
}

func TestListSettingsByEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	list := func(t *testing.T, envID uuid.UUID) []database.ListSettingsByEnvRow {
		t.Helper()
		got, err := store.ListSettingsByEnv(ctx, envID)
		if err != nil {
			t.Fatalf("ListSettingsByEnv(%v): %v", envID, err)
		}
		return got
	}
	keys := func(rows []database.ListSettingsByEnvRow) []string {
		var out []string
		for _, r := range rows {
			out = append(out, r.Key)
		}
		return out
	}

	// Each setting comes back as stored, paired with its own flag's key.
	t.Run("returns each setting with its flag's key", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		a := mustCreateFlag(t, store, "a")
		b := mustCreateFlag(t, store, "b")
		want := []database.ListSettingsByEnvRow{
			{FlagSetting: mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: a.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1}), Key: "a"},
			{FlagSetting: mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: b.ID, EnvironmentID: env.ID, Fallthrough: 0.25}), Key: "b"},
		}
		if diff := cmp.Diff(want, list(t, env.ID)); diff != "" {
			t.Fatalf("ListSettingsByEnv() mismatch (-want +got):\n%s", diff)
		}
	})

	// Rows follow the keys, not the order the flags were made in.
	t.Run("ordered by key", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		for _, key := range []string{"c", "a", "b"} {
			mustCreateFlag(t, store, key)
		}
		if diff := cmp.Diff([]string{"a", "b", "c"}, keys(list(t, env.ID))); diff != "" {
			t.Fatalf("ListSettingsByEnv() order mismatch (-want +got):\n%s", diff)
		}
	})

	// The key is read from the flag, so a new key shows, in its new place.
	t.Run("follows a flag's new key", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		a := mustCreateFlag(t, store, "a")
		mustCreateFlag(t, store, "b")
		if _, err := store.UpdateFlag(ctx, a.ID, "c"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		got := list(t, env.ID)
		if diff := cmp.Diff([]string{"b", "c"}, keys(got)); diff != "" {
			t.Fatalf("ListSettingsByEnv() mismatch (-want +got):\n%s", diff)
		}
		if got[1].FlagSetting.FlagID != a.ID {
			t.Fatalf("key %q paired with flag %v, want %v", got[1].Key, got[1].FlagSetting.FlagID, a.ID)
		}
	})

	// A flag's settings in other environments are left out.
	t.Run("only the given environment", func(t *testing.T) {
		resetDB(t, store.db)
		flags := []database.Flag{mustCreateFlag(t, store, "a"), mustCreateFlag(t, store, "b")}
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		for _, e := range envs {
			got := list(t, e.ID)
			if len(got) != len(flags) {
				t.Fatalf("ListSettingsByEnv(%s) returned %d rows, want %d", e.Name, len(got), len(flags))
			}
			for _, r := range got {
				if r.FlagSetting.EnvironmentID != e.ID {
					t.Fatalf("ListSettingsByEnv(%s) returned a setting of environment %v", e.Name, r.FlagSetting.EnvironmentID)
				}
			}
		}
	})

	// A flag's settings go with it, so it drops out of the list.
	t.Run("deleted flag is left out", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		a := mustCreateFlag(t, store, "a")
		mustCreateFlag(t, store, "b")
		if _, err := store.DeleteFlag(ctx, a.ID); err != nil {
			t.Fatalf("DeleteFlag(): %v", err)
		}
		if diff := cmp.Diff([]string{"b"}, keys(list(t, env.ID))); diff != "" {
			t.Fatalf("ListSettingsByEnv() mismatch (-want +got):\n%s", diff)
		}
	})

	// No settings is not an error, whether or not the environment exists.
	t.Run("empty", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "staging")
		other := mustCreateEnv(t, store, "production")
		flag := mustCreateFlag(t, store, "new-checkout")
		mustRemoveFlagSetting(t, store, flag.ID, env.ID)
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: other.ID})
		tests := map[string]uuid.UUID{
			"environment without settings": env.ID,
			"unknown environment":          uuid.New(),
			"zero ID":                      uuid.Nil(),
			// Neither is an environment ID.
			"flag ID":    flag.ID,
			"setting ID": s.ID,
		}
		for name, id := range tests {
			t.Run(name, func(t *testing.T) {
				if got := list(t, id); len(got) != 0 {
					t.Fatalf("ListSettingsByEnv(%v) = %+v, want none", id, got)
				}
			})
		}
	})
}

func TestListSettingsByEnvReportsErrors(t *testing.T) {
	store := newTestStore(t)
	env := mustCreateEnv(t, store, "production")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, "can't list the environment's settings"),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, "can't list the environment's settings"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.ListSettingsByEnv(test.ctx, env.ID)
			wantConnectErr(t, err, test.want)
			if got != nil {
				t.Fatalf("ListSettingsByEnv() = %+v on error, want nil", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.ListSettingsByEnv(context.Background(), env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't list the environment's settings"))
		if got != nil {
			t.Fatalf("ListSettingsByEnv() = %+v on error, want nil", got)
		}
	})
}

// wantSettingForEveryPair checks every flag has a setting in every environment.
// Settings are unique per pair and can only point at flags and environments
// that exist, so the count is enough.
func wantSettingForEveryPair(t *testing.T, store *Store) {
	t.Helper()
	flags, envs := flagCount(t, store), envCount(t, store)
	if n := flagSettingCount(t, store); n != flags*envs {
		t.Fatalf("%d flag settings for %d flags and %d environments, want %d", n, flags, envs, flags*envs)
	}
}

// wantNewSetting checks s has the values of a setting nobody has changed yet.
func wantNewSetting(t *testing.T, s database.FlagSetting) {
	t.Helper()
	if s.ID == uuid.Nil() {
		t.Error("setting has a zero ID")
	}
	if s.Enabled || s.Fallthrough != 0 {
		t.Errorf("enabled, fallthrough = %v, %v, want false, 0", s.Enabled, s.Fallthrough)
	}
	if !s.UpdatedAt.Equal(s.CreatedAt) {
		t.Errorf("updated_at = %v, want it equal to created_at %v", s.UpdatedAt, s.CreatedAt)
	}
}

// rejectNewFlagSettings makes every insert into flag_settings fail until the
// test ends, leaving the rows already there alone.
func rejectNewFlagSettings(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec("ALTER TABLE flag_settings ADD CONSTRAINT reject_new CHECK (false) NOT VALID"); err != nil {
		t.Fatalf("adding the constraint: %v", err)
	}
	t.Cleanup(func() {
		if _, err := store.db.Exec("ALTER TABLE flag_settings DROP CONSTRAINT reject_new"); err != nil {
			t.Errorf("dropping the constraint: %v", err)
		}
	})
}

// beginMatrixTx starts a transaction holding the flag/env matrix lock, for a test
// to stand in for a write stopped halfway.
func beginMatrixTx(t *testing.T, store *Store) (*sql.Tx, *database.Queries) {
	t.Helper()
	ctx := context.Background()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx(): %v", err)
	}
	q := store.WithTx(tx)
	if err := q.LockFlagEnvMatrix(ctx); err != nil {
		tx.Rollback()
		t.Fatalf("LockFlagEnvMatrix(): %v", err)
	}
	return tx, q
}

// wantWaits runs fn in the background, checks it is still going after a moment,
// and gives the channel its error will arrive on.
func wantWaits(t *testing.T, fn func() error) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		t.Fatalf("finished with %v, want it to wait", err)
	case <-time.After(200 * time.Millisecond):
	}
	return done
}

func TestCreateFlagCreatesSettings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("one new setting per environment", func(t *testing.T) {
		resetDB(t, store.db)
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		flag := mustCreateFlag(t, store, "new-checkout")
		got, err := store.ListSettingsByFlag(ctx, flag.ID)
		if err != nil {
			t.Fatalf("ListSettingsByFlag(): %v", err)
		}
		if len(got) != len(envs) {
			t.Fatalf("%d settings, want %d", len(got), len(envs))
		}
		for i, s := range got {
			if s.EnvironmentID != envs[i].ID {
				t.Errorf("setting %d in environment %v, want %v", i, s.EnvironmentID, envs[i].ID)
			}
			wantNewSetting(t, s)
		}
		if got[0].ID == got[1].ID {
			t.Errorf("settings share ID %v", got[0].ID)
		}
	})

	t.Run("no environments, no settings", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "new-checkout")
		if n := flagSettingCount(t, store); n != 0 {
			t.Fatalf("%d flag settings, want 0", n)
		}
	})

	t.Run("leaves other flags' settings alone", func(t *testing.T) {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		other := mustCreateFlag(t, store, "dark-mode")
		want := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: other.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		mustCreateFlag(t, store, "new-checkout")
		if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
			t.Fatalf("other flag's setting changed (-want +got):\n%s", diff)
		}
	})

	// A flag without its settings would break the rule for every pair, so it
	// doesn't stay behind.
	t.Run("failed settings undo the flag", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		rejectNewFlagSettings(t, store)
		got, err := store.CreateFlag(ctx, "new-checkout")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the flag"))
		if diff := cmp.Diff(database.Flag{}, got); diff != "" {
			t.Fatalf("returned a flag on error (-want +got):\n%s", diff)
		}
		if n := flagCount(t, store); n != 0 {
			t.Fatalf("%d flags after a failed create, want 0", n)
		}
	})
}

func TestCreateEnvCreatesSettings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("one new setting per flag", func(t *testing.T) {
		resetDB(t, store.db)
		flags := []database.Flag{mustCreateFlag(t, store, "a"), mustCreateFlag(t, store, "b")}
		env := mustCreateEnv(t, store, "production")
		got, err := store.ListSettingsByEnv(ctx, env.ID)
		if err != nil {
			t.Fatalf("ListSettingsByEnv(): %v", err)
		}
		if len(got) != len(flags) {
			t.Fatalf("%d settings, want %d", len(got), len(flags))
		}
		for i, r := range got {
			if r.FlagSetting.FlagID != flags[i].ID {
				t.Errorf("setting %d for flag %v, want %v", i, r.FlagSetting.FlagID, flags[i].ID)
			}
			wantNewSetting(t, r.FlagSetting)
		}
		if got[0].FlagSetting.ID == got[1].FlagSetting.ID {
			t.Errorf("settings share ID %v", got[0].FlagSetting.ID)
		}
	})

	t.Run("no flags, no settings", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateEnv(t, store, "production")
		if n := flagSettingCount(t, store); n != 0 {
			t.Fatalf("%d flag settings, want 0", n)
		}
	})

	t.Run("leaves other environments' settings alone", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		other := mustCreateEnv(t, store, "staging")
		want := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: other.ID, Enabled: true, Fallthrough: 0.5})
		mustCreateEnv(t, store, "production")
		if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
			t.Fatalf("other environment's setting changed (-want +got):\n%s", diff)
		}
	})

	// An environment without its settings would break the rule for every
	// pair, so it doesn't stay behind.
	t.Run("failed settings undo the environment", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "new-checkout")
		rejectNewFlagSettings(t, store)
		got, err := store.CreateEnv(ctx, "production")
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't create the environment"))
		if diff := cmp.Diff(database.Environment{}, got); diff != "" {
			t.Fatalf("returned an environment on error (-want +got):\n%s", diff)
		}
		if n := envCount(t, store); n != 0 {
			t.Fatalf("%d environments after a failed create, want 0", n)
		}
	})
}

// Whatever order flags and environments come and go in, each pair has a setting.
func TestSettingForEveryPair(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	a := mustCreateFlag(t, store, "a")
	wantSettingForEveryPair(t, store)
	staging := mustCreateEnv(t, store, "staging")
	wantSettingForEveryPair(t, store)
	mustCreateFlag(t, store, "b")
	mustCreateEnv(t, store, "production")
	wantSettingForEveryPair(t, store)
	if n := flagSettingCount(t, store); n != 4 {
		t.Fatalf("%d flag settings, want 4", n)
	}

	if _, err := store.DeleteFlag(ctx, a.ID); err != nil {
		t.Fatalf("DeleteFlag(): %v", err)
	}
	wantSettingForEveryPair(t, store)
	if _, err := store.DeleteEnv(ctx, staging.ID); err != nil {
		t.Fatalf("DeleteEnv(): %v", err)
	}
	wantSettingForEveryPair(t, store)
	mustCreateFlag(t, store, "c")
	mustCreateEnv(t, store, "staging")
	wantSettingForEveryPair(t, store)
	if n := flagSettingCount(t, store); n != 4 {
		t.Fatalf("%d flag settings, want 4", n)
	}
}

// Neither can see the other's uncommitted row, so CreateFlag has to wait for a
// CreateEnv in progress or that pair never gets a setting.
func TestCreateFlagWaitsForCreateEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tx, q := beginMatrixTx(t, store)
	defer tx.Rollback()
	env, err := q.CreateEnv(ctx, "production")
	if err != nil {
		t.Fatalf("CreateEnv(): %v", err)
	}
	if err := q.CreateSettingsForEnv(ctx, env.ID); err != nil {
		t.Fatalf("CreateSettingsForEnv(): %v", err)
	}

	var flag database.Flag
	done := wantWaits(t, func() error {
		var err error
		flag, err = store.CreateFlag(ctx, "new-checkout")
		return err
	})
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("CreateFlag(): %v", err)
	}
	if _, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID}); err != nil {
		t.Fatalf("GetFlagSetting(): %v", err)
	}
	wantSettingForEveryPair(t, store)
}

// The same race as TestCreateFlagWaitsForCreateEnv, the other way round.
func TestCreateEnvWaitsForCreateFlag(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tx, q := beginMatrixTx(t, store)
	defer tx.Rollback()
	flag, err := q.CreateFlag(ctx, database.CreateFlagParams{Key: "new-checkout", Salt: "salt"})
	if err != nil {
		t.Fatalf("CreateFlag(): %v", err)
	}
	if err := q.CreateSettingsForFlag(ctx, flag.ID); err != nil {
		t.Fatalf("CreateSettingsForFlag(): %v", err)
	}

	var env database.Environment
	done := wantWaits(t, func() error {
		var err error
		env, err = store.CreateEnv(ctx, "production")
		return err
	})
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit(): %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("CreateEnv(): %v", err)
	}
	if _, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID}); err != nil {
		t.Fatalf("GetFlagSetting(): %v", err)
	}
	wantSettingForEveryPair(t, store)
}

// Creates racing each other wait their turn, so no pair is missed.
func TestCreateFlagAndCreateEnvRacing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	const racers = 8
	errs := make(chan error, 2*racers)
	for i := range racers {
		go func() {
			_, err := store.CreateFlag(ctx, fmt.Sprintf("flag-%d", i))
			errs <- err
		}()
		go func() {
			_, err := store.CreateEnv(ctx, fmt.Sprintf("env-%d", i))
			errs <- err
		}()
	}
	for range 2 * racers {
		if err := <-errs; err != nil {
			t.Errorf("create failed: %v", err)
		}
	}
	if n := flagSettingCount(t, store); n != racers*racers {
		t.Fatalf("%d flag settings, want %d", n, racers*racers)
	}
}

// A create can't tell yet whether a delete in progress will commit, so it waits
// to find out, then skips the row if it went and fails on neither outcome.
func TestCreatesWhileOwnerIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	finishes := map[string]struct {
		finish       func(*sql.Tx) error
		wantSettings int
	}{
		"delete commits":    {(*sql.Tx).Commit, 1},
		"delete rolls back": {(*sql.Tx).Rollback, 2},
	}
	for name, test := range finishes {
		t.Run("CreateFlag, "+name, func(t *testing.T) {
			resetDB(t, store.db)
			env := mustCreateEnv(t, store, "staging")
			mustCreateEnv(t, store, "production")
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("BeginTx(): %v", err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "DELETE FROM environments WHERE id = $1", env.ID); err != nil {
				t.Fatalf("deleting the environment: %v", err)
			}

			done := wantWaits(t, func() error {
				_, err := store.CreateFlag(ctx, "new-checkout")
				return err
			})
			if err := test.finish(tx); err != nil {
				t.Fatalf("finishing the delete: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("CreateFlag(): %v", err)
			}
			if n := flagSettingCount(t, store); n != test.wantSettings {
				t.Fatalf("%d flag settings, want %d", n, test.wantSettings)
			}
			wantSettingForEveryPair(t, store)
		})

		t.Run("CreateEnv, "+name, func(t *testing.T) {
			resetDB(t, store.db)
			flag := mustCreateFlag(t, store, "new-checkout")
			mustCreateFlag(t, store, "dark-mode")
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("BeginTx(): %v", err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(ctx, "DELETE FROM flags WHERE id = $1", flag.ID); err != nil {
				t.Fatalf("deleting the flag: %v", err)
			}

			done := wantWaits(t, func() error {
				_, err := store.CreateEnv(ctx, "production")
				return err
			})
			if err := test.finish(tx); err != nil {
				t.Fatalf("finishing the delete: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("CreateEnv(): %v", err)
			}
			if n := flagSettingCount(t, store); n != test.wantSettings {
				t.Fatalf("%d flag settings, want %d", n, test.wantSettings)
			}
			wantSettingForEveryPair(t, store)
		})
	}
}

// These wait for the lock before touching a row. DeleteFlag and
// UpdateFlagSetting take settings and then environments, and DeleteEnv the other
// way round, so without it they can deadlock.
func TestWritesWaitForMatrixLock(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]func(flag database.Flag, env database.Environment) error{
		"UpdateFlag": func(flag database.Flag, _ database.Environment) error {
			_, err := store.UpdateFlag(ctx, flag.ID, "checkout-v2")
			return err
		},
		"DeleteFlag": func(flag database.Flag, _ database.Environment) error {
			_, err := store.DeleteFlag(ctx, flag.ID)
			return err
		},
		"DeleteEnv": func(_ database.Flag, env database.Environment) error {
			_, err := store.DeleteEnv(ctx, env.ID)
			return err
		},
		"UpdateFlagSetting": func(flag database.Flag, env database.Environment) error {
			_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
			return err
		},
	}
	for name, write := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			flag := mustCreateFlag(t, store, "new-checkout")
			env := mustCreateEnv(t, store, "production")
			tx, _ := beginMatrixTx(t, store)
			defer tx.Rollback()

			done := wantWaits(t, func() error { return write(flag, env) })
			if err := tx.Rollback(); err != nil {
				t.Fatalf("Rollback(): %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("%s(): %v", name, err)
			}
		})
	}
}

// A flag and an environment deleted together both go, whichever gets there
// first, and neither delete is aborted as a deadlock.
func TestDeleteFlagAndDeleteEnvRacing(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	for range 20 {
		resetDB(t, store.db)
		env := mustCreateEnv(t, store, "production")
		mustCreateEnv(t, store, "staging")
		flag := mustCreateFlag(t, store, "new-checkout")

		errs := make(chan error, 2)
		go func() {
			_, err := store.DeleteFlag(ctx, flag.ID)
			errs <- err
		}()
		go func() {
			_, err := store.DeleteEnv(ctx, env.ID)
			errs <- err
		}()
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("delete failed: %v", err)
			}
		}
		if flags, envs := flagCount(t, store), envCount(t, store); flags != 0 || envs != 1 {
			t.Fatalf("%d flags and %d environments left, want 0 and 1", flags, envs)
		}
	}
}

// A setting belongs to one environment, so only that one changes.
func TestUpdateFlagSettingBumpsEnvVersion(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Only the version and updated_at move, and only in the setting's own
	// environment.
	t.Run("its environment goes up by one", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		other := mustCreateEnv(t, store, "staging")
		mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		got := mustGetEnv(t, store, env.ID)
		if !got.UpdatedAt.After(env.UpdatedAt) {
			t.Errorf("updated_at = %v, want it after %v", got.UpdatedAt, env.UpdatedAt)
		}
		want := env
		want.Version++
		want.UpdatedAt = got.UpdatedAt
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("environment mismatch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(other, mustGetEnv(t, store, other.ID)); diff != "" {
			t.Errorf("other environment changed (-want +got):\n%s", diff)
		}
	})

	// A change to either field alone is enough. A new setting starts disabled
	// with a fallthrough of 0.
	t.Run("each change counts", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		changes := []struct {
			enabled  bool
			fraction float32
		}{{true, 0}, {true, 0.5}, {false, 0.5}}
		for i, c := range changes {
			s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: c.enabled, Fallthrough: c.fraction})
			if s.Enabled != c.enabled || s.Fallthrough != c.fraction {
				t.Errorf("enabled, fallthrough = %v, %v, want %v, %v", s.Enabled, s.Fallthrough, c.enabled, c.fraction)
			}
			if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+int64(i)+1; got != want {
				t.Fatalf("after change %d: version = %d, want %d", i, got, want)
			}
		}
	})

	// Nothing changed, so readers have nothing to pick up. 0.1 has no exact
	// binary form, so it also has to survive the trip to the database and back
	// to compare equal.
	t.Run("same values leave the version alone", func(t *testing.T) {
		tests := map[string]database.UpdateFlagSettingParams{
			"the values a new setting starts with": {},
			"values set earlier":                   {Enabled: true, Fallthrough: 0.5},
			"a fraction with no exact binary form": {Enabled: true, Fallthrough: 0.1},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				resetDB(t, store.db)
				arg.FlagID = mustCreateFlag(t, store, "new-checkout").ID
				arg.EnvironmentID = mustCreateEnv(t, store, "production").ID
				if _, err := store.UpdateFlagSetting(ctx, arg); err != nil {
					t.Fatalf("UpdateFlagSetting(): %v", err)
				}
				want := mustGetEnv(t, store, arg.EnvironmentID)
				if _, err := store.UpdateFlagSetting(ctx, arg); err != nil {
					t.Fatalf("UpdateFlagSetting(): %v", err)
				}
				if diff := cmp.Diff(want, mustGetEnv(t, store, arg.EnvironmentID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// Updates racing each other don't overwrite each other's bump. Every value
	// is different, so each racer has something to change.
	t.Run("racing changes all count", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: float32(i+1) / racers})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlagSetting(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+racers; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Racers writing the same values each compare before they write, so only
	// the first one finds something to change.
	t.Run("racing same values count once", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for range racers {
			go func() {
				_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlagSetting(): %v", err)
			}
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Fatalf("version = %d, want %d", got, want)
		}
	})

	// Nothing changed, so readers have nothing to pick up.
	t.Run("rejected update leaves versions alone", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		tests := map[string]database.UpdateFlagSettingParams{
			"bad fallthrough":     {FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 2},
			"unknown flag":        {FlagID: uuid.New(), EnvironmentID: env.ID},
			"unknown environment": {FlagID: flag.ID, EnvironmentID: uuid.New()},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.UpdateFlagSetting(ctx, arg); err == nil {
					t.Fatal("UpdateFlagSetting() = nil error, want it rejected")
				}
				if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
					t.Fatalf("environment changed (-want +got):\n%s", diff)
				}
			})
		}
	})

	// The update and the bump land together or not at all, and the caller is
	// told which.
	t.Run("failed bump undoes the update", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		want, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
		if err != nil {
			t.Fatalf("GetFlagSetting(): %v", err)
		}
		rejectEnvBumps(t, store)
		got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag setting"))
		if got != (database.FlagSetting{}) {
			t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
		}
		if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
			t.Fatalf("setting changed after a failed bump (-want +got):\n%s", diff)
		}
	})

	// A reader never sees the new values with the old version: while the bump
	// waits for the environment's row, the update hasn't happened yet.
	t.Run("update stays hidden until the bump commits", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		want, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
		if err != nil {
			t.Fatalf("GetFlagSetting(): %v", err)
		}
		tx, err := store.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx(): %v", err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "SELECT id FROM environments WHERE id = $1 FOR UPDATE", env.ID); err != nil {
			t.Fatalf("locking the environment: %v", err)
		}

		done := wantWaits(t, func() error {
			_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
			return err
		})
		if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
			t.Errorf("update visible before the bump (-want +got):\n%s", diff)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("Rollback(): %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("UpdateFlagSetting(): %v", err)
		}
		if got := storedFlagSetting(t, store, want.ID); !got.Enabled || got.Fallthrough != 1 {
			t.Errorf("enabled, fallthrough = %v, %v, want true, 1", got.Enabled, got.Fallthrough)
		}
		if got, want := mustGetEnv(t, store, env.ID).Version, env.Version+1; got != want {
			t.Errorf("version = %d, want %d", got, want)
		}
	})
}

// A changed setting is logged in its environment, so SDKs can pick it up.
func TestUpdateFlagSettingLogsChange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// The entry carries the flag's key, the new values and the version the
	// environment moved to, and nothing else. Other flags and environments
	// are around, so an entry for any of them would show.
	t.Run("one entry for the setting", func(t *testing.T) {
		resetDB(t, store.db)
		mustCreateFlag(t, store, "dark-mode")
		flag := mustCreateFlag(t, store, "new-checkout")
		mustCreateEnv(t, store, "staging")
		env := mustCreateEnv(t, store, "production")
		before := changeLogCount(t, store)
		mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		want := []database.ChangeLog{{
			TargetVersion: mustGetEnv(t, store, env.ID).Version,
			EnvironmentID: env.ID,
			FlagNewKey:    sql.NullString{String: "new-checkout", Valid: true},
			Enabled:       sql.NullBool{Bool: true, Valid: true},
			Fallthrough:   sql.NullFloat64{Float64: 0.5, Valid: true},
			Action:        "setting-updated",
		}}
		ignore := cmpopts.IgnoreFields(database.ChangeLog{}, "ID", "CreatedAt")
		if diff := cmp.Diff(want, storedChangeLogsFor(t, store, "setting-updated"), ignore); diff != "" {
			t.Fatalf("change logs mismatch (-want +got):\n%s", diff)
		}
		if got, want := changeLogCount(t, store), before+1; got != want {
			t.Fatalf("%d entries, want %d", got, want)
		}
	})

	// An SDK replaying the entry ends up with the stored setting. 0.1 has no
	// exact binary form, so the entry has to hold the same float32.
	t.Run("entry matches the stored setting", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		s := mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.1})
		logs := storedChangeLogsFor(t, store, "setting-updated")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		stored := storedFlagSetting(t, store, s.ID)
		if got := float32(logs[0].Fallthrough.Float64); got != stored.Fallthrough {
			t.Errorf("logged fallthrough %v, want %v", got, stored.Fallthrough)
		}
		if logs[0].Enabled.Bool != stored.Enabled {
			t.Errorf("logged enabled %v, want %v", logs[0].Enabled.Bool, stored.Enabled)
		}
	})

	// The key is read when the setting changes, so a renamed flag is logged
	// under its new key.
	t.Run("logs the flag's current key", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		if _, err := store.UpdateFlag(ctx, flag.ID, "checkout-v2"); err != nil {
			t.Fatalf("UpdateFlag(): %v", err)
		}
		mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true})
		logs := storedChangeLogsFor(t, store, "setting-updated")
		if len(logs) != 1 {
			t.Fatalf("%d entries, want 1", len(logs))
		}
		if got := logs[0].FlagNewKey.String; got != "checkout-v2" {
			t.Errorf("logged key %q, want %q", got, "checkout-v2")
		}
	})

	// Each change gets its own entry with its own version, and earlier ones
	// stay. A new setting starts disabled with a fallthrough of 0.
	t.Run("each change counts", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		changes := []struct {
			enabled  bool
			fraction float32
		}{{true, 0}, {true, 0.5}, {false, 0.5}}
		for _, c := range changes {
			mustUpdateFlagSetting(t, store, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: c.enabled, Fallthrough: c.fraction})
		}
		logs := storedChangeLogsFor(t, store, "setting-updated")
		if len(logs) != len(changes) {
			t.Fatalf("%d entries, want %d", len(logs), len(changes))
		}
		for i, c := range changes {
			l := logs[i]
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if l.Enabled.Bool != c.enabled || float32(l.Fallthrough.Float64) != c.fraction {
				t.Errorf("entry %d logged enabled, fallthrough = %v, %v, want %v, %v", i, l.Enabled.Bool, l.Fallthrough.Float64, c.enabled, c.fraction)
			}
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("same values log nothing", func(t *testing.T) {
		resetDB(t, store.db)
		arg := database.UpdateFlagSettingParams{
			FlagID:        mustCreateFlag(t, store, "new-checkout").ID,
			EnvironmentID: mustCreateEnv(t, store, "production").ID,
			Enabled:       true,
			Fallthrough:   0.1,
		}
		mustUpdateFlagSetting(t, store, arg)
		before := changeLogCount(t, store)
		mustUpdateFlagSetting(t, store, arg)
		if got := changeLogCount(t, store); got != before {
			t.Fatalf("%d entries, want %d", got, before)
		}
	})

	// Updates racing each other each log the version they moved to, so no two
	// entries claim the same one. Every value is different, so each racer has
	// something to change.
	t.Run("racing changes log their own versions", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		const racers = 8
		errs := make(chan error, racers)
		for i := range racers {
			go func() {
				_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: float32(i+1) / racers})
				errs <- err
			}()
		}
		for range racers {
			if err := <-errs; err != nil {
				t.Errorf("UpdateFlagSetting(): %v", err)
			}
		}
		logs := storedChangeLogsFor(t, store, "setting-updated")
		if len(logs) != racers {
			t.Fatalf("%d entries, want %d", len(logs), racers)
		}
		fractions := map[float64]bool{}
		for i, l := range logs {
			if want := env.Version + int64(i) + 1; l.TargetVersion != want {
				t.Errorf("entry %d logged version %d, want %d", i, l.TargetVersion, want)
			}
			if fractions[l.Fallthrough.Float64] {
				t.Errorf("fallthrough %v logged twice", l.Fallthrough.Float64)
			}
			fractions[l.Fallthrough.Float64] = true
		}
		// The last entry is what an SDK ends up with, so it has to be what's
		// stored.
		stored, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
		if err != nil {
			t.Fatalf("GetFlagSetting(): %v", err)
		}
		if got := float32(logs[racers-1].Fallthrough.Float64); got != stored.Fallthrough {
			t.Errorf("last entry logged fallthrough %v, want the stored %v", got, stored.Fallthrough)
		}
	})

	// Nothing changed, so there is nothing to log.
	t.Run("rejected update logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		lonely := mustCreateFlag(t, store, "beta-banner")
		mustRemoveFlagSetting(t, store, lonely.ID, env.ID)
		before := changeLogCount(t, store)
		tests := map[string]database.UpdateFlagSettingParams{
			"bad fallthrough":     {FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 2},
			"unknown flag":        {FlagID: uuid.New(), EnvironmentID: env.ID, Enabled: true},
			"unknown environment": {FlagID: flag.ID, EnvironmentID: uuid.New(), Enabled: true},
			"no setting":          {FlagID: lonely.ID, EnvironmentID: env.ID, Enabled: true},
		}
		for name, arg := range tests {
			t.Run(name, func(t *testing.T) {
				if _, err := store.UpdateFlagSetting(ctx, arg); err == nil {
					t.Fatal("UpdateFlagSetting() = nil error, want it rejected")
				}
				if got := changeLogCount(t, store); got != before {
					t.Fatalf("%d entries, want %d", got, before)
				}
			})
		}
	})

	t.Run("failed bump logs nothing", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		rejectEnvBumps(t, store)
		if _, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true}); err == nil {
			t.Fatal("UpdateFlagSetting() = nil error, want it to fail")
		}
		if n := len(storedChangeLogsFor(t, store, "setting-updated")); n != 0 {
			t.Fatalf("%d entries, want 0", n)
		}
	})

	// The update, the bump and the log land together or not at all, so SDKs
	// never miss a change.
	t.Run("failed log undoes the update and the bump", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		want, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
		if err != nil {
			t.Fatalf("GetFlagSetting(): %v", err)
		}
		rejectNewChangeLogs(t, store)
		got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, "can't update the flag setting"))
		if got != (database.FlagSetting{}) {
			t.Fatalf("UpdateFlagSetting() = %+v on error, want the zero FlagSetting", got)
		}
		if diff := cmp.Diff(want, storedFlagSetting(t, store, want.ID)); diff != "" {
			t.Errorf("setting changed after a failed log (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(env, mustGetEnv(t, store, env.ID)); diff != "" {
			t.Errorf("environment changed after a failed log (-want +got):\n%s", diff)
		}
	})
}

// An update racing the delete of its flag or environment either gets in first or
// finds nothing, and is never aborted as a deadlock.
func TestUpdateFlagSettingWhileOwnerIsDeleted(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]func(flag database.Flag, env database.Environment) error{
		"DeleteFlag": func(flag database.Flag, _ database.Environment) error {
			_, err := store.DeleteFlag(ctx, flag.ID)
			return err
		},
		"DeleteEnv": func(_ database.Flag, env database.Environment) error {
			_, err := store.DeleteEnv(ctx, env.ID)
			return err
		},
	}
	for name, del := range tests {
		t.Run(name, func(t *testing.T) {
			for range 20 {
				resetDB(t, store.db)
				flag := mustCreateFlag(t, store, "new-checkout")
				env := mustCreateEnv(t, store, "production")
				mustCreateEnv(t, store, "staging")

				updated, deleted := make(chan error, 1), make(chan error, 1)
				go func() {
					_, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 1})
					updated <- err
				}()
				go func() { deleted <- del(flag, env) }()
				if err := <-deleted; err != nil {
					t.Fatalf("%s(): %v", name, err)
				}
				if err := <-updated; err != nil {
					wantConnectErr(t, err, connect.NewError(connect.CodeNotFound, "can't update the flag setting"))
				}
			}
		})
	}
}
