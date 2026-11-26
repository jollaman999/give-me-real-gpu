// Package store keeps the emulator settings, the metric sinks and the topology in a sqlite file.
// The web settings screen writes here and the running emulator reads a Snapshot whenever Changes fires.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// FileName is the database file created inside the data directory.
const FileName = "give-me-real-gpu.db"

// Default values written on the first run.
const (
	DefaultTokenTTL = time.Hour
	DefaultInterval = 10 * time.Second
	DefaultSeed     = 1
	DefaultXIDRate  = 0.002
)

// ErrNotFound is returned when a sink id does not exist.
var ErrNotFound = errors.New("not found")

// Settings are the emulator-wide settings (a single row).
type Settings struct {
	AdvertiseHost     string        // host put into the keystone catalog URLs. Empty until the first setup
	OpenStackPassword string        // stored as plain text, never sent out by the API
	TokenTTL          time.Duration // 0 means 1h
	Interval          time.Duration // metric collection interval, at least 1s
	Seed              int64
	XIDRate           float64 // 0..1
	DryRun            bool
}

// SinkType is the wire format a sink writes.
type SinkType string

const (
	SinkInfluxDB SinkType = "influxdb"
	SinkOTLP     SinkType = "otlp"
)

// Sink is one destination to write metrics to.
type Sink struct {
	ID           uint
	Name         string   // unique, not empty
	Type         SinkType // influxdb or otlp
	URL          string   // influxdb: http(s)://host:port, otlp: the full endpoint URL
	DB           string   // influxdb only
	Username     string   // Basic auth. No auth when both username and password are empty
	Password     string   // stored as plain text, never sent out by the API
	Token        string   // otlp only, Bearer. Stored as plain text, never sent out by the API
	CredentialID string   // influxdb only, a UUID when enabled
	Enabled      bool
}

// Snapshot is everything the emulator needs to run, read at one point in time.
type Snapshot struct {
	Settings Settings
	Sinks    []Sink
	Topology *topology.Topology
}

// ValidationError lists every problem found in the value that was about to be saved. Nothing was saved.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "validation failed: " + strings.Join(e.Problems, "; ")
}

// settingsRow, sinkRow and topologyRow are the gorm tables.
type settingsRow struct {
	ID                uint `gorm:"primaryKey"`
	AdvertiseHost     string
	OpenStackPassword string
	TokenTTLNanos     int64
	IntervalNanos     int64
	Seed              int64
	XIDRate           float64
	DryRun            bool
}

func (settingsRow) TableName() string { return "settings" }

type sinkRow struct {
	ID           uint   `gorm:"primaryKey"`
	Name         string `gorm:"not null;uniqueIndex"`
	Type         string `gorm:"not null"`
	URL          string
	DB           string
	Username     string
	Password     string
	Token        string
	CredentialID string
	Enabled      bool
}

func (sinkRow) TableName() string { return "sinks" }

type topologyRow struct {
	ID   uint   `gorm:"primaryKey"`
	JSON string `gorm:"not null"`
}

func (topologyRow) TableName() string { return "topology" }

// singleRowID is the primary key of the settings and topology rows.
const singleRowID = 1

// Store is the open database. It is safe for concurrent use.
type Store struct {
	db      *gorm.DB
	mu      sync.Mutex // serializes writes so validation and the write see the same rows
	changes chan struct{}
}

// Open opens <dir>/give-me-real-gpu.db, creating the directory (0700) and the file (0600) if needed.
// On the first run it stores the default settings, no sinks and topology.Default().
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	path := filepath.Join(dir, FileName)
	// sqlite would create the file with 0644. Creating it first keeps the passwords in it private,
	// and sqlite gives the -wal and -shm files the same mode as the database file.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod database file: %w", err)
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)

	s := &Store{db: db, changes: make(chan struct{}, 1)}
	if err := s.init(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("database %s: %w", path, err)
	}
	return s, nil
}

// init creates the tables and seeds the rows that are missing.
func (s *Store) init() error {
	if err := s.db.AutoMigrate(&settingsRow{}, &sinkRow{}, &topologyRow{}); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&settingsRow{}).Where("id = ?", singleRowID).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			row := toSettingsRow(DefaultSettings())
			if err := tx.Create(&row).Error; err != nil {
				return fmt.Errorf("seed settings: %w", err)
			}
		}
		if err := tx.Model(&topologyRow{}).Where("id = ?", singleRowID).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			b, err := json.Marshal(topology.Default())
			if err != nil {
				return err
			}
			if err := tx.Create(&topologyRow{ID: singleRowID, JSON: string(b)}).Error; err != nil {
				return fmt.Errorf("seed topology: %w", err)
			}
		}
		return nil
	})
}

