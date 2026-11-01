package store

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/uuid"
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

func mustCreateFlagSetting(t *testing.T, store *Store, arg database.CreateFlagSettingParams) database.FlagSetting {
	t.Helper()
	s, err := store.CreateFlagSetting(context.Background(), arg)
	if err != nil {
		t.Fatalf("CreateFlagSetting(%+v): %v", arg, err)
	}
	return s
}

func TestCreateFlagSetting(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	t.Run("creates the setting", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		before := time.Now()
		got, err := store.CreateFlagSetting(ctx, database.CreateFlagSettingParams{
			FlagID:        flag.ID,
			EnvironmentID: env.ID,
			Enabled:       true,
			Fallthrough:   0.25,
		})
		if err != nil {
			t.Fatalf("CreateFlagSetting(): %v", err)
		}
		if got.ID == uuid.Nil {
			t.Error("CreateFlagSetting() returned a zero ID")
		}
		if got.FlagID != flag.ID || got.EnvironmentID != env.ID {
			t.Errorf("CreateFlagSetting() flag, environment = %v, %v, want %v, %v", got.FlagID, got.EnvironmentID, flag.ID, env.ID)
		}
		if !got.Enabled || got.Fallthrough != 0.25 {
			t.Errorf("CreateFlagSetting() enabled, fallthrough = %v, %v, want true, 0.25", got.Enabled, got.Fallthrough)
		}
		// A new setting has never been updated.
		if !got.UpdatedAt.Equal(got.CreatedAt) {
			t.Errorf("CreateFlagSetting() updated_at = %v, want it equal to created_at %v", got.UpdatedAt, got.CreatedAt)
		}
		// The database and the tests share a clock closely enough for a
		// minute's slack either way.
		if d := got.CreatedAt.Sub(before); d < -time.Minute || d > time.Minute {
			t.Errorf("CreateFlagSetting() created_at = %v, want close to %v", got.CreatedAt, before)
		}
		if diff := cmp.Diff(got, storedFlagSetting(t, store, got.ID)); diff != "" {
			t.Fatalf("stored setting mismatch (-returned +stored):\n%s", diff)
		}
	})

	// Every value in [0, 1] is kept as given, with either enabled state.
	t.Run("stores enabled and fallthrough as given", func(t *testing.T) {
		tests := map[string]struct {
			enabled  bool
			fraction float32
		}{
			"disabled, nobody":           {false, 0},
			"enabled, nobody":            {true, 0},
			"enabled, everybody":         {true, 1},
			"disabled, everybody":        {false, 1},
			"half":                       {true, 0.5},
			"not exact in binary":        {true, 0.1},
			"smallest above 0":           {true, math.SmallestNonzeroFloat32},
			"largest below 1":            {true, math.Nextafter32(1, 0)},
			"many significant digits":    {true, 0.123456789},
			"one in a million":           {true, 0.000001},
			"disabled keeps fallthrough": {false, 0.75},
		}
		for name, test := range tests {
			t.Run(name, func(t *testing.T) {
				resetDB(t, store.db)
				flag := mustCreateFlag(t, store, "new-checkout")
				env := mustCreateEnv(t, store, "production")
				got := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{
					FlagID:        flag.ID,
					EnvironmentID: env.ID,
					Enabled:       test.enabled,
					Fallthrough:   test.fraction,
				})
				stored := storedFlagSetting(t, store, got.ID)
				for _, s := range []database.FlagSetting{got, stored} {
					if s.Enabled != test.enabled || s.Fallthrough != test.fraction {
						t.Errorf("enabled, fallthrough = %v, %v, want %v, %v", s.Enabled, s.Fallthrough, test.enabled, test.fraction)
					}
				}
			})
		}
	})

	// Only the pair is unique: a flag can have a setting in every
	// environment, and an environment a setting for every flag.
	t.Run("one setting per flag and environment pair", func(t *testing.T) {
		resetDB(t, store.db)
		flags := []database.Flag{mustCreateFlag(t, store, "a"), mustCreateFlag(t, store, "b")}
		envs := []database.Environment{mustCreateEnv(t, store, "staging"), mustCreateEnv(t, store, "production")}
		ids := make(map[uuid.UUID]bool)
		for _, f := range flags {
			for _, e := range envs {
				s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID})
				if ids[s.ID] {
					t.Fatalf("CreateFlagSetting() reused ID %v", s.ID)
				}
				ids[s.ID] = true
				if s.FlagID != f.ID || s.EnvironmentID != e.ID {
					t.Errorf("setting for %s in %s has flag, environment = %v, %v", f.Key, e.Name, s.FlagID, s.EnvironmentID)
				}
			}
		}
		if n := flagSettingCount(t, store); n != 4 {
			t.Fatalf("%d flag settings, want 4", n)
		}
	})

	// The clock is read per setting, not per transaction, so settings
	// created together still get increasing times.
	t.Run("settings created in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{mustCreateEnv(t, store, "a"), mustCreateEnv(t, store, "b"), mustCreateEnv(t, store, "c")}
		var settings []database.FlagSetting
		err := store.ExecTx(ctx, func(q *database.Queries) error {
			for _, e := range envs {
				s, err := q.CreateFlagSetting(ctx, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: e.ID})
				if err != nil {
					return err
				}
				settings = append(settings, s)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ExecTx(): %v", err)
		}
		for i := 1; i < len(settings); i++ {
			if !settings[i].CreatedAt.After(settings[i-1].CreatedAt) {
				t.Errorf("setting %d created_at %v not after setting %d created_at %v", i, settings[i].CreatedAt, i-1, settings[i-1].CreatedAt)
			}
			if !settings[i].UpdatedAt.Equal(settings[i].CreatedAt) {
				t.Errorf("setting %d updated_at %v, want it equal to created_at %v", i, settings[i].UpdatedAt, settings[i].CreatedAt)
			}
		}
	})
}

