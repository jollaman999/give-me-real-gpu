// Package config reads and validates the emulator runtime settings (a JSON file plus environment variable overrides).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration written as a string such as "10s" in JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"10s\": %s", b)
	}
	if s == "" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the whole config.json.
type Config struct {
	Listen        string    `json:"listen"`         // address the fake OpenStack API listens on (e.g. :15000)
	AdvertiseHost string    `json:"advertise_host"` // host put into the keystone catalog URLs
	OpenStack     OpenStack `json:"openstack"`
	TopologyFile  string    `json:"topology_file"` // empty means the default 6-node topology
	Interval      Duration  `json:"interval"`      // metric collection interval
	Seed          int64     `json:"seed"`
	XIDRate       float64   `json:"xid_rate"`
	DryRun        bool      `json:"dry_run"` // true logs line counts instead of writing to InfluxDB
	Sinks         []Sink    `json:"sinks"`
}

// OpenStack is the fake keystone account. The user name and project come from the topology.
type OpenStack struct {
	Password string   `json:"password"`
	TokenTTL Duration `json:"token_ttl"` // 0 means 1h
}

// Sink is one InfluxDB to write metrics to.
type Sink struct {
	Name         string `json:"name"`
	URL          string `json:"url"`
	DB           string `json:"db"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	CredentialID string `json:"credential_id"` // id the management platform assigned when the OpenStack credential was registered
	Enabled      bool   `json:"enabled"`
}

// defaults holds the values used when a key is missing from the file. A key that is present wins even if it is 0.
func defaults() Config {
	return Config{
		Listen:   ":15000",
		Interval: Duration(10 * time.Second),
		Seed:     1,
		XIDRate:  0.002,
	}
}

// Load reads the JSON at path, applies environment variable overrides and validates the result.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse does what Load does, on JSON bytes.
func Parse(b []byte) (*Config, error) {
	c := defaults()
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("JSON: %w", err)
	}
	if err := c.applyEnv(os.LookupEnv); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// EnvName builds the environment variable prefix for a sink name. Non-alphanumeric characters become _.
// e.g. "site-a" -> EMU_SINK_SITE_A
func EnvName(sinkName string) string {
	var sb strings.Builder
	sb.WriteString("EMU_SINK_")
	for _, r := range strings.ToUpper(sinkName) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

// applyEnv overrides settings with non-empty environment variables.
func (c *Config) applyEnv(lookup func(string) (string, bool)) error {
	get := func(k string) (string, bool) {
		v, ok := lookup(k)
		return v, ok && v != ""
	}
	if v, ok := get("EMU_OS_PASSWORD"); ok {
		c.OpenStack.Password = v
	}
	if v, ok := get("EMU_ADVERTISE_HOST"); ok {
		c.AdvertiseHost = v
	}
	if v, ok := get("EMU_LISTEN"); ok {
		c.Listen = v
	}
	for i := range c.Sinks {
		s := &c.Sinks[i]
		p := EnvName(s.Name)
		if v, ok := get(p + "_USERNAME"); ok {
			s.Username = v
		}
		if v, ok := get(p + "_PASSWORD"); ok {
			s.Password = v
		}
		if v, ok := get(p + "_CREDENTIAL_ID"); ok {
			s.CredentialID = v
		}
		if v, ok := get(p + "_ENABLED"); ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("%s_ENABLED=%q: must be true or false", p, v)
			}
			s.Enabled = b
		}
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Validate checks the settings and returns every problem found.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Listen == "" {
		add("listen is empty")
	} else if _, err := c.Port(); err != nil {
		add("listen %q: %v", c.Listen, err)
	}
	if c.AdvertiseHost == "" {
		add("advertise_host is empty (the address of this machine that the management platform can reach)")
	}
	if c.OpenStack.Password == "" {
		add("openstack.password is empty (can also be set with EMU_OS_PASSWORD)")
	}
	if c.OpenStack.TokenTTL < 0 {
		add("openstack.token_ttl is negative")
	}
	if c.Interval.Std() < time.Second {
		add("interval %s: must be at least 1s", c.Interval.Std())
	}
	if c.XIDRate < 0 || c.XIDRate > 1 {
		add("xid_rate %v: must be between 0 and 1", c.XIDRate)
	}

	seen := map[string]bool{}
	for i, s := range c.Sinks {
		label := fmt.Sprintf("sinks[%d]", i)
		if s.Name == "" {
			add("%s: name is empty", label)
		} else {
			label = fmt.Sprintf("sinks[%d] %q", i, s.Name)
			env := EnvName(s.Name)
			if seen[env] {
				add("%s: environment variable name %s collides with another sink", label, env)
			}
			seen[env] = true
		}
		if !s.Enabled {
			continue
		}
		if s.URL == "" {
			add("%s: url is empty", label)
		} else if u, err := url.Parse(s.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("%s: url %q must look like http://host:port", label, s.URL)
		}
		if s.DB == "" {
			add("%s: db is empty", label)
		}
		if s.CredentialID == "" {
			add("%s: credential_id is empty (the id returned after registering the credential in the management platform)", label)
		} else if !uuidRe.MatchString(s.CredentialID) {
			add("%s: credential_id %q is not a UUID", label, s.CredentialID)
		}
	}
	return errors.Join(errs...)
}

// Port returns the port number of the listen address.
func (c *Config) Port() (int, error) {
	_, p, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("port %q is not a number in 1-65535", p)
	}
	return n, nil
}

// EnabledSinks returns the enabled sinks and each one's index in the sinks array.
// The index is added to the generator seed. Using the array index keeps a sink's seed stable when other sinks are toggled.
func (c *Config) EnabledSinks() (sinks []Sink, index []int) {
	for i, s := range c.Sinks {
		if s.Enabled {
			sinks = append(sinks, s)
			index = append(index, i)
		}
	}
	return sinks, index
}
