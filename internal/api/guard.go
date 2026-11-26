package api

import (
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/labstack/echo/v5"
)

// guard keeps other web sites open in a browser on this machine away from the API, which has no login.
//   - The Host header must be an IP literal or localhost. A DNS-rebound attacker host name is refused,
//     so a page from that name cannot read the responses.
//   - A state-changing request must come from the same origin (Sec-Fetch-Site, or Origin when that is absent).
//     Requests with neither header (curl, scripts) pass.
//   - A request with a body must be application/json, which a cross-site form or text/plain fetch cannot send
//     without a preflight.
func guard(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		r := c.Request()
		if !localHost(r.Host) {
			return c.JSON(http.StatusForbidden, errorBody{Error: "forbidden host"})
		}
		if !safeMethod(r.Method) {
			if !sameOrigin(r) {
				return c.JSON(http.StatusForbidden, errorBody{Error: "cross-site request refused"})
			}
			if hasBody(r) && !jsonContent(r.Header.Get("Content-Type")) {
				return c.JSON(http.StatusUnsupportedMediaType, errorBody{Error: "content type must be application/json"})
			}
		}
		return next(c)
	}
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// localHost reports whether the host part of a Host header is an IP address or localhost.
func localHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "none"
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// hasBody is true when the request carries a body or its length is unknown (chunked).
func hasBody(r *http.Request) bool {
	return r.ContentLength != 0
}

func jsonContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}
