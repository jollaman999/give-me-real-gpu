package openstack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const (
	testPassword = "test-password" // hook:allow fake test credential
	testHost     = "192.0.2.10"
	testPort     = 15000
)

// fixture builds a small topology: a VGPU node (2 GPUs, 3 VMs), a MIG node
// (1 GPU, 2 instances, 1 VM) and a passthrough node (2 GPUs, 1 VM).
func fixture() *topology.Topology {
	vm := func(id, name, flavor, ip, hv string, gpu int, mode topology.GPUMode) *topology.VM {
		return &topology.VM{
			ID: id, Name: name, InstanceName: "instance-" + id[:8], FlavorID: flavor, IP: ip,
			Hypervisor: hv, GPUIndex: gpu, Mode: mode, MIGGPUInstanceID: -1,
			VCPUs: 8, MemoryMB: 32768, DiskGB: 100,
		}
	}
	vgpuProfile := &topology.VGPUProfile{Name: "NVIDIA A10-4Q", Type: "nvidia-592", FramebufferMB: 4096, MaxInstances: 6}
	migVM := vm("44444444-0000-4000-8000-000000000001", "emu-mig-vm-01", "f-mig", "198.51.100.21", "emu-mig-01", 0, topology.ModeMIG)
	migVM.MIGGPUInstanceID = 1
	return &topology.Topology{
		Region: "RegionOne", DomainID: "default",
		ProjectID: "0123456789abcdef0123456789abcdef", ProjectName: "gpu-emu",
		UserID: "fedcba9876543210fedcba9876543210", UserName: "emu-admin",
		Hypervisors: []*topology.Hypervisor{
			{
				ID: 1, UUID: "11111111-0000-4000-8000-000000000001", Hostname: "emu-vgpu-01", HostIP: "192.0.2.11",
				Mode: topology.ModeVGPU, VCPUs: 96, MemoryMB: 515072, LocalGB: 1800,
				GPUs: []*topology.GPU{
					{Index: 0, Profile: vgpuProfile, VMs: []*topology.VM{
						vm("22222222-0000-4000-8000-000000000001", "emu-vgpu-vm-01", "f-vgpu", "198.51.100.11", "emu-vgpu-01", 0, topology.ModeVGPU),
						vm("22222222-0000-4000-8000-000000000002", "emu-vgpu-vm-02", "f-vgpu", "198.51.100.12", "emu-vgpu-01", 0, topology.ModeVGPU),
					}},
					{Index: 1, Profile: vgpuProfile, VMs: []*topology.VM{
						vm("22222222-0000-4000-8000-000000000003", "emu-vgpu-vm-03", "f-vgpu", "198.51.100.13", "emu-vgpu-01", 1, topology.ModeVGPU),
					}},
				},
			},
			{
				ID: 2, UUID: "11111111-0000-4000-8000-000000000002", Hostname: "emu-mig-01", HostIP: "192.0.2.12",
				Mode: topology.ModeMIG, VCPUs: 128, MemoryMB: 1031168, LocalGB: 3600,
				GPUs: []*topology.GPU{{
					Index: 0,
					MIGInstances: []*topology.MIGInstance{
						{GPUInstanceID: 1, Profile: "3g.40gb", Slices: 3, MemoryMiB: 40192},
						{GPUInstanceID: 2, Profile: "3g.40gb", Slices: 3, MemoryMiB: 40192},
					},
					VMs: []*topology.VM{migVM},
				}},
			},
			{
				ID: 3, UUID: "11111111-0000-4000-8000-000000000003", Hostname: "emu-pt-01", HostIP: "192.0.2.13",
				Mode: topology.ModePassthrough, VCPUs: 64, MemoryMB: 257536, LocalGB: 900,
				GPUs: []*topology.GPU{
					{Index: 0, VMs: []*topology.VM{
						vm("33333333-0000-4000-8000-000000000001", "emu-pt-vm-01", "f-pt", "203.0.113.31", "emu-pt-01", 0, topology.ModePassthrough),
					}},
					{Index: 1},
				},
			},
		},
		Flavors: []*topology.Flavor{
			{ID: "f-vgpu", Name: "g1.vgpu.a10-4q", VCPUs: 8, RAMMB: 32768, DiskGB: 100,
				ExtraSpecs: map[string]string{"resources:VGPU": "1"}},
			{ID: "f-mig", Name: "g1.mig.3g40", VCPUs: 8, RAMMB: 32768, DiskGB: 100,
				ExtraSpecs: map[string]string{"resources:VGPU": "1", "trait:CUSTOM_MIG_3G_40GB": "required"}},
			{ID: "f-pt", Name: "g1.pt.l40s", VCPUs: 8, RAMMB: 32768, DiskGB: 100,
				ExtraSpecs: map[string]string{"pci_passthrough:alias": "l40s:1"}},
		},
	}
}