func TestCreateFlagSettingRejectsInvalidFallthrough(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	outside := connect.NewError(connect.CodeInvalidArgument, "can't create a flag setting with a fallthrough outside [0, 1]")

	tests := map[string]float32{
		"negative":          -0.5,
		"just below 0":      -math.SmallestNonzeroFloat32,
		"minus one":         -1,
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
			got, err := store.CreateFlagSetting(ctx, database.CreateFlagSettingParams{
				FlagID:        flag.ID,
				EnvironmentID: env.ID,
				Enabled:       true,
				Fallthrough:   fraction,
			})
			wantConnectErr(t, err, outside)
			if got != (database.FlagSetting{}) {
				t.Fatalf("CreateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			if n := flagSettingCount(t, store); n != 0 {
				t.Fatalf("%d flag settings after a rejected fallthrough, want 0", n)
			}
		})
	}

	// Validation comes first, so a bad fallthrough is reported even when
	// the flag and environment don't exist.
	t.Run("fallthrough checked before the IDs", func(t *testing.T) {
		resetDB(t, store.db)
		_, err := store.CreateFlagSetting(ctx, database.CreateFlagSettingParams{
			FlagID:        uuid.New(),
			EnvironmentID: uuid.New(),
			Fallthrough:   2,
		})
		wantConnectErr(t, err, outside)
	})
}

func TestCreateFlagSettingRejectsDuplicate(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	first := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{
		FlagID:        flag.ID,
		EnvironmentID: env.ID,
		Enabled:       true,
		Fallthrough:   0.25,
	})

	// The pair is taken whatever the other values are.
	got, err := store.CreateFlagSetting(ctx, database.CreateFlagSettingParams{
		FlagID:        flag.ID,
		EnvironmentID: env.ID,
		Enabled:       false,
		Fallthrough:   0.75,
	})
	wantConnectErr(t, err, connect.NewError(connect.CodeAlreadyExists, connect.CodeAlreadyExists.String()))
	if got != (database.FlagSetting{}) {
		t.Fatalf("CreateFlagSetting() = %+v on error, want the zero FlagSetting", got)
	}
	if n := flagSettingCount(t, store); n != 1 {
		t.Fatalf("%d flag settings after a duplicate, want 1", n)
	}
	if diff := cmp.Diff(first, storedFlagSetting(t, store, first.ID)); diff != "" {
		t.Fatalf("first setting changed (-want +got):\n%s", diff)
	}
}

// A setting must point at a flag and an environment that exist.
func TestCreateFlagSettingNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	notFound := connect.NewError(connect.CodeNotFound, connect.CodeNotFound.String())

	resetDB(t, store.db)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")

	tests := map[string]struct{ flagID, envID uuid.UUID }{
		"unknown flag":        {uuid.New(), env.ID},
		"unknown environment": {flag.ID, uuid.New()},
		"both unknown":        {uuid.New(), uuid.New()},
		"zero flag ID":        {uuid.Nil, env.ID},
		"zero environment ID": {flag.ID, uuid.Nil},
		// Easy to do with two arguments of the same type.
		"IDs swapped": {env.ID, flag.ID},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.CreateFlagSetting(ctx, database.CreateFlagSettingParams{
				FlagID:        test.flagID,
				EnvironmentID: test.envID,
			})
			wantConnectErr(t, err, notFound)
			if got != (database.FlagSetting{}) {
				t.Fatalf("CreateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			if n := flagSettingCount(t, store); n != 0 {
				t.Fatalf("%d flag settings after a failed create, want 0", n)
			}
		})
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
		for _, f := range []database.Flag{flag, otherFlag} {
			for _, e := range []database.Environment{env, otherEnv} {
				mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID})
			}
		}
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

