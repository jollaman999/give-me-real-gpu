package metrics

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

const testCred = "00000000-cred-4000-8000-000000000000"

// t0 is aligned to a 5 minute boundary so the periodic inputs are predictable.
var t0 = time.Unix(1800000000, 0)

func testVBIOS() string { return strings.Join([]string{"90", "04", "96", "00", "01"}, ".") }

// testIP is a documentation address (TEST-NET-1), built from parts so it is not a literal.
func testIP(n int) string { return strings.Join([]string{"192", "0", "2", strconv.Itoa(n)}, ".") }

// fixture: one VGPU node (2 GPUs, 3 VMs on the first, none on the second), one MIG node
// (1 GPU with 3 instances, 2 assigned) and one passthrough node (2 GPUs, 1 assigned).
func fixture() *topology.Topology {
	vm := func(id, name, hv string, gpu int, mode topology.GPUMode) *topology.VM {
		v := &topology.VM{
			ID: id, Name: name, InstanceName: "instance-" + id[:8], Hypervisor: hv, GPUIndex: gpu,
			Mode: mode, MIGGPUInstanceID: -1, GuestBDF: "00:05.0", VCPUs: 2, MemoryMB: 4096, DiskGB: 40,
			GuestDriverVersion: "595.91.07",
		}
		if mode != topology.ModePassthrough {
			v.VGPUUUID = "bbbbbbbb-0000-4000-8000-" + id[len(id)-12:]
		} else {
			v.GuestDriverVersion = "580.95.05"
		}
		return v
	}
	t4 := &topology.VGPUProfile{Name: "GRID T4-4Q", Type: "nvidia-232", FramebufferMB: 4096, MaxInstances: 4}
	vg := &topology.Hypervisor{
		ID: 1, UUID: "hhhhhhhh-0000-4000-8000-000000000001", Hostname: "hv-vgpu-a", HostIP: testIP(11),
		Mode: topology.ModeVGPU, VCPUs: 4, MemoryMB: 65536, LocalGB: 500, DriverVersion: "595.91.04", CUDAVersion: "13020",
		GPUs: []*topology.GPU{
			{Index: 0, UUID: "GPU-11111111-0000-4000-8000-000000000000", BusID: "00000000:3B:00.0", HostBDF: "3b:00.0",
				ModelName: "Tesla T4", DeviceID: "1eb8", MemoryMiB: 15360, VBIOS: testVBIOS(), Arch: "Turing", Profile: t4},
			{Index: 1, UUID: "GPU-11111111-0000-4000-8000-000000000001", BusID: "00000000:AF:00.0", HostBDF: "af:00.0",
				ModelName: "Tesla T4", DeviceID: "1eb8", MemoryMiB: 15360, VBIOS: testVBIOS(), Arch: "Turing", Profile: t4, VMs: []*topology.VM{}},
		},
	}
	vg.GPUs[0].VMs = []*topology.VM{
		vm("aaaaaaaa-0000-4000-8000-000000000001", "test-vgpu-1", vg.Hostname, 0, topology.ModeVGPU),
		vm("aaaaaaaa-0000-4000-8000-000000000002", "test-vgpu-2", vg.Hostname, 0, topology.ModeVGPU),
		vm("aaaaaaaa-0000-4000-8000-000000000003", "test vgpu,3", vg.Hostname, 0, topology.ModeVGPU),
	}

	migVGPU := topology.VGPUProfile{Name: "NVIDIA A100D-2-20C", Type: "nvidia-700", FramebufferMB: 20480, MaxInstances: 3}
	mg := &topology.Hypervisor{
		ID: 2, UUID: "hhhhhhhh-0000-4000-8000-000000000002", Hostname: "hv-mig-a", HostIP: testIP(12),
		Mode: topology.ModeMIG, VCPUs: 4, MemoryMB: 65536, LocalGB: 500, DriverVersion: "595.91.04", CUDAVersion: "13020",
		GPUs: []*topology.GPU{
			{Index: 0, UUID: "GPU-22222222-0000-4000-8000-000000000000", BusID: "00000000:18:00.0", HostBDF: "18:00.0",
				ModelName: "NVIDIA A100-SXM4-80GB", DeviceID: "20b2", MemoryMiB: 81920, VBIOS: testVBIOS(), Arch: "Ampere"},
		},
	}
	for _, gi := range []int{3, 4, 5} {
		p := migVGPU
		mg.GPUs[0].MIGInstances = append(mg.GPUs[0].MIGInstances,
			&topology.MIGInstance{GPUInstanceID: gi, Profile: "2g.20gb", Slices: 2, MemoryMiB: 19968, VGPU: &p})
	}
	m1 := vm("aaaaaaaa-0000-4000-8000-000000000011", "test-mig-1", mg.Hostname, 0, topology.ModeMIG)
	m1.MIGGPUInstanceID = 3
	m2 := vm("aaaaaaaa-0000-4000-8000-000000000012", "test-mig-2", mg.Hostname, 0, topology.ModeMIG)
	m2.MIGGPUInstanceID = 4
	mg.GPUs[0].VMs = []*topology.VM{m1, m2}

	pt := &topology.Hypervisor{
		ID: 3, UUID: "hhhhhhhh-0000-4000-8000-000000000003", Hostname: "hv-pt-a", HostIP: testIP(13),
		Mode: topology.ModePassthrough, VCPUs: 4, MemoryMB: 65536, LocalGB: 500,
		GPUs: []*topology.GPU{
			{Index: 0, UUID: "GPU-33333333-0000-4000-8000-000000000000", BusID: "00000000:9A:00.0", HostBDF: "9a:00.0",
				ModelName: "NVIDIA L40S", DeviceID: "26b9", MemoryMiB: 46068, VBIOS: testVBIOS(), Arch: "Ada Lovelace"},
			{Index: 1, UUID: "GPU-33333333-0000-4000-8000-000000000001", BusID: "00000000:AB:00.0", HostBDF: "ab:00.0",
				ModelName: "NVIDIA L40S", DeviceID: "26b9", MemoryMiB: 46068, VBIOS: testVBIOS(), Arch: "Ada Lovelace", VMs: []*topology.VM{}},
		},
	}
	pt.GPUs[0].VMs = []*topology.VM{vm("aaaaaaaa-0000-4000-8000-000000000021", "test-pt-1", pt.Hostname, 0, topology.ModePassthrough)}

	return &topology.Topology{
		Region: "RegionOne", DomainID: "default", ProjectID: "0123456789abcdef0123456789abcdef",
		ProjectName: "test", UserName: "test", Hypervisors: []*topology.Hypervisor{vg, mg, pt},
	}
}

