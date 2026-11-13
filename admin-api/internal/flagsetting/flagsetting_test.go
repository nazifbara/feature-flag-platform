package flagsetting

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/validate"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/testutil"
	flagsettingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1/flagsettingv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// TestMain runs the tests while holding testutil.DBLock.
func TestMain(m *testing.M) { testutil.Main(m) }

// newStore opens a store at dbURL.
func newStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()
	st, err := store.New(dbURL)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	return st
}

// newClient serves a Service backed by st through a real connect server, with
// the validation interceptor the server runs, and returns a client for it.
func newClient(t *testing.T, st *store.Store) flagsettingv1connect.FlagSettingServiceClient {
	t.Helper()
	server := connect.NewServer(validate.NewServerInterceptor())
	flagsettingv1connect.RegisterFlagSettingServiceHandler(server, New(st))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return flagsettingv1connect.NewFlagSettingServiceClient(
		connect.NewClient(connecthttp.NewTransport(ts.Client(), ts.URL)),
	)
}

// fixture is a flag in an environment, with the setting the store made for
// them.
type fixture struct {
	flag    database.Flag
	env     database.Environment
	setting database.FlagSetting
}

// mustSetUp creates a flag keyed key in an environment named name, and gives
// them with their setting, which starts disabled with a fallthrough of 0.
func mustSetUp(t *testing.T, st *store.Store, key, name string) fixture {
	t.Helper()
	ctx := context.Background()
	env, err := st.CreateEnv(ctx, name)
	if err != nil {
		t.Fatalf("CreateEnv(%q): %v", name, err)
	}
	flag, err := st.CreateFlag(ctx, key)
	if err != nil {
		t.Fatalf("CreateFlag(%q): %v", key, err)
	}
	setting, err := st.GetFlagSetting(ctx, database.GetFlagSettingParams{FlagID: flag.ID, EnvironmentID: env.ID})
	if err != nil {
		t.Fatalf("GetFlagSetting(): %v", err)
	}
	return fixture{flag: flag, env: env, setting: setting}
}

// settingRow is a flag setting as stored.
type settingRow struct {
	flagID, envID    string
	enabled          bool
	fallthroughValue float32
	created, updated time.Time
}

// readSetting reads the flag setting with id straight from the table.
func readSetting(t *testing.T, db *sql.DB, id string) settingRow {
	t.Helper()
	var s settingRow
	err := db.QueryRow("SELECT flag_id, environment_id, enabled, fallthrough, created_at, updated_at FROM flag_settings WHERE id = $1", id).
		Scan(&s.flagID, &s.envID, &s.enabled, &s.fallthroughValue, &s.created, &s.updated)
	if err != nil {
		t.Fatalf("reading flag setting %s: %v", id, err)
	}
	return s
}

// envVersion reads the version of the environment with id.
func envVersion(t *testing.T, db *sql.DB, id string) int64 {
	t.Helper()
	var v int64
	if err := db.QueryRow("SELECT version FROM environments WHERE id = $1", id).Scan(&v); err != nil {
		t.Fatalf("reading the version of %s: %v", id, err)
	}
	return v
}

// changeLogCount gives the number of change log entries with action.
func changeLogCount(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM change_logs WHERE action = $1", action).Scan(&n); err != nil {
		t.Fatalf("counting %s entries: %v", action, err)
	}
	return n
}

// mask gives a field mask of paths.
func mask(paths ...string) *fieldmaskpb.FieldMask {
	return &fieldmaskpb.FieldMask{Paths: paths}
}

// mustUpdate gives the flag setting with req's id the values its mask names,
// and returns the response's setting, failing the test if the call fails or the response
// carries none.
func mustUpdate(t *testing.T, client flagsettingv1connect.FlagSettingServiceClient, req *flagsettingv1.UpdateRequest) *flagsettingv1.FlagSetting {
	t.Helper()
	res, err := client.Update(context.Background(), req)
	if err != nil {
		t.Fatalf("Update(%v): %v", req, err)
	}
	if res.GetFlagSetting() == nil {
		t.Fatalf("Update(%v) returned no flag setting", req)
	}
	return res.GetFlagSetting()
}