func TestCreateFlagSettingReportsErrors(t *testing.T) {
	store := newTestStore(t)
	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	arg := database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5}

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
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.CreateFlagSetting(test.ctx, arg)
			wantConnectErr(t, err, test.want)
			if got != (database.FlagSetting{}) {
				t.Fatalf("CreateFlagSetting() = %+v on error, want the zero FlagSetting", got)
			}
			if n := flagSettingCount(t, store); n != 0 {
				t.Fatalf("%d flag settings after a failed create, want 0", n)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.CreateFlagSetting(context.Background(), arg)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
		if got != (database.FlagSetting{}) {
			t.Fatalf("CreateFlagSetting() = %+v on error, want the zero FlagSetting", got)
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
			created = append(created, mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{
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

	notFound := connect.NewError(connect.CodeNotFound, connect.CodeNotFound.String())
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

	// Both exist, but no setting has been made for this pair yet.
	t.Run("flag without a setting in the environment", func(t *testing.T) {
		flag := mustCreateFlag(t, store, "beta-banner")
		_, err := store.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: envs[0].ID})
		wantConnectErr(t, err, notFound)
	})

	t.Run("deleted environment", func(t *testing.T) {
		env := mustCreateEnv(t, store, "qa")
		mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flags[0].ID, EnvironmentID: env.ID})
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
	mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
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
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
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
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
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
		s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.25})
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
				s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: !test.enabled, Fallthrough: 0.3})
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
				s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID, Fallthrough: 0.5})
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

	// Writing the same values again still counts as an update.
	t.Run("same values still move updated_at", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		got, err := store.UpdateFlagSetting(ctx, database.UpdateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Enabled: true, Fallthrough: 0.5})
		if err != nil {
			t.Fatalf("UpdateFlagSetting(): %v", err)
		}
		if !got.UpdatedAt.After(s.UpdatedAt) {
			t.Errorf("UpdateFlagSetting() updated_at = %v, want it after %v", got.UpdatedAt, s.UpdatedAt)
		}
	})

	// The clock is read per statement, not per transaction, so each update
	// in one transaction gets a later time.
	t.Run("updates in one transaction get increasing times", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		env := mustCreateEnv(t, store, "production")
		mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
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
			s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
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
	notFound := connect.NewError(connect.CodeNotFound, connect.CodeNotFound.String())

	flag := mustCreateFlag(t, store, "new-checkout")
	env := mustCreateEnv(t, store, "production")
	s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
	// Both exist, but have no setting together.
	lonely := mustCreateFlag(t, store, "beta-banner")

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
	s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})

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
	s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID, Fallthrough: 0.5})
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
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
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
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
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
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: staging.ID, Enabled: true, Fallthrough: 1}),
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: prod.ID, Fallthrough: 0.25}),
		}
		if diff := cmp.Diff(want, list(t, flag.ID)); diff != "" {
			t.Fatalf("ListSettingsByFlag() mismatch (-want +got):\n%s", diff)
		}
	})

	// Settings follow the environments' order, not the order the settings
	// were made in.
	t.Run("ordered by environment creation", func(t *testing.T) {
		resetDB(t, store.db)
		flag := mustCreateFlag(t, store, "new-checkout")
		envs := []database.Environment{
			mustCreateEnv(t, store, "development"),
			mustCreateEnv(t, store, "staging"),
			mustCreateEnv(t, store, "production"),
		}
		for _, i := range []int{2, 0, 1} {
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: envs[i].ID})
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
		for _, e := range envs {
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: e.ID})
		}
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
		for _, e := range envs {
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: e.ID})
		}
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
			for _, e := range envs {
				mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: f.ID, EnvironmentID: e.ID})
			}
		}
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
		for _, e := range envs {
			mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: e.ID})
		}
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
		s := mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: other.ID, EnvironmentID: env.ID})
		tests := map[string]uuid.UUID{
			"flag without settings": flag.ID,
			"unknown flag":          uuid.New(),
			"zero ID":               uuid.Nil,
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
	env := mustCreateEnv(t, store, "production")
	mustCreateFlagSetting(t, store, database.CreateFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})

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
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
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
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
		if got != nil {
			t.Fatalf("ListSettingsByFlag() = %+v on error, want nil", got)
		}
	})
}