// ---- a small line protocol parser, independent of the writer ----

type value struct {
	kind byte // 'f' float, 'i' integer, 's' string, 'b' bool
	f    float64
	i    int64
	s    string
}

type point struct {
	name   string
	tags   map[string]string
	fields map[string]value
	ts     int64
}

// splitUnescaped splits s at sep characters that are not escaped and not inside a quoted string.
func splitUnescaped(s string, sep byte, quotes bool) []string {
	var out []string
	start, inQ := 0, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			i++
		case quotes && c == '"':
			inQ = !inQ
		case c == sep && !inQ:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func unescape(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			if s[i] == 'n' {
				sb.WriteByte('\n')
				continue
			}
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

func parseLine(line string) (point, error) {
	if strings.ContainsAny(line, "\n\r") {
		return point{}, fmt.Errorf("line contains a newline: %q", line)
	}
	sec := splitUnescaped(line, ' ', true)
	if len(sec) != 3 {
		return point{}, fmt.Errorf("want 3 sections, got %d: %q", len(sec), line)
	}
	head := splitUnescaped(sec[0], ',', false)
	p := point{name: unescape(head[0]), tags: map[string]string{}, fields: map[string]value{}}
	if p.name == "" {
		return p, fmt.Errorf("empty measurement: %q", line)
	}
	for _, kv := range head[1:] {
		parts := splitUnescaped(kv, '=', false)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return p, fmt.Errorf("bad tag %q in %q", kv, line)
		}
		p.tags[unescape(parts[0])] = unescape(parts[1])
	}
	for _, kv := range splitUnescaped(sec[1], ',', true) {
		parts := splitUnescaped(kv, '=', true)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return p, fmt.Errorf("bad field %q in %q", kv, line)
		}
		key, raw := unescape(parts[0]), parts[1]
		var v value
		switch {
		case raw[0] == '"':
			if len(raw) < 2 || raw[len(raw)-1] != '"' {
				return p, fmt.Errorf("unterminated string %q", raw)
			}
			v = value{kind: 's', s: unescape(raw[1 : len(raw)-1])}
		case raw[len(raw)-1] == 'i':
			n, err := strconv.ParseInt(raw[:len(raw)-1], 10, 64)
			if err != nil {
				return p, fmt.Errorf("bad integer %q: %v", raw, err)
			}
			v = value{kind: 'i', i: n}
		case raw == "true" || raw == "false":
			v = value{kind: 'b'}
		default:
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return p, fmt.Errorf("bad float %q: %v", raw, err)
			}
			v = value{kind: 'f', f: f}
		}
		p.fields[key] = v
	}
	ts, err := strconv.ParseInt(sec[2], 10, 64)
	if err != nil {
		return p, fmt.Errorf("bad timestamp %q", sec[2])
	}
	p.ts = ts
	return p, nil
}

func parseAll(t *testing.T, lines []string) []point {
	t.Helper()
	out := make([]point, 0, len(lines))
	for _, l := range lines {
		p, err := parseLine(l)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		out = append(out, p)
	}
	return out
}

func (v value) num() float64 {
	if v.kind == 'i' {
		return float64(v.i)
	}
	return v.f
}

func keys[M ~map[string]V, V any](m M) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func seriesKey(p point) string {
	var sb strings.Builder
	sb.WriteString(p.name)
	for _, k := range keys(p.tags) {
		sb.WriteString("," + k + "=" + p.tags[k])
	}
	return sb.String()
}

