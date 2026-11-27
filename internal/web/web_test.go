package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func embedded(t *testing.T, name string) bool {
	t.Helper()
	_, err := fs.Stat(staticFS, "static/"+name)
	return err == nil
}

func TestServesFiles(t *testing.T) {
	h := Handler()
	cases := []struct {
		path     string
		ct       string
		optional bool
	}{
		{"/ui/", "text/html; charset=utf-8", false},
		{"/ui/index.html", "text/html; charset=utf-8", false},
		{"/ui/app.js", "text/javascript; charset=utf-8", false},
		{"/ui/theme.js", "text/javascript; charset=utf-8", false},
		{"/ui/style.css", "text/css; charset=utf-8", false},
		{"/ui/i18n/en.json", "application/json; charset=utf-8", false},
		{"/ui/i18n/ko.json", "application/json; charset=utf-8", false},
		{"/ui/topology.js", "text/javascript; charset=utf-8", true},
		{"/ui/topology.css", "text/css; charset=utf-8", true},
	}
	for _, c := range cases {
		if c.optional && !embedded(t, strings.TrimPrefix(c.path, Prefix)) {
			t.Logf("%s is not embedded yet, skipped", c.path)
			continue
		}
		rec := get(t, h, http.MethodGet, c.path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); got != c.ct {
			t.Errorf("%s: Content-Type %q, want %q", c.path, got, c.ct)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", c.path, got)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", c.path)
		}
	}
	if rec := get(t, h, http.MethodGet, "/ui/", nil); !strings.Contains(rec.Body.String(), `id="topology-root"`) {
		t.Error("/ui/ does not look like index.html")
	}
	if rec := get(t, h, http.MethodGet, "/ui/", nil); rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("index.html has no Content-Security-Policy")
	}
}

var (
	tagRe        = regexp.MustCompile(`(?is)<(script|link|style)\b[^>]*>`)
	inlineAttrRe = regexp.MustCompile(`(?i)\s(style|on[a-z]+)\s*=`)
)

