package store

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const credID = "0b3e6a52-7f1d-4c2a-9e8b-1a2b3c4d5e6f"

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func influxSink(name string) Sink {
	return Sink{
		Name:         name,
		Type:         SinkInfluxDB,
		URL:          "http://192.0.2.10:8086",
		DB:           "telegraf",
		Username:     "writer",
		Password:     "<change-me>",
		CredentialID: credID,
		Enabled:      true,
	}
}

// drain empties the change channel so the next check sees only new signals.
func drain(s *Store) {
	for {
		select {
		case <-s.Changes():
		default:
			return
		}
	}
}

func changed(s *Store) bool {
	select {
	case <-s.Changes():
		return true
	default:
		return false
	}
}

func problemsOf(t *testing.T, err error) []string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("want *ValidationError, got %T %v", err, err)
	}
	return ve.Problems
}

func hasProblem(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}

func TestOpenSeedsDefaults(t *testing.T) {
	s, _ := openTemp(t)
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Settings != DefaultSettings() {
		t.Errorf("settings %+v, want %+v", snap.Settings, DefaultSettings())
	}
	if snap.Settings.Interval != 10*time.Second || snap.Settings.Seed != 1 || snap.Settings.XIDRate != 0.002 {
		t.Errorf("seeded settings %+v", snap.Settings)
	}
	if len(snap.Sinks) != 0 {
		t.Errorf("sinks %v, want none", snap.Sinks)
	}
	if !reflect.DeepEqual(snap.Topology, topology.Default()) {
		t.Error("seeded topology differs from topology.Default()")
	}
}

func TestReopenKeepsData(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := DefaultSettings()
	st.AdvertiseHost = "192.0.2.5"
	st.OpenStackPassword = "<change-me>"
	st.Interval = 30 * time.Second
	st.DryRun = true
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	created, err := s.CreateSink(influxSink("site-a"))
	if err != nil {
		t.Fatal(err)
	}
	top := topology.Default()
	top.Hypervisors = top.Hypervisors[:1]
	if err := s.SaveTopology(top); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	snap, err := s2.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Settings != st {
		t.Errorf("settings after reopen %+v, want %+v", snap.Settings, st)
	}
	if len(snap.Sinks) != 1 || snap.Sinks[0] != created {
		t.Errorf("sinks after reopen %+v, want [%+v]", snap.Sinks, created)
	}
	if len(snap.Topology.Hypervisors) != 1 || snap.Topology.Hypervisors[0].Hostname != top.Hypervisors[0].Hostname {
		t.Errorf("topology after reopen has %d hypervisors", len(snap.Topology.Hypervisors))
	}
}

func TestFilePermissions(t *testing.T) {
	s, dir := openTemp(t)
	if _, err := s.CreateSink(influxSink("site-a")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("data dir mode %o, want 700", got)
	}
	for _, name := range []string{FileName, FileName + "-wal", FileName + "-shm"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode %o, want 600", name, got)
		}
	}
}

func TestExistingFileIsMadePrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode %o, want 600", got)
	}
}

func TestWALMode(t *testing.T) {
	s, _ := openTemp(t)
	var mode string
	if err := s.db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode %q, want wal", mode)
	}
}

func TestSettingsValidation(t *testing.T) {
	s, _ := openTemp(t)
	bad := Settings{Interval: 500 * time.Millisecond, XIDRate: 1.5, TokenTTL: -time.Second}
	p := problemsOf(t, s.SaveSettings(bad))
	for _, want := range []string{"interval", "xid_rate", "token_ttl"} {
		if !hasProblem(p, want) {
			t.Errorf("problems %q do not mention %s", p, want)
		}
	}
	if len(p) != 3 {
		t.Errorf("got %d problems, want 3: %q", len(p), p)
	}
	snap, _ := s.Snapshot()
	if snap.Settings != DefaultSettings() {
		t.Error("invalid settings were saved")
	}

	ok := DefaultSettings()
	ok.Interval = time.Second
	ok.XIDRate = 1
	if err := s.SaveSettings(ok); err != nil {
		t.Errorf("boundary values rejected: %v", err)
	}
	ok.XIDRate = 0
	if err := s.SaveSettings(ok); err != nil {
		t.Errorf("xid_rate 0 rejected: %v", err)
	}
}

func TestSettingsEmptyPasswordKeepsStored(t *testing.T) {
	s, _ := openTemp(t)
	st := DefaultSettings()
	st.OpenStackPassword = "<change-me>"
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	st.OpenStackPassword = ""
	st.AdvertiseHost = "192.0.2.7"
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	if snap.Settings.OpenStackPassword != "<change-me>" {
		t.Errorf("password %q, want it kept", snap.Settings.OpenStackPassword)
	}
	if snap.Settings.AdvertiseHost != "192.0.2.7" {
		t.Errorf("advertise_host %q", snap.Settings.AdvertiseHost)
	}
	st.OpenStackPassword = "<other>"
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot()
	if snap.Settings.OpenStackPassword != "<other>" {
		t.Errorf("password %q, want replaced", snap.Settings.OpenStackPassword)
	}
}