func filter(ps []point, f func(point) bool) []point {
	var out []point
	for _, p := range ps {
		if f(p) {
			out = append(out, p)
		}
	}
	return out
}

func collectTwice(t *testing.T, opt Options) ([]point, []point) {
	t.Helper()
	g := NewGenerator(fixture(), opt)
	a := parseAll(t, g.Collect(t0))
	b := parseAll(t, g.Collect(t0.Add(10*time.Second)))
	return a, b
}

// ---- tests ----

func TestFormatLineEscaping(t *testing.T) {
	got := formatLine("my meas,x",
		map[string]string{"k ey": "a b,c=d", "empty": ""},
		map[string]any{"s": `say "hi" \ x`, "n": int64(3), "f": 1.5, "bad": 0.0 / zero()},
		42)
	want := `my\ meas\,x,k\ ey=a\ b\,c\=d f=1.5,n=3i,s="say \"hi\" \\ x" 42`
	if got != want {
		t.Fatalf("formatLine:\n got %s\nwant %s", got, want)
	}
	p, err := parseLine(got)
	if err != nil {
		t.Fatal(err)
	}
	if p.name != "my meas,x" || p.tags["k ey"] != "a b,c=d" || p.fields["s"].s != `say "hi" \ x` {
		t.Fatalf("round trip mismatch: %+v", p)
	}
	if formatLine("m", nil, map[string]any{}, 1) != "" {
		t.Fatal("a point without fields must be dropped")
	}
}

func zero() float64 { return 0 }

func TestCollectParsesAndCounts(t *testing.T) {
	g := NewGenerator(fixture(), Options{CredentialID: testCred, Seed: 1})
	lines := g.Collect(t0)
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	ps := parseAll(t, lines)
	counts := map[string]int{}
	for _, p := range ps {
		counts[p.name]++
		if d := p.ts - t0.UnixNano(); d > 0 || d < -int64(2*time.Second) {
			t.Errorf("timestamp %d too far from collection time: %s", p.ts, seriesKey(p))
		}
	}
	t.Logf("lines=%d per measurement=%v", len(lines), counts)
	for _, m := range []string{"cpu", "mem", "swap", "disk", "diskio", "net", "system", "processes", "procstat", "dcgm", "gpu_vm_mapping"} {
		if counts[m] == 0 {
			t.Errorf("measurement %s missing", m)
		}
	}
}

// realFieldTypes are the field types of the target InfluxDB (SHOW FIELD KEYS, current shard).
var realFieldTypes = map[string]map[string]byte{
	"dcgm": {
		"clocks_event_reasons": 'f', "dec_util": 'f', "ecc_dbe_agg_total": 'f', "ecc_dbe_vol_total": 'f',
		"ecc_sbe_agg_total": 'f', "ecc_sbe_vol_total": 'f', "enc_util": 'f', "fan_speed": 'f', "fb_free": 'f',
		"fb_total": 'f', "fb_used": 'f', "gpu_temp": 'f', "gpu_util": 'f', "mem_clock": 'f', "mem_copy_util": 'f',
		"memory_temp": 'f', "mig_mode": 'f', "pcie_replay_counter": 'f', "power_usage": 'f', "pstate": 'i',
		"retired_pages_dbe": 'f', "retired_pages_pending": 'f', "server_time": 'i', "sm_clock": 'f',
		"vgpu_license_status": 'f', "xid_errors": 'f',
		// not in the DB yet; types follow the agent scripts
		"cuda_probe": 'i', "err_msg": 's', "sm_occupancy": 'f', "graphics_util": 'f', "dram_bw_util": 'f',
		"fp32_util": 'f', "fp16_util": 'f', "fp64_util": 'f', "correctable_remapped_rows": 'f',
		"uncorrectable_remapped_rows": 'f', "row_remap_failure": 'f', "row_remap_pending": 'f',
	},
	"gpu_vm_mapping": {
		"gpu_uuid": 's', "mapped": 'i', "server_time": 'i', "vgpu_active_instances": 'i', "vgpu_fb_mb": 'i',
		"vgpu_max_instances": 'i', "vgpu_model_name": 's', "vgpu_profile": 's',
	},
	"cpu": {
		"server_time": 'i', "usage_idle": 'f', "usage_iowait": 'f', "usage_irq": 'f', "usage_nice": 'f',
		"usage_softirq": 'f', "usage_steal": 'f', "usage_system": 'f', "usage_user": 'f',
	},
	"mem": {
		"active": 'i', "available": 'i', "available_percent": 'f', "buffered": 'i', "cached": 'i', "free": 'i',
		"inactive": 'i', "server_time": 'i', "shared": 'i', "total": 'i', "used": 'i', "used_percent": 'f',
	},
	"swap": {"free": 'i', "server_time": 'i', "total": 'i', "used": 'i', "used_percent": 'f'},
	"disk": {
		"free": 'i', "inodes_free": 'i', "inodes_total": 'i', "inodes_used": 'i', "inodes_used_percent": 'f',
		"server_time": 'i', "total": 'i', "used": 'i', "used_percent": 'f',
	},
	"diskio": {
		"io_await": 'f', "io_svctm": 'f', "io_time": 'i', "io_util": 'f', "iops_in_progress": 'i', "read_bytes": 'i',
		"read_time": 'i', "reads": 'i', "server_time": 'i', "write_bytes": 'i', "write_time": 'i', "writes": 'i',
	},
	"net": {
		"bytes_recv": 'i', "bytes_sent": 'i', "drop_in": 'i', "drop_out": 'i', "err_in": 'i', "err_out": 'i',
		"ip": 's', "packets_recv": 'i', "packets_sent": 'i', "server_time": 'i',
	},
	"system": {
		"load1": 'f', "load15": 'f', "load5": 'f', "n_cpus": 'i', "n_physical_cpus": 'i', "n_unique_users": 'i',
		"n_users": 'i', "server_time": 'i', "uptime": 'i',
	},
	"processes": {
		"blocked": 'i', "dead": 'i', "idle": 'i', "running": 'i', "server_time": 'i', "sleeping": 'i',
		"stopped": 'i', "total": 'i', "unknown": 'i', "zombies": 'i',
	},
	"procstat": {"cpu_usage": 'f', "memory_usage": 'f', "server_time": 'i'},
}

