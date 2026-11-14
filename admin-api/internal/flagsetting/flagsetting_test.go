package flagsetting

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"buf.build/go/protovalidate"
	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/validate"
	"github.com/google/go-cmp/cmp"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/database"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/testutil"
	flagsettingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/ffp/flagsetting/v1/flagsettingv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
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
// ids that aren't UUIDs, before reaching the store, naming what the id
// is for.
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
		"CreateRule": func(id string) error {
			_, err := svc.CreateRule(ctx, &flagsettingv1.CreateRuleRequest{FlagSettingId: id, Name: "rule", Serve: 1, Clauses: []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true)}})
			return err
		},
		"UpdateRule": func(id string) error {
			_, err := svc.UpdateRule(ctx, &flagsettingv1.UpdateRuleRequest{Id: id, Serve: 0.5, UpdateMask: mask("serve")})
			return err
		},
		"ChangeRulePosition": func(id string) error {
			_, err := svc.ChangeRulePosition(ctx, &flagsettingv1.ChangeRulePositionRequest{Id: id})
			return err
		},
	}
	for rpc, call := range calls {
		for _, id := range testutil.UnparsableIDs {
			t.Run(fmt.Sprintf("%s %q", rpc, id), func(t *testing.T) {
				want := "invalid flag setting id"
				if rpc == "UpdateRule" || rpc == "ChangeRulePosition" {
					want = "invalid rule id"
				}
				testutil.WantCode(t, call(id), connect.CodeInvalidArgument, want)
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

// wantJSON fails the test unless got and want hold the same JSON value,
// whatever their spacing and key order.
func wantJSON(t *testing.T, got json.RawMessage, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s isn't JSON: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want %s isn't JSON: %v", want, err)
	}
	if diff := cmp.Diff(w, g); diff != "" {
		t.Fatalf("JSON mismatch (-want +got):\n%s", diff)
	}
}

func stringClause(attribute string, op flagsettingv1.Operator, values ...string) *flagsettingv1.Clause {
	return &flagsettingv1.Clause{Attribute: attribute, Operator: op, Values: &flagsettingv1.Clause_StringValues{StringValues: &flagsettingv1.StringValues{Values: values}}}
}

func numberClause(attribute string, op flagsettingv1.Operator, values ...float64) *flagsettingv1.Clause {
	return &flagsettingv1.Clause{Attribute: attribute, Operator: op, Values: &flagsettingv1.Clause_NumberValues{NumberValues: &flagsettingv1.NumberValues{Values: values}}}
}

func boolClause(attribute string, op flagsettingv1.Operator, values ...bool) *flagsettingv1.Clause {
	return &flagsettingv1.Clause{Attribute: attribute, Operator: op, Values: &flagsettingv1.Clause_BoolValues{BoolValues: &flagsettingv1.BoolValues{Values: values}}}
}

func TestClausesToJSON(t *testing.T) {
	tests := map[string]struct {
		clauses []*flagsettingv1.Clause
		want    string
	}{
		"a string clause": {
			clauses: []*flagsettingv1.Clause{stringClause("country", flagsettingv1.Operator_OPERATOR_IL, "fr", "bj")},
			want:    `[{"attribute": "country", "operator": "il", "kind": "string", "values": ["fr", "bj"]}]`,
		},
		"a number clause": {
			clauses: []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18.5)},
			want:    `[{"attribute": "age", "operator": "gte", "kind": "number", "values": [18.5]}]`,
		},
		"a boolean clause": {
			clauses: []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, false)},
			want:    `[{"attribute": "beta", "operator": "eq", "kind": "boolean", "values": [false]}]`,
		},
		// Clauses keep the request's order.
		"several clauses": {
			clauses: []*flagsettingv1.Clause{
				boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
				stringClause("email", flagsettingv1.Operator_OPERATOR_EDW, "@example.com"),
			},
			want: `[
				{"attribute": "beta", "operator": "eq", "kind": "boolean", "values": [true]},
				{"attribute": "email", "operator": "edw", "kind": "string", "values": ["@example.com"]}
			]`,
		},
		// An empty list, not null, so the store's own check rejects it.
		"no clauses": {clauses: nil, want: `[]`},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := clausesToJSON(tc.clauses)
			if err != nil {
				t.Fatalf("clausesToJSON(): %v", err)
			}
			wantJSON(t, got, tc.want)
		})
	}

	// Every operator the API defines has a stored name, the evaluator's.
	t.Run("maps every operator", func(t *testing.T) {
		want := map[flagsettingv1.Operator]string{
			flagsettingv1.Operator_OPERATOR_EQ: "eq", flagsettingv1.Operator_OPERATOR_NEQ: "neq",
			flagsettingv1.Operator_OPERATOR_GT: "gt", flagsettingv1.Operator_OPERATOR_LT: "lt",
			flagsettingv1.Operator_OPERATOR_GTE: "gte", flagsettingv1.Operator_OPERATOR_LTE: "lte",
			flagsettingv1.Operator_OPERATOR_STW: "stw", flagsettingv1.Operator_OPERATOR_EDW: "edw",
			flagsettingv1.Operator_OPERATOR_CNT: "cnt", flagsettingv1.Operator_OPERATOR_IL: "il",
		}
		for value, name := range flagsettingv1.Operator_name {
			op := flagsettingv1.Operator(value)
			if op == flagsettingv1.Operator_OPERATOR_UNSPECIFIED {
				continue
			}
			if _, ok := want[op]; !ok {
				t.Errorf("operator %s has no expected stored name in this test", name)
			}
		}
		for op, name := range want {
			got, err := clausesToJSON([]*flagsettingv1.Clause{stringClause("a", op, "x")})
			if err != nil {
				t.Errorf("clausesToJSON() with %v: %v", op, err)
				continue
			}
			wantJSON(t, got, fmt.Sprintf(`[{"attribute": "a", "operator": %q, "kind": "string", "values": ["x"]}]`, name))
		}
	})

	// The proto's rules reject these, but a caller without them still gets an
	// error, naming the clause, instead of JSON the store can't read.
	rejected := map[string]struct {
		clauses []*flagsettingv1.Clause
		message string
	}{
		"an unspecified operator": {
			clauses: []*flagsettingv1.Clause{stringClause("a", flagsettingv1.Operator_OPERATOR_UNSPECIFIED, "x")},
			message: "clause at index 0: unknown operator OPERATOR_UNSPECIFIED",
		},
		"an operator the API doesn't define": {
			clauses: []*flagsettingv1.Clause{stringClause("a", flagsettingv1.Operator(99), "x")},
			message: "clause at index 0: unknown operator 99",
		},
		"no values": {
			clauses: []*flagsettingv1.Clause{{Attribute: "a", Operator: flagsettingv1.Operator_OPERATOR_EQ}},
			message: "clause at index 0: no values",
		},
		"a nil clause": {
			clauses: []*flagsettingv1.Clause{nil},
			message: "clause at index 0: unknown operator OPERATOR_UNSPECIFIED",
		},
		"a bad clause after a good one": {
			clauses: []*flagsettingv1.Clause{
				stringClause("a", flagsettingv1.Operator_OPERATOR_EQ, "x"),
				{Attribute: "b", Operator: flagsettingv1.Operator_OPERATOR_EQ},
			},
			message: "clause at index 1: no values",
		},
		// No JSON form, and nothing in the proto's rules catches them.
		"a NaN value": {
			clauses: []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_EQ, math.NaN())},
			message: "clause at index 0: NaN isn't a finite number",
		},
		"an infinite value": {
			clauses: []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_IL, 1, math.Inf(1))},
			message: "clause at index 0: +Inf isn't a finite number",
		},
		"a negative infinite value": {
			clauses: []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_IL, math.Inf(-1))},
			message: "clause at index 0: -Inf isn't a finite number",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			got, err := clausesToJSON(tc.clauses)
			if err == nil {
				t.Fatalf("clausesToJSON() = %s, want an error", got)
			}
			if err.Error() != tc.message {
				t.Errorf("error = %q, want %q", err, tc.message)
			}
			if got != nil {
				t.Errorf("clausesToJSON() = %s on error, want nil", got)
			}
		})
	}
}