func newTestServer(t *testing.T) (*server, *topology.Topology) {
	t.Helper()
	topo := fixture()
	h := NewServer(topo, Config{Password: testPassword, AdvertiseHost: testHost, Port: testPort})
	return h.(*server), topo
}

type response struct {
	code   int
	header http.Header
	body   map[string]any
	raw    string
}

func call(t *testing.T, h http.Handler, method, path, token string, hdr map[string]string, body string) response {
	t.Helper()
	req := httptest.NewRequest(method, "http://"+testHost+":15000"+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := response{code: rec.Code, header: rec.Header(), raw: rec.Body.String()}
	if ct := rec.Header().Get("Content-Type"); rec.Code != http.StatusNoContent && ct != "application/json" {
		t.Fatalf("%s %s: Content-Type = %q, want application/json", method, path, ct)
	}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &res.body); err != nil {
			t.Fatalf("%s %s: body is not a JSON object: %v: %s", method, path, err, rec.Body.String())
		}
	}
	return res
}

func authBody(user, password, project string) string {
	return `{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"` + user +
		`","domain":{"id":"default"},"password":"` + password + `"}}},"scope":{"project":{"id":"` + project + `"}}}}`
}

func login(t *testing.T, s *server) string {
	t.Helper()
	r := call(t, s, "POST", "/v3/auth/tokens", "", nil, authBody(s.topo.UserName, testPassword, s.topo.ProjectID))
	if r.code != http.StatusCreated {
		t.Fatalf("token issue: status %d: %s", r.code, r.raw)
	}
	return r.header.Get("X-Subject-Token")
}

