package openstack

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// catalogService is one entry of the keystone service catalog.
type catalogService struct {
	Type   string
	Name   string
	Prefix string // path appended to the base URL
	Desc   string
}

// services lists the catalog in order. openstack4j resolves an endpoint by
// walking the catalog and taking the first service whose type or name
// matches, so volumev3 must come before volume.
func (s *server) services() []catalogService {
	vol := "/volume/v3/" + s.topo.ProjectID
	return []catalogService{
		{"identity", "keystone", "/v3", "Keystone Identity Service"},
		{"compute", "nova", "/compute/v2.1", "Nova Compute Service"},
		{"placement", "placement", "/placement", "Placement Service"},
		{"image", "glance", "/image", "Glance Image Service"},
		{"volumev3", "cinderv3", vol, "Cinder Volume Service V3"},
		{"volume", "cinder", vol, "Cinder Volume Service"},
		{"network", "neutron", "/network", "Neutron Networking Service"},
		{"load-balancer", "octavia", "/load-balancer", "Octavia Load Balancing Service"},
	}
}

var interfaces = []string{"public", "internal", "admin"}

func serviceID(typ string) string         { return stableHex32("service", typ) }
func endpointID(typ, iface string) string { return stableHex32("endpoint", typ, iface) }
func roleID(name string) string           { return stableHex32("role", name) }
func (s *server) domainName() string {
	if s.topo.DomainID == "default" {
		return "Default"
	}
	return s.topo.DomainID
}

func (s *server) domainRef() obj { return obj{"id": s.topo.DomainID, "name": s.domainName()} }

func (s *server) endpointJSON(svc catalogService, iface string, withService bool) obj {
	e := obj{
		"id":        endpointID(svc.Type, iface),
		"interface": iface,
		"region":    s.topo.Region,
		"region_id": s.topo.Region,
		"url":       s.baseURL() + svc.Prefix,
	}
	if withService {
		e["service_id"] = serviceID(svc.Type)
		e["enabled"] = true
		e["links"] = obj{"self": s.baseURL() + "/v3/endpoints/" + e["id"].(string)}
	}
	return e
}

func (s *server) catalogJSON() []obj {
	var out []obj
	for _, svc := range s.services() {
		var eps []obj
		for _, iface := range interfaces {
			eps = append(eps, s.endpointJSON(svc, iface, false))
		}
		out = append(out, obj{"id": serviceID(svc.Type), "type": svc.Type, "name": svc.Name, "endpoints": eps})
	}
	return out
}

// tokenInfo is what the store remembers about an issued token.
type tokenInfo struct {
	Expires time.Time
	Issued  time.Time
	Scope   string // "project", "domain" or "" (unscoped)
	Methods []string
	AuditID string
}

type tokenStore struct {
	mu     sync.Mutex
	tokens map[string]tokenInfo
}

func newTokenStore() *tokenStore { return &tokenStore{tokens: map[string]tokenInfo{}} }

func (ts *tokenStore) add(id string, info tokenInfo, now time.Time) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for k, v := range ts.tokens {
		if !now.Before(v.Expires) {
			delete(ts.tokens, k)
		}
	}
	ts.tokens[id] = info
}

func (ts *tokenStore) get(id string, now time.Time) (tokenInfo, bool) {
	if id == "" {
		return tokenInfo{}, false
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	info, ok := ts.tokens[id]
	if !ok || !now.Before(info.Expires) {
		return tokenInfo{}, false
	}
	return info, true
}

func (ts *tokenStore) valid(id string, now time.Time) bool {
	_, ok := ts.get(id, now)
	return ok
}

func (ts *tokenStore) remove(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.tokens, id)
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type idName struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type authRequest struct {
	Auth struct {
		Identity struct {
			Methods  []string `json:"methods"`
			Password *struct {
				User struct {
					ID       string  `json:"id"`
					Name     string  `json:"name"`
					Domain   *idName `json:"domain"`
					Password string  `json:"password"`
				} `json:"user"`
			} `json:"password"`
			Token *struct {
				ID string `json:"id"`
			} `json:"token"`
		} `json:"identity"`
		Scope json.RawMessage `json:"scope"`
	} `json:"auth"`
}

type authScope struct {
	Project *struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Domain *idName `json:"domain"`
	} `json:"project"`
	Domain *idName `json:"domain"`
}