// The JSON is what the store reads: it creates rules from it and keeps it as
// given, and the store's own checks still apply to it.
func TestClausesToJSONSuitsTheStore(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	create := func(t *testing.T, clauses []*flagsettingv1.Clause) (database.Rule, error) {
		t.Helper()
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		raw, err := clausesToJSON(clauses)
		if err != nil {
			t.Fatalf("clausesToJSON(): %v", err)
		}
		return st.CreateRule(ctx, database.CreateRuleParams{FlagSettingID: f.setting.ID, Name: "rule", Serve: 1, Clauses: raw})
	}

	accepted := map[string][]*flagsettingv1.Clause{
		"a string clause":  {stringClause("country", flagsettingv1.Operator_OPERATOR_IL, "fr", "bj")},
		"a number clause":  {numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18.5)},
		"a boolean clause": {boolClause("beta", flagsettingv1.Operator_OPERATOR_NEQ, false)},
		"one of each": {
			stringClause("email", flagsettingv1.Operator_OPERATOR_STW, "admin@"),
			numberClause("age", flagsettingv1.Operator_OPERATOR_IL, 18, 21),
			boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
		},
	}
	for name, clauses := range accepted {
		t.Run("accepts "+name, func(t *testing.T) {
			rule, err := create(t, clauses)
			if err != nil {
				t.Fatalf("CreateRule(): %v", err)
			}
			raw, _ := clausesToJSON(clauses)
			wantJSON(t, rule.Clauses, string(raw))
		})
	}

	// What makes a clause valid beyond its shape is the store's to check.
	rejected := map[string][]*flagsettingv1.Clause{
		"no clauses":                    nil,
		"an empty attribute":            {stringClause("", flagsettingv1.Operator_OPERATOR_EQ, "x")},
		"set but empty values":          {stringClause("country", flagsettingv1.Operator_OPERATOR_IL)},
		"an operator the kind forbids":  {boolClause("beta", flagsettingv1.Operator_OPERATOR_GT, true)},
		"eq with several values":        {stringClause("country", flagsettingv1.Operator_OPERATOR_EQ, "fr", "bj")},
		"a string operator on a number": {numberClause("age", flagsettingv1.Operator_OPERATOR_STW, 1)},
	}
	for name, clauses := range rejected {
		t.Run("store rejects "+name, func(t *testing.T) {
			_, err := create(t, clauses)
			testutil.WantCode(t, err, connect.CodeInvalidArgument, "")
		})
	}
}