func items(t *testing.T, r response, key string) []map[string]any {
	t.Helper()
	if r.code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", r.code, r.raw)
	}
	raw, ok := r.body[key].([]any)
	if !ok {
		t.Fatalf("key %q is not an array: %s", key, r.raw)
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func TestIssueToken(t *testing.T) {
	s, topo := newTestServer(t)
	r := call(t, s, "POST", "/v3/auth/tokens", "", nil, authBody(topo.UserName, testPassword, topo.ProjectID))
	if r.code != http.StatusCreated {
		t.Fatalf("status %d, want 201: %s", r.code, r.raw)
	}
	if r.header.Get("X-Subject-Token") == "" {
		t.Fatal("X-Subject-Token header is empty")
	}
	tok := r.body["token"].(map[string]any)
	stamp := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z$`)
	for _, k := range []string{"expires_at", "issued_at"} {
		if v, _ := tok[k].(string); !stamp.MatchString(v) {
			t.Errorf("%s = %q, want keystone format 2006-01-02T15:04:05.000000Z", k, v)
		}
	}
	exp, _ := time.Parse(layoutMicroZ, tok["expires_at"].(string))
	if d := time.Until(exp); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("expires_at is %v from now, want the default 1h TTL", d)
	}
	if got := tok["project"].(map[string]any)["id"]; got != topo.ProjectID {
		t.Errorf("project id = %v, want %v", got, topo.ProjectID)
	}
	if got := tok["user"].(map[string]any)["name"]; got != topo.UserName {
		t.Errorf("user name = %v, want %v", got, topo.UserName)
	}
	roles := tok["roles"].([]any)
	if len(roles) == 0 || roles[0].(map[string]any)["name"] != "admin" {
		t.Errorf("roles = %v, want admin first", roles)
	}

	urls := map[string]map[string]string{}
	var order []string
	for _, c := range tok["catalog"].([]any) {
		svc := c.(map[string]any)
		typ := svc["type"].(string)
		order = append(order, typ)
		urls[typ] = map[string]string{}
		for _, e := range svc["endpoints"].([]any) {
			ep := e.(map[string]any)
			urls[typ][ep["interface"].(string)] = ep["url"].(string)
		}
	}
	base := "http://192.0.2.10:15000"
	want := map[string]string{
		"identity":      base + "/v3",
		"compute":       base + "/compute/v2.1",
		"placement":     base + "/placement",
		"image":         base + "/image",
		"volumev3":      base + "/volume/v3/" + topo.ProjectID,
		"volume":        base + "/volume/v3/" + topo.ProjectID,
		"network":       base + "/network",
		"load-balancer": base + "/load-balancer",
	}
	for typ, u := range want {
		for _, iface := range []string{"public", "internal", "admin"} {
			if urls[typ][iface] != u {
				t.Errorf("catalog %s %s url = %q, want %q", typ, iface, urls[typ][iface], u)
			}
		}
	}
	if strings.Join(order, ",") != "identity,compute,placement,image,volumev3,volume,network,load-balancer" {
		t.Errorf("catalog order = %v, volumev3 must precede volume", order)
	}
}

func TestIssueTokenRejects(t *testing.T) {
	s, topo := newTestServer(t)
	cases := map[string]string{
		"wrong password": authBody(topo.UserName, "wrong", topo.ProjectID),
		"wrong user":     authBody("nobody", testPassword, topo.ProjectID),
		"wrong project":  authBody(topo.UserName, testPassword, "00000000000000000000000000000000"),
		"no identity":    `{"auth":{"identity":{"methods":[]}}}`,
	}
	for name, body := range cases {
		r := call(t, s, "POST", "/v3/auth/tokens", "", nil, body)
		if r.code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, r.code)
			continue
		}
		if r.header.Get("X-Subject-Token") != "" {
			t.Errorf("%s: a token was issued", name)
		}
		if e, ok := r.body["error"].(map[string]any); !ok || e["code"] != float64(401) {
			t.Errorf("%s: body = %s, want keystone error with code 401", name, r.raw)
		}
	}
	if r := call(t, s, "POST", "/v3/auth/tokens", "", nil, "{"); r.code != http.StatusBadRequest {
		t.Errorf("malformed body: status %d, want 400", r.code)
	}
}

func TestTokenRequired(t *testing.T) {
	s, _ := newTestServer(t)
	for _, tok := range []string{"", "not-a-token"} {
		r := call(t, s, "GET", "/compute/v2.1/servers/detail", tok, nil, "")
		if r.code != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, r.code)
		}
	}
	tok := login(t, s)
	if r := call(t, s, "GET", "/compute/v2.1/servers/detail", tok, nil, ""); r.code != http.StatusOK {
		t.Fatalf("valid token: status %d, want 200", r.code)
	}
	s.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if r := call(t, s, "GET", "/compute/v2.1/servers/detail", tok, nil, ""); r.code != http.StatusUnauthorized {
		t.Errorf("expired token: status %d, want 401", r.code)
	}
}

func TestTokenValidationAndRevoke(t *testing.T) {
	s, _ := newTestServer(t)
	tok := login(t, s)
	hdr := map[string]string{"X-Subject-Token": tok}
	if r := call(t, s, "GET", "/v3/auth/tokens", tok, hdr, ""); r.code != http.StatusOK {
		t.Fatalf("validate: status %d, want 200", r.code)
	}
	if r := call(t, s, "DELETE", "/v3/auth/tokens", tok, hdr, ""); r.code != http.StatusNoContent {
		t.Fatalf("revoke: status %d, want 204", r.code)
	}
	if r := call(t, s, "GET", "/compute/v2.1/flavors", tok, nil, ""); r.code != http.StatusUnauthorized {
		t.Errorf("revoked token: status %d, want 401", r.code)
	}
}

func TestVersionDocumentsArePublic(t *testing.T) {
	s, _ := newTestServer(t)
	for _, p := range []string{"/v3", "/compute/", "/compute/v2.1", "/placement/", "/image/",
		"/volume/", "/volume/v3/", "/network/", "/load-balancer/"} {
		r := call(t, s, "GET", p, "", nil, "")
		if r.code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", p, r.code)
		}
	}
	r := call(t, s, "GET", "/compute/v2.1", "", nil, "")
	v := r.body["version"].(map[string]any)
	if v["version"] != "2.103" || v["min_version"] != "2.1" {
		t.Errorf("nova version = %v..%v, want 2.1..2.103", v["min_version"], v["version"])
	}
	if r := call(t, s, "GET", "/", "", nil, ""); r.code != http.StatusMultipleChoices {
		t.Errorf("/: status %d, want 300", r.code)
	}
}

func TestHypervisorsDetail(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	hvs := items(t, call(t, s, "GET", "/compute/v2.1/os-hypervisors/detail", tok, nil, ""), "hypervisors")
	if len(hvs) != len(topo.Hypervisors) {
		t.Fatalf("got %d hypervisors, want %d", len(hvs), len(topo.Hypervisors))
	}
	for i, h := range topo.Hypervisors {
		got := hvs[i]
		if got["hypervisor_hostname"] != h.Hostname || got["id"] != float64(h.ID) || got["host_ip"] != h.HostIP {
			t.Errorf("hypervisor %d = %v/%v/%v, want %s/%d/%s", i, got["hypervisor_hostname"], got["id"], got["host_ip"], h.Hostname, h.ID, h.HostIP)
		}
		u := usageOf(h)
		if got["vcpus"] != float64(h.VCPUs) || got["vcpus_used"] != float64(u.vcpus) || got["running_vms"] != float64(u.vms) {
			t.Errorf("%s vcpus %v used %v vms %v, want %d %d %d", h.Hostname, got["vcpus"], got["vcpus_used"], got["running_vms"], h.VCPUs, u.vcpus, u.vms)
		}
		if got["free_ram_mb"] != float64(h.MemoryMB-u.memMB) {
			t.Errorf("%s free_ram_mb = %v, want %d", h.Hostname, got["free_ram_mb"], h.MemoryMB-u.memMB)
		}
		// openstack4j only parses cpu_info given as a JSON string.
		ci, ok := got["cpu_info"].(string)
		if !ok {
			t.Fatalf("%s cpu_info is %T, want string", h.Hostname, got["cpu_info"])
		}
		var parsed struct {
			Topology struct{ Sockets, Cores, Threads int } `json:"topology"`
		}
		if err := json.Unmarshal([]byte(ci), &parsed); err != nil {
			t.Fatalf("cpu_info is not JSON: %v", err)
		}
		if tp := parsed.Topology; tp.Sockets*tp.Cores*tp.Threads != h.VCPUs {
			t.Errorf("%s topology %+v does not multiply to %d vcpus", h.Hostname, tp, h.VCPUs)
		}
		if svc := got["service"].(map[string]any); svc["host"] != h.Hostname {
			t.Errorf("service host = %v, want %s", svc["host"], h.Hostname)
		}
	}
	if u := usageOf(topo.Hypervisors[0]); u.vcpus != 24 || u.vms != 3 || u.memMB != 3*32768+reservedHostMemoryMB {
		t.Errorf("usage of VGPU node = %+v, want 24 vcpus, 3 vms", u)
	}

	newer := items(t, call(t, s, "GET", "/compute/v2.1/os-hypervisors/detail", tok,
		map[string]string{"OpenStack-API-Version": "compute 2.53"}, ""), "hypervisors")
	if newer[0]["id"] != topo.Hypervisors[0].UUID {
		t.Errorf("2.53 id = %v, want uuid %s", newer[0]["id"], topo.Hypervisors[0].UUID)
	}
	if _, ok := newer[0]["cpu_info"].(map[string]any); !ok {
		t.Errorf("2.53 cpu_info is %T, want object", newer[0]["cpu_info"])
	}

	for _, key := range []string{"2", topo.Hypervisors[1].UUID, topo.Hypervisors[1].Hostname} {
		r := call(t, s, "GET", "/compute/v2.1/os-hypervisors/"+key, tok, nil, "")
		if r.code != http.StatusOK || r.body["hypervisor"].(map[string]any)["hypervisor_hostname"] != "emu-mig-01" {
			t.Errorf("get hypervisor %q: status %d body %s", key, r.code, r.raw)
		}
	}
	stats := call(t, s, "GET", "/compute/v2.1/os-hypervisors/statistics", tok, nil, "")
	if st := stats.body["hypervisor_statistics"].(map[string]any); st["count"] != float64(3) || st["running_vms"] != float64(5) {
		t.Errorf("statistics = %v, want count 3 running_vms 5", st)
	}
}

func TestHypervisorServers(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	for _, h := range topo.Hypervisors {
		hvs := items(t, call(t, s, "GET", "/compute/v2.1/os-hypervisors/"+h.Hostname+"/servers", tok, nil, ""), "hypervisors")
		if hvs[0]["hypervisor_hostname"] != h.Hostname {
			t.Fatalf("first entry is %v, want exact match %s", hvs[0]["hypervisor_hostname"], h.Hostname)
		}
		want := vmsOn(h)
		servers, _ := hvs[0]["servers"].([]any)
		if len(servers) != len(want) {
			t.Fatalf("%s: got %d servers, want %d", h.Hostname, len(servers), len(want))
		}
		for i, sv := range servers {
			m := sv.(map[string]any)
			if m["uuid"] != want[i].ID || m["name"] != want[i].InstanceName {
				t.Errorf("%s server %d = %v, want %s %s", h.Hostname, i, m, want[i].ID, want[i].InstanceName)
			}
		}
	}
	all := items(t, call(t, s, "GET", "/compute/v2.1/os-hypervisors/emu-/servers", tok, nil, ""), "hypervisors")
	if len(all) != 3 {
		t.Errorf("substring match returned %d hypervisors, want 3", len(all))
	}
	if r := call(t, s, "GET", "/compute/v2.1/os-hypervisors/nothing/servers", tok, nil, ""); r.code != http.StatusNotFound {
		t.Errorf("unknown host: status %d, want 404", r.code)
	}
}

func TestServersDetail(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	mv27 := map[string]string{"OpenStack-API-Version": "compute 2.27"}
	r := call(t, s, "GET", "/compute/v2.1/servers/detail?all_tenants=1", tok, mv27, "")
	if r.header.Get("OpenStack-API-Version") != "compute 2.27" {
		t.Errorf("response microversion = %q, want compute 2.27", r.header.Get("OpenStack-API-Version"))
	}
	servers := items(t, r, "servers")
	vms := topo.AllVMs()
	if len(servers) != len(vms) {
		t.Fatalf("got %d servers, want %d", len(servers), len(vms))
	}
	launched := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}$`)
	created := regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
	for i, vm := range vms {
		sv := servers[i]
		if sv["id"] != vm.ID || sv["name"] != vm.Name {
			t.Errorf("server %d = %v %v, want %s %s", i, sv["id"], sv["name"], vm.ID, vm.Name)
		}
		if sv["OS-EXT-SRV-ATTR:hypervisor_hostname"] != vm.Hypervisor || sv["OS-EXT-SRV-ATTR:host"] != vm.Hypervisor {
			t.Errorf("%s hypervisor = %v, want %s", vm.Name, sv["OS-EXT-SRV-ATTR:hypervisor_hostname"], vm.Hypervisor)
		}
		if sv["OS-EXT-SRV-ATTR:instance_name"] != vm.InstanceName || sv["tenant_id"] != topo.ProjectID {
			t.Errorf("%s instance_name/tenant = %v/%v", vm.Name, sv["OS-EXT-SRV-ATTR:instance_name"], sv["tenant_id"])
		}
		fl := sv["flavor"].(map[string]any)
		if fl["id"] != vm.FlavorID || fl["links"] == nil || fl["original_name"] != nil {
			t.Errorf("%s flavor at 2.27 = %v, want {id, links}", vm.Name, fl)
		}
		if sv["image"] != "" {
			t.Errorf("%s image = %v, want empty string", vm.Name, sv["image"])
		}
		addrs := sv["addresses"].(map[string]any)["emu-net"].([]any)[0].(map[string]any)
		if addrs["addr"] != vm.IP || addrs["version"] != float64(4) || addrs["OS-EXT-IPS:type"] != "fixed" ||
			!regexp.MustCompile(`^fa:16:3e(:[0-9a-f]{2}){3}$`).MatchString(addrs["OS-EXT-IPS-MAC:mac_addr"].(string)) {
			t.Errorf("%s address = %v", vm.Name, addrs)
		}
		if !launched.MatchString(sv["OS-SRV-USG:launched_at"].(string)) || !created.MatchString(sv["created"].(string)) {
			t.Errorf("%s times launched_at=%v created=%v", vm.Name, sv["OS-SRV-USG:launched_at"], sv["created"])
		}
		if sv["status"] != "ACTIVE" || sv["OS-EXT-STS:power_state"] != float64(1) || sv["OS-EXT-STS:task_state"] != nil {
			t.Errorf("%s state = %v %v %v", vm.Name, sv["status"], sv["OS-EXT-STS:power_state"], sv["OS-EXT-STS:task_state"])
		}
	}

	r = call(t, s, "GET", "/compute/v2.1/servers/"+vms[0].ID, tok, map[string]string{"OpenStack-API-Version": "compute 2.47"}, "")
	fl := r.body["server"].(map[string]any)["flavor"].(map[string]any)
	if fl["original_name"] != "g1.vgpu.a10-4q" || fl["extra_specs"].(map[string]any)["resources:VGPU"] != "1" {
		t.Errorf("flavor at 2.47 = %v, want embedded flavor", fl)
	}
	if r := call(t, s, "GET", "/compute/v2.1/servers/00000000-0000-0000-0000-000000000000", tok, nil, ""); r.code != http.StatusNotFound || r.body["itemNotFound"] == nil {
		t.Errorf("unknown server: status %d body %s", r.code, r.raw)
	}
}

