package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testHost = "127.0.0.1:15080"

const validSink = `{"name":"site-a","type":"otlp"}`

func TestGuard(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		path    string
		host    string
		body    string
		headers map[string]string
		code    int
		errMsg  string
	}{
		{"text/plain POST", http.MethodPost, "/api/v1/sinks", testHost, validSink,
			map[string]string{"Content-Type": "text/plain"}, http.StatusUnsupportedMediaType, "content type must be application/json"},
		{"form POST", http.MethodPost, "/api/v1/sinks", testHost, "name=x",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, http.StatusUnsupportedMediaType, "content type must be application/json"},
		{"POST without content type", http.MethodPost, "/api/v1/sinks", testHost, validSink,
			nil, http.StatusUnsupportedMediaType, "content type must be application/json"},
		{"cross Origin", http.MethodPost, "/api/v1/sinks", testHost, validSink,
			map[string]string{"Content-Type": "application/json", "Origin": "http://evil.example"}, http.StatusForbidden, "cross-site request refused"},
		{"Origin on another port", http.MethodDelete, "/api/v1/sinks/1", testHost, "",
			map[string]string{"Origin": "http://127.0.0.1:8080"}, http.StatusForbidden, "cross-site request refused"},
		{"null Origin", http.MethodPost, "/api/v1/topology/reset", testHost, "",
			map[string]string{"Origin": "null"}, http.StatusForbidden, "cross-site request refused"},
		{"Sec-Fetch-Site cross-site", http.MethodPut, "/api/v1/settings", testHost, `{}`,
			map[string]string{"Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}, http.StatusForbidden, "cross-site request refused"},
		{"Sec-Fetch-Site same-site", http.MethodPost, "/api/v1/topology/reset", testHost, "",
			map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden, "cross-site request refused"},
		{"Sec-Fetch-Site wins over a matching Origin", http.MethodPost, "/api/v1/topology/reset", testHost, "",
			map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "http://" + testHost}, http.StatusForbidden, "cross-site request refused"},
		{"host name GET", http.MethodGet, "/api/v1/settings", "evil.example", "",
			nil, http.StatusForbidden, "forbidden host"},
		{"host name with port GET", http.MethodGet, "/api/v1/status", "evil.example:15080", "",
			nil, http.StatusForbidden, "forbidden host"},
		{"empty host", http.MethodGet, "/api/v1/status", "", "",
			nil, http.StatusForbidden, "forbidden host"},

		{"same origin JSON POST", http.MethodPost, "/api/v1/sinks", testHost, validSink,
			map[string]string{"Content-Type": "application/json; charset=utf-8", "Origin": "http://" + testHost, "Sec-Fetch-Site": "same-origin"}, http.StatusCreated, ""},
		{"same Origin without Sec-Fetch-Site", http.MethodPut, "/api/v1/settings", testHost, `{}`,
			map[string]string{"Content-Type": "application/json", "Origin": "http://" + testHost}, http.StatusOK, ""},
		{"Sec-Fetch-Site none", http.MethodPost, "/api/v1/topology/reset", testHost, "",
			map[string]string{"Sec-Fetch-Site": "none"}, http.StatusOK, ""},
		{"curl style POST", http.MethodPost, "/api/v1/topology/reset", testHost, "",
			nil, http.StatusOK, ""},
		{"curl style PUT", http.MethodPut, "/api/v1/settings", testHost, `{}`,
			map[string]string{"Content-Type": "application/json"}, http.StatusOK, ""},
		{"localhost", http.MethodGet, "/api/v1/settings", "localhost:15080", "",
			nil, http.StatusOK, ""},
		{"localhost without port", http.MethodGet, "/api/v1/settings", "LocalHost", "",
			nil, http.StatusOK, ""},
		{"IPv6 loopback", http.MethodGet, "/api/v1/settings", "[::1]:15080", "",
			nil, http.StatusOK, ""},
		{"IPv6 loopback same origin PUT", http.MethodPut, "/api/v1/settings", "[::1]:15080", `{}`,
			map[string]string{"Content-Type": "application/json", "Origin": "http://[::1]:15080"}, http.StatusOK, ""},
		{"LAN address", http.MethodGet, "/api/v1/settings", "192.0.2.5:15080", "",
			nil, http.StatusOK, ""},
		{"cross-site GET is allowed on an IP host", http.MethodGet, "/api/v1/settings", testHost, "",
			map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newServer(t, nil)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Host = tc.host
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.code, rec.Body.String())
			}
			if tc.errMsg == "" {
				return
			}
			var eb struct {
				Error string `json:"error"`
			}
			decodeJSON(t, rec.Body.String(), &eb)
			if eb.Error != tc.errMsg {
				t.Errorf("error %q, want %q", eb.Error, tc.errMsg)
			}
		})
	}
}

func TestGuardRejectedRequestChangesNothing(t *testing.T) {
	e, st := newServer(t, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sinks", strings.NewReader(validSink))
	req.Host = testHost
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnsupportedMediaType || len(snap.Sinks) != 0 {
		t.Errorf("status %d, %d sinks stored after a rejected POST", rec.Code, len(snap.Sinks))
	}
}

func TestLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.1:15080": true, "[::1]": true, "[::1]:15080": true, "::1": true,
		"localhost": true, "LOCALHOST:1": true, "192.0.2.5:15080": true,
		"evil.example": false, "evil.example:15080": false, "localhost.evil.example": false,
		"127.0.0.1.nip.example": false, "": false, ":15080": false,
	} {
		if got := localHost(host); got != want {
			t.Errorf("localHost(%q) = %v, want %v", host, got, want)
		}
	}
}
