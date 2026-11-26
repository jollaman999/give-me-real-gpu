package config

import (
	"strings"
	"testing"
	"time"
)

const validJSON = `{
  "listen": ":15000",
  "advertise_host": "192.0.2.10",
  "openstack": {"password": "<change-me>", "token_ttl": "2h"},
  "interval": "15s",
  "seed": 7,
  "xid_rate": 0,
  "sinks": [
    {"name": "site-a", "url": "http://192.0.2.20:8086", "db": "metrics", "credential_id": "", "enabled": false},
    {"name": "site-b", "url": "http://192.0.2.30:8086", "db": "metrics", "credential_id": "0b3e6a52-1c4d-4f7e-9a10-5d2c8e7f6a01", "enabled": true}
  ]
}`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Interval.Std() != 15*time.Second {
		t.Errorf("Interval = %s", c.Interval.Std())
	}
	if c.OpenStack.TokenTTL.Std() != 2*time.Hour {
		t.Errorf("TokenTTL = %s", c.OpenStack.TokenTTL.Std())
	}
	if c.Seed != 7 {
		t.Errorf("Seed = %d", c.Seed)
	}
	if c.XIDRate != 0 {
		t.Errorf("XIDRate = %v although the file sets 0", c.XIDRate)
	}
	if p, err := c.Port(); err != nil || p != 15000 {
		t.Errorf("Port = %d, %v", p, err)
	}
	sinks, idx := c.EnabledSinks()
	if len(sinks) != 1 || sinks[0].Name != "site-b" || idx[0] != 1 {
		t.Errorf("EnabledSinks = %+v %v", sinks, idx)
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]byte(`{"advertise_host":"192.0.2.10","openstack":{"password":"<change-me>"}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Listen != ":15000" || c.Interval.Std() != 10*time.Second || c.Seed != 1 || c.XIDRate != 0.002 {
		t.Errorf("defaults = listen %q interval %s seed %d xid %v", c.Listen, c.Interval.Std(), c.Seed, c.XIDRate)
	}
	if c.OpenStack.TokenTTL != 0 {
		t.Errorf("TokenTTL = %s, want 0", c.OpenStack.TokenTTL.Std())
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("EMU_OS_PASSWORD", "from-env")
	t.Setenv("EMU_ADVERTISE_HOST", "192.0.2.99")
	t.Setenv("EMU_LISTEN", ":16000")
	t.Setenv("EMU_SINK_SITE_A_USERNAME", "writer")
	t.Setenv("EMU_SINK_SITE_A_PASSWORD", "env-secret")
	t.Setenv("EMU_SINK_SITE_A_CREDENTIAL_ID", "AAAAAAAA-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	t.Setenv("EMU_SINK_SITE_A_ENABLED", "true")
	t.Setenv("EMU_SINK_SITE_B_ENABLED", "false")
	t.Setenv("EMU_SINK_SITE_B_USERNAME", "")

	c, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.OpenStack.Password != "from-env" || c.AdvertiseHost != "192.0.2.99" || c.Listen != ":16000" {
		t.Errorf("overrides = %q %q %q", c.OpenStack.Password, c.AdvertiseHost, c.Listen)
	}
	a := c.Sinks[0]
	if a.Username != "writer" || a.Password != "env-secret" || a.CredentialID != "AAAAAAAA-bbbb-4ccc-8ddd-eeeeeeeeeeee" || !a.Enabled {
		t.Errorf("site-a = %+v", a)
	}
	if c.Sinks[1].Enabled {
		t.Errorf("site-b is enabled despite EMU_SINK_SITE_B_ENABLED=false")
	}
	sinks, idx := c.EnabledSinks()
	if len(sinks) != 1 || sinks[0].Name != "site-a" || idx[0] != 0 {
		t.Errorf("EnabledSinks = %+v %v", sinks, idx)
	}
}

func TestEnvEnabledInvalid(t *testing.T) {
	t.Setenv("EMU_SINK_SITE_A_ENABLED", "yes-please")
	_, err := Parse([]byte(validJSON))
	if err == nil || !strings.Contains(err.Error(), "EMU_SINK_SITE_A_ENABLED") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvName(t *testing.T) {
	cases := map[string]string{
		"site-a": "EMU_SINK_SITE_A",
		"lab2":   "EMU_SINK_LAB2",
		"a.b c":  "EMU_SINK_A_B_C",
	}
	for in, want := range cases {
		if got := EnvName(in); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{"listen empty", `{"listen":"","advertise_host":"h","openstack":{"password":"p"}}`, "listen is empty"},
		{"listen without port", `{"listen":"15000","advertise_host":"h","openstack":{"password":"p"}}`, "listen \"15000\""},
		{"advertise missing", `{"openstack":{"password":"p"}}`, "advertise_host"},
		{"password missing", `{"advertise_host":"h"}`, "openstack.password"},
		{"interval too short", `{"advertise_host":"h","openstack":{"password":"p"},"interval":"500ms"}`, "interval 500ms"},
		{"interval as number", `{"advertise_host":"h","openstack":{"password":"p"},"interval":10}`, "must be a string"},
		{"interval without unit", `{"advertise_host":"h","openstack":{"password":"p"},"interval":"10"}`, "missing unit"},
		{"xid_rate out of range", `{"advertise_host":"h","openstack":{"password":"p"},"xid_rate":1.5}`, "xid_rate"},
		{"unknown key", `{"advertise_host":"h","openstack":{"password":"p"},"intervall":"10s"}`, "unknown field"},
		{"sink name missing", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"url":"http://x:1"}]}`, "name is empty"},
		{"sink names collide", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"site-a"},{"name":"SITE_A"}]}`, "collides"},
		{"enabled url missing", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","db":"d","credential_id":"0b3e6a52-1c4d-4f7e-9a10-5d2c8e7f6a01","enabled":true}]}`, "url is empty"},
		{"enabled url scheme", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","url":"192.0.2.20:8086","db":"d","credential_id":"0b3e6a52-1c4d-4f7e-9a10-5d2c8e7f6a01","enabled":true}]}`, "http://host:port"},
		{"enabled db missing", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","url":"http://x:1","credential_id":"0b3e6a52-1c4d-4f7e-9a10-5d2c8e7f6a01","enabled":true}]}`, "db is empty"},
		{"enabled cred missing", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","url":"http://x:1","db":"d","enabled":true}]}`, "credential_id is empty"},
		{"enabled cred not uuid", `{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","url":"http://x:1","db":"d","credential_id":"0b3e6a52","enabled":true}]}`, "is not a UUID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json))
			if err == nil {
				t.Fatalf("no error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDisabledSinkNotValidated(t *testing.T) {
	_, err := Parse([]byte(`{"advertise_host":"h","openstack":{"password":"p"},"sinks":[{"name":"s","enabled":false}]}`))
	if err != nil {
		t.Fatalf("a disabled sink may have empty url, db and credential_id: %v", err)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	b, err := Duration(90 * time.Second).MarshalJSON()
	if err != nil || string(b) != `"1m30s"` {
		t.Fatalf("MarshalJSON = %s, %v", b, err)
	}
	var d Duration
	if err := d.UnmarshalJSON(b); err != nil || d.Std() != 90*time.Second {
		t.Fatalf("UnmarshalJSON = %s, %v", d.Std(), err)
	}
}

func TestExampleConfigParses(t *testing.T) {
	c, err := Load("../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json: %v", err)
	}
	if len(c.Sinks) != 2 {
		t.Fatalf("sinks = %d, want 2", len(c.Sinks))
	}
	if sinks, _ := c.EnabledSinks(); len(sinks) != 0 {
		t.Errorf("all sinks in the example config must be disabled: %+v", sinks)
	}
}