func TestServerDiagnostics(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	vm := topo.AllVMs()[0]
	r := call(t, s, "GET", "/compute/v2.1/servers/"+vm.ID+"/diagnostics", tok, nil, "")
	if r.code != http.StatusOK {
		t.Fatalf("status %d", r.code)
	}
	cpus := 0
	for k, v := range r.body {
		if _, ok := v.(float64); !ok {
			t.Errorf("diagnostics %s = %v (%T), every value must be a number", k, v, v)
		}
		if regexp.MustCompile(`^cpu\d+_time$`).MatchString(k) {
			cpus++
		}
	}
	if cpus != vm.VCPUs {
		t.Errorf("got %d cpuN_time keys, want %d", cpus, vm.VCPUs)
	}
	rss := r.body["memory-rss"].(float64)
	if rss <= 0 || rss > float64(vm.MemoryMB*1024) {
		t.Errorf("memory-rss = %v KiB, want within (0, %d]", rss, vm.MemoryMB*1024)
	}
}

func TestFlavors(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	fls := items(t, call(t, s, "GET", "/compute/v2.1/flavors/detail", tok,
		map[string]string{"OpenStack-API-Version": "compute 2.61"}, ""), "flavors")
	if len(fls) != len(topo.Flavors) {
		t.Fatalf("got %d flavors, want %d", len(fls), len(topo.Flavors))
	}
	specs := map[string]map[string]any{}
	for _, f := range fls {
		specs[f["id"].(string)] = f["extra_specs"].(map[string]any)
		if f["swap"] != float64(0) || f["OS-FLV-EXT-DATA:ephemeral"] != float64(0) {
			t.Errorf("flavor %v swap/ephemeral = %v/%v, want 0/0", f["id"], f["swap"], f["OS-FLV-EXT-DATA:ephemeral"])
		}
	}
	if specs["f-vgpu"]["resources:VGPU"] != "1" || specs["f-pt"]["pci_passthrough:alias"] != "l40s:1" {
		t.Errorf("extra_specs = %v", specs)
	}
	r := call(t, s, "GET", "/compute/v2.1/flavors/f-pt", tok, nil, "")
	if f := r.body["flavor"].(map[string]any); f["name"] != "g1.pt.l40s" {
		t.Errorf("flavor get = %v", f)
	}
	r = call(t, s, "GET", "/compute/v2.1/flavors/f-mig/os-extra_specs", tok, nil, "")
	if es := r.body["extra_specs"].(map[string]any); es["resources:VGPU"] != "1" || len(es) != 2 {
		t.Errorf("os-extra_specs = %v", es)
	}
	if r := call(t, s, "GET", "/compute/v2.1/flavors/nope", tok, nil, ""); r.code != http.StatusNotFound {
		t.Errorf("unknown flavor: status %d, want 404", r.code)
	}
}