func TestFieldTypes(t *testing.T) {
	a, b := collectTwice(t, Options{CredentialID: testCred, Seed: 7, XIDRate: 1})
	seen := map[string]byte{}
	for _, p := range append(a, b...) {
		want, known := realFieldTypes[p.name]
		if !known {
			t.Errorf("unexpected measurement %s", p.name)
			continue
		}
		for k, v := range p.fields {
			id := p.name + "." + k
			if prev, ok := seen[id]; ok && prev != v.kind {
				t.Errorf("%s written as %c and %c", id, prev, v.kind)
			}
			seen[id] = v.kind
			if wt, ok := want[k]; !ok {
				t.Errorf("%s is not a known field", id)
			} else if wt != v.kind {
				t.Errorf("%s type %c, target DB has %c", id, v.kind, wt)
			}
		}
	}
	for _, id := range []string{"dcgm.pstate", "dcgm.xid_errors", "dcgm.err_msg", "dcgm.cuda_probe", "net.ip", "gpu_vm_mapping.mapped"} {
		if _, ok := seen[id]; !ok {
			t.Errorf("%s never generated", id)
		}
	}
}

func set(s ...string) []string { sort.Strings(s); return s }

var hostGlobal = []string{"cloud_service", "collection_type", "credential_id", "gpu_type", "host", "time_diff", "type", "uuid"}

// Tag and field key sets of real rows written by the agent pipeline.
var (
	realMappingVGPUTags = set(append([]string{"device_id", "driver", "guest_bdf", "host_bdf", "openstack_vm_name",
		"openstack_vm_uuid", "status", "vendor_id", "vgpu_uuid"}, hostGlobal...)...)
	realMappingVGPUFields = set("gpu_uuid", "mapped", "server_time", "vgpu_active_instances", "vgpu_fb_mb",
		"vgpu_max_instances", "vgpu_model_name", "vgpu_profile")
	realMappingPTTags = set(append([]string{"device_id", "driver", "guest_bdf", "host_bdf", "openstack_vm_name",
		"openstack_vm_uuid", "status", "vendor_id"}, hostGlobal...)...)
	realDcgmHostTags = set(append([]string{"DCGM_FI_DEV_PCI_BUSID", "DCGM_FI_DEV_VBIOS_VERSION", "DCGM_FI_DRIVER_VERSION",
		"DCGM_FI_NVML_VERSION", "UUID", "arch", "compute_mode", "gpu", "index", "modelName", "name", "pci_bus_id",
		"pstate"}, hostGlobal...)...)
	realDcgmHostT4Fields = set("clocks_event_reasons", "dec_util", "ecc_dbe_agg_total", "ecc_dbe_vol_total",
		"ecc_sbe_agg_total", "ecc_sbe_vol_total", "enc_util", "fb_free", "fb_total", "fb_used", "gpu_temp", "gpu_util",
		"mem_clock", "mem_copy_util", "pcie_replay_counter", "power_usage", "pstate", "retired_pages_dbe",
		"retired_pages_pending", "sm_clock")
	realDcgmVMTags = set(append([]string{"DCGM_FI_DEV_PCI_BUSID", "DCGM_FI_DEV_VBIOS_VERSION", "DCGM_FI_DRIVER_VERSION",
		"UUID", "gpu", "gpu_instance_id", "modelName", "pci_bus_id", "physical_gpu_uuid"}, hostGlobal...)...)
	realDcgmVMFields = set("dec_util", "enc_util", "fb_free", "fb_total", "fb_used", "gpu_util", "mem_copy_util",
		"mig_mode", "pstate", "server_time", "vgpu_license_status")
)