func TestSinkValidation(t *testing.T) {
	cases := []struct {
		name string
		sink Sink
		want []string
	}{
		{"empty name and type", Sink{}, []string{"name is empty", "type"}},
		{"bad scheme", Sink{Name: "a", Type: SinkInfluxDB, URL: "ftp://192.0.2.1"}, []string{"must start with http"}},
		{"no host", Sink{Name: "a", Type: SinkOTLP, URL: "http://"}, []string{"must start with http"}},
		{"enabled without url", Sink{Name: "a", Type: SinkOTLP, Enabled: true}, []string{"url is empty"}},
		{"unparsable url", Sink{Name: "a", Type: SinkOTLP, URL: "http://192.0.2.1:bad port"}, []string{"url cannot be parsed"}},
		{"userinfo in url", Sink{Name: "a", Type: SinkInfluxDB, URL: "http://admin:" + userinfoSecret + "@192.0.2.1:8086"},
			[]string{"must not contain a user name or password"}},
		{"user only in url", Sink{Name: "a", Type: SinkOTLP, URL: "https://" + userinfoSecret + "@192.0.2.1/v1/metrics"},
			[]string{"must not contain a user name or password"}},
		{"empty userinfo in url", Sink{Name: "a", Type: SinkOTLP, URL: "https://@192.0.2.1/v1/metrics"},
			[]string{"must not contain a user name or password"}},
		{"enabled influx missing db and cred", Sink{Name: "a", Type: SinkInfluxDB, URL: "http://192.0.2.1:8086", Enabled: true},
			[]string{"db is empty", "credential_id is empty"}},
		{"enabled influx bad cred", Sink{Name: "a", Type: SinkInfluxDB, URL: "http://192.0.2.1:8086", DB: "d", CredentialID: "0b3e6a52", Enabled: true},
			[]string{"is not a UUID"}},
		{"enabled otlp with influx fields", Sink{Name: "a", Type: SinkOTLP, URL: "http://192.0.2.1:4318/v1/metrics", DB: "d", CredentialID: credID, Enabled: true},
			[]string{"db is only used", "credential_id is only used"}},
	}
	for _, tc := range cases {
		p := ValidateSink(tc.sink, nil)
		if strings.Contains(strings.Join(p, "\n"), userinfoSecret) {
			t.Errorf("%s: problems repeat the url credentials: %q", tc.name, p)
		}
		if len(p) != len(tc.want) {
			t.Errorf("%s: got %d problems %q, want %d", tc.name, len(p), p, len(tc.want))
		}
		for _, w := range tc.want {
			if !hasProblem(p, w) {
				t.Errorf("%s: problems %q do not mention %q", tc.name, p, w)
			}
		}
	}

	good := []Sink{
		influxSink("a"),
		{Name: "b", Type: SinkOTLP, URL: "https://192.0.2.1:8429/opentelemetry/v1/metrics", Token: "<change-me>", Enabled: true},
		{Name: "c", Type: SinkInfluxDB},
		{Name: "d", Type: SinkOTLP, DB: "left-over", CredentialID: "x"},
	}
	for _, sk := range good {
		if p := ValidateSink(sk, nil); len(p) != 0 {
			t.Errorf("%s: unexpected problems %q", sk.Name, p)
		}
	}
	if p := ValidateSink(influxSink("a"), []string{"b", "a"}); !hasProblem(p, "already used") {
		t.Errorf("duplicate name not reported: %q", p)
	}
}

// userinfoSecret stands for credentials typed into a sink URL. It must not come back in any message.
const userinfoSecret = "userinfo-secret-1"