func TestComputeServices(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	svcs := items(t, call(t, s, "GET", "/compute/v2.1/os-services", tok, nil, ""), "services")
	hosts := map[string]bool{}
	for _, sv := range svcs {
		if sv["binary"] == "nova-compute" {
			hosts[sv["host"].(string)] = true
		}
		if sv["state"] != "up" || sv["status"] != "enabled" {
			t.Errorf("service %v state/status = %v/%v", sv["host"], sv["state"], sv["status"])
		}
	}
	if len(hosts) != len(topo.Hypervisors) || len(svcs) != len(topo.Hypervisors)+2 {
		t.Errorf("got %d services (%d compute), want %d compute + conductor + scheduler", len(svcs), len(hosts), len(topo.Hypervisors))
	}
}

func TestPlacementProviders(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	rps := items(t, call(t, s, "GET", "/placement/resource_providers", tok, nil, ""), "resource_providers")
	if len(rps) != len(topo.Hypervisors) {
		t.Fatalf("got %d providers, want %d", len(rps), len(topo.Hypervisors))
	}
	for i, h := range topo.Hypervisors {
		if rps[i]["name"] != h.Hostname || rps[i]["uuid"] != h.UUID {
			t.Errorf("provider %d = %v %v, want %s %s", i, rps[i]["name"], rps[i]["uuid"], h.Hostname, h.UUID)
		}
	}
}