func TestRealShapes(t *testing.T) {
	ps, _ := collectTwice(t, Options{CredentialID: testCred, Seed: 3})
	check := func(what string, got []point, wantTags, wantFields []string) {
		t.Helper()
		if len(got) == 0 {
			t.Fatalf("%s: no rows", what)
		}
		for _, p := range got {
			if tk := keys(p.tags); !reflect.DeepEqual(tk, wantTags) {
				t.Errorf("%s tag keys\n got %v\nwant %v", what, tk, wantTags)
			}
			if wantFields != nil {
				if fk := keys(p.fields); !reflect.DeepEqual(fk, wantFields) {
					t.Errorf("%s field keys\n got %v\nwant %v", what, fk, wantFields)
				}
			}
		}
	}
	check("gpu_vm_mapping vgpu assigned", filter(ps, func(p point) bool {
		return p.name == "gpu_vm_mapping" && p.tags["gpu_type"] == "vgpu" && p.tags["status"] == "assigned"
	}), realMappingVGPUTags, realMappingVGPUFields)
	check("gpu_vm_mapping passthrough assigned", filter(ps, func(p point) bool {
		return p.name == "gpu_vm_mapping" && p.tags["gpu_type"] == "passthrough" && p.tags["status"] == "assigned"
	}), realMappingPTTags, set("mapped", "server_time"))
	check("gpu_vm_mapping unassigned", filter(ps, func(p point) bool {
		return p.name == "gpu_vm_mapping" && p.tags["status"] == "unassigned"
	}), realMappingPTTags, nil)
	check("dcgm host (vGPU node, T4)", filter(ps, func(p point) bool {
		return p.name == "dcgm" && p.tags["type"] == "host" && p.tags["host"] == "hv-vgpu-a"
	}), realDcgmHostTags, realDcgmHostT4Fields)
	check("dcgm host (MIG node)", filter(ps, func(p point) bool {
		return p.name == "dcgm" && p.tags["type"] == "host" && p.tags["host"] == "hv-mig-a"
	}), realDcgmHostTags, nil)
	check("dcgm vm (vGPU, from hypervisor)", filter(ps, func(p point) bool {
		return p.name == "dcgm" && p.tags["type"] == "vm" && p.tags["gpu_type"] == "VGPU" && p.tags["project_id"] == ""
	}), realDcgmVMTags, realDcgmVMFields)

	vmGlobal := set(append([]string{"project_id"}, hostGlobal...)...)
	for _, c := range []struct {
		name  string
		extra []string
	}{
		{"cpu", []string{"cpu"}}, {"mem", nil}, {"swap", nil}, {"system", nil}, {"processes", nil},
		{"procstat", []string{"filter", "process_name", "user"}},
	} {
		check(c.name+" host", filter(ps, func(p point) bool { return p.name == c.name && p.tags["type"] == "host" }),
			set(append(append([]string{}, c.extra...), hostGlobal...)...), nil)
		check(c.name+" vm", filter(ps, func(p point) bool { return p.name == c.name && p.tags["type"] == "vm" }),
			set(append(append([]string{}, c.extra...), vmGlobal...)...), nil)
	}
	check("net ip", filter(ps, func(p point) bool { return p.name == "net" && p.tags["interface"] == "" }),
		set(hostGlobal...), set("ip", "server_time"))
	check("net vm", filter(ps, func(p point) bool { return p.name == "net" && p.tags["type"] == "vm" }),
		set(append([]string{"interface"}, vmGlobal...)...),
		set("bytes_recv", "bytes_sent", "drop_in", "drop_out", "err_in", "err_out", "packets_recv", "packets_sent", "server_time"))
}

func TestUUIDRule(t *testing.T) {
	topo := fixture()
	hosts := map[string]bool{}
	vms := map[string]*topology.VM{}
	for _, h := range topo.Hypervisors {
		hosts[h.Hostname] = true
	}
	for _, v := range topo.AllVMs() {
		vms[v.ID] = v
	}
	a, b := collectTwice(t, Options{CredentialID: testCred, Seed: 5, XIDRate: 1})
	for _, p := range append(a, b...) {
		u := p.tags["uuid"]
		suffix, ok := strings.CutPrefix(u, testCred+":")
		if !ok {
			t.Fatalf("uuid %q lacks the credential prefix: %s", u, seriesKey(p))
		}
		switch p.tags["type"] {
		case "host":
			if suffix != p.tags["host"] || !hosts[suffix] {
				t.Errorf("host row uuid %q does not match host %q", u, p.tags["host"])
			}
			if p.tags["credential_id"] != testCred {
				t.Errorf("credential_id %q", p.tags["credential_id"])
			}
		case "vm":
			vm := vms[suffix]
			if vm == nil {
				t.Errorf("vm row uuid %q is not a server id", u)
			} else if p.tags["host"] != vm.Name {
				t.Errorf("vm row host %q, want server name %q", p.tags["host"], vm.Name)
			}
		case "":
			if p.tags["err_code"] == "" || (!hosts[suffix] && vms[suffix] == nil) {
				t.Errorf("row without type is not an XID row: %s", seriesKey(p))
			}
		default:
			t.Errorf("unexpected type %q", p.tags["type"])
		}
	}
}