// The theme has to be on <html> before the first paint, so theme.js is a plain
// (not deferred) script in <head> ahead of the stylesheets. The CSP allows no
// inline script or style, so the page must not need any.
func TestIndexThemeScriptAndCSP(t *testing.T) {
	b, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	head, _, ok := strings.Cut(html, "</head>")
	if !ok {
		t.Fatal("index.html has no </head>")
	}
	themeAt, firstCSS := -1, -1
	for _, loc := range tagRe.FindAllStringIndex(head, -1) {
		tag := head[loc[0]:loc[1]]
		lower := strings.ToLower(tag)
		switch {
		case strings.Contains(lower, `src="theme.js"`):
			themeAt = loc[0]
			if strings.Contains(lower, "defer") || strings.Contains(lower, "async") {
				t.Errorf("theme.js must load synchronously: %s", tag)
			}
		case strings.HasPrefix(lower, "<link") && strings.Contains(lower, `rel="stylesheet"`) && firstCSS < 0:
			firstCSS = loc[0]
		}
	}
	if themeAt < 0 {
		t.Fatal("theme.js is not loaded in <head>")
	}
	if firstCSS < 0 || themeAt > firstCSS {
		t.Errorf("theme.js (at %d) must come before the first stylesheet (at %d)", themeAt, firstCSS)
	}
	for _, m := range tagRe.FindAllStringSubmatch(html, -1) {
		tag, kind := strings.ToLower(m[0]), strings.ToLower(m[1])
		if kind == "style" || (kind == "script" && !strings.Contains(tag, "src=")) {
			t.Errorf("inline %s is blocked by the CSP: %s", m[1], m[0])
		}
	}
	if m := inlineAttrRe.FindString(html); m != "" {
		t.Errorf("index.html has an inline attribute the CSP blocks: %q", m)
	}
	for _, want := range []string{`id="theme-toggle"`, `id="lang-pick"`, `<option value="en"`, `<option value="ko"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}

func TestI18nKeyNames(t *testing.T) {
	bad := regexp.MustCompile(`(?i)passw|pw`)
	for _, lang := range []string{"en", "ko"} {
		for k := range readDict(t, lang) {
			if bad.MatchString(k) {
				t.Errorf("%s.json key %q names a secret; use \"secret\" instead", lang, k)
			}
		}
	}
	if en := readDict(t, "en"); en["theme.dark"] == "" || en["lang.label"] == "" {
		t.Error("en.json lacks theme.dark or lang.label")
	}
}

func TestNotFound(t *testing.T) {
	h := Handler()
	for _, p := range []string{
		"/ui/missing.js",
		"/ui/i18n/",
		"/ui/i18n",
		"/ui/i18n/fr.json",
		"/ui//app.js",
		"/ui/./app.js",
		"/ui/i18n/../app.js",
		"/ui/static/app.js",
		"/app.js",
		"/",
		"/api/v1/status",
	} {
		if rec := get(t, h, http.MethodGet, p, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, rec.Code)
		}
	}
}

func TestRedirectMethodAndETag(t *testing.T) {
	h := Handler()

	rec := get(t, h, http.MethodGet, "/ui?x=1", nil)
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/ui/?x=1" {
		t.Errorf("/ui: status %d location %q, want 301 to /ui/?x=1", rec.Code, rec.Header().Get("Location"))
	}

	rec = get(t, h, http.MethodPost, "/ui/app.js", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", rec.Code)
	}

	rec = get(t, h, http.MethodHead, "/ui/app.js", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD: status %d body %d bytes, want 200 and no body", rec.Code, rec.Body.Len())
	}

	rec = get(t, h, http.MethodGet, "/ui/app.js", nil)
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	rec = get(t, h, http.MethodGet, "/ui/app.js", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: status %d, want 304", rec.Code)
	}
	rec = get(t, h, http.MethodGet, "/ui/style.css", map[string]string{"If-None-Match": etag})
	if rec.Code != http.StatusOK {
		t.Errorf("other file with app.js ETag: status %d, want 200", rec.Code)
	}
}

func readDict(t *testing.T, lang string) map[string]string {
	t.Helper()
	b, err := fs.ReadFile(staticFS, "static/i18n/"+lang+".json")
	if err != nil {
		t.Fatal(err)
	}
	var d map[string]string
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%s.json: %v", lang, err)
	}
	return d
}

var placeholderRe = regexp.MustCompile(`\{\w+\}`)

func placeholders(s string) []string {
	p := placeholderRe.FindAllString(s, -1)
	sort.Strings(p)
	return p
}

func TestI18nKeysMatch(t *testing.T) {
	en, ko := readDict(t, "en"), readDict(t, "ko")
	for k, v := range en {
		kv, ok := ko[k]
		if !ok {
			t.Errorf("ko.json is missing %q", k)
			continue
		}
		if strings.TrimSpace(v) == "" || strings.TrimSpace(kv) == "" {
			t.Errorf("%q is empty in en or ko", k)
		}
		if !reflect.DeepEqual(placeholders(v), placeholders(kv)) {
			t.Errorf("%q: placeholders differ, en %v ko %v", k, placeholders(v), placeholders(kv))
		}
	}
	for k := range ko {
		if _, ok := en[k]; !ok {
			t.Errorf("en.json is missing %q", k)
		}
	}
}

var (
	attrKeyRe = regexp.MustCompile(`data-i18n(?:-placeholder|-title|-aria-label)?="([^"]+)"`)
	jsKeyRe   = regexp.MustCompile(`"((?:app|lang|tab|common|status|general|sinks)\.[a-z0-9_]+)"`)
)

func TestUsedKeysExist(t *testing.T) {
	en := readDict(t, "en")
	used := map[string]string{}
	html, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range attrKeyRe.FindAllStringSubmatch(string(html), -1) {
		used[m[1]] = "index.html"
	}
	js, err := fs.ReadFile(staticFS, "static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range jsKeyRe.FindAllStringSubmatch(string(js), -1) {
		used[m[1]] = "app.js"
	}
	if len(used) < 50 {
		t.Fatalf("found only %d keys in use, the patterns look broken", len(used))
	}
	for k, where := range used {
		if _, ok := en[k]; !ok {
			t.Errorf("%s uses %q but en.json does not have it", where, k)
		}
	}
}

var ipv4Re = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)

func allowedIP(ip string) bool {
	for _, p := range []string{"192.0.2.", "198.51.100.", "203.0.113.", "127."} {
		if strings.HasPrefix(ip, p) {
			return true
		}
	}
	return false
}

func dotted(parts ...string) string { return strings.Join(parts, ".") }

func TestAllowedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"192.0.2.10": true, "198.51.100.7": true, "203.0.113.255": true, "127.0.0.1": true,
		dotted("10", "0", "0", "1"): false, dotted("192", "168", "1", "1"): false,
		dotted("8", "8", "8", "8"): false, dotted("192", "0", "20", "1"): false,
	} {
		if got := allowedIP(ip); got != want {
			t.Errorf("allowedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestNoRealIPs(t *testing.T) {
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(staticFS, p)
		if err != nil {
			return err
		}
		for _, ip := range ipv4Re.FindAllString(string(b), -1) {
			if !allowedIP(ip) {
				t.Errorf("%s contains %s; use TEST-NET (192.0.2.x, 198.51.100.x, 203.0.113.x) or loopback", p, ip)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func nodeOrSkip(t *testing.T) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found, JavaScript checks skipped")
	}
	return node
}

func TestJavaScriptSyntax(t *testing.T) {
	node := nodeOrSkip(t)
	files, err := filepath.Glob("static/*.js")
	if err != nil || len(files) == 0 {
		t.Fatalf("no js files: %v", err)
	}
	for _, f := range files {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("node --check %s: %v\n%s", f, err, out)
		}
	}
}

func TestThemeHelper(t *testing.T) {
	node := nodeOrSkip(t)
	abs, err := filepath.Abs("static/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(abs)
	script := `const h = require(` + string(path) + `);
console.log(JSON.stringify([
  h.pickTheme(null, false), h.pickTheme(null, true),
  h.pickTheme("dark", false), h.pickTheme("light", true),
  h.pickTheme("blue", true), h.pickTheme("", false), h.pickTheme(undefined, true)
]));`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	want := []string{"light", "dark", "dark", "light", "dark", "light", "dark"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pickTheme = %v, want %v", got, want)
	}
}

func TestJavaScriptHelpers(t *testing.T) {
	node := nodeOrSkip(t)
	abs, err := filepath.Abs("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	path, _ := json.Marshal(abs)
	script := `const h = require(` + string(path) + `);
const orig = {url: "http://192.0.2.10:8086", password_set: true, token_set: false};
console.log(JSON.stringify({
  koFirst: h.pickLanguage(["ko-KR", "en-US"]),
  enFirst: h.pickLanguage(["en-US", "ko-KR"]),
  koAfterOther: h.pickLanguage(["fr-FR", "KO"]),
  neither: h.pickLanguage(["fr", "de"]),
  empty: h.pickLanguage([]),
  savedWins: h.initialLanguage("ko", ["en"]),
  badSaved: h.initialLanguage("xx", ["ko"]),
  format: h.format("{a} and {b} and {c}", {a: 1, b: "x"}),
  influx: h.sinkPayload({name: " site-a ", type: "influxdb", url: " http://192.0.2.10:8086 ", db: "telegraf",
    username: "u", password: "", token: "<change-me>", credential_id: "cid", enabled: 1}),
  otlp: h.sinkPayload({name: "site-b", type: "otlp", url: "http://192.0.2.20:8429/opentelemetry/v1/metrics", db: "x",
    username: "", password: "<change-me>", token: "<change-me>", credential_id: "cid", enabled: false,
    influx_version: "v2", org: "org-a"}),
  influxV2: h.sinkPayload({name: "site-a", type: "influxdb", url: "http://192.0.2.20:8086", influx_version: "v2",
    org: " org-a ", db: " bucket-a ", username: "u", password: "<change-me>", token: "<change-me>",
    credential_id: " cid ", enabled: true}),
  influxV3: h.sinkPayload({name: "site-a", type: "influxdb", url: "http://192.0.2.20:8181", influx_version: "v3",
    org: "org-a", db: "metrics", username: "u", password: "<change-me>", token: "", credential_id: "cid",
    enabled: true}),
  influxBadVersion: h.sinkPayload({name: "site-a", type: "influxdb", url: "http://192.0.2.20:8086", influx_version: "v9",
    org: "org-a", db: "telegraf", username: "u", password: "<change-me>", token: "<change-me>", credential_id: "cid",
    enabled: true}),
  versions: ["v1", "V2", " v3 ", "", "v4", null].map(h.influxVersion),
  variants: [h.sinkVariant("influxdb", ""), h.sinkVariant("influxdb", "v2"), h.sinkVariant("influxdb", "v3"),
    h.sinkVariant("otlp", "v2")],
  keptSameURL: h.secretKept(orig, "password_set", " http://192.0.2.10:8086 "),
  clearedNewURL: h.secretKept(orig, "password_set", "http://192.0.2.11:8086"),
  notSet: h.secretKept(orig, "token_set", "http://192.0.2.10:8086"),
  newSink: h.secretKept(null, "password_set", ""),
  rules: h.tagRules([
    {action: "rename", key: " host ", new_key: " hostname ", value: "left over"},
    {action: "drop", key: "time_diff", new_key: "x", value: "y"},
    {action: "add", key: "env", new_key: "x", value: " dev "},
    {action: "rename", key: "", new_key: "  ", value: "ignored"},
    {action: "drop", key: " "},
    {action: "add", key: "", new_key: "ignored", value: ""},
    {action: "rename", key: "", new_key: "only_new"},
    null
  ]),
  noRules: h.tagRules(undefined),
  ordered: h.orderRules([{action: "add", key: "c"}, {action: "drop", key: "a"}, null, {action: "add", key: "d"},
    {action: "rename", key: "b"}]).map(function (r) { return r.action + ":" + r.key; }),
  preview: h.tagPreview([
    {key: "uuid", example: "u1", where: ["cpu"]},
    {key: "hostname", example: "x", where: ["cpu"]},
    {key: "env", example: "prod", where: ["cpu"]},
    {key: "host", example: "h1", where: ["cpu", "dcgm"]}
  ], [
    {action: "add", key: "site", value: " a "},
    {action: "rename", key: "host", new_key: "hostname"},
    {action: "drop", key: "uuid"},
    {action: "add", key: "env", value: "dev"},
    {action: "add", key: "site", value: "b"},
    {action: "drop", key: "host"}
  ]),
  problems: [
    {action: "rename", key: "host", new_key: "hostname"},
    {action: "rename", key: "host", new_key: ""},
    {action: "rename", key: "host", new_key: " host "},
    {action: "rename", key: "", new_key: "hostname"},
    {action: "drop", key: "host"},
    {action: "drop", key: ""},
    {action: "add", key: "env", value: ""},
    {action: "add", key: "", value: "dev"},
    {action: "add", key: "env", value: "dev"}
  ].map(h.tagRuleProblem),
  influxRules: h.sinkPayload({name: "site-a", type: "influxdb", url: "http://192.0.2.10:8086", db: "telegraf",
    credential_id: "cid", enabled: true, tag_rules: [{action: "drop", key: "uuid", value: "x"}, {action: "add", key: "", value: ""}]}),
  statusView: [
    h.sinkStatusView({enabled: true, last_write: "", lines: 2032, elapsed_ms: 0}, true),
    h.sinkStatusView({enabled: true, last_write: "", lines: 0}, true),
    h.sinkStatusView({enabled: false, last_write: "", lines: 0}, true),
    h.sinkStatusView({enabled: true, last_write: "2026-11-27T03:00:00Z", lines: 120, elapsed_ms: 15}, false),
    h.sinkStatusView({enabled: true, last_write: "2026-11-27T03:00:00Z", lines: 0, elapsed_ms: 3}, false),
    h.sinkStatusView({enabled: true, last_write: "", lines: 0}, false),
    h.sinkStatusView({enabled: true, last_write: "", lines: 50}, false),
    h.sinkStatusView({enabled: true, last_write: "2026-11-27T03:00:00Z", lines: 120, elapsed_ms: 15}, true),
    h.sinkStatusView({enabled: false, last_write: "2026-11-27T03:00:00Z", lines: 120, elapsed_ms: 15}, true)
  ],
  toggled: h.sinkPayload(Object.assign(h.sinkValues({id: 3, name: "site-b", type: "otlp",
    url: "http://192.0.2.20:8429/opentelemetry/v1/metrics", influx_version: "", org: "", db: "", username: "",
    credential_id: "", enabled: true, password_set: true, token_set: true,
    tag_rules: [{action: "rename", key: "entity_id", new_key: "id"}, {action: "add", key: "env", value: "dev"}]}),
    {enabled: false}))
}));`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node output %q: %v", out, err)
	}
	want := map[string]any{
		"koFirst":      "ko",
		"enFirst":      "en",
		"koAfterOther": "ko",
		"neither":      "en",
		"empty":        "en",
		"savedWins":    "ko",
		"badSaved":     "ko",
		"format":       "1 and x and {c}",
		"influx": map[string]any{
			"name": "site-a", "type": "influxdb", "url": "http://192.0.2.10:8086", "influx_version": "v1", "org": "",
			"db": "telegraf", "username": "u", "credential_id": "cid", "enabled": true, "tag_rules": []any{},
		},
		"otlp": map[string]any{
			"name": "site-b", "type": "otlp", "url": "http://192.0.2.20:8429/opentelemetry/v1/metrics", "influx_version": "",
			"org": "", "db": "", "username": "", "credential_id": "", "enabled": false,
			"password": "<change-me>", "token": "<change-me>", "tag_rules": []any{},
		},
		"influxV2": map[string]any{
			"name": "site-a", "type": "influxdb", "url": "http://192.0.2.20:8086", "influx_version": "v2", "org": "org-a",
			"db": "bucket-a", "username": "", "credential_id": "cid", "enabled": true, "token": "<change-me>",
			"tag_rules": []any{},
		},
		"influxV3": map[string]any{
			"name": "site-a", "type": "influxdb", "url": "http://192.0.2.20:8181", "influx_version": "v3", "org": "",
			"db": "metrics", "username": "", "credential_id": "cid", "enabled": true, "tag_rules": []any{},
		},
		"influxBadVersion": map[string]any{
			"name": "site-a", "type": "influxdb", "url": "http://192.0.2.20:8086", "influx_version": "v1", "org": "",
			"db": "telegraf", "username": "u", "credential_id": "cid", "enabled": true, "password": "<change-me>",
			"tag_rules": []any{},
		},
		"versions":      []any{"v1", "v2", "v3", "v1", "v1", "v1"},
		"variants":      []any{"influxdb-v1", "influxdb-v2", "influxdb-v3", "otlp"},
		"keptSameURL":   true,
		"clearedNewURL": false,
		"notSet":        false,
		"newSink":       false,
		"rules": []any{
			map[string]any{"action": "rename", "key": "host", "new_key": "hostname"},
			map[string]any{"action": "drop", "key": "time_diff"},
			map[string]any{"action": "rename", "key": "", "new_key": "only_new"},
			map[string]any{"action": "add", "key": "env", "value": "dev"},
		},
		"noRules": []any{},
		"ordered": []any{"drop:a", "rename:b", "add:c", "add:d"},
		"preview": map[string]any{
			"keys": []any{
				map[string]any{"key": "host", "example": "h1", "where": []any{"cpu", "dcgm"}, "to": "hostname", "dropped": false, "value": ""},
				map[string]any{"key": "env", "example": "prod", "where": []any{"cpu"}, "to": "", "dropped": false, "value": "dev"},
				map[string]any{"key": "hostname", "example": "x", "where": []any{"cpu"}, "to": "", "dropped": true, "value": ""},
				map[string]any{"key": "uuid", "example": "u1", "where": []any{"cpu"}, "to": "", "dropped": true, "value": ""},
			},
			"added": []any{map[string]any{"key": "site", "value": "b"}},
		},
		"problems": []any{
			"", "sinks.rule_new_key_missing", "sinks.rule_same_key", "sinks.rule_key_missing",
			"", "", "sinks.rule_value_missing", "sinks.rule_key_missing", "",
		},
		"influxRules": map[string]any{
			"name": "site-a", "type": "influxdb", "url": "http://192.0.2.10:8086", "influx_version": "v1", "org": "",
			"db": "telegraf", "username": "", "credential_id": "cid", "enabled": true,
			"tag_rules": []any{map[string]any{"action": "drop", "key": "uuid"}},
		},
		"statusView": []any{
			map[string]any{"last": "dry_run", "lines": 2032.0, "elapsed_ms": nil},
			map[string]any{"last": "dry_run", "lines": nil, "elapsed_ms": nil},
			map[string]any{"last": "never", "lines": nil, "elapsed_ms": nil},
			map[string]any{"last": "time", "lines": 120.0, "elapsed_ms": 15.0},
			map[string]any{"last": "time", "lines": 0.0, "elapsed_ms": 3.0},
			map[string]any{"last": "never", "lines": nil, "elapsed_ms": nil},
			map[string]any{"last": "never", "lines": 50.0, "elapsed_ms": nil},
			map[string]any{"last": "dry_run", "lines": 120.0, "elapsed_ms": nil},
			map[string]any{"last": "time", "lines": 120.0, "elapsed_ms": 15.0},
		},
		"toggled": map[string]any{
			"name": "site-b", "type": "otlp", "url": "http://192.0.2.20:8429/opentelemetry/v1/metrics", "influx_version": "",
			"org": "", "db": "", "username": "", "credential_id": "", "enabled": false,
			"tag_rules": []any{
				map[string]any{"action": "rename", "key": "entity_id", "new_key": "id"},
				map[string]any{"action": "add", "key": "env", "value": "dev"},
			},
		},
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d results, want %d", len(got), len(want))
	}
}