func TestClausesFromJSON(t *testing.T) {
	// Whatever clausesToJSON writes comes back as it was sent, in order.
	roundTrips := map[string][]*flagsettingv1.Clause{
		"a string clause":  {stringClause("country", flagsettingv1.Operator_OPERATOR_IL, "fr", "bj")},
		"a number clause":  {numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18.5)},
		"a boolean clause": {boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, false)},
		// Numbers JSON keeps exactly, including ones far from 1.
		"numbers at the edges": {numberClause("n", flagsettingv1.Operator_OPERATOR_IL, 0, -1, 0.1, math.MaxFloat64, math.SmallestNonzeroFloat64)},
		"several clauses": {
			boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
			stringClause("email", flagsettingv1.Operator_OPERATOR_EDW, "@example.com"),
			numberClause("age", flagsettingv1.Operator_OPERATOR_LT, 65),
		},
		"no clauses": {},
	}
	for name, want := range roundTrips {
		t.Run("round-trips "+name, func(t *testing.T) {
			raw, err := clausesToJSON(want)
			if err != nil {
				t.Fatalf("clausesToJSON(): %v", err)
			}
			got, err := clausesFromJSON(raw)
			if err != nil {
				t.Fatalf("clausesFromJSON(%s): %v", raw, err)
			}
			if len(got) != len(want) {
				t.Fatalf("clausesFromJSON(%s) = %d clauses, want %d", raw, len(got), len(want))
			}
			for i := range want {
				if !proto.Equal(got[i], want[i]) {
					t.Errorf("clause %d = %v, want %v", i, got[i], want[i])
				}
			}
		})
	}

	// Each stored operator name comes back as its API operator.
	t.Run("maps every stored operator", func(t *testing.T) {
		for op, name := range storedOperators {
			got, err := clausesFromJSON(json.RawMessage(fmt.Sprintf(`[{"attribute": "a", "operator": %q, "kind": "string", "values": ["x"]}]`, name)))
			if err != nil {
				t.Errorf("clausesFromJSON() with %q: %v", name, err)
				continue
			}
			if got[0].GetOperator() != op {
				t.Errorf("operator %q = %v, want %v", name, got[0].GetOperator(), op)
			}
		}
	})

	// The column is jsonb, which doesn't keep spacing or key order, and
	// integers have no decimal point.
	t.Run("reads the column's own formatting", func(t *testing.T) {
		got, err := clausesFromJSON(json.RawMessage(`[{"kind":"number","values":[18,21],"operator":"il","attribute":"age"}]`))
		if err != nil {
			t.Fatalf("clausesFromJSON(): %v", err)
		}
		want := numberClause("age", flagsettingv1.Operator_OPERATOR_IL, 18, 21)
		if len(got) != 1 || !proto.Equal(got[0], want) {
			t.Fatalf("clausesFromJSON() = %v, want [%v]", got, want)
		}
	})

	// The store only saves clauses it can parse, so these are corrupt data:
	// each is an error naming what's wrong, never a half-read clause.
	corrupt := map[string]struct {
		raw     string
		message string
	}{
		"not JSON": {
			raw:     `[{"attribute":`,
			message: "invalid clauses: unexpected end of JSON input",
		},
		"not a list": {
			raw:     `{"attribute": "a"}`,
			message: "invalid clauses: json: cannot unmarshal object into Go value of type []store.StoredClause",
		},
		"an unknown operator": {
			raw:     `[{"attribute": "a", "operator": "like", "kind": "string", "values": ["x"]}]`,
			message: `clause at index 0: unknown operator "like"`,
		},
		"an operator in another case": {
			raw:     `[{"attribute": "a", "operator": "EQ", "kind": "string", "values": ["x"]}]`,
			message: `clause at index 0: unknown operator "EQ"`,
		},
		"an unknown kind": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "date", "values": ["x"]}]`,
			message: `clause at index 0: unknown kind "date"`,
		},
		"no values": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "string", "values": []}]`,
			message: "clause at index 0: no values",
		},
		"null values": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "string", "values": null}]`,
			message: "clause at index 0: no values",
		},
		"a number in a string clause": {
			raw:     `[{"attribute": "a", "operator": "il", "kind": "string", "values": ["x", 1]}]`,
			message: "clause at index 0: value at index 1 is float64, want string",
		},
		"a string in a number clause": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "number", "values": ["1"]}]`,
			message: "clause at index 0: value at index 0 is string, want float64",
		},
		"a null in a boolean clause": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "boolean", "values": [null]}]`,
			message: "clause at index 0: value at index 0 is <nil>, want bool",
		},
		"a bad clause after a good one": {
			raw:     `[{"attribute": "a", "operator": "eq", "kind": "string", "values": ["x"]}, {"attribute": "b", "operator": "eq", "kind": "date", "values": ["x"]}]`,
			message: `clause at index 1: unknown kind "date"`,
		},
	}
	for name, tc := range corrupt {
		t.Run("rejects "+name, func(t *testing.T) {
			got, err := clausesFromJSON(json.RawMessage(tc.raw))
			if err == nil {
				t.Fatalf("clausesFromJSON() = %v, want an error", got)
			}
			if err.Error() != tc.message {
				t.Errorf("error = %q, want %q", err, tc.message)
			}
			if got != nil {
				t.Errorf("clausesFromJSON() = %v on error, want nil", got)
			}
		})
	}
}

// Clauses read back from a rule the store saved are the ones sent.
func TestClausesFromJSONReadsStoredRules(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	f := mustSetUp(t, st, "new-checkout", "production")
	want := []*flagsettingv1.Clause{
		stringClause("email", flagsettingv1.Operator_OPERATOR_STW, "admin@"),
		numberClause("age", flagsettingv1.Operator_OPERATOR_IL, 18, 21.5),
		boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
	}
	raw, err := clausesToJSON(want)
	if err != nil {
		t.Fatalf("clausesToJSON(): %v", err)
	}
	rule, err := st.CreateRule(context.Background(), database.CreateRuleParams{FlagSettingID: f.setting.ID, Name: "rule", Serve: 1, Clauses: raw})
	if err != nil {
		t.Fatalf("CreateRule(): %v", err)
	}
	// Read straight from the table, as jsonb gives it back.
	var stored []byte
	if err := db.QueryRow("SELECT clauses FROM rules WHERE id = $1", rule.ID).Scan(&stored); err != nil {
		t.Fatalf("reading the rule's clauses: %v", err)
	}
	got, err := clausesFromJSON(stored)
	if err != nil {
		t.Fatalf("clausesFromJSON(%s): %v", stored, err)
	}
	if len(got) != len(want) {
		t.Fatalf("clausesFromJSON(%s) = %d clauses, want %d", stored, len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("clause %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestRuleToProto(t *testing.T) {
	created := time.Date(2026, 3, 4, 5, 6, 7, 8000, time.UTC)
	updated := created.Add(time.Hour)
	clauses := []*flagsettingv1.Clause{
		stringClause("email", flagsettingv1.Operator_OPERATOR_EDW, "@example.com"),
		numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18),
	}
	raw, err := clausesToJSON(clauses)
	if err != nil {
		t.Fatalf("clausesToJSON(): %v", err)
	}
	stored := database.Rule{
		ID:            uuid.New(),
		FlagSettingID: uuid.New(),
		Position:      3,
		Name:          "adults",
		Serve:         0.25,
		Clauses:       raw,
		CreatedAt:     created,
		UpdatedAt:     updated,
	}

	t.Run("copies every field", func(t *testing.T) {
		got, err := ruleToProto(stored)
		if err != nil {
			t.Fatalf("ruleToProto(): %v", err)
		}
		want := &flagsettingv1.Rule{
			Id:            stored.ID.String(),
			FlagSettingId: stored.FlagSettingID.String(),
			Position:      3,
			Name:          "adults",
			Serve:         0.25,
			Clauses:       clauses,
			CreatedAt:     timestamppb.New(created),
			UpdatedAt:     timestamppb.New(updated),
		}
		if !proto.Equal(got, want) {
			t.Fatalf("ruleToProto() = %v, want %v", got, want)
		}
	})

	t.Run("widens serve without changing it", func(t *testing.T) {
		for _, serve := range []float32{0, 0.1, 0.5, 1, math.SmallestNonzeroFloat32} {
			rule := stored
			rule.Serve = serve
			got, err := ruleToProto(rule)
			if err != nil {
				t.Fatalf("ruleToProto(serve %v): %v", serve, err)
			}
			if float32(got.Serve) != serve {
				t.Errorf("ruleToProto(serve %v).Serve = %v, which isn't %v as a float32", serve, got.Serve, serve)
			}
		}
	})

	t.Run("gives no clauses as none", func(t *testing.T) {
		rule := stored
		rule.Clauses = json.RawMessage(`[]`)
		got, err := ruleToProto(rule)
		if err != nil {
			t.Fatalf("ruleToProto(): %v", err)
		}
		if len(got.Clauses) != 0 {
			t.Fatalf("ruleToProto().Clauses = %v, want none", got.Clauses)
		}
	})

	t.Run("rejects corrupt clauses, naming the rule", func(t *testing.T) {
		for _, clauses := range []string{
			``,
			`{}`,
			`[{"attribute":"age","operator":"eq","kind":"number","values":["18"]}]`,
		} {
			rule := stored
			rule.Clauses = json.RawMessage(clauses)
			_, wantErr := clausesFromJSON(rule.Clauses)
			if wantErr == nil {
				t.Fatalf("clausesFromJSON(%s) took corrupt clauses", clauses)
			}
			got, err := ruleToProto(rule)
			if err == nil {
				t.Fatalf("ruleToProto(clauses %s) = %v, want an error", clauses, got)
			}
			if got != nil {
				t.Errorf("ruleToProto(clauses %s) = %v with its error, want nil", clauses, got)
			}
			if want := fmt.Sprintf("rule %s: %v", rule.ID, wantErr); err.Error() != want {
				t.Errorf("ruleToProto(clauses %s) error = %q, want %q", clauses, err, want)
			}
		}
	})
}

func TestRuleToProtoReadsStoredRules(t *testing.T) {
	testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	f := mustSetUp(t, st, "new-checkout", "production")
	clauses := []*flagsettingv1.Clause{
		stringClause("country", flagsettingv1.Operator_OPERATOR_IL, "FR", "BJ"),
		boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
	}
	raw, err := clausesToJSON(clauses)
	if err != nil {
		t.Fatalf("clausesToJSON(): %v", err)
	}
	rule, err := st.CreateRule(context.Background(), database.CreateRuleParams{FlagSettingID: f.setting.ID, Name: "rule", Serve: 0.3, Clauses: raw})
	if err != nil {
		t.Fatalf("CreateRule(): %v", err)
	}
	got, err := ruleToProto(rule)
	if err != nil {
		t.Fatalf("ruleToProto(%v): %v", rule, err)
	}
	want := &flagsettingv1.Rule{
		Id:            rule.ID.String(),
		FlagSettingId: f.setting.ID.String(),
		Position:      rule.Position,
		Name:          "rule",
		Serve:         float64(float32(0.3)),
		Clauses:       clauses,
		CreatedAt:     timestamppb.New(rule.CreatedAt),
		UpdatedAt:     timestamppb.New(rule.UpdatedAt),
	}
	if !proto.Equal(got, want) {
		t.Fatalf("ruleToProto() = %v, want %v", got, want)
	}
}

// ruleRow is a rule as stored.
type ruleRow struct {
	settingID, name  string
	position         int32
	serve            float32
	clauses          []byte
	created, updated time.Time
}

// readRule reads the rule with id straight from the table.
func readRule(t *testing.T, db *sql.DB, id string) ruleRow {
	t.Helper()
	var r ruleRow
	err := db.QueryRow("SELECT flag_setting_id, name, position, serve, clauses, created_at, updated_at FROM rules WHERE id = $1", id).
		Scan(&r.settingID, &r.name, &r.position, &r.serve, &r.clauses, &r.created, &r.updated)
	if err != nil {
		t.Fatalf("reading rule %s: %v", id, err)
	}
	return r
}

// ruleCount gives the number of rules stored.
func ruleCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM rules").Scan(&n); err != nil {
		t.Fatalf("counting rules: %v", err)
	}
	return n
}

// mustCreateRule creates the rule req describes and returns the response's
// rule, failing the test if the call fails or the response carries none.
func mustCreateRule(t *testing.T, client flagsettingv1connect.FlagSettingServiceClient, req *flagsettingv1.CreateRuleRequest) *flagsettingv1.Rule {
	t.Helper()
	res, err := client.CreateRule(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateRule(%v): %v", req, err)
	}
	if res.GetRule() == nil {
		t.Fatalf("CreateRule(%v) returned no rule", req)
	}
	return res.GetRule()
}

// betaClauses are clauses the store takes, for tests about something else.
func betaClauses() []*flagsettingv1.Clause {
	return []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true)}
}

func TestCreateRule(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	// The response is what's stored, with every value the request sent.
	t.Run("returns the created rule", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		clauses := []*flagsettingv1.Clause{
			stringClause("email", flagsettingv1.Operator_OPERATOR_EDW, "@example.com"),
			numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18),
			boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true),
		}
		before := testutil.DBNow(t, db)
		got := mustCreateRule(t, client, &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Position: 0, Name: "Adult staff", Serve: 0.25, Clauses: clauses})
		after := testutil.DBNow(t, db)

		if _, err := uuid.Parse(got.Id); err != nil {
			t.Errorf("id = %q, want a UUID: %v", got.Id, err)
		}
		if got.FlagSettingId != f.setting.ID.String() {
			t.Errorf("flag_setting_id = %s, want %s", got.FlagSettingId, f.setting.ID)
		}
		if got.Position != 0 || got.Name != "Adult staff" || got.Serve != 0.25 {
			t.Errorf("position, name, serve = %d, %q, %v, want 0, %q, 0.25", got.Position, got.Name, got.Serve, "Adult staff")
		}
		if len(got.Clauses) != len(clauses) {
			t.Fatalf("%d clauses, want %d", len(got.Clauses), len(clauses))
		}
		for i := range clauses {
			if !proto.Equal(got.Clauses[i], clauses[i]) {
				t.Errorf("clause %d = %v, want %v", i, got.Clauses[i], clauses[i])
			}
		}
		created, updated := got.GetCreatedAt().AsTime(), got.GetUpdatedAt().AsTime()
		if created.Before(before) || created.After(after) {
			t.Errorf("created_at = %v, want between %v and %v", created, before, after)
		}
		if !updated.Equal(created) {
			t.Errorf("updated_at = %v, want created_at %v", updated, created)
		}

		stored := readRule(t, db, got.Id)
		if stored.settingID != got.FlagSettingId || stored.name != got.Name || stored.position != got.Position ||
			float64(stored.serve) != got.Serve || !stored.created.Equal(created) || !stored.updated.Equal(updated) {
			t.Errorf("stored rule = %+v, want the response's %v", stored, got)
		}
		storedClauses, err := clausesFromJSON(stored.clauses)
		if err != nil {
			t.Fatalf("clausesFromJSON(%s): %v", stored.clauses, err)
		}
		for i := range clauses {
			if !proto.Equal(storedClauses[i], clauses[i]) {
				t.Errorf("stored clause %d = %v, want %v", i, storedClauses[i], clauses[i])
			}
		}
	})

	// SDKs pick the new rule up from the environment's version and its log.
	t.Run("bumps the version and logs the rule", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		version := envVersion(t, db, f.env.ID.String())
		logs := changeLogCount(t, db, "rule-created")
		mustCreateRule(t, client, &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Name: "beta", Serve: 1, Clauses: betaClauses()})
		if got := envVersion(t, db, f.env.ID.String()); got != version+1 {
			t.Errorf("version = %d, want %d", got, version+1)
		}
		if got := changeLogCount(t, db, "rule-created"); got != logs+1 {
			t.Errorf("%d rule-created entries, want %d", got, logs+1)
		}
	})

	// The store trims the name, and the response gives it as stored.
	t.Run("trims the name", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		got := mustCreateRule(t, client, &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Name: "  Beta users \t", Serve: 1, Clauses: betaClauses()})
		if got.Name != "Beta users" {
			t.Errorf("name = %q, want %q", got.Name, "Beta users")
		}
		if s := readRule(t, db, got.Id); s.name != "Beta users" {
			t.Errorf("stored name = %q, want %q", s.name, "Beta users")
		}
	})

	// A rule goes at the requested position, and the ones from there on move
	// down one.
	t.Run("puts the rule at the requested position", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		create := func(name string, position int32) *flagsettingv1.Rule {
			t.Helper()
			got := mustCreateRule(t, client, &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Position: position, Name: name, Serve: 1, Clauses: betaClauses()})
			if got.Position != position {
				t.Errorf("rule %s at position %d, want %d", name, got.Position, position)
			}
			return got
		}
		a := create("a", 0)
		c := create("c", 1)
		b := create("b", 1)
		first := create("first", 0)
		want := map[string]int32{first.Id: 0, a.Id: 1, b.Id: 2, c.Id: 3}
		for id, position := range want {
			if got := readRule(t, db, id).position; got != position {
				t.Errorf("rule %s at stored position %d, want %d", id, got, position)
			}
		}
	})

	// Every serve in [0, 1] is stored as sent. The response's double holds the
	// request's float exactly, so it converts back to it.
	t.Run("keeps serve as sent", func(t *testing.T) {
		for _, serve := range []float32{0, 0.1, 0.5, 1, math.SmallestNonzeroFloat32, math.Nextafter32(1, 0)} {
			testutil.ResetDB(t, db)
			f := mustSetUp(t, st, "new-checkout", "production")
			got := mustCreateRule(t, client, &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Name: "rule", Serve: serve, Clauses: betaClauses()})
			if float32(got.Serve) != serve || got.Serve != float64(serve) {
				t.Errorf("serve %v came back as %v", serve, got.Serve)
			}
			if s := readRule(t, db, got.Id); s.serve != serve {
				t.Errorf("serve %v stored as %v", serve, s.serve)
			}
		}
	})

	rejected := map[string]struct {
		archived bool
		edit     func(req *flagsettingv1.CreateRuleRequest)
		code     connect.Code
		message  string
	}{
		// The store's checks.
		"an id no setting has": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.FlagSettingId = "6ba7b810-9dad-11d1-80b4-00c04fd430c8" },
			code:    connect.CodeNotFound,
			message: "can't create the rule",
		},
		"a setting of an archived flag": {
			archived: true,
			code:     connect.CodeFailedPrecondition,
			message:  "can't create rules for archived flag",
		},
		"a position past the rules": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Position = 1 },
			code:    connect.CodeInvalidArgument,
			message: "can't create a rule with a position outside [0, 0]",
		},
		"a name over 200 characters": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Name = strings.Repeat("é", 201) },
			code:    connect.CodeInvalidArgument,
			message: "can't create a rule with a name over 200 characters",
		},
		"no clauses": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Clauses = nil },
			code:    connect.CodeInvalidArgument,
			message: "invalid clauses: at least one clause is required",
		},
		"eq with several values": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{stringClause("country", flagsettingv1.Operator_OPERATOR_EQ, "FR", "BJ")}
			},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 0: eq operator on multi-value clause",
		},
		"an operator the kind doesn't allow": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = append(betaClauses(), boolClause("beta", flagsettingv1.Operator_OPERATOR_GT, true))
			},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 1: unsupported operator for BooleanClause: gt",
		},
		// The handler's own check.
		"a number that isn't finite": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_GT, math.Inf(1))}
			},
			code:    connect.CodeInvalidArgument,
			message: "invalid request clauses",
		},
		// The proto's rules.
		"a negative position": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Position = -1 },
			code:    connect.CodeInvalidArgument,
			message: "validation error: position: must be greater than or equal to 0",
		},
		"an empty name": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Name = "" },
			code:    connect.CodeInvalidArgument,
			message: "validation error: name: must be at least 1 characters",
		},
		"a serve above 1": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Serve = 1.5 },
			code:    connect.CodeInvalidArgument,
			message: "validation error: serve: must be greater than or equal to 0 and less than or equal to 1",
		},
		"the smallest serve above 1": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Serve = math.Nextafter32(1, 2) },
			code:    connect.CodeInvalidArgument,
			message: "validation error: serve: must be greater than or equal to 0 and less than or equal to 1",
		},
		"a negative serve": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Serve = -0.5 },
			code:    connect.CodeInvalidArgument,
			message: "validation error: serve: must be greater than or equal to 0 and less than or equal to 1",
		},
		"a serve that isn't a number": {
			edit:    func(req *flagsettingv1.CreateRuleRequest) { req.Serve = float32(math.NaN()) },
			code:    connect.CodeInvalidArgument,
			message: "validation error: serve: must be greater than or equal to 0 and less than or equal to 1",
		},
		"a clause without an attribute": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{boolClause("", flagsettingv1.Operator_OPERATOR_EQ, true)}
			},
			code:    connect.CodeInvalidArgument,
			message: "validation error: clauses[0].attribute: must be at least 1 characters",
		},
		"a clause without an operator": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_UNSPECIFIED, true)}
			},
			code:    connect.CodeInvalidArgument,
			message: "validation error: clauses[0].operator: must not be in list [0]",
		},
		"a clause with an undefined operator": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator(99), true)}
			},
			code:    connect.CodeInvalidArgument,
			message: "validation error: clauses[0].operator: value must be one of the defined enum values",
		},
		"a clause without values": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{{Attribute: "beta", Operator: flagsettingv1.Operator_OPERATOR_EQ}}
			},
			code:    connect.CodeInvalidArgument,
			message: "validation error: clauses[0].values: exactly one field is required in oneof",
		},
		"a clause with an empty list of values": {
			edit: func(req *flagsettingv1.CreateRuleRequest) {
				req.Clauses = []*flagsettingv1.Clause{stringClause("email", flagsettingv1.Operator_OPERATOR_IL)}
			},
			code:    connect.CodeInvalidArgument,
			message: "validation error: clauses[0].string_values.values: must contain at least 1 item(s)",
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
			version := envVersion(t, db, f.env.ID.String())
			req := &flagsettingv1.CreateRuleRequest{FlagSettingId: f.setting.ID.String(), Name: "rule", Serve: 1, Clauses: betaClauses()}
			if tc.edit != nil {
				tc.edit(req)
			}
			_, err := client.CreateRule(ctx, req)
			testutil.WantCode(t, err, tc.code, tc.message)
			if n := ruleCount(t, db); n != 0 {
				t.Errorf("%d rules after a rejected create, want none", n)
			}
			if got := envVersion(t, db, f.env.ID.String()); got != version {
				t.Errorf("version = %d, want %d unchanged", got, version)
			}
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.CreateRule(ctx, &flagsettingv1.CreateRuleRequest{FlagSettingId: tc.ID, Name: "rule", Serve: 1, Clauses: betaClauses()})
			want := strings.Replace(tc.Message, "validation error: id:", "validation error: flag_setting_id:", 1)
			testutil.WantCode(t, err, connect.CodeInvalidArgument, want)
		})
	}

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.CreateRule(ctx, &flagsettingv1.CreateRuleRequest{FlagSettingId: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Name: "rule", Serve: 1, Clauses: betaClauses()})
		testutil.WantCode(t, err, connect.CodeInternal, "can't create the rule")
	})
}

// Without the validation interceptor, the handler's own check still rejects
// clauses the proto's rules would, before reaching the store. The
// error keeps clausesToJSON's as its cause, for the logs; it isn't sent.
func TestCreateRuleWithoutValidationRejectsBadClauses(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	ctx := context.Background()
	clauses := map[string]*flagsettingv1.Clause{
		"no operator":        boolClause("beta", flagsettingv1.Operator_OPERATOR_UNSPECIFIED, true),
		"undefined operator": boolClause("beta", flagsettingv1.Operator(99), true),
		"no values":          {Attribute: "beta", Operator: flagsettingv1.Operator_OPERATOR_EQ},
	}
	for name, clause := range clauses {
		t.Run(name, func(t *testing.T) {
			_, err := svc.CreateRule(ctx, &flagsettingv1.CreateRuleRequest{
				FlagSettingId: "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
				Name:          "rule",
				Serve:         1,
				Clauses:       []*flagsettingv1.Clause{clause},
			})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, "invalid request clauses")
			_, want := clausesToJSON([]*flagsettingv1.Clause{clause})
			if cause := errors.Unwrap(err); cause == nil || cause.Error() != want.Error() {
				t.Errorf("cause = %v, want %v", cause, want)
			}
		})
	}
}

// mustCreateStoredRule creates a rule named "a" serving 0.25 to the clauses
// betaClauses gives, through the store, in f's setting.
func mustCreateStoredRule(t *testing.T, st *store.Store, f fixture) database.Rule {
	t.Helper()
	raw, err := clausesToJSON(betaClauses())
	if err != nil {
		t.Fatalf("clausesToJSON(): %v", err)
	}
	rule, err := st.CreateRule(context.Background(), database.CreateRuleParams{FlagSettingID: f.setting.ID, Name: "a", Serve: 0.25, Clauses: raw})
	if err != nil {
		t.Fatalf("CreateRule(): %v", err)
	}
	return rule
}

// mustUpdateRule gives the rule with req's id the values its mask names, and
// returns the response's rule, failing the test if the call fails or the
// response carries none.
func mustUpdateRule(t *testing.T, client flagsettingv1connect.FlagSettingServiceClient, req *flagsettingv1.UpdateRuleRequest) *flagsettingv1.Rule {
	t.Helper()
	res, err := client.UpdateRule(context.Background(), req)
	if err != nil {
		t.Fatalf("UpdateRule(%v): %v", req, err)
	}
	if res.GetRule() == nil {
		t.Fatalf("UpdateRule(%v) returned no rule", req)
	}
	return res.GetRule()
}

// wantClauses fails the test unless got holds want's clauses, in order.
func wantClauses(t *testing.T, got, want []*flagsettingv1.Clause) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d clauses, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("clause %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestUpdateRule(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()
	newClauses := []*flagsettingv1.Clause{
		stringClause("country", flagsettingv1.Operator_OPERATOR_IL, "FR", "BJ"),
		numberClause("age", flagsettingv1.Operator_OPERATOR_GTE, 18),
	}

	// The response is what's stored: the new values, the same id, setting,
	// position and created_at, and a later updated_at.
	t.Run("returns the updated rule", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rule := mustCreateStoredRule(t, st, f)
		got := mustUpdateRule(t, client, &flagsettingv1.UpdateRuleRequest{Id: rule.ID.String(), Name: "Everyone in Benin", Serve: 0.75, Clauses: newClauses, UpdateMask: mask("name", "serve", "clauses")})

		if got.Id != rule.ID.String() || got.FlagSettingId != f.setting.ID.String() || got.Position != rule.Position {
			t.Errorf("id, setting, position = %s, %s, %d, want %s, %s, %d", got.Id, got.FlagSettingId, got.Position, rule.ID, f.setting.ID, rule.Position)
		}
		if got.Name != "Everyone in Benin" || got.Serve != 0.75 {
			t.Errorf("name, serve = %q, %v, want %q, 0.75", got.Name, got.Serve, "Everyone in Benin")
		}
		wantClauses(t, got.Clauses, newClauses)
		if created := got.GetCreatedAt().AsTime(); !created.Equal(rule.CreatedAt) {
			t.Errorf("created_at = %v, want %v unchanged", created, rule.CreatedAt)
		}
		if updated := got.GetUpdatedAt().AsTime(); !updated.After(rule.UpdatedAt) {
			t.Errorf("updated_at = %v, want it after %v", updated, rule.UpdatedAt)
		}
		stored := readRule(t, db, got.Id)
		if stored.name != got.Name || float64(stored.serve) != got.Serve || stored.position != got.Position || !stored.updated.Equal(got.UpdatedAt.AsTime()) {
			t.Errorf("stored rule = %+v, want the response's %v", stored, got)
		}
		storedClauses, err := clausesFromJSON(stored.clauses)
		if err != nil {
			t.Fatalf("clausesFromJSON(%s): %v", stored.clauses, err)
		}
		wantClauses(t, storedClauses, newClauses)
	})

	// A value the mask leaves out keeps the one the rule has. Only a new
	// serve or new clauses change how the rule evaluates, so only they bump
	// the version and log the rule; a new name is stored without either.
	t.Run("updates only the values the mask names", func(t *testing.T) {
		tests := map[string]struct {
			paths     []string
			evaluated bool
		}{
			"only the name":    {[]string{"name"}, false},
			"only the serve":   {[]string{"serve"}, true},
			"only the clauses": {[]string{"clauses"}, true},
			"name and serve":   {[]string{"name", "serve"}, true},
			"every value":      {[]string{"name", "serve", "clauses"}, true},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, db)
				f := mustSetUp(t, st, "new-checkout", "production")
				rule := mustCreateStoredRule(t, st, f)
				version := envVersion(t, db, f.env.ID.String())
				logs := changeLogCount(t, db, "rule-updated")

				got := mustUpdateRule(t, client, &flagsettingv1.UpdateRuleRequest{Id: rule.ID.String(), Name: "b", Serve: 0.75, Clauses: newClauses, UpdateMask: mask(tc.paths...)})
				wantName, wantServe, wantClausesOf := "a", 0.25, betaClauses()
				for _, p := range tc.paths {
					switch p {
					case "name":
						wantName = "b"
					case "serve":
						wantServe = 0.75
					case "clauses":
						wantClausesOf = newClauses
					}
				}
				if got.Name != wantName || got.Serve != wantServe {
					t.Errorf("name, serve = %q, %v, want %q, %v", got.Name, got.Serve, wantName, wantServe)
				}
				wantClauses(t, got.Clauses, wantClausesOf)
				if s := readRule(t, db, got.Id); s.name != wantName || float64(s.serve) != wantServe {
					t.Errorf("stored name, serve = %q, %v, want %q, %v", s.name, s.serve, wantName, wantServe)
				}

				wantVersion, wantLogs := version, logs
				if tc.evaluated {
					wantVersion, wantLogs = version+1, logs+1
				}
				if got := envVersion(t, db, f.env.ID.String()); got != wantVersion {
					t.Errorf("version = %d, want %d", got, wantVersion)
				}
				if got := changeLogCount(t, db, "rule-updated"); got != wantLogs {
					t.Errorf("%d rule-updated entries, want %d", got, wantLogs)
				}
			})
		}
	})

	// A name is optional, so an empty one clears it.
	t.Run("an empty name clears it", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rule := mustCreateStoredRule(t, st, f)
		got := mustUpdateRule(t, client, &flagsettingv1.UpdateRuleRequest{Id: rule.ID.String(), Name: "", UpdateMask: mask("name")})
		if got.Name != "" {
			t.Errorf("name = %q, want it cleared", got.Name)
		}
		if s := readRule(t, db, got.Id); s.name != "" {
			t.Errorf("stored name = %q, want it cleared", s.name)
		}
	})

	// An empty mask has nothing to change: the rule comes back as it is, and
	// SDKs have nothing to pick up.
	t.Run("an empty mask changes nothing", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rule := mustCreateStoredRule(t, st, f)
		want, err := ruleToProto(rule)
		if err != nil {
			t.Fatalf("ruleToProto(): %v", err)
		}
		version := envVersion(t, db, f.env.ID.String())
		got := mustUpdateRule(t, client, &flagsettingv1.UpdateRuleRequest{Id: rule.ID.String(), Name: "b", Serve: 0.75, Clauses: newClauses, UpdateMask: mask()})
		if !proto.Equal(got, want) {
			t.Errorf("UpdateRule() = %v, want %v unchanged", got, want)
		}
		if got := envVersion(t, db, f.env.ID.String()); got != version {
			t.Errorf("version = %d, want %d unchanged", got, version)
		}
		if got := changeLogCount(t, db, "rule-updated"); got != 0 {
			t.Errorf("%d rule-updated entries, want none", got)
		}
	})

	// The values outside the mask aren't stored or checked, so a request can
	// carry anything there, even values the proto's rules would reject if the
	// mask named them.
	t.Run("ignores the values the mask leaves out", func(t *testing.T) {
		tests := map[string]*flagsettingv1.UpdateRuleRequest{
			"an empty name":          {Name: "", Serve: 0.75, UpdateMask: mask("serve")},
			"a serve outside [0, 1]": {Name: "b", Serve: 50, UpdateMask: mask("name")},
			"clauses without values": {Name: "b", Clauses: []*flagsettingv1.Clause{{Attribute: "beta", Operator: flagsettingv1.Operator_OPERATOR_EQ}}, UpdateMask: mask("name")},
		}
		for name, req := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, db)
				f := mustSetUp(t, st, "new-checkout", "production")
				rule := mustCreateStoredRule(t, st, f)
				req.Id = rule.ID.String()
				got := mustUpdateRule(t, client, req)
				if got.Serve != 0.25 && req.UpdateMask.Paths[0] != "serve" {
					t.Errorf("serve = %v, want 0.25 unchanged", got.Serve)
				}
				if got.Name != "a" && req.UpdateMask.Paths[0] != "name" {
					t.Errorf("name = %q, want %q unchanged", got.Name, "a")
				}
				wantClauses(t, got.Clauses, betaClauses())
			})
		}
	})

	rejected := map[string]struct {
		id       string
		archived bool
		req      *flagsettingv1.UpdateRuleRequest
		noMask   bool
		code     connect.Code
		message  string
	}{
		// The store's checks.
		"an id no rule has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Serve: 0.75, UpdateMask: mask("serve")},
			code:    connect.CodeNotFound,
			message: "can't update the rule",
		},
		"an empty mask for an id no rule has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", UpdateMask: mask()},
			code:    connect.CodeNotFound,
			message: "can't update the rule",
		},
		"a rule of an archived flag": {
			archived: true,
			req:      &flagsettingv1.UpdateRuleRequest{Name: "b", Serve: 0.75, UpdateMask: mask("serve")},
			code:     connect.CodeFailedPrecondition,
			message:  "can't update rules of archived flag",
		},
		"an empty mask for a rule of an archived flag": {
			archived: true,
			req:      &flagsettingv1.UpdateRuleRequest{Name: "b", UpdateMask: mask()},
			code:     connect.CodeFailedPrecondition,
			message:  "can't update rules of archived flag",
		},
		"a name over 200 characters": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: strings.Repeat("é", 201), UpdateMask: mask("name")},
			code:    connect.CodeInvalidArgument,
			message: "can't update a rule with a name over 200 characters",
		},
		"a serve above 1": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Serve: 1.5, UpdateMask: mask("serve")},
			code:    connect.CodeInvalidArgument,
			message: "can't update a rule with a serve value outside [0, 1]",
		},
		"a serve that isn't a number": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Serve: float32(math.NaN()), UpdateMask: mask("serve")},
			code:    connect.CodeInvalidArgument,
			message: "can't update a rule with a serve value outside [0, 1]",
		},
		"no clauses": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "invalid clauses: at least one clause is required",
		},
		"eq with several values": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{stringClause("country", flagsettingv1.Operator_OPERATOR_EQ, "FR", "BJ")}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 0: eq operator on multi-value clause",
		},
		// The handler's own checks, which also run the clause rules,
		// as the proto leaves them to it.
		"a number that isn't finite": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{numberClause("age", flagsettingv1.Operator_OPERATOR_GT, math.NaN())}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "invalid clauses",
		},
		"a sub-path of a value": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", UpdateMask: mask("name.value")},
			code:    connect.CodeInvalidArgument,
			message: `invalid update mask path "name.value"`,
		},
		"a clause without an operator": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_UNSPECIFIED, true)}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 0: validation error: operator: must not be in list [0]",
		},
		"a clause without an attribute": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{boolClause("", flagsettingv1.Operator_OPERATOR_EQ, true)}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 0: validation error: attribute: must be at least 1 characters",
		},
		"a clause with an empty list of values": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{stringClause("email", flagsettingv1.Operator_OPERATOR_IL)}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 0: validation error: string_values.values: must contain at least 1 item(s)",
		},
		"a bad clause after a good one": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Clauses: []*flagsettingv1.Clause{boolClause("beta", flagsettingv1.Operator_OPERATOR_EQ, true), {Attribute: "beta", Operator: flagsettingv1.Operator_OPERATOR_EQ}}, UpdateMask: mask("clauses")},
			code:    connect.CodeInvalidArgument,
			message: "clause at index 1: validation error: values: exactly one field is required in oneof",
		},
		// The proto's rules.
		"a request without a mask": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", Serve: 0.75},
			noMask:  true,
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: value is required",
		},
		"a path outside the rule's values": {
			req:     &flagsettingv1.UpdateRuleRequest{Name: "b", UpdateMask: mask("position")},
			code:    connect.CodeInvalidArgument,
			message: "validation error: update_mask: must only contain paths in [name, serve, clauses]",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			testutil.ResetDB(t, db)
			f := mustSetUp(t, st, "new-checkout", "production")
			rule := mustCreateStoredRule(t, st, f)
			if tc.archived {
				if _, err := st.ArchiveFlag(ctx, f.flag.ID); err != nil {
					t.Fatalf("ArchiveFlag(): %v", err)
				}
			}
			before := readRule(t, db, rule.ID.String())
			version := envVersion(t, db, f.env.ID.String())
			req := proto.Clone(tc.req).(*flagsettingv1.UpdateRuleRequest)
			req.Id = rule.ID.String()
			if tc.id != "" {
				req.Id = tc.id
			}
			if tc.noMask {
				req.UpdateMask = nil
			}
			_, err := client.UpdateRule(ctx, req)
			testutil.WantCode(t, err, tc.code, tc.message)
			after := readRule(t, db, rule.ID.String())
			if after.name != before.name || after.serve != before.serve || string(after.clauses) != string(before.clauses) || !after.updated.Equal(before.updated) {
				t.Errorf("rule = %+v after a rejected update, want %+v unchanged", after, before)
			}
			if got := envVersion(t, db, f.env.ID.String()); got != version {
				t.Errorf("version = %d, want %d unchanged", got, version)
			}
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.UpdateRule(ctx, &flagsettingv1.UpdateRuleRequest{Id: tc.ID, Name: "b", Serve: 0.5, UpdateMask: mask("serve")})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.UpdateRule(ctx, &flagsettingv1.UpdateRuleRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Name: "b", Serve: 0.5, UpdateMask: mask("serve")})
		testutil.WantCode(t, err, connect.CodeInternal, "can't update the rule")
	})
}

// Without the validation interceptor, the handler's own checks still reject
// paths that aren't one of the rule's values, and clauses the proto's rules
// would, before reaching the store. A clause error keeps protovalidate's as
// its cause.
func TestUpdateRuleWithoutValidation(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	ctx := context.Background()
	const id = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	for _, path := range []string{"", "position", "Name", "serve.value", "*"} {
		t.Run(fmt.Sprintf("path %q", path), func(t *testing.T) {
			_, err := svc.UpdateRule(ctx, &flagsettingv1.UpdateRuleRequest{Id: id, Name: "b", UpdateMask: mask("name", path)})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, fmt.Sprintf("invalid update mask path %q", path))
		})
	}
	clauses := map[string]*flagsettingv1.Clause{
		"no operator":        boolClause("beta", flagsettingv1.Operator_OPERATOR_UNSPECIFIED, true),
		"undefined operator": boolClause("beta", flagsettingv1.Operator(99), true),
		"no values":          {Attribute: "beta", Operator: flagsettingv1.Operator_OPERATOR_EQ},
	}
	for name, clause := range clauses {
		t.Run("clause with "+name, func(t *testing.T) {
			_, err := svc.UpdateRule(ctx, &flagsettingv1.UpdateRuleRequest{Id: id, Clauses: []*flagsettingv1.Clause{clause}, UpdateMask: mask("clauses")})
			want := protovalidate.Validate(clause)
			if want == nil {
				t.Fatalf("protovalidate.Validate(%v) took the clause", clause)
			}
			testutil.WantCode(t, err, connect.CodeInvalidArgument, "clause at index 0: "+want.Error())
			if cause := errors.Unwrap(err); cause == nil || cause.Error() != want.Error() {
				t.Errorf("cause = %v, want %v", cause, want)
			}
		})
	}
}

// mustCreateStoredRules creates a rule for each name, in order, through the
// store, in f's setting, so the first is at position 0.
func mustCreateStoredRules(t *testing.T, st *store.Store, f fixture, names ...string) []database.Rule {
	t.Helper()
	raw, err := clausesToJSON(betaClauses())
	if err != nil {
		t.Fatalf("clausesToJSON(): %v", err)
	}
	rules := make([]database.Rule, len(names))
	for i, name := range names {
		rules[i], err = st.CreateRule(context.Background(), database.CreateRuleParams{FlagSettingID: f.setting.ID, Position: int32(i), Name: name, Serve: 0.25, Clauses: raw})
		if err != nil {
			t.Fatalf("CreateRule(%q): %v", name, err)
		}
	}
	return rules
}

// mustChangeRulePosition moves the rule with id to position and returns the
// response's rule, failing the test if the call fails or the response carries
// none.
func mustChangeRulePosition(t *testing.T, client flagsettingv1connect.FlagSettingServiceClient, id string, position int32) *flagsettingv1.Rule {
	t.Helper()
	res, err := client.ChangeRulePosition(context.Background(), &flagsettingv1.ChangeRulePositionRequest{Id: id, Position: position})
	if err != nil {
		t.Fatalf("ChangeRulePosition(%s, %d): %v", id, position, err)
	}
	if res.GetRule() == nil {
		t.Fatalf("ChangeRulePosition(%s, %d) returned no rule", id, position)
	}
	return res.GetRule()
}

// wantPositions fails the test unless each rule is stored at the position
// want gives for its name.
func wantPositions(t *testing.T, db *sql.DB, rules []database.Rule, want map[string]int32) {
	t.Helper()
	for _, r := range rules {
		if got := readRule(t, db, r.ID.String()).position; got != want[r.Name] {
			t.Errorf("rule %s at position %d, want %d", r.Name, got, want[r.Name])
		}
	}
}

func TestChangeRulePosition(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	// The response is the moved rule as stored: its new position, and its
	// other values as they were.
	t.Run("returns the moved rule", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rules := mustCreateStoredRules(t, st, f, "a", "b", "c")
		got := mustChangeRulePosition(t, client, rules[2].ID.String(), 0)

		want, err := ruleToProto(rules[2])
		if err != nil {
			t.Fatalf("ruleToProto(): %v", err)
		}
		want.Position = 0
		// updated_at is the stored one, checked against the table below.
		want.UpdatedAt = got.UpdatedAt
		if !proto.Equal(got, want) {
			t.Errorf("ChangeRulePosition() = %v, want %v", got, want)
		}
		stored := readRule(t, db, got.Id)
		if stored.position != 0 || !stored.updated.Equal(got.UpdatedAt.AsTime()) {
			t.Errorf("stored position, updated_at = %d, %v, want 0, %v", stored.position, stored.updated, got.UpdatedAt.AsTime())
		}
	})

	// The rules between the old and new positions move by one, toward the
	// gap, so positions stay 0 to n-1.
	t.Run("shifts the rules in between", func(t *testing.T) {
		tests := map[string]struct {
			move     string
			position int32
			want     map[string]int32
		}{
			"last to first":  {"d", 0, map[string]int32{"d": 0, "a": 1, "b": 2, "c": 3}},
			"first to last":  {"a", 3, map[string]int32{"b": 0, "c": 1, "d": 2, "a": 3}},
			"up by one":      {"c", 1, map[string]int32{"a": 0, "c": 1, "b": 2, "d": 3}},
			"down by one":    {"b", 2, map[string]int32{"a": 0, "c": 1, "b": 2, "d": 3}},
			"middle to last": {"b", 3, map[string]int32{"a": 0, "c": 1, "d": 2, "b": 3}},
		}
		for name, tc := range tests {
			t.Run(name, func(t *testing.T) {
				testutil.ResetDB(t, db)
				f := mustSetUp(t, st, "new-checkout", "production")
				rules := mustCreateStoredRules(t, st, f, "a", "b", "c", "d")
				byName := map[string]database.Rule{}
				for _, r := range rules {
					byName[r.Name] = r
				}
				got := mustChangeRulePosition(t, client, byName[tc.move].ID.String(), tc.position)
				if got.Position != tc.position {
					t.Errorf("response position = %d, want %d", got.Position, tc.position)
				}
				wantPositions(t, db, rules, tc.want)
			})
		}
	})

	// SDKs pick the move up from the environment's version and its log.
	t.Run("bumps the version and logs the move", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rules := mustCreateStoredRules(t, st, f, "a", "b")
		version := envVersion(t, db, f.env.ID.String())
		mustChangeRulePosition(t, client, rules[1].ID.String(), 0)
		if got := envVersion(t, db, f.env.ID.String()); got != version+1 {
			t.Errorf("version = %d, want %d", got, version+1)
		}
		if got := changeLogCount(t, db, "rule-position-changed"); got != 1 {
			t.Errorf("%d rule-position-changed entries, want 1", got)
		}
	})

	// A rule already at the position has nowhere to go: it comes back as it
	// is, and SDKs have nothing to pick up.
	t.Run("the current position changes nothing", func(t *testing.T) {
		testutil.ResetDB(t, db)
		f := mustSetUp(t, st, "new-checkout", "production")
		rules := mustCreateStoredRules(t, st, f, "a", "b")
		want, err := ruleToProto(rules[1])
		if err != nil {
			t.Fatalf("ruleToProto(): %v", err)
		}
		version := envVersion(t, db, f.env.ID.String())
		got := mustChangeRulePosition(t, client, rules[1].ID.String(), 1)
		if !proto.Equal(got, want) {
			t.Errorf("ChangeRulePosition() = %v, want %v unchanged", got, want)
		}
		if got := envVersion(t, db, f.env.ID.String()); got != version {
			t.Errorf("version = %d, want %d unchanged", got, version)
		}
		if got := changeLogCount(t, db, "rule-position-changed"); got != 0 {
			t.Errorf("%d rule-position-changed entries, want none", got)
		}
	})

	rejected := map[string]struct {
		id       string
		archived bool
		position int32
		code     connect.Code
		message  string
	}{
		// The store's checks.
		"an id no rule has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			code:    connect.CodeNotFound,
			message: "can't update the rule",
		},
		"a rule of an archived flag": {
			archived: true,
			code:     connect.CodeFailedPrecondition,
			message:  "can't update rules of archived flag",
		},
		"the current position of a rule of an archived flag": {
			archived: true,
			position: 2,
			code:     connect.CodeFailedPrecondition,
			message:  "can't update rules of archived flag",
		},
		"a position past the rules": {
			position: 3,
			code:     connect.CodeInvalidArgument,
			message:  "can't update a rule with a position outside [0, 2]",
		},
		"a position far past the rules": {
			position: math.MaxInt32,
			code:     connect.CodeInvalidArgument,
			message:  "can't update a rule with a position outside [0, 2]",
		},
		// The proto's rules.
		"a negative position": {
			position: -1,
			code:     connect.CodeInvalidArgument,
			message:  "validation error: position: must be greater than or equal to 0",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			testutil.ResetDB(t, db)
			f := mustSetUp(t, st, "new-checkout", "production")
			rules := mustCreateStoredRules(t, st, f, "a", "b", "c")
			if tc.archived {
				if _, err := st.ArchiveFlag(ctx, f.flag.ID); err != nil {
					t.Fatalf("ArchiveFlag(): %v", err)
				}
			}
			version := envVersion(t, db, f.env.ID.String())
			id := rules[2].ID.String()
			if tc.id != "" {
				id = tc.id
			}
			_, err := client.ChangeRulePosition(ctx, &flagsettingv1.ChangeRulePositionRequest{Id: id, Position: tc.position})
			testutil.WantCode(t, err, tc.code, tc.message)
			wantPositions(t, db, rules, map[string]int32{"a": 0, "b": 1, "c": 2})
			if got := envVersion(t, db, f.env.ID.String()); got != version {
				t.Errorf("version = %d, want %d unchanged", got, version)
			}
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.ChangeRulePosition(ctx, &flagsettingv1.ChangeRulePositionRequest{Id: tc.ID})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.ChangeRulePosition(ctx, &flagsettingv1.ChangeRulePositionRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		testutil.WantCode(t, err, connect.CodeInternal, "can't update the rule")
	})
}

// Without the validation interceptor, a negative position still reaches the
// store's own check, which rejects it before using the database.
func TestChangeRulePositionWithoutValidationRejectsNegativePositions(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	_, err := svc.ChangeRulePosition(context.Background(), &flagsettingv1.ChangeRulePositionRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Position: -1})
	testutil.WantCode(t, err, connect.CodeInvalidArgument, "can't update a rule with a negative position")
}
