// Package api is the JSON API of the web settings screen, served under /api/v1.
// Passwords and tokens go in but never come out: responses only say whether one is set.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jollaman999/give-me-real-gpu/internal/metrics"
	"github.com/jollaman999/give-me-real-gpu/internal/otelgen"
	"github.com/jollaman999/give-me-real-gpu/internal/store"
	"github.com/jollaman999/give-me-real-gpu/internal/tagrules"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// maxBody limits request bodies. A large topology is still far below this.
const maxBody = 4 << 20

// StatusProvider reports the running state of the emulator.
type StatusProvider interface {
	Status() Status
}

// Status is the body of GET /api/v1/status.
type Status struct {
	Version   string          `json:"version"`
	OpenStack OpenStackStatus `json:"openstack"`
	Sinks     []SinkStatus    `json:"sinks"`
}

// OpenStackStatus tells whether the fake OpenStack API is usable and what to register in the management platform.
type OpenStackStatus struct {
	Ready       bool   `json:"ready"`
	Reason      string `json:"reason"` // why it is not ready. Empty when ready
	AuthURL     string `json:"auth_url"`
	User        string `json:"user"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
	DomainID    string `json:"domain_id"`
	Region      string `json:"region"`
}

// SinkStatus is the write state of one sink.
type SinkStatus struct {
	ID        uint           `json:"id"`
	Name      string         `json:"name"`
	Type      store.SinkType `json:"type"`
	Enabled   bool           `json:"enabled"`
	LastWrite string         `json:"last_write"` // RFC3339, empty before the first write
	Lines     int            `json:"lines"`
	ElapsedMS int64          `json:"elapsed_ms"`
	LastError string         `json:"last_error"`
}

// settingsOut is the body of GET /api/v1/settings.
type settingsOut struct {
	AdvertiseHost        string  `json:"advertise_host"`
	OpenStackPasswordSet bool    `json:"openstack_password_set"`
	TokenTTL             string  `json:"token_ttl"`
	Interval             string  `json:"interval"`
	Seed                 int64   `json:"seed"`
	XIDRate              float64 `json:"xid_rate"`
	DryRun               bool    `json:"dry_run"`
}

// settingsIn is the body of PUT /api/v1/settings. openstack_password_set is accepted so a client
// can send back what it got, and is ignored.
type settingsIn struct {
	AdvertiseHost        string  `json:"advertise_host"`
	OpenStackPassword    string  `json:"openstack_password"`
	OpenStackPasswordSet bool    `json:"openstack_password_set"`
	TokenTTL             string  `json:"token_ttl"`
	Interval             string  `json:"interval"`
	Seed                 int64   `json:"seed"`
	XIDRate              float64 `json:"xid_rate"`
	DryRun               bool    `json:"dry_run"`
}

// sinkOut is one sink in GET /api/v1/sinks.
type sinkOut struct {
	ID            uint            `json:"id"`
	Name          string          `json:"name"`
	Type          store.SinkType  `json:"type"`
	URL           string          `json:"url"`
	InfluxVersion string          `json:"influx_version"`
	Org           string          `json:"org"`
	DB            string          `json:"db"`
	Username      string          `json:"username"`
	PasswordSet   bool            `json:"password_set"`
	TokenSet      bool            `json:"token_set"`
	CredentialID  string          `json:"credential_id"`
	Enabled       bool            `json:"enabled"`
	TagRules      []tagrules.Rule `json:"tag_rules"` // [] when there are none
}

// sinkIn is the body of POST /api/v1/sinks and PUT /api/v1/sinks/:id. id, password_set and token_set
// are accepted so a client can send back what it got, and are ignored. tag_rules replaces the whole list;
// missing or null keeps the stored list on PUT.
type sinkIn struct {
	ID            uint             `json:"id"`
	Name          string           `json:"name"`
	Type          store.SinkType   `json:"type"`
	URL           string           `json:"url"`
	InfluxVersion string           `json:"influx_version"`
	Org           string           `json:"org"`
	DB            string           `json:"db"`
	Username      string           `json:"username"`
	Password      string           `json:"password"`
	Token         string           `json:"token"`
	PasswordSet   bool             `json:"password_set"`
	TokenSet      bool             `json:"token_set"`
	CredentialID  string           `json:"credential_id"`
	Enabled       bool             `json:"enabled"`
	TagRules      *[]tagrules.Rule `json:"tag_rules"`
}

type handler struct {
	st *store.Store
	sp StatusProvider
}

// Register adds the /api/v1 routes to e. sp may be nil, in which case /status reports an empty status.
func Register(e *echo.Echo, st *store.Store, sp StatusProvider) {
	h := &handler{st: st, sp: sp}
	g := e.Group("/api/v1", Guard)
	g.GET("/status", h.getStatus)
	g.GET("/settings", h.getSettings)
	g.PUT("/settings", h.putSettings)
	g.GET("/sinks", h.listSinks)
	g.POST("/sinks", h.createSink)
	g.PUT("/sinks/:id", h.updateSink)
	g.DELETE("/sinks/:id", h.deleteSink)
	g.GET("/topology", h.getTopology)
	g.PUT("/topology", h.putTopology)
	g.POST("/topology/reset", h.resetTopology)
	g.GET("/tag-keys", h.getTagKeys)
}

func (h *handler) getStatus(c *echo.Context) error {
	var s Status
	if h.sp != nil {
		s = h.sp.Status()
	}
	if s.Sinks == nil {
		s.Sinks = []SinkStatus{}
	}
	return c.JSON(http.StatusOK, s)
}

func (h *handler) getSettings(c *echo.Context) error {
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(http.StatusOK, toSettingsOut(snap.Settings))
}

// putSettings starts from the stored values, so keys missing from the body stay as they are.
func (h *handler) putSettings(c *echo.Context) error {
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	cur := snap.Settings
	in := settingsIn{
		AdvertiseHost: cur.AdvertiseHost,
		TokenTTL:      formatDuration(cur.TokenTTL),
		Interval:      formatDuration(cur.Interval),
		Seed:          cur.Seed,
		XIDRate:       cur.XIDRate,
		DryRun:        cur.DryRun,
	}
	if err := decode(c, &in); err != nil {
		return badBody(c, err)
	}
	var problems []string
	ttl, err := time.ParseDuration(in.TokenTTL)
	if err != nil {
		problems = append(problems, fmt.Sprintf("token_ttl %q: must be a duration such as \"1h\"", in.TokenTTL))
	}
	interval, err := time.ParseDuration(in.Interval)
	if err != nil {
		problems = append(problems, fmt.Sprintf("interval %q: must be a duration such as \"10s\"", in.Interval))
	}
	if len(problems) > 0 {
		return invalid(c, problems)
	}
	err = h.st.SaveSettings(store.Settings{
		AdvertiseHost:     in.AdvertiseHost,
		OpenStackPassword: in.OpenStackPassword,
		TokenTTL:          ttl,
		Interval:          interval,
		Seed:              in.Seed,
		XIDRate:           in.XIDRate,
		DryRun:            in.DryRun,
	})
	if err != nil {
		return fail(c, err)
	}
	return h.getSettings(c)
}

func (h *handler) listSinks(c *echo.Context) error {
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	out := make([]sinkOut, len(snap.Sinks))
	for i, s := range snap.Sinks {
		out[i] = toSinkOut(s)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *handler) createSink(c *echo.Context) error {
	var in sinkIn
	if err := decode(c, &in); err != nil {
		return badBody(c, err)
	}
	s, err := h.st.CreateSink(in.sink(0))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(http.StatusCreated, toSinkOut(s))
}

// updateSink starts from the stored sink, so keys missing from the body stay as they are.
// Password and token start empty, which the store reads as "keep the stored value".
func (h *handler) updateSink(c *echo.Context) error {
	id, ok := sinkID(c)
	if !ok {
		return notFound(c, "sink not found")
	}
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	var cur *store.Sink
	for i := range snap.Sinks {
		if snap.Sinks[i].ID == id {
			cur = &snap.Sinks[i]
		}
	}
	if cur == nil {
		return notFound(c, "sink not found")
	}
	in := sinkIn{
		Name:          cur.Name,
		Type:          cur.Type,
		URL:           cur.URL,
		InfluxVersion: cur.InfluxVersion,
		Org:           cur.Org,
		DB:            cur.DB,
		Username:      cur.Username,
		CredentialID:  cur.CredentialID,
		Enabled:       cur.Enabled,
	}
	if err := decode(c, &in); err != nil {
		return badBody(c, err)
	}
	if in.TagRules == nil {
		in.TagRules = &cur.TagRules
	}
	s, err := h.st.UpdateSink(in.sink(id))
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(http.StatusOK, toSinkOut(s))
}

func (h *handler) deleteSink(c *echo.Context) error {
	id, ok := sinkID(c)
	if !ok {
		return notFound(c, "sink not found")
	}
	if err := h.st.DeleteSink(id); err != nil {
		return fail(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *handler) getTopology(c *echo.Context) error {
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	return c.JSON(http.StatusOK, snap.Topology)
}

// tagKeysOut is the body of GET /api/v1/tag-keys: the keys the stored topology emits before any rule.
type tagKeysOut struct {
	InfluxDB []tagrules.KeyInfo `json:"influxdb"` // line protocol tags
	OTLP     []tagrules.KeyInfo `json:"otlp"`     // data point and resource attributes
}

// exampleCredentialID stands in for the credential_id tag value when the request names none, so the key is listed.
const exampleCredentialID = "credential-id"

// getTagKeys runs one throwaway collection of each generator over the stored topology. The optional
// credential_id query value is used for the credential_id tag and the uuid prefix of the examples.
func (h *handler) getTagKeys(c *echo.Context) error {
	snap, err := h.st.Snapshot()
	if err != nil {
		return fail(c, err)
	}
	cred := strings.TrimSpace(c.QueryParam("credential_id"))
	if cred == "" {
		cred = exampleCredentialID
	}
	now := time.Now()
	return c.JSON(http.StatusOK, tagKeysOut{
		InfluxDB: metrics.TagKeys(snap.Topology, cred, now),
		OTLP:     otelgen.LabelKeys(snap.Topology, now),
	})
}

func (h *handler) putTopology(c *echo.Context) error {
	var t topology.Topology
	if err := decode(c, &t); err != nil {
		return badBody(c, err)
	}
	if err := h.st.SaveTopology(&t); err != nil {
		return fail(c, err)
	}
	return h.getTopology(c)
}

func (h *handler) resetTopology(c *echo.Context) error {
	if err := h.st.ResetTopology(); err != nil {
		return fail(c, err)
	}
	return h.getTopology(c)
}

func (in sinkIn) sink(id uint) store.Sink {
	var rules []tagrules.Rule
	if in.TagRules != nil {
		rules = *in.TagRules
	}
	return store.Sink{
		ID:            id,
		Name:          in.Name,
		Type:          in.Type,
		URL:           in.URL,
		InfluxVersion: in.InfluxVersion,
		Org:           in.Org,
		DB:            in.DB,
		Username:      in.Username,
		Password:      in.Password,
		Token:         in.Token,
		CredentialID:  in.CredentialID,
		Enabled:       in.Enabled,
		TagRules:      rules,
	}
}

func toSettingsOut(s store.Settings) settingsOut {
	return settingsOut{
		AdvertiseHost:        s.AdvertiseHost,
		OpenStackPasswordSet: s.OpenStackPassword != "",
		TokenTTL:             formatDuration(s.TokenTTL),
		Interval:             formatDuration(s.Interval),
		Seed:                 s.Seed,
		XIDRate:              s.XIDRate,
		DryRun:               s.DryRun,
	}
}

func toSinkOut(s store.Sink) sinkOut {
	rules := s.TagRules
	if rules == nil {
		rules = []tagrules.Rule{}
	}
	return sinkOut{
		ID:            s.ID,
		Name:          s.Name,
		Type:          s.Type,
		URL:           s.URL,
		InfluxVersion: s.InfluxVersion,
		Org:           s.Org,
		DB:            s.DB,
		Username:      s.Username,
		PasswordSet:   s.Password != "",
		TokenSet:      s.Token != "",
		CredentialID:  s.CredentialID,
		Enabled:       s.Enabled,
		TagRules:      rules,
	}
}

// formatDuration writes a duration the way a person would type it: 1h instead of 1h0m0s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return s
}

func sinkID(c *echo.Context) (uint, bool) {
	n, err := strconv.ParseUint(c.Param("id"), 10, 32)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint(n), true
}

// decode reads one JSON value from the body into v. Unknown keys are rejected so a typo in a key
// is reported instead of silently leaving the old value.
func decode(c *echo.Context, v any) error {
	r := c.Request()
	dec := json.NewDecoder(http.MaxBytesReader(c.Response(), r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

type errorBody struct {
	Error    string   `json:"error"`
	Problems []string `json:"problems,omitempty"`
}

func badBody(c *echo.Context, err error) error {
	return c.JSON(http.StatusBadRequest, errorBody{Error: "invalid request body", Problems: []string{err.Error()}})
}

func invalid(c *echo.Context, problems []string) error {
	return c.JSON(http.StatusBadRequest, errorBody{Error: "validation failed", Problems: problems})
}

func notFound(c *echo.Context, msg string) error {
	return c.JSON(http.StatusNotFound, errorBody{Error: msg})
}

// ErrorHandler is an echo.HTTPErrorHandler that answers {"error":"..."} like the handlers above,
// so an unknown path (404) or method (405) has the same shape as any other API error.
// The message is the status text or the message of an *echo.HTTPError; other error text is logged, not sent.
func ErrorHandler(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	code := echo.StatusCode(err)
	if code == 0 {
		code = http.StatusInternalServerError
	}
	msg := http.StatusText(code)
	var he *echo.HTTPError
	if errors.As(err, &he) && he.Message != "" {
		msg = he.Message
	}
	if code >= http.StatusInternalServerError {
		c.Logger().Error("request failed", "method", c.Request().Method, "path", c.Request().URL.Path, "error", err)
	}
	var werr error
	if c.Request().Method == http.MethodHead {
		werr = c.NoContent(code)
	} else {
		werr = c.JSON(code, errorBody{Error: msg})
	}
	if werr != nil {
		c.Logger().Warn("failed to send the error response", "error", werr)
	}
}

// fail maps a store error to 400 (validation), 404 (unknown id) or 500.
func fail(c *echo.Context, err error) error {
	var ve *store.ValidationError
	switch {
	case errors.As(err, &ve):
		return invalid(c, ve.Problems)
	case errors.Is(err, store.ErrNotFound):
		return notFound(c, "sink not found")
	default:
		c.Logger().Error("api request failed", "path", c.Request().URL.Path, "error", err)
		return c.JSON(http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}