// mustGet gets the flag setting with id and returns the response's setting,
// failing the test if the call fails or the response carries none.
func mustGet(t *testing.T, client flagsettingv1connect.FlagSettingServiceClient, id string) *flagsettingv1.FlagSetting {
	t.Helper()
	res, err := client.Get(context.Background(), &flagsettingv1.GetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if res.GetFlagSetting() == nil {
		t.Fatalf("Get(%s) returned no flag setting", id)
	}
	return res.GetFlagSetting()
}

func TestGet(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	t.Run("returns the stored setting", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		got := mustGet(t, client, f.setting.ID.String())
		if !proto.Equal(got, settingToProto(f.setting)) {
			t.Fatalf("Get() = %v, want %v", got, settingToProto(f.setting))
		}
		stored := readSetting(t, db, got.Id)
		if stored.flagID != got.FlagId || stored.envID != got.EnvironmentId || stored.enabled != got.Enabled ||
			stored.fallthroughValue != got.Fallthrough || !stored.created.Equal(got.CreatedAt.AsTime()) || !stored.updated.Equal(got.UpdatedAt.AsTime()) {
			t.Errorf("Get() = %v, want the stored %+v", got, stored)
		}
	})

	// Each setting of a flag in two environments, and of two flags in one,
	// has its own values, so a lookup that returns the wrong row is caught.
	t.Run("returns the setting asked for", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		other := mustSetUp(t, st, "dark-mode", "staging")
		var settings []*flagsettingv1.FlagSetting
		for i, pair := range [][2]string{
			{f.flag.ID.String(), f.env.ID.String()},
			{f.flag.ID.String(), other.env.ID.String()},
			{other.flag.ID.String(), f.env.ID.String()},
			{other.flag.ID.String(), other.env.ID.String()},
		} {
			var id string
			if err := db.QueryRow("SELECT id FROM flag_settings WHERE flag_id = $1 AND environment_id = $2", pair[0], pair[1]).Scan(&id); err != nil {
				t.Fatalf("finding the setting of %s in %s: %v", pair[0], pair[1], err)
			}
			settings = append(settings, mustUpdate(t, client, &flagsettingv1.UpdateRequest{
				Id:          id,
				Enabled:     i%2 == 0,
				Fallthrough: float32(i+1) / 4,
				UpdateMask:  mask("enabled", "fallthrough"),
			}))
		}
		for _, want := range settings {
			if got := mustGet(t, client, want.Id); !proto.Equal(got, want) {
				t.Errorf("Get(%s) = %v, want %v", want.Id, got, want)
			}
		}
	})

	// Get gives what Update returned, not a stale copy.
	t.Run("returns the setting as updated", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		updated := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 0.1, UpdateMask: mask("enabled", "fallthrough")})
		if got := mustGet(t, client, f.setting.ID.String()); !proto.Equal(got, updated) {
			t.Fatalf("Get() = %v, want the updated %v", got, updated)
		}
	})

	// An archived flag's settings are frozen, not hidden.
	t.Run("returns a setting of an archived flag", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		if _, err := st.ArchiveFlag(ctx, f.flag.ID); err != nil {
			t.Fatalf("ArchiveFlag(): %v", err)
		}
		if got := mustGet(t, client, f.setting.ID.String()); !proto.Equal(got, settingToProto(f.setting)) {
			t.Fatalf("Get() = %v, want %v", got, settingToProto(f.setting))
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		if got := mustGet(t, client, strings.ToUpper(f.setting.ID.String())); got.Id != f.setting.ID.String() {
			t.Fatalf("id = %s, want %s", got.Id, f.setting.ID)
		}
	})

	notFound := map[string]func(t *testing.T) string{
		"an id no setting has": func(t *testing.T) string {
			return "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		},
		// A flag's id isn't a setting's.
		"a flag's id": func(t *testing.T) string {
			return mustSetUp(t, st, "new-checkout", "production").flag.ID.String()
		},
		// Settings go with their environment.
		"a setting of a deleted environment": func(t *testing.T) string {
			f := mustSetUp(t, st, "new-checkout", "production")
			if _, err := st.DeleteEnv(ctx, f.env.ID); err != nil {
				t.Fatalf("DeleteEnv(): %v", err)
			}
			return f.setting.ID.String()
		},
	}
	for name, id := range notFound {
		t.Run("rejects "+name, func(t *testing.T) {
			testutil.ResetDB(t, db)
			_, err := client.Get(ctx, &flagsettingv1.GetRequest{Id: id(t)})
			testutil.WantCode(t, err, connect.CodeNotFound, "can't get the flag setting")
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Get(ctx, &flagsettingv1.GetRequest{Id: tc.ID})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.Get(ctx, &flagsettingv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		testutil.WantCode(t, err, connect.CodeInternal, "can't get the flag setting")
	})
}

func TestUpdate(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	// enabled, fallthrough and updated_at move; the id, the pair and
	// created_at stay, and the response is what's stored.
	t.Run("returns the updated setting", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		before := testutil.DBNow(t, db)
		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 0.25, UpdateMask: mask("enabled", "fallthrough")})
		after := testutil.DBNow(t, db)

		if got.Id != f.setting.ID.String() {
			t.Errorf("id = %s, want %s", got.Id, f.setting.ID)
		}
		if got.FlagId != f.flag.ID.String() || got.EnvironmentId != f.env.ID.String() {
			t.Errorf("flag, environment = %s, %s, want %s, %s", got.FlagId, got.EnvironmentId, f.flag.ID, f.env.ID)
		}
		if !got.Enabled || got.Fallthrough != 0.25 {
			t.Errorf("enabled, fallthrough = %v, %v, want true, 0.25", got.Enabled, got.Fallthrough)
		}
		if created := got.GetCreatedAt().AsTime(); !created.Equal(f.setting.CreatedAt) {
			t.Errorf("created_at = %v, want %v unchanged", created, f.setting.CreatedAt)
		}
		if updated := got.GetUpdatedAt().AsTime(); updated.Before(before) || updated.After(after) {
			t.Errorf("updated_at = %v, want between %v and %v", updated, before, after)
		}

		stored := readSetting(t, db, got.Id)
		want := settingRow{
			flagID:           got.FlagId,
			envID:            got.EnvironmentId,
			enabled:          got.Enabled,
			fallthroughValue: got.Fallthrough,
			created:          got.CreatedAt.AsTime(),
			updated:          got.UpdatedAt.AsTime(),
		}
		if stored.flagID != want.flagID || stored.envID != want.envID || stored.enabled != want.enabled ||
			stored.fallthroughValue != want.fallthroughValue || !stored.created.Equal(want.created) || !stored.updated.Equal(want.updated) {
			t.Errorf("stored setting = %+v, want the response's %+v", stored, want)
		}
	})

	// Every value in [0, 1] comes back as sent, including ones with no exact
	// binary form, through the proto's float and the table's REAL.
	t.Run("keeps the values as sent", func(t *testing.T) {
		tests := map[string]struct {
			enabled  bool
			fraction float32
		}{
			"disabled, nobody":    {false, 0},
			"enabled, everybody":  {true, 1},
			"not exact in binary": {true, 0.1},
			"smallest above 0":    {true, math.SmallestNonzeroFloat32},
			"largest below 1":     {true, math.Nextafter32(1, 0)},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, db)
				f := mustSetUp(t, st, "new-checkout", "production")
				// Start from the opposite state, so each case is a change.
				mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: !tc.enabled, Fallthrough: 0.5, UpdateMask: mask("enabled", "fallthrough")})
				got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: tc.enabled, Fallthrough: tc.fraction, UpdateMask: mask("enabled", "fallthrough")})
				if got.Enabled != tc.enabled || got.Fallthrough != tc.fraction {
					t.Errorf("response enabled, fallthrough = %v, %v, want %v, %v", got.Enabled, got.Fallthrough, tc.enabled, tc.fraction)
				}
				if s := readSetting(t, db, got.Id); s.enabled != tc.enabled || s.fallthroughValue != tc.fraction {
					t.Errorf("stored enabled, fallthrough = %v, %v, want %v, %v", s.enabled, s.fallthroughValue, tc.enabled, tc.fraction)
				}
			})
		}
	})

	// A value the mask leaves out keeps the one the setting has, and one it
	// names is set even to false or 0.
	t.Run("updates only the values the mask names", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		id := f.setting.ID.String()
		mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: id, Enabled: true, Fallthrough: 0.5, UpdateMask: mask("enabled", "fallthrough")})

		steps := []struct {
			name     string
			req      *flagsettingv1.UpdateRequest
			enabled  bool
			fraction float32
		}{
			{"only fallthrough", &flagsettingv1.UpdateRequest{Id: id, Fallthrough: 0.25, UpdateMask: mask("fallthrough")}, true, 0.25},
			{"only enabled", &flagsettingv1.UpdateRequest{Id: id, Enabled: false, UpdateMask: mask("enabled")}, false, 0.25},
			{"fallthrough of 0", &flagsettingv1.UpdateRequest{Id: id, Fallthrough: 0, UpdateMask: mask("fallthrough")}, false, 0},
			{"enabled again", &flagsettingv1.UpdateRequest{Id: id, Enabled: true, UpdateMask: mask("enabled")}, true, 0},
		}
		for _, step := range steps {
			got := mustUpdate(t, client, step.req)
			if got.Enabled != step.enabled || got.Fallthrough != step.fraction {
				t.Errorf("after %s: enabled, fallthrough = %v, %v, want %v, %v", step.name, got.Enabled, got.Fallthrough, step.enabled, step.fraction)
			}
			if s := readSetting(t, db, id); s.enabled != step.enabled || s.fallthroughValue != step.fraction {
				t.Errorf("after %s: stored enabled, fallthrough = %v, %v, want %v, %v", step.name, s.enabled, s.fallthroughValue, step.enabled, step.fraction)
			}
		}
	})

	// An empty mask has nothing to change: the setting comes back as it is,
	// and SDKs have nothing to pick up.
	t.Run("an empty mask changes nothing", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		first := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 0.5, UpdateMask: mask("enabled", "fallthrough")})
		version := envVersion(t, db, f.env.ID.String())
		logs := changeLogCount(t, db, "setting-updated")

		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), UpdateMask: mask()})
		if !proto.Equal(got, first) {
			t.Errorf("Update() = %v, want %v unchanged", got, first)
		}
		if got := envVersion(t, db, f.env.ID.String()); got != version {
			t.Errorf("version = %d, want %d unchanged", got, version)
		}
		if got := changeLogCount(t, db, "setting-updated"); got != logs {
			t.Errorf("%d setting-updated entries, want %d", got, logs)
		}
	})

	// The values outside the mask aren't stored or checked, so a request can
	// carry anything there.
	t.Run("ignores the values the mask leaves out", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 50, UpdateMask: mask("enabled")})
		if !got.Enabled || got.Fallthrough != 0 {
			t.Errorf("enabled, fallthrough = %v, %v, want true, 0", got.Enabled, got.Fallthrough)
		}
		got = mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: false, Fallthrough: 0.5, UpdateMask: mask("fallthrough")})
		if !got.Enabled || got.Fallthrough != 0.5 {
			t.Errorf("enabled, fallthrough = %v, %v, want true, 0.5", got.Enabled, got.Fallthrough)
		}
	})

	// A path named twice is still one value to set.
	t.Run("accepts a path named twice", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, UpdateMask: mask("enabled", "enabled")})
		if !got.Enabled {
			t.Errorf("enabled = false, want true")
		}
	})

	// SDKs sync by the environment's version, so a change moves it by one
	// and is logged at it.
	t.Run("bumps the environment's version and logs the change", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		other := mustSetUp(t, st, "dark-mode", "staging")
		version, otherVersion := envVersion(t, db, f.env.ID.String()), envVersion(t, db, other.env.ID.String())
		logs := changeLogCount(t, db, "setting-updated")

		mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 1, UpdateMask: mask("enabled", "fallthrough")})
		if got := envVersion(t, db, f.env.ID.String()); got != version+1 {
			t.Errorf("version = %d, want %d", got, version+1)
		}
		if got := envVersion(t, db, other.env.ID.String()); got != otherVersion {
			t.Errorf("other environment's version = %d, want %d unchanged", got, otherVersion)
		}
		if got := changeLogCount(t, db, "setting-updated"); got != logs+1 {
			t.Errorf("%d setting-updated entries, want %d", got, logs+1)
		}
	})

	// Sending the values the setting has isn't a change: the setting comes
	// back as it is, and SDKs have nothing to pick up.
	t.Run("same values change nothing", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		first := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 0.1, UpdateMask: mask("enabled", "fallthrough")})
		version := envVersion(t, db, f.env.ID.String())
		logs := changeLogCount(t, db, "setting-updated")

		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 0.1, UpdateMask: mask("enabled", "fallthrough")})
		if !proto.Equal(got, first) {
			t.Errorf("Update() = %v, want %v unchanged", got, first)
		}
		if got := envVersion(t, db, f.env.ID.String()); got != version {
			t.Errorf("version = %d, want %d unchanged", got, version)
		}
		if got := changeLogCount(t, db, "setting-updated"); got != logs {
			t.Errorf("%d setting-updated entries, want %d", got, logs)
		}
	})

	// The flag's setting in another environment and another flag's setting in
	// this one stay as they were.
	t.Run("updates only the setting asked for", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		otherEnv := mustSetUp(t, st, "dark-mode", "staging")
		var others []database.FlagSetting
		for _, p := range [][2]string{
			{f.flag.ID.String(), otherEnv.env.ID.String()},
			{otherEnv.flag.ID.String(), f.env.ID.String()},
			{otherEnv.flag.ID.String(), otherEnv.env.ID.String()},
		} {
			var s database.FlagSetting
			err := db.QueryRow("SELECT id, enabled, fallthrough, updated_at FROM flag_settings WHERE flag_id = $1 AND environment_id = $2", p[0], p[1]).
				Scan(&s.ID, &s.Enabled, &s.Fallthrough, &s.UpdatedAt)
			if err != nil {
				t.Fatalf("reading the setting of %s in %s: %v", p[0], p[1], err)
			}
			others = append(others, s)
		}

		mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: f.setting.ID.String(), Enabled: true, Fallthrough: 1, UpdateMask: mask("enabled", "fallthrough")})
		for _, want := range others {
			got := readSetting(t, db, want.ID.String())
			if got.enabled != want.Enabled || got.fallthroughValue != want.Fallthrough || !got.updated.Equal(want.UpdatedAt) {
				t.Errorf("setting %s = %+v, want %+v unchanged", want.ID, got, want)
			}
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		got := mustUpdate(t, client, &flagsettingv1.UpdateRequest{Id: strings.ToUpper(f.setting.ID.String()), Enabled: true, UpdateMask: mask("enabled")})
		if got.Id != f.setting.ID.String() {
			t.Fatalf("id = %s, want %s", got.Id, f.setting.ID)
		}
	})

	// Rejected updates leave the setting and its environment as they were.
	outside := "can't update a flag setting with a fallthrough outside [0, 1]"
	rejected := map[string]struct {
		id       string
		fraction float32
		archived bool
		// mask replaces the request's mask of both values, and noMask leaves
		// it out.
		mask    *fieldmaskpb.FieldMask
		noMask  bool
		code    connect.Code
		message string
	}{
		"an id no setting has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			code:    connect.CodeNotFound,
			message: "can't update the flag setting",
		},
		// The proto has no range rule, so the store's check catches these.
		"a negative fallthrough": {
			fraction: -0.1,
			code:     connect.CodeInvalidArgument,
			message:  outside,
		},
		"a fallthrough over 1": {
			fraction: 1.5,
			code:     connect.CodeInvalidArgument,
			message:  outside,
		},
		"a percentage as fallthrough": {
			fraction: 50,
			code:     connect.CodeInvalidArgument,
			message:  outside,
		},
		"a NaN fallthrough": {
			fraction: float32(math.NaN()),
			code:     connect.CodeInvalidArgument,
			message:  outside,
		},
		"an infinite fallthrough": {
			fraction: float32(math.Inf(1)),
			code:     connect.CodeInvalidArgument,
			message:  outside,
		},
		// Archived flags' settings are frozen until they're unarchived.
		"a setting of an archived flag": {
			fraction: 0.5,
			archived: true,
			code:     connect.CodeFailedPrecondition,
			message:  "can't update archived flag",
		},
		// An empty mask is still an update, so it's checked like one.
		"an empty mask for an id no setting has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			mask:    mask(),
			code:    connect.CodeNotFound,
			message: "can't update the flag setting",
		},
		"an empty mask for a setting of an archived flag": {
			archived: true,
			mask:     mask(),
			code:     connect.CodeFailedPrecondition,
			message:  "can't update archived flag",
		},
		// The proto's rules.
		"a request without a mask": {
			noMask:  true,
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: value is required",
		},
		"a path outside the setting's values": {
			mask:    mask("flag_id"),
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: must only contain paths in [enabled, fallthrough]",
		},
		"a valid path with an invalid one": {
			mask:    mask("enabled", "created_at"),
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: must only contain paths in [enabled, fallthrough]",
		},
		"a path in another case": {
			mask:    mask("Enabled"),
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: must only contain paths in [enabled, fallthrough]",
		},
		// The proto's rules take sub-paths of the names they list, so the
		// handler's own check catches these.
		"a sub-path of a value": {
			mask:    mask("enabled.value"),
			code:    connect.CodeInvalidArgument,
			message: `invalid update mask path "enabled.value"`,
		},
		"a valid path with a sub-path": {
			mask:    mask("fallthrough", "fallthrough.value"),
			code:    connect.CodeInvalidArgument,
			message: `invalid update mask path "fallthrough.value"`,
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			testutil.ResetDB(t, db)
			f := mustSetUp(t, st, "new-checkout", "production")
			if tc.archived {
				if _, err := st.ArchiveFlag(ctx, f.flag.ID); err != nil {
					t.Fatalf("ArchiveFlag(): %v", err)
				}
			}
			before := readSetting(t, db, f.setting.ID.String())
			version := envVersion(t, db, f.env.ID.String())
			id := f.setting.ID.String()
			if tc.id != "" {
				id = tc.id
			}
			req := &flagsettingv1.UpdateRequest{Id: id, Enabled: true, Fallthrough: tc.fraction, UpdateMask: mask("enabled", "fallthrough")}
			if tc.mask != nil {
				req.UpdateMask = tc.mask
			}
			if tc.noMask {
				req.UpdateMask = nil
			}
			_, err := client.Update(ctx, req)
			testutil.WantCode(t, err, tc.code, tc.message)
			if after := readSetting(t, db, f.setting.ID.String()); after != before {
				t.Errorf("setting = %+v after a rejected update, want %+v unchanged", after, before)
			}
			if got := envVersion(t, db, f.env.ID.String()); got != version {
				t.Errorf("version = %d, want %d unchanged", got, version)
			}
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Update(ctx, &flagsettingv1.UpdateRequest{Id: tc.ID, Enabled: true, UpdateMask: mask("enabled")})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.Update(ctx, &flagsettingv1.UpdateRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Enabled: true, UpdateMask: mask("enabled")})
		testutil.WantCode(t, err, connect.CodeInternal, "can't update the flag setting")
	})
}