func (s *server) keystoneRoutes() {
	m := s.mux
	m.HandleFunc("GET /{$}", s.keystoneRoot)
	m.HandleFunc("GET /v3", s.keystoneVersion)
	m.HandleFunc("GET /v3/{$}", s.keystoneVersion)
	m.HandleFunc("POST /v3/auth/tokens", s.issueToken)
	m.HandleFunc("GET /v3/auth/tokens", s.validateToken)
	m.HandleFunc("DELETE /v3/auth/tokens", s.revokeToken)
	m.HandleFunc("GET /v3/auth/catalog", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, obj{"catalog": s.catalogJSON(), "links": s.listLinks("/v3/auth/catalog")})
	})
	m.HandleFunc("GET /v3/auth/projects", s.listProjects)
	m.HandleFunc("GET /v3/projects", s.listProjects)
	m.HandleFunc("GET /v3/users/{id}/projects", s.listProjects)
	m.HandleFunc("GET /v3/projects/{id}", s.getProject)
	m.HandleFunc("GET /v3/users", s.listUsers)
	m.HandleFunc("GET /v3/users/{id}", s.getUser)
	m.HandleFunc("GET /v3/domains", s.listDomains)
	m.HandleFunc("GET /v3/domains/{id}", s.getDomain)
	m.HandleFunc("GET /v3/roles", s.listRoles)
	m.HandleFunc("GET /v3/services", s.listServices)
	m.HandleFunc("GET /v3/services/{id}", s.getService)
	m.HandleFunc("GET /v3/endpoints", s.listEndpoints)
	m.HandleFunc("GET /v3/endpoints/{id}", s.getEndpoint)
	m.HandleFunc("GET /v3/regions", s.listRegions)
	m.HandleFunc("GET /v3/regions/{id}", s.getRegion)
}

func (s *server) keystoneVersionDoc(r *http.Request) obj {
	return obj{
		"id":      "v3.14",
		"status":  "stable",
		"updated": "2020-04-07T00:00:00Z",
		"links":   []obj{{"rel": "self", "href": requestBase(r) + "/v3/"}},
		"media-types": []obj{{
			"base": "application/json",
			"type": "application/vnd.openstack.identity-v3+json",
		}},
	}
}

func (s *server) keystoneRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusMultipleChoices, obj{"versions": obj{"values": []obj{s.keystoneVersionDoc(r)}}})
}

func (s *server) keystoneVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"version": s.keystoneVersionDoc(r)})
}

func (s *server) unauthorized(w http.ResponseWriter, msg string) {
	writeError(w, http.StatusUnauthorized, "error", msg)
}

func (s *server) issueToken(w http.ResponseWriter, r *http.Request) {
	var req authRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "error", "Malformed authentication request: "+err.Error())
		return
	}
	now := s.now()
	id := req.Auth.Identity
	methods := id.Methods
	switch {
	case id.Password != nil:
		u := id.Password.User
		if !s.userMatches(u.ID, u.Name, u.Domain) || !s.passwordMatches(u.Password) {
			s.unauthorized(w, "The request you have made requires authentication.")
			return
		}
	case id.Token != nil:
		if !s.tokens.valid(id.Token.ID, now) {
			s.unauthorized(w, "The request you have made requires authentication.")
			return
		}
	default:
		s.unauthorized(w, "Unsupported authentication method.")
		return
	}
	if len(methods) == 0 {
		methods = []string{"password"}
	}

	scope, ok := s.resolveScope(req.Auth.Scope)
	if !ok {
		s.unauthorized(w, "The request you have made requires authentication.")
		return
	}
	info := tokenInfo{
		Issued:  now,
		Expires: now.Add(s.cfg.TokenTTL),
		Scope:   scope,
		Methods: methods,
		AuditID: randomString(16),
	}
	tok := "gAAAAAB" + randomString(96)
	s.tokens.add(tok, info, now)
	w.Header().Set("X-Subject-Token", tok)
	writeJSON(w, http.StatusCreated, obj{"token": s.tokenBody(info)})
}

func (s *server) userMatches(id, name string, dom *idName) bool {
	switch {
	case id != "":
		return id == s.topo.UserID
	case name != s.topo.UserName:
		return false
	case dom == nil:
		return false
	case dom.ID != "":
		return dom.ID == s.topo.DomainID
	default:
		return dom.Name == s.domainName()
	}
}