func TestNeutronModel(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	nets := items(t, call(t, s, "GET", "/network/v2.0/networks", tok, nil, ""), "networks")
	subnets := items(t, call(t, s, "GET", "/network/v2.0/subnets", tok, nil, ""), "subnets")
	ports := items(t, call(t, s, "GET", "/network/v2.0/ports", tok, nil, ""), "ports")
	if len(nets) != 1 || nets[0]["name"] != "emu-net" {
		t.Fatalf("networks = %v", nets)
	}
	// 198.51.100.0/24 and 203.0.113.0/24
	if len(subnets) != 2 || subnets[0]["cidr"] != "198.51.100.0/24" || subnets[1]["cidr"] != "203.0.113.0/24" {
		t.Errorf("subnets = %v", subnets)
	}
	if len(ports) != len(topo.AllVMs()) {
		t.Errorf("got %d ports, want one per VM (%d)", len(ports), len(topo.AllVMs()))
	}
	for _, p := range ports {
		ip := p["fixed_ips"].([]any)[0].(map[string]any)
		vm := s.findVM(p["device_id"].(string))
		if vm == nil || ip["ip_address"] != vm.IP || p["binding:host_id"] != vm.Hypervisor {
			t.Errorf("port %v does not match its VM", p["id"])
		}
	}
	byDevice := items(t, call(t, s, "GET", "/network/v2.0/ports?device_id="+topo.AllVMs()[4].ID, tok, nil, ""), "ports")
	if len(byDevice) != 1 {
		t.Errorf("device_id filter returned %d ports, want 1", len(byDevice))
	}
	agents := items(t, call(t, s, "GET", "/network/v2.0/agents", tok, nil, ""), "agents")
	if len(agents) != 4+len(topo.Hypervisors) {
		t.Errorf("got %d agents", len(agents))
	}
}