func TestGlobalGPUType(t *testing.T) {
	ps, _ := collectTwice(t, Options{CredentialID: testCred, Seed: 5})
	want := map[string]string{
		"hv-vgpu-a": "BARE_METAL", "hv-mig-a": "MIG", "hv-pt-a": "NONE",
		"test-vgpu-1": "VGPU", "test-mig-1": "MIG", "test-pt-1": "PASSTHROUGH",
	}
	for _, p := range filter(ps, func(p point) bool { return p.name == "cpu" }) {
		if w, ok := want[p.tags["host"]]; ok && p.tags["gpu_type"] != w {
			t.Errorf("%s gpu_type %q, want %q", p.tags["host"], p.tags["gpu_type"], w)
		}
	}
}

func TestMappingRows(t *testing.T) {
	ps, _ := collectTwice(t, Options{CredentialID: testCred, Seed: 9})
	m := filter(ps, func(p point) bool { return p.name == "gpu_vm_mapping" })
	byHost := func(h string) []point { return filter(m, func(p point) bool { return p.tags["host"] == h }) }

	vg := byHost("hv-vgpu-a")
	if len(vg) != 4 {
		t.Fatalf("vgpu node: want 3 assigned + 1 unassigned rows, got %d", len(vg))
	}
	for _, p := range vg[:3] {
		if p.tags["status"] != "assigned" || p.tags["host_bdf"] != "3b:00.0" || p.tags["guest_bdf"] != "vgpu" ||
			p.tags["driver"] != "nvidia" || p.fields["mapped"].i != 1 || p.fields["vgpu_active_instances"].i != 3 ||
			p.fields["vgpu_max_instances"].i != 4 || p.fields["vgpu_fb_mb"].i != 4096 ||
			p.fields["gpu_uuid"].s != "GPU-11111111-0000-4000-8000-000000000000" {
			t.Errorf("bad vgpu row: %s %v", seriesKey(p), p.fields)
		}
	}
	if vg[2].tags["openstack_vm_name"] != "test vgpu,3" {
		t.Errorf("escaped vm name lost: %q", vg[2].tags["openstack_vm_name"])
	}
	un := vg[3]
	if un.tags["status"] != "unassigned" || un.tags["host_bdf"] != "af:00.0" || un.tags["gpu_type"] != "vgpu" ||
		un.tags["openstack_vm_name"] != "N/A" || un.tags["guest_bdf"] != "N/A" || un.fields["mapped"].i != 0 ||
		un.fields["gpu_uuid"].s != "GPU-11111111-0000-4000-8000-000000000001" || un.fields["vgpu_active_instances"].i != 0 {
		t.Errorf("bad unassigned vgpu row: %s %v", seriesKey(un), un.fields)
	}

	// MIG: one row per assigned instance. The script sees the GPU as assigned as a whole,
	// so the free instance produces no row.
	mg := byHost("hv-mig-a")
	if len(mg) != 2 {
		t.Fatalf("mig node: want 2 rows, got %d", len(mg))
	}
	for _, p := range mg {
		if p.tags["gpu_type"] != "mig" || p.tags["status"] != "assigned" || p.fields["vgpu_active_instances"].i != 2 ||
			p.fields["vgpu_model_name"].s != "NVIDIA A100D-2-20C" || p.fields["vgpu_max_instances"].i != 3 {
			t.Errorf("bad mig row: %s %v", seriesKey(p), p.fields)
		}
	}

	pt := byHost("hv-pt-a")
	if len(pt) != 2 {
		t.Fatalf("passthrough node: want 1 assigned + 1 unassigned rows, got %d", len(pt))
	}
	if p := pt[0]; p.tags["status"] != "assigned" || p.tags["driver"] != "vfio-pci" || p.tags["guest_bdf"] != "00:05.0" ||
		p.tags["openstack_vm_uuid"] != "aaaaaaaa-0000-4000-8000-000000000021" || p.tags["gpu_type"] != "passthrough" {
		t.Errorf("bad passthrough row: %s", seriesKey(p))
	}
	if _, has := pt[0].fields["gpu_uuid"]; has {
		t.Error("passthrough hypervisor has no nvidia driver, gpu_uuid must be absent")
	}
	if p := pt[1]; p.tags["status"] != "unassigned" || p.tags["host_bdf"] != "ab:00.0" || p.tags["driver"] != "vfio-pci" ||
		p.fields["mapped"].i != 0 {
		t.Errorf("bad passthrough unassigned row: %s %v", seriesKey(p), p.fields)
	}

	// The monitoring server fills the passthrough GPU UUID from the guest dcgm series
	// whose uuid is credential:server id. That series must carry the physical UUID.
	guest := filter(ps, func(p point) bool {
		return p.name == "dcgm" && p.tags["uuid"] == testCred+":aaaaaaaa-0000-4000-8000-000000000021" && p.tags["err_code"] == ""
	})
	if len(guest) != 1 || guest[0].tags["UUID"] != "GPU-33333333-0000-4000-8000-000000000000" ||
		guest[0].tags["gpu_type"] != "PASSTHROUGH" || guest[0].tags["DCGM_FI_CUDA_DRIVER_VERSION"] != "13.0" ||
		guest[0].tags["DCGM_FI_DEV_PCI_BUSID"] != "00000000:00:05.0" {
		t.Errorf("bad passthrough guest dcgm rows: %v", guest)
	}
	if _, ok := guest[0].fields["gpu_util"]; !ok {
		t.Error("passthrough guest row lacks gpu_util")
	}
}