// DefaultSettings returns the settings stored on the first run.
func DefaultSettings() Settings {
	return Settings{
		TokenTTL: DefaultTokenTTL,
		Interval: DefaultInterval,
		Seed:     DefaultSeed,
		XIDRate:  DefaultXIDRate,
	}
}

// Close closes the database.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Changes fires after every committed write. The channel has a buffer of 1 and extra signals are dropped,
// so a reader that falls behind sees one signal and reads the latest Snapshot.
func (s *Store) Changes() <-chan struct{} {
	return s.changes
}

func (s *Store) notify() {
	select {
	case s.changes <- struct{}{}:
	default:
	}
}

// Snapshot reads the settings, the sinks (ordered by id) and the topology in one read transaction.
func (s *Store) Snapshot() (Snapshot, error) {
	var snap Snapshot
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var st settingsRow
		if err := tx.First(&st, singleRowID).Error; err != nil {
			return fmt.Errorf("read settings: %w", err)
		}
		snap.Settings = st.settings()

		var rows []sinkRow
		if err := tx.Order("id").Find(&rows).Error; err != nil {
			return fmt.Errorf("read sinks: %w", err)
		}
		snap.Sinks = make([]Sink, len(rows))
		for i, r := range rows {
			snap.Sinks[i] = r.sink()
		}

		var tr topologyRow
		if err := tx.First(&tr, singleRowID).Error; err != nil {
			return fmt.Errorf("read topology: %w", err)
		}
		var t topology.Topology
		if err := json.Unmarshal([]byte(tr.JSON), &t); err != nil {
			return fmt.Errorf("decode stored topology: %w", err)
		}
		snap.Topology = &t
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// SaveSettings validates and stores the settings. An empty OpenStackPassword keeps the stored one,
// since the API never sends the password out and a client cannot echo it back.
func (s *Store) SaveSettings(in Settings) error {
	if p := ValidateSettings(in); len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var cur settingsRow
		if err := tx.First(&cur, singleRowID).Error; err != nil {
			return fmt.Errorf("read settings: %w", err)
		}
		if in.OpenStackPassword == "" {
			in.OpenStackPassword = cur.OpenStackPassword
		}
		row := toSettingsRow(in)
		return tx.Save(&row).Error
	})
	if err != nil {
		return err
	}
	s.notify()
	return nil
}

// CreateSink validates and stores a new sink. The ID of the argument is ignored.
func (s *Store) CreateSink(in Sink) (Sink, error) {
	in.ID = 0
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Sink
	err := s.db.Transaction(func(tx *gorm.DB) error {
		others, err := sinkNames(tx, 0)
		if err != nil {
			return err
		}
		if p := ValidateSink(in, others); len(p) > 0 {
			return &ValidationError{Problems: p}
		}
		row := toSinkRow(in)
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		out = row.sink()
		return nil
	})
	if err != nil {
		return Sink{}, err
	}
	s.notify()
	return out, nil
}

// UpdateSink replaces the sink with in.ID.
// Empty Password or Token means "keep the stored value", because the API never sends them out.
// When the URL changes the stored Password and Token are dropped instead, so a sink cannot be pointed
// at another server and still send the old credentials there. New values sent with the URL are used.
func (s *Store) UpdateSink(in Sink) (Sink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Sink
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var cur sinkRow
		if err := tx.First(&cur, in.ID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if in.URL == cur.URL {
			if in.Password == "" {
				in.Password = cur.Password
			}
			if in.Token == "" {
				in.Token = cur.Token
			}
		}
		others, err := sinkNames(tx, in.ID)
		if err != nil {
			return err
		}
		if p := ValidateSink(in, others); len(p) > 0 {
			return &ValidationError{Problems: p}
		}
		row := toSinkRow(in)
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		out = row.sink()
		return nil
	})
	if err != nil {
		return Sink{}, err
	}
	s.notify()
	return out, nil
}