func TestEmptyLists(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	vol := "/volume/v3/" + topo.ProjectID
	cases := []struct{ path, key string }{
		{"/compute/v2.1/os-keypairs", "keypairs"},
		{"/compute/v2.1/os-aggregates", "aggregates"},
		{"/compute/v2.1/os-server-groups", "server_groups"},
		{"/image/v2/images?limit=25", "images"},
		{vol + "/volumes", "volumes"},
		{vol + "/volumes/detail?all_tenants=1", "volumes"},
		{vol + "/snapshots/detail", "snapshots"},
		{vol + "/backups/detail", "backups"},
		{vol + "/v3/" + topo.ProjectID + "/volumes", "volumes"},
		{"/network/v2.0/routers", "routers"},
		{"/network/v2.0/floatingips", "floatingips"},
		{"/network/v2.0/qos/policies", "policies"},
		{"/network/v2.0/rbac-policies", "rbac_policies"},
		{"/load-balancer/v2.0/lbaas/loadbalancers", "loadbalancers"},
		{"/load-balancer/v2/lbaas/loadbalancers", "loadbalancers"},
		{"/load-balancer/v2.0/lbaas/pools", "pools"},
	}
	for _, c := range cases {
		got := items(t, call(t, s, "GET", c.path, tok, nil, ""), c.key)
		if len(got) != 0 {
			t.Errorf("%s: got %d items, want 0", c.path, len(got))
		}
	}
	types := items(t, call(t, s, "GET", vol+"/types", tok, nil, ""), "volume_types")
	if len(types) != 1 || types[0]["name"] != "__DEFAULT__" {
		t.Errorf("volume types = %v", types)
	}
}