func TestSinkURLCredentialsRefused(t *testing.T) {
	s, _ := openTemp(t)
	in := influxSink("site-a")
	in.URL = "http://admin:" + userinfoSecret + "@192.0.2.1:8086"
	_, err := s.CreateSink(in)
	var ve *ValidationError
	if !errors.As(err, &ve) || !hasProblem(ve.Problems, "must not contain a user name or password") {
		t.Fatalf("create with url credentials: %v", err)
	}
	if strings.Contains(err.Error(), userinfoSecret) {
		t.Errorf("error repeats the url credentials: %v", err)
	}

	a, err := s.CreateSink(influxSink("site-a"))
	if err != nil {
		t.Fatal(err)
	}
	a.URL = in.URL
	if _, err := s.UpdateSink(a); !errors.As(err, &ve) || strings.Contains(err.Error(), userinfoSecret) {
		t.Errorf("update with url credentials: %v", err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sinks) != 1 || strings.Contains(snap.Sinks[0].URL, "@") {
		t.Errorf("stored sinks %+v", snap.Sinks)
	}
}

func TestSinkCRUD(t *testing.T) {
	s, _ := openTemp(t)
	a, err := s.CreateSink(influxSink("site-a"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == 0 {
		t.Fatal("created sink has id 0")
	}
	p := problemsOf(t, func() error { _, err := s.CreateSink(influxSink("site-a")); return err }())
	if !hasProblem(p, "already used") {
		t.Errorf("duplicate create problems %q", p)
	}
	b, err := s.CreateSink(Sink{Name: "site-b", Type: SinkOTLP})
	if err != nil {
		t.Fatal(err)
	}
	b.Name = "site-a"
	if _, err := s.UpdateSink(b); !hasProblem(problemsOf(t, err), "already used") {
		t.Errorf("rename onto an existing name was not rejected")
	}
	a.DB = "other"
	if _, err := s.UpdateSink(a); err != nil {
		t.Errorf("update keeping its own name failed: %v", err)
	}
	if _, err := s.UpdateSink(Sink{ID: 999, Name: "x", Type: SinkOTLP}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update unknown id: %v, want ErrNotFound", err)
	}
	if err := s.DeleteSink(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSink(a.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
	snap, _ := s.Snapshot()
	if len(snap.Sinks) != 1 || snap.Sinks[0].Name != "site-b" {
		t.Errorf("sinks after delete %+v", snap.Sinks)
	}
}

func TestUpdateSinkSecrets(t *testing.T) {
	s, _ := openTemp(t)
	in := influxSink("site-a")
	in.Token = "<tok>"
	a, err := s.CreateSink(in)
	if err != nil {
		t.Fatal(err)
	}

	// Same URL, empty secrets: keep both.
	u := a
	u.Password, u.Token, u.Username = "", "", "other-user"
	got, err := s.UpdateSink(u)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "<change-me>" || got.Token != "<tok>" || got.Username != "other-user" {
		t.Errorf("same url, empty secrets: %+v", got)
	}

	// Same URL, new password: replace it, keep the token.
	u.Password = "<new>"
	got, err = s.UpdateSink(u)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "<new>" || got.Token != "<tok>" {
		t.Errorf("same url, new password: %+v", got)
	}

	// New URL, empty secrets: drop both.
	u.URL = "http://198.51.100.9:8086"
	u.Password = ""
	got, err = s.UpdateSink(u)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "" || got.Token != "" {
		t.Errorf("new url, empty secrets kept %q / %q", got.Password, got.Token)
	}
	snap, _ := s.Snapshot()
	if snap.Sinks[0].Password != "" || snap.Sinks[0].Token != "" {
		t.Errorf("stored secrets after url change: %+v", snap.Sinks[0])
	}

	// New URL with a new password: use it.
	u.URL = "http://203.0.113.9:8086"
	u.Password = "<third>"
	got, err = s.UpdateSink(u)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != "<third>" {
		t.Errorf("new url, new password: %q", got.Password)
	}
}

func TestChangesSignal(t *testing.T) {
	s, _ := openTemp(t)
	drain(s)

	if err := s.SaveSettings(Settings{Interval: 0}); err == nil {
		t.Fatal("invalid settings accepted")
	}
	if changed(s) {
		t.Error("signal after a rejected save")
	}

	if err := s.SaveSettings(DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if !changed(s) {
		t.Error("no signal after SaveSettings")
	}

	a, err := s.CreateSink(influxSink("site-a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSink(a); err != nil {
		t.Fatal(err)
	}
	if !changed(s) {
		t.Error("no signal after CreateSink/UpdateSink")
	}
	if changed(s) {
		t.Error("buffer holds more than one signal")
	}

	if err := s.DeleteSink(a.ID); err != nil {
		t.Fatal(err)
	}
	if !changed(s) {
		t.Error("no signal after DeleteSink")
	}
	if err := s.DeleteSink(a.ID); err == nil || changed(s) {
		t.Error("signal or no error after deleting a missing sink")
	}

	bad := topology.Default()
	bad.ProjectID = ""
	if err := s.SaveTopology(bad); err == nil || changed(s) {
		t.Error("signal or no error after an invalid topology")
	}
	if err := s.ResetTopology(); err != nil {
		t.Fatal(err)
	}
	if !changed(s) {
		t.Error("no signal after ResetTopology")
	}
}

func TestTopologyValidation(t *testing.T) {
	s, _ := openTemp(t)
	bad := topology.Default()
	bad.ProjectID = ""
	bad.UserName = ""
	p := problemsOf(t, s.SaveTopology(bad))
	if len(p) != 2 || !hasProblem(p, "project_id is empty") || !hasProblem(p, "user_name is empty") {
		t.Errorf("problems %q, want project_id and user_name separately", p)
	}
	if p := problemsOf(t, s.SaveTopology(nil)); len(p) != 1 {
		t.Errorf("nil topology problems %q", p)
	}

	small := topology.Default()
	small.Hypervisors = small.Hypervisors[:2]
	if err := s.SaveTopology(small); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Snapshot()
	if !reflect.DeepEqual(snap.Topology, small) {
		t.Error("stored topology differs from the saved one")
	}
	if err := s.ResetTopology(); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Snapshot()
	if !reflect.DeepEqual(snap.Topology, topology.Default()) {
		t.Error("ResetTopology did not restore topology.Default()")
	}
}