// Without the validation interceptor, the handler's own parse still rejects
// ids that aren't UUIDs, before reaching the store.
func TestHandlersWithoutValidationRejectBadIDs(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	ctx := context.Background()
	calls := map[string]func(id string) error{
		"Get": func(id string) error {
			_, err := svc.Get(ctx, &flagsettingv1.GetRequest{Id: id})
			return err
		},
		"Update": func(id string) error {
			_, err := svc.Update(ctx, &flagsettingv1.UpdateRequest{Id: id, Enabled: true, UpdateMask: mask("enabled")})
			return err
		},
	}
	for rpc, call := range calls {
		for _, id := range testutil.UnparsableIDs {
			t.Run(fmt.Sprintf("%s %q", rpc, id), func(t *testing.T) {
				testutil.WantCode(t, call(id), connect.CodeInvalidArgument, "invalid flag setting id")
			})
		}
	}
}

// Without the validation interceptor, the handler's own check still rejects
// paths that aren't one of the setting's values, before reaching the store.
func TestUpdateWithoutValidationRejectsUnknownPaths(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	ctx := context.Background()
	for _, path := range []string{"", "flag_id", "Enabled", "enabled.value", "*"} {
		t.Run(fmt.Sprintf("%q", path), func(t *testing.T) {
			_, err := svc.Update(ctx, &flagsettingv1.UpdateRequest{
				Id:         "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
				Enabled:    true,
				UpdateMask: mask("enabled", path),
			})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, fmt.Sprintf("invalid update mask path %q", path))
		})
	}
}