func TestMIGSeries(t *testing.T) {
	ps, _ := collectTwice(t, Options{CredentialID: testCred, Seed: 11})
	phys := filter(ps, func(p point) bool { return p.name == "dcgm" && p.tags["host"] == "hv-mig-a" })
	if len(phys) != 2 {
		t.Fatalf("want nvidia-smi point + GPM device point, got %d", len(phys))
	}
	if seriesKey(phys[0]) != seriesKey(phys[1]) {
		t.Error("GPM device point must land on the physical GPU series")
	}
	if _, ok := phys[0].fields["gpu_util"]; ok {
		t.Error("nvidia-smi reports no utilization on a MIG enabled GPU")
	}
	if _, ok := phys[1].fields["sm_occupancy"]; !ok {
		t.Error("GPM device point lacks sm_occupancy")
	}

	for _, id := range []string{"aaaaaaaa-0000-4000-8000-000000000011", "aaaaaaaa-0000-4000-8000-000000000012"} {
		rows := filter(ps, func(p point) bool {
			return p.name == "dcgm" && p.tags["uuid"] == testCred+":"+id && p.tags["gpu_instance_id"] != ""
		})
		if len(rows) != 2 {
			t.Fatalf("%s: want GPM instance + mapping point, got %d", id, len(rows))
		}
		if seriesKey(rows[0]) != seriesKey(rows[1]) {
			t.Errorf("%s: GPM instance point is not on the VM series", id)
		}
		var gpm, exec point
		for _, r := range rows {
			if _, ok := r.fields["sm_occupancy"]; ok {
				gpm = r
			} else {
				exec = r
			}
		}
		if exec.fields["mig_mode"].f != 1 || exec.tags["gpu_type"] != "MIG" ||
			exec.tags["physical_gpu_uuid"] != "GPU-22222222-0000-4000-8000-000000000000" {
			t.Errorf("%s: bad mapping point %s %v", id, seriesKey(exec), exec.fields)
		}
		if _, ok := exec.fields["gpu_util"]; ok {
			t.Errorf("%s: MIG backed vGPU utilization is N/A in nvidia-smi", id)
		}
		if gpm.fields["gpu_util"].kind != 'f' || gpm.fields["server_time"].kind != 'i' {
			t.Errorf("%s: bad GPM point %v", id, gpm.fields)
		}
	}
	if got := exactlyOne(ps, func(p point) bool { return p.tags["gpu_instance_id"] == "3" }); got.tags["gpu"] != "0" {
		t.Errorf("first MIG VM gpu index %q", got.tags["gpu"])
	}
}

func exactlyOne(ps []point, f func(point) bool) point {
	rows := filter(ps, func(p point) bool { return p.name == "dcgm" && f(p) && p.fields["mig_mode"].kind != 0 })
	if len(rows) != 1 {
		return point{tags: map[string]string{}}
	}
	return rows[0]
}