func (s *server) passwordMatches(p string) bool {
	return subtle.ConstantTimeCompare([]byte(p), []byte(s.cfg.Password)) == 1
}

// resolveScope returns the token scope for the request. A scope that names
// an unknown project or domain is rejected like keystone does.
func (s *server) resolveScope(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == `"unscoped"` {
		return "", true
	}
	var sc authScope
	if err := json.Unmarshal(raw, &sc); err != nil {
		return "", false
	}
	switch {
	case sc.Project != nil:
		p := sc.Project
		if p.ID != "" {
			return "project", p.ID == s.topo.ProjectID
		}
		if p.Name != s.topo.ProjectName || p.Domain == nil {
			return "", false
		}
		if p.Domain.ID != "" {
			return "project", p.Domain.ID == s.topo.DomainID
		}
		return "project", p.Domain.Name == s.domainName()
	case sc.Domain != nil:
		if sc.Domain.ID != "" {
			return "domain", sc.Domain.ID == s.topo.DomainID
		}
		return "domain", sc.Domain.Name == s.domainName()
	}
	return "", true
}

func (s *server) tokenBody(info tokenInfo) obj {
	tok := obj{
		"methods":   info.Methods,
		"audit_ids": []string{info.AuditID},
		"issued_at": fmtTime(info.Issued, layoutMicroZ),
		// keystone always prints six fractional digits and a Z, which
		// openstack4j parses into KeystoneToken.expires (java.util.Date).
		"expires_at": fmtTime(info.Expires, layoutMicroZ),
		"user": obj{
			"id":                  s.topo.UserID,
			"name":                s.topo.UserName,
			"domain":              s.domainRef(),
			"password_expires_at": nil,
		},
	}
	switch info.Scope {
	case "project":
		tok["project"] = obj{"id": s.topo.ProjectID, "name": s.topo.ProjectName, "domain": s.domainRef()}
		tok["is_domain"] = false
	case "domain":
		tok["domain"] = s.domainRef()
	}
	if info.Scope != "" {
		tok["roles"] = s.rolesJSON(false)
		tok["catalog"] = s.catalogJSON()
	}
	return tok
}

func (s *server) validateToken(w http.ResponseWriter, r *http.Request) {
	info, ok := s.tokens.get(r.Header.Get("X-Subject-Token"), s.now())
	if !ok {
		writeError(w, http.StatusNotFound, "error", "Could not find token.")
		return
	}
	w.Header().Set("X-Subject-Token", r.Header.Get("X-Subject-Token"))
	writeJSON(w, http.StatusOK, obj{"token": s.tokenBody(info)})
}

func (s *server) revokeToken(w http.ResponseWriter, r *http.Request) {
	s.tokens.remove(r.Header.Get("X-Subject-Token"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) listLinks(path string) obj {
	return obj{"self": s.baseURL() + path, "previous": nil, "next": nil}
}

func (s *server) projectJSON() obj {
	return obj{
		"id":          s.topo.ProjectID,
		"name":        s.topo.ProjectName,
		"domain_id":   s.topo.DomainID,
		"description": "GPU emulator project",
		"enabled":     true,
		"parent_id":   s.topo.DomainID,
		"is_domain":   false,
		"tags":        []string{},
		"options":     obj{},
		"links":       obj{"self": s.baseURL() + "/v3/projects/" + s.topo.ProjectID},
	}
}

func (s *server) listProjects(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var out []obj
	if (q.Get("name") == "" || q.Get("name") == s.topo.ProjectName) &&
		(q.Get("domain_id") == "" || q.Get("domain_id") == s.topo.DomainID) {
		out = append(out, s.projectJSON())
	}
	writeJSON(w, http.StatusOK, obj{"projects": list(out), "links": s.listLinks(r.URL.Path)})
}

func (s *server) getProject(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.topo.ProjectID {
		writeError(w, http.StatusNotFound, "error", "Could not find project: "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"project": s.projectJSON()})
}

func (s *server) userJSON() obj {
	return obj{
		"id":                  s.topo.UserID,
		"name":                s.topo.UserName,
		"domain_id":           s.topo.DomainID,
		"default_project_id":  s.topo.ProjectID,
		"enabled":             true,
		"password_expires_at": nil,
		"options":             obj{},
		"links":               obj{"self": s.baseURL() + "/v3/users/" + s.topo.UserID},
	}
}

func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var out []obj
	if (q.Get("name") == "" || q.Get("name") == s.topo.UserName) &&
		(q.Get("domain_id") == "" || q.Get("domain_id") == s.topo.DomainID) {
		out = append(out, s.userJSON())
	}
	writeJSON(w, http.StatusOK, obj{"users": list(out), "links": s.listLinks("/v3/users")})
}

func (s *server) getUser(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.topo.UserID {
		writeError(w, http.StatusNotFound, "error", "Could not find user: "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"user": s.userJSON()})
}

func (s *server) domainJSON() obj {
	return obj{
		"id":          s.topo.DomainID,
		"name":        s.domainName(),
		"description": "The default domain",
		"enabled":     true,
		"tags":        []string{},
		"options":     obj{},
		"links":       obj{"self": s.baseURL() + "/v3/domains/" + s.topo.DomainID},
	}
}

func (s *server) listDomains(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"domains": []obj{s.domainJSON()}, "links": s.listLinks("/v3/domains")})
}