func TestIdentityLists(t *testing.T) {
	s, topo := newTestServer(t)
	tok := login(t, s)
	eps := items(t, call(t, s, "GET", "/v3/endpoints", tok, nil, ""), "endpoints")
	svcs := items(t, call(t, s, "GET", "/v3/services", tok, nil, ""), "services")
	if len(svcs) != 8 || len(eps) != 8*3 {
		t.Fatalf("got %d services %d endpoints, want 8 and 24", len(svcs), len(eps))
	}
	for _, e := range eps {
		r := call(t, s, "GET", "/v3/services/"+e["service_id"].(string), tok, nil, "")
		if r.code != http.StatusOK {
			t.Errorf("service %v of endpoint %v: status %d", e["service_id"], e["id"], r.code)
		}
	}
	projects := items(t, call(t, s, "GET", "/v3/projects", tok, nil, ""), "projects")
	if len(projects) != 1 || projects[0]["id"] != topo.ProjectID {
		t.Errorf("projects = %v", projects)
	}
	users := items(t, call(t, s, "GET", "/v3/users?name="+topo.UserName, tok, nil, ""), "users")
	if len(users) != 1 || users[0]["id"] != topo.UserID {
		t.Errorf("users = %v", users)
	}
	if got := items(t, call(t, s, "GET", "/v3/users?name=other", tok, nil, ""), "users"); len(got) != 0 {
		t.Errorf("name filter returned %v", got)
	}
}

func TestUnknownAndWrites(t *testing.T) {
	s, _ := newTestServer(t)
	tok := login(t, s)
	r := call(t, s, "GET", "/compute/v2.1/os-nothing", tok, nil, "")
	if e, ok := r.body["itemNotFound"].(map[string]any); r.code != http.StatusNotFound || !ok || e["code"] != float64(404) {
		t.Errorf("unknown path: status %d body %s", r.code, r.raw)
	}
	r = call(t, s, "POST", "/compute/v2.1/servers", tok, nil, `{"server":{}}`)
	if r.code != http.StatusForbidden {
		t.Errorf("create server: status %d, want 403", r.code)
	}
}

func TestIDsAreDeterministic(t *testing.T) {
	a, _ := newTestServer(t)
	b, _ := newTestServer(t)
	catA, _ := json.Marshal(a.catalogJSON())
	catB, _ := json.Marshal(b.catalogJSON())
	if string(catA) != string(catB) {
		t.Error("catalog differs between two servers built from the same topology")
	}
	if a.net.id != b.net.id || a.net.ports[0].id != b.net.ports[0].id || a.net.sgID != b.net.sgID {
		t.Error("network ids differ between two servers built from the same topology")
	}
	if got := stableUUID("x"); !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(got) {
		t.Errorf("stableUUID = %q, want uuid v5 layout", got)
	}
}

func TestMicroversionParsing(t *testing.T) {
	cases := map[string]int{"": 1, "compute 2.27": 27, "compute latest": 103, "compute 2.999": 103, "volume 3.5": 1, "compute 2.x": 1}
	for h, want := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		if h != "" {
			req.Header.Set("OpenStack-API-Version", h)
		}
		if got := novaMicroversion(req); got != want {
			t.Errorf("%q: got 2.%d, want 2.%d", h, got, want)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-OpenStack-Nova-API-Version", "2.30")
	if got := novaMicroversion(req); got != 30 {
		t.Errorf("legacy header: got 2.%d, want 2.30", got)
	}
}