func TestCountersMonotonic(t *testing.T) {
	a, b := collectTwice(t, Options{CredentialID: testCred, Seed: 13})
	counters := map[string][]string{
		"diskio": {"reads", "writes", "read_bytes", "write_bytes", "read_time", "write_time", "io_time"},
		"net":    {"bytes_recv", "bytes_sent", "packets_recv", "packets_sent", "drop_in", "err_in"},
		"system": {"uptime"},
	}
	first := map[string]point{}
	for _, p := range a {
		first[seriesKey(p)] = p
	}
	grew, compared := 0, 0
	for _, p := range b {
		fs, ok := counters[p.name]
		if !ok {
			continue
		}
		prev, ok := first[seriesKey(p)]
		if !ok {
			continue
		}
		for _, f := range fs {
			v1, ok1 := prev.fields[f]
			v2, ok2 := p.fields[f]
			if !ok1 || !ok2 {
				continue
			}
			compared++
			if v2.i < v1.i {
				t.Errorf("%s %s went down: %d -> %d", seriesKey(p), f, v1.i, v2.i)
			}
			if v2.i > v1.i {
				grew++
			}
		}
	}
	if compared == 0 || grew == 0 {
		t.Fatalf("counters not compared (%d) or never grew (%d)", compared, grew)
	}

	for _, p := range append(a, b...) {
		if p.name != "dcgm" {
			continue
		}
		if tot, ok := p.fields["fb_total"]; ok {
			if used := p.fields["fb_used"]; used.f > tot.f || used.f < 0 || p.fields["fb_free"].f != tot.f-used.f {
				t.Errorf("framebuffer inconsistent: %s %v", seriesKey(p), p.fields)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	run := func(seed int64) []string {
		g := NewGenerator(fixture(), Options{CredentialID: testCred, Seed: seed, XIDRate: 0.3})
		var out []string
		for i := 0; i < 4; i++ {
			out = append(out, g.Collect(t0.Add(time.Duration(i)*30*time.Second))...)
		}
		return out
	}
	x, y := run(21), run(21)
	if !reflect.DeepEqual(x, y) {
		t.Fatal("same seed produced different output")
	}
	if reflect.DeepEqual(x, run(22)) {
		t.Fatal("different seeds produced identical output")
	}
}

func TestPeriodicInputs(t *testing.T) {
	g := NewGenerator(fixture(), Options{CredentialID: testCred, Seed: 1})
	count := func(lines []string, f func(point) bool) int {
		return len(filter(parseAll(t, lines), f))
	}
	isMapping := func(p point) bool { return p.name == "gpu_vm_mapping" }
	isProbe := func(p point) bool { return p.fields["cuda_probe"].kind == 'i' }
	isIP := func(p point) bool { return p.name == "net" && p.fields["ip"].kind == 's' }

	l0 := g.Collect(t0)
	if count(l0, isMapping) == 0 || count(l0, isIP) != 3 || count(l0, isProbe) != 5 {
		t.Fatalf("first collection: mapping=%d ip=%d probe=%d", count(l0, isMapping), count(l0, isIP), count(l0, isProbe))
	}
	l1 := g.Collect(t0.Add(10 * time.Second))
	if count(l1, isMapping) != 0 || count(l1, isIP) != 0 || count(l1, isProbe) != 0 {
		t.Fatal("60s/5m inputs repeated inside their interval")
	}
	l2 := g.Collect(t0.Add(60 * time.Second))
	if count(l2, isMapping) == 0 || count(l2, isProbe) != 0 {
		t.Fatal("60s inputs missing after a minute, or the 5m probe repeated")
	}
	l3 := g.Collect(t0.Add(5 * time.Minute))
	if count(l3, isProbe) != 5 {
		t.Fatal("5m probe missing after five minutes")
	}
	for _, p := range filter(parseAll(t, l0), isProbe) {
		if strings.HasPrefix(p.tags["UUID"], "GPU-") || p.tags["DCGM_FI_CUDA_DRIVER_VERSION"] != "13.2" || p.tags["project_id"] == "" {
			t.Errorf("bad cuda probe: %s", seriesKey(p))
		}
	}
}

func TestXIDRows(t *testing.T) {
	g := NewGenerator(fixture(), Options{CredentialID: testCred, Seed: 3, XIDRate: 1})
	ps := parseAll(t, g.Collect(t0))
	xids := filter(ps, func(p point) bool { return p.tags["err_code"] != "" })
	if len(xids) != 6 {
		t.Fatalf("want one XID per GPU VM (6), got %d", len(xids))
	}
	for _, p := range xids {
		code, _ := strconv.Atoi(p.tags["err_code"])
		msg := p.fields["err_msg"].s
		prefix := fmt.Sprintf("NVRM: Xid (PCI:%s): %d, ", p.tags["DCGM_FI_DEV_PCI_BUSID"], code)
		if p.fields["xid_errors"].f != float64(code) || !strings.HasPrefix(msg, prefix) {
			t.Errorf("bad XID row: %s %v", seriesKey(p), p.fields)
		}
		if p.tags["credential_id"] != "" || p.tags["type"] != "" {
			t.Errorf("processor-built XID point must not have global tags: %s", seriesKey(p))
		}
		if _, ok := p.fields["server_time"]; ok {
			t.Error("XID point must not have server_time")
		}
	}
	pt := filter(xids, func(p point) bool { return p.tags["uuid"] == testCred+":aaaaaaaa-0000-4000-8000-000000000021" })
	if len(pt) != 1 || pt[0].tags["DCGM_FI_DEV_PCI_BUSID"] != "0000:00:05" ||
		pt[0].tags["UUID"] != "GPU-33333333-0000-4000-8000-000000000000" {
		t.Errorf("passthrough XID must come from the guest: %v", pt)
	}
	vg := filter(xids, func(p point) bool { return p.tags["uuid"] == testCred+":hv-vgpu-a" })
	if len(vg) != 3 || vg[0].tags["DCGM_FI_DEV_PCI_BUSID"] != "0000:3b:00" ||
		vg[0].tags["UUID"] != "GPU-11111111-0000-4000-8000-000000000000" || vg[0].tags["DCGM_FI_DRIVER_VERSION"] != "595.91.04" {
		t.Errorf("vGPU XID must come from the hypervisor on the physical GPU: %v", vg)
	}

	none := NewGenerator(fixture(), Options{CredentialID: testCred, Seed: 3})
	if n := len(filter(parseAll(t, none.Collect(t0)), func(p point) bool { return p.tags["err_code"] != "" })); n != 0 {
		t.Errorf("XIDRate 0 produced %d XID rows", n)
	}
}

func TestNormBDF(t *testing.T) {
	for in, want := range map[string]string{
		"00000000:3B:00.0": "0000:3b:00",
		"PCI:0000:0b:00":   "0000:0b:00",
		"00000000:00:05.0": "0000:00:05",
		"bogus":            "",
	} {
		if got := normBDF(in); got != want {
			t.Errorf("normBDF(%q) = %q, want %q", in, got, want)
		}
	}
}