func (s *server) getDomain(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.topo.DomainID {
		writeError(w, http.StatusNotFound, "error", "Could not find domain: "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"domain": s.domainJSON()})
}

var roleNames = []string{"admin", "member", "reader"}

func (s *server) rolesJSON(full bool) []obj {
	var out []obj
	for _, n := range roleNames {
		r := obj{"id": roleID(n), "name": n}
		if full {
			r["domain_id"] = nil
			r["description"] = nil
			r["options"] = obj{}
			r["links"] = obj{"self": s.baseURL() + "/v3/roles/" + roleID(n)}
		}
		out = append(out, r)
	}
	return out
}

func (s *server) listRoles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"roles": s.rolesJSON(true), "links": s.listLinks("/v3/roles")})
}

func (s *server) serviceJSON(svc catalogService) obj {
	id := serviceID(svc.Type)
	return obj{
		"id":          id,
		"name":        svc.Name,
		"type":        svc.Type,
		"description": svc.Desc,
		"enabled":     true,
		"links":       obj{"self": s.baseURL() + "/v3/services/" + id},
	}
}

func (s *server) listServices(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	var out []obj
	for _, svc := range s.services() {
		if typ == "" || typ == svc.Type {
			out = append(out, s.serviceJSON(svc))
		}
	}
	writeJSON(w, http.StatusOK, obj{"services": list(out), "links": s.listLinks("/v3/services")})
}

func (s *server) getService(w http.ResponseWriter, r *http.Request) {
	for _, svc := range s.services() {
		if serviceID(svc.Type) == r.PathValue("id") {
			writeJSON(w, http.StatusOK, obj{"service": s.serviceJSON(svc)})
			return
		}
	}
	writeError(w, http.StatusNotFound, "error", "Could not find service: "+r.PathValue("id"))
}

func (s *server) allEndpoints() []obj {
	var out []obj
	for _, svc := range s.services() {
		for _, iface := range interfaces {
			out = append(out, s.endpointJSON(svc, iface, true))
		}
	}
	return out
}

func (s *server) listEndpoints(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var out []obj
	for _, e := range s.allEndpoints() {
		if (q.Get("interface") == "" || q.Get("interface") == e["interface"]) &&
			(q.Get("service_id") == "" || q.Get("service_id") == e["service_id"]) {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, obj{"endpoints": list(out), "links": s.listLinks("/v3/endpoints")})
}

func (s *server) getEndpoint(w http.ResponseWriter, r *http.Request) {
	for _, e := range s.allEndpoints() {
		if e["id"] == r.PathValue("id") {
			writeJSON(w, http.StatusOK, obj{"endpoint": e})
			return
		}
	}
	writeError(w, http.StatusNotFound, "error", "Could not find endpoint: "+r.PathValue("id"))
}

func (s *server) regionJSON() obj {
	return obj{
		"id":               s.topo.Region,
		"description":      "",
		"parent_region_id": nil,
		"links":            obj{"self": s.baseURL() + "/v3/regions/" + s.topo.Region},
	}
}

func (s *server) listRegions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"regions": []obj{s.regionJSON()}, "links": s.listLinks("/v3/regions")})
}

func (s *server) getRegion(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != s.topo.Region {
		writeError(w, http.StatusNotFound, "error", "Could not find region: "+r.PathValue("id"))
		return
	}
	writeJSON(w, http.StatusOK, obj{"region": s.regionJSON()})
}