// DeleteSink removes the sink with the id.
func (s *Store) DeleteSink(id uint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.db.Delete(&sinkRow{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	s.notify()
	return nil
}

// SaveTopology validates and stores the topology.
func (s *Store) SaveTopology(t *topology.Topology) error {
	if t == nil {
		return &ValidationError{Problems: []string{"topology is empty"}}
	}
	if err := t.Validate(); err != nil {
		return &ValidationError{Problems: splitErrors(err)}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.Save(&topologyRow{ID: singleRowID, JSON: string(b)}).Error; err != nil {
		return err
	}
	s.notify()
	return nil
}

// ResetTopology stores topology.Default().
func (s *Store) ResetTopology() error {
	return s.SaveTopology(topology.Default())
}

// sinkNames returns the names of every sink except the one with id skip.
func sinkNames(tx *gorm.DB, skip uint) ([]string, error) {
	var names []string
	if err := tx.Model(&sinkRow{}).Where("id <> ?", skip).Order("id").Pluck("name", &names).Error; err != nil {
		return nil, fmt.Errorf("read sink names: %w", err)
	}
	return names, nil
}

// splitErrors turns an errors.Join result into one string per joined error.
func splitErrors(err error) []string {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, e.Error())
		}
		return out
	}
	return []string{err.Error()}
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateSettings returns every problem in the settings. An empty AdvertiseHost is allowed,
// since it is not known before the first setup.
func ValidateSettings(st Settings) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }
	if st.Interval < time.Second {
		add("interval %s: must be at least 1s", st.Interval)
	}
	if st.XIDRate < 0 || st.XIDRate > 1 {
		add("xid_rate %v: must be between 0 and 1", st.XIDRate)
	}
	if st.TokenTTL < 0 {
		add("token_ttl %s: must not be negative", st.TokenTTL)
	}
	return p
}

// ValidateSink returns every problem in the sink. others are the names of the other sinks.
func ValidateSink(sk Sink, others []string) []string {
	var p []string
	add := func(format string, a ...any) { p = append(p, fmt.Sprintf(format, a...)) }

	label := "sink"
	if strings.TrimSpace(sk.Name) == "" {
		add("name is empty")
	} else {
		label = fmt.Sprintf("sink %q", sk.Name)
		for _, o := range others {
			if o == sk.Name {
				add("%s: name is already used by another sink", label)
				break
			}
		}
	}
	switch sk.Type {
	case SinkInfluxDB, SinkOTLP:
	default:
		add("%s: type %q must be %q or %q", label, sk.Type, SinkInfluxDB, SinkOTLP)
	}
	if sk.URL != "" {
		u, err := url.Parse(sk.URL)
		switch {
		case err != nil:
			add("%s: url cannot be parsed; it must start with http:// or https:// and have a host", label)
		case u.User != nil:
			add("%s: url must not contain a user name or password; put them in the username and password fields", label)
		case (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
			add("%s: url %q must start with http:// or https:// and have a host", label, sk.URL)
		}
	}
	if !sk.Enabled {
		return p
	}
	if sk.URL == "" {
		add("%s: url is empty", label)
	}
	switch sk.Type {
	case SinkInfluxDB:
		if sk.DB == "" {
			add("%s: db is empty", label)
		}
		if sk.CredentialID == "" {
			add("%s: credential_id is empty (the id returned after registering the credential in the management platform)", label)
		} else if !uuidRe.MatchString(sk.CredentialID) {
			add("%s: credential_id %q is not a UUID", label, sk.CredentialID)
		}
	case SinkOTLP:
		if sk.DB != "" {
			add("%s: db is only used by influxdb sinks", label)
		}
		if sk.CredentialID != "" {
			add("%s: credential_id is only used by influxdb sinks", label)
		}
	}
	return p
}

func toSettingsRow(st Settings) settingsRow {
	return settingsRow{
		ID:                singleRowID,
		AdvertiseHost:     st.AdvertiseHost,
		OpenStackPassword: st.OpenStackPassword,
		TokenTTLNanos:     int64(st.TokenTTL),
		IntervalNanos:     int64(st.Interval),
		Seed:              st.Seed,
		XIDRate:           st.XIDRate,
		DryRun:            st.DryRun,
	}
}

func (r settingsRow) settings() Settings {
	return Settings{
		AdvertiseHost:     r.AdvertiseHost,
		OpenStackPassword: r.OpenStackPassword,
		TokenTTL:          time.Duration(r.TokenTTLNanos),
		Interval:          time.Duration(r.IntervalNanos),
		Seed:              r.Seed,
		XIDRate:           r.XIDRate,
		DryRun:            r.DryRun,
	}
}

func toSinkRow(sk Sink) sinkRow {
	return sinkRow{
		ID:           sk.ID,
		Name:         sk.Name,
		Type:         string(sk.Type),
		URL:          sk.URL,
		DB:           sk.DB,
		Username:     sk.Username,
		Password:     sk.Password,
		Token:        sk.Token,
		CredentialID: sk.CredentialID,
		Enabled:      sk.Enabled,
	}
}

func (r sinkRow) sink() Sink {
	return Sink{
		ID:           r.ID,
		Name:         r.Name,
		Type:         SinkType(r.Type),
		URL:          r.URL,
		DB:           r.DB,
		Username:     r.Username,
		Password:     r.Password,
		Token:        r.Token,
		CredentialID: r.CredentialID,
		Enabled:      r.Enabled,
	}
}
