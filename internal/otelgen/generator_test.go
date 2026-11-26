package otelgen

import (
	"bytes"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/otlp"
	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

var t0 = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// series is one flattened data point.
type series struct {
	name      string
	stored    string
	unit      string
	typ       otlp.MetricType
	monotonic bool
	attrs     map[string]string
	start     uint64
	time      uint64
	value     float64
}

func (s series) key() string {
	keys := make([]string, 0, len(s.attrs))
	for k := range s.attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(s.name)
	for _, k := range keys {
		b.WriteString("," + k + "=" + s.attrs[k])
	}
	return b.String()
}

// unitSuffix is the name suffix a backend storing OTLP under Prometheus names
// appends for a unit: "{...}" annotations add nothing, unknown units are kept as written.
func unitSuffix(unit string, typ otlp.MetricType) string {
	switch unit {
	case "", "{cpu}", "{thread}", "{operations}", "{faults}", "{inodes}", "{packets}", "{errors}",
		"{connections}", "{processes}", "{error}":
		return ""
	case "1":
		if typ == otlp.Gauge {
			return "ratio"
		}
		return ""
	case "s":
		return "seconds"
	case "By":
		return "bytes"
	case "MiBy":
		return "mebibytes"
	case "%":
		return "percent"
	case "Cel":
		return "celsius"
	case "W":
		return "watts"
	}
	return unit
}

// storedName is the Prometheus name of an OTLP metric: dots to underscores, the unit
// suffix unless the name already has it, and _total on monotonic sums.
func storedName(name, unit string, typ otlp.MetricType, monotonic bool) string {
	tokens := strings.Split(strings.ReplaceAll(name, ".", "_"), "_")
	if u := unitSuffix(unit, typ); u != "" && !slices.Contains(tokens, u) {
		tokens = append(tokens, u)
	}
	if typ == otlp.Sum && monotonic {
		tokens = append(slices.DeleteFunc(tokens, func(s string) bool { return s == "total" }), "total")
	}
	return strings.Join(tokens, "_")
}

func flatten(t *testing.T, req *otlp.ExportRequest) []series {
	t.Helper()
	var out []series
	for _, rm := range req.ResourceMetrics {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				for _, p := range m.Points {
					s := series{
						name: m.Name, unit: m.Unit, typ: m.Type, monotonic: m.Monotonic,
						stored: storedName(m.Name, m.Unit, m.Type, m.Monotonic),
						attrs:  map[string]string{}, start: p.StartTimeUnixNano, time: p.TimeUnixNano,
					}
					for _, kv := range p.Attributes {
						if _, dup := s.attrs[kv.Key]; dup {
							t.Fatalf("%s: attribute %s twice", m.Name, kv.Key)
						}
						if kv.IsInt {
							s.attrs[kv.Key] = fmt.Sprint(kv.Int)
						} else {
							s.attrs[kv.Key] = kv.Str
						}
					}
					if p.IsInt {
						s.value = float64(p.Int)
					} else {
						s.value = p.Double
					}
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func TestStoredNameRule(t *testing.T) {
	cases := []struct {
		name, unit string
		typ        otlp.MetricType
		mono       bool
		want       string
	}{
		{"system.cpu.time", "s", otlp.Sum, true, "system_cpu_time_seconds_total"},
		{"system.memory.utilization", "1", otlp.Gauge, false, "system_memory_utilization_ratio"},
		{"system.memory.usage", "By", otlp.Sum, false, "system_memory_usage_bytes"},
		{"system.network.packets", "{packets}", otlp.Sum, true, "system_network_packets_total"},
		{"nvidia_smi.utilization.gpu", "%", otlp.Gauge, false, "nvidia_smi_utilization_gpu_percent"},
		{"nvidia_smi.memory.total", "MiBy", otlp.Gauge, false, "nvidia_smi_memory_total_mebibytes"},
		{"nvidia_smi.clocks.sm", "MHz", otlp.Gauge, false, "nvidia_smi_clocks_sm_MHz"},
		{"nvidia_smi.ecc.errors.corrected.volatile.total", "", otlp.Gauge, false, "nvidia_smi_ecc_errors_corrected_volatile_total"},
		{"nvidia_kmsg.xid.errors", "{error}", otlp.Sum, true, "nvidia_kmsg_xid_errors_total"},
	}
	for _, c := range cases {
		if got := storedName(c.name, c.unit, c.typ, c.mono); got != c.want {
			t.Errorf("storedName(%s) = %s, want %s", c.name, got, c.want)
		}
	}
}

func collect(t *testing.T, top *topology.Topology, opt Options, n int) [][]series {
	t.Helper()
	g := NewGenerator(top, opt)
	var out [][]series
	for i := 0; i < n; i++ {
		out = append(out, flatten(t, g.Collect(t0.Add(time.Duration(i)*10*time.Second))))
	}
	return out
}

func expectedEntities(top *topology.Topology) map[string]string {
	want := map[string]string{}
	for _, h := range top.Hypervisors {
		want[topology.EntityID(h.Hostname, h.EntityID)] = h.Hostname
	}
	for _, vm := range top.AllVMs() {
		want[topology.EntityID(vm.Name, vm.EntityID)] = vm.Name
	}
	return want
}

func TestEveryPointHasEntityID(t *testing.T) {
	top := topology.Default()
	top.Hypervisors[1].EntityID = "override-host"
	top.Hypervisors[0].GPUs[0].VMs[0].EntityID = "override-vm"
	want := expectedEntities(top)
	if want["override-host"] != "emu-vgpu-02" || want["override-vm"] == "" {
		t.Fatalf("override not used: %v", want)
	}
	seen := map[string]int{}
	pts := collect(t, top, Options{Seed: 1, XIDRate: 1}, 2)[1]
	for _, s := range pts {
		id, ok := s.attrs[LabelEntityID]
		if !ok {
			t.Fatalf("%s %v has no entity_id", s.name, s.attrs)
		}
		if _, known := want[id]; !known {
			t.Fatalf("%s has unknown entity_id %q", s.name, id)
		}
		seen[id]++
	}
	if len(seen) != len(want) || len(want) != 6+45 {
		t.Fatalf("entity ids seen %d, want %d (6 hypervisors + 45 VMs)", len(seen), len(want))
	}
	t.Logf("%d data points, %d agents", len(pts), len(seen))
}

// catalogNames are the stored series names the monitoring server's catalog reads
// from hostmetrics and the NVIDIA GPU receiver, with labels its queries use.
var catalogNames = []struct {
	name   string
	labels []string
}{
	{"system_cpu_time_seconds_total", []string{"state"}},
	{"system_cpu_logical_count", nil},
	{"system_cpu_physical_count", nil},
	{"system_memory_usage_bytes", []string{"state"}},
	{"system_memory_limit_bytes", nil},
	{"system_memory_utilization_ratio", []string{"state"}},
	{"system_linux_memory_available_bytes", nil},
	{"system_memory_linux_shared_bytes", nil},
	{"system_paging_usage_bytes", []string{"state"}},
	{"system_disk_io_bytes_total", []string{"device", "direction"}},
	{"system_disk_operations_total", []string{"device", "direction"}},
	{"system_disk_operation_time_seconds_total", []string{"device", "direction"}},
	{"system_disk_merged_total", []string{"device", "direction"}},
	{"system_disk_io_time_seconds_total", []string{"device"}},
	{"system_disk_pending_operations", []string{"device"}},
	{"system_filesystem_usage_bytes", []string{"mountpoint", "state"}},
	{"system_filesystem_inodes_usage", []string{"mountpoint", "state"}},
	{"system_network_io_bytes_total", []string{"device", "direction"}},
	{"system_network_packets_total", []string{"device", "direction"}},
	{"system_network_errors_total", []string{"device", "direction"}},
	{"system_network_dropped_total", []string{"device", "direction"}},
	{"system_uptime_seconds", nil},
	{"system_cpu_load_average_1m", nil},
	{"system_cpu_load_average_5m", nil},
	{"system_cpu_load_average_15m", nil},
	{"system_processes_count", []string{"status"}},
	{"process_cpu_utilization_ratio", []string{"process.executable.name"}},
	{"process_memory_utilization_ratio", []string{"process.executable.name"}},
	{"nvidia_smi_utilization_gpu_percent", []string{"uuid"}},
	{"nvidia_smi_utilization_encoder_percent", []string{"uuid"}},
	{"nvidia_smi_utilization_decoder_percent", []string{"uuid"}},
	{"nvidia_smi_utilization_memory_percent", []string{"uuid"}},
	{"nvidia_smi_memory_used_mebibytes", []string{"uuid"}},
	{"nvidia_smi_memory_total_mebibytes", []string{"uuid"}},
	{"nvidia_smi_power_draw_watts", []string{"uuid"}},
	{"nvidia_smi_temperature_gpu_celsius", []string{"uuid"}},
	{"nvidia_smi_temperature_memory_celsius", []string{"uuid"}},
	{"nvidia_smi_clocks_sm_MHz", []string{"uuid"}},
	{"nvidia_smi_clocks_mem_MHz", []string{"uuid"}},
	{"nvidia_smi_pstate", []string{"uuid"}},
	{"nvidia_smi_clocks_event_reasons_active", []string{"uuid"}},
	{"nvidia_smi_ecc_errors_corrected_volatile_total", []string{"uuid"}},
	{"nvidia_smi_ecc_errors_uncorrected_volatile_total", []string{"uuid"}},
	{"nvidia_smi_ecc_errors_corrected_aggregate_total", []string{"uuid"}},
	{"nvidia_smi_ecc_errors_uncorrected_aggregate_total", []string{"uuid"}},
	{"nvidia_smi_retired_pages_double_bit_count", []string{"uuid"}},
	{"nvidia_smi_retired_pages_pending", []string{"uuid"}},
	{"nvidia_smi_remapped_rows_correctable", []string{"uuid"}},
	{"nvidia_smi_remapped_rows_uncorrectable", []string{"uuid"}},
	{"nvidia_smi_remapped_rows_pending", []string{"uuid"}},
	{"nvidia_smi_remapped_rows_failure", []string{"uuid"}},
	{"nvidia_kmsg_xid_errors_total", []string{"uuid"}},
	{"nvml_gpm_sm_utilization_percent", []string{"uuid", "gpu_instance_id", "mig"}},
}

// fanCatalogName is in the catalog too, but nvidia-smi reports fan.speed as [N/A] on the
// passively cooled boards of the default topology, so it only shows up with a fan board.
const fanCatalogName = "nvidia_smi_fan_speed_percent"

func TestCatalogNamesPresent(t *testing.T) {
	pts := collect(t, topology.Default(), Options{Seed: 1, XIDRate: 1}, 2)[1]
	byName := map[string][]series{}
	for _, s := range pts {
		byName[s.stored] = append(byName[s.stored], s)
	}
	for _, c := range catalogNames {
		ss := byName[c.name]
		if len(ss) == 0 {
			t.Errorf("catalog name %s not generated", c.name)
			continue
		}
		found := false
		for _, s := range ss {
			ok := true
			for _, l := range c.labels {
				if _, has := s.attrs[l]; !has {
					ok = false
				}
			}
			found = found || ok
		}
		if !found {
			t.Errorf("%s has no point with labels %v", c.name, c.labels)
		}
	}
	if len(byName[fanCatalogName]) != 0 {
		t.Errorf("%s generated for passively cooled boards", fanCatalogName)
	}
	for _, s := range byName["nvml_gpm_sm_utilization_percent"] {
		if s.attrs["mig"] == "true" && s.attrs["gpu_instance_id"] == "" {
			t.Errorf("MIG GPM point without gpu_instance_id: %v", s.attrs)
		}
	}
}

func TestFanSpeedOnFanBoards(t *testing.T) {
	top := topology.Default()
	for _, g := range top.Hypervisors[0].GPUs {
		g.ModelName = "NVIDIA RTX A6000"
	}
	pts := collect(t, top, Options{Seed: 1}, 1)[0]
	n := 0
	for _, s := range pts {
		if s.stored == fanCatalogName {
			n++
			if s.attrs["name"] != "NVIDIA RTX A6000" {
				t.Errorf("fan.speed on %s", s.attrs["name"])
			}
		}
	}
	if n != len(top.Hypervisors[0].GPUs) {
		t.Errorf("fan.speed points %d, want one per GPU of the host (%d)", n, len(top.Hypervisors[0].GPUs))
	}
}

func TestCumulativeSumsNeverDecrease(t *testing.T) {
	runs := collect(t, topology.Default(), Options{Seed: 3, XIDRate: 0.3}, 6)
	prev := map[string]series{}
	grew := map[string]bool{}
	for i, pts := range runs {
		for _, s := range pts {
			if s.typ != otlp.Sum || !s.monotonic {
				continue
			}
			if !strings.HasSuffix(s.stored, "_total") {
				t.Fatalf("monotonic sum %s stored without _total", s.stored)
			}
			if s.start == 0 || s.start > s.time {
				t.Fatalf("%s start %d time %d", s.key(), s.start, s.time)
			}
			k := s.key()
			if p, ok := prev[k]; ok {
				if s.value < p.value {
					t.Fatalf("collection %d: %s went down %v -> %v", i, k, p.value, s.value)
				}
				if s.start != p.start {
					t.Fatalf("%s start time changed", k)
				}
				if s.value > p.value {
					grew[s.name] = true
				}
			}
			prev[k] = s
		}
	}
	for _, name := range []string{"system.cpu.time", "system.disk.io", "system.network.io", "process.cpu.time", "nvidia_kmsg.xid.errors"} {
		if !grew[name] {
			t.Errorf("%s never grew over 6 collections", name)
		}
	}
}

func TestGaugeRanges(t *testing.T) {
	for _, s := range collect(t, topology.Default(), Options{Seed: 5}, 3)[2] {
		switch {
		case s.unit == "%":
			if s.value < 0 || s.value > 100 {
				t.Errorf("%s %v = %v outside 0..100", s.name, s.attrs, s.value)
			}
		case s.name == "system.memory.utilization", s.name == "process.cpu.utilization":
			if s.value < 0 || s.value > 1 {
				t.Errorf("%s %v = %v outside 0..1", s.name, s.attrs, s.value)
			}
		case s.unit == "By" || s.unit == "MiBy":
			if s.value < 0 {
				t.Errorf("%s %v negative", s.name, s.attrs)
			}
		}
	}
}

func TestDeterministic(t *testing.T) {
	run := func(seed int64) []byte {
		g := NewGenerator(topology.Default(), Options{Seed: seed, XIDRate: 0.1})
		var b bytes.Buffer
		for i := 0; i < 3; i++ {
			b.Write(g.Collect(t0.Add(time.Duration(i) * 10 * time.Second)).Marshal())
		}
		return b.Bytes()
	}
	a, b := run(7), run(7)
	if !bytes.Equal(a, b) {
		t.Fatal("same seed gave different requests")
	}
	if bytes.Equal(a, run(8)) {
		t.Fatal("different seeds gave the same requests")
	}
}

// TestXIDRateDoesNotShiftValues: turning XIDs on adds the XID series and changes nothing else.
func TestXIDRateDoesNotShiftValues(t *testing.T) {
	off := collect(t, topology.Default(), Options{Seed: 2, XIDRate: 0}, 3)
	on := collect(t, topology.Default(), Options{Seed: 2, XIDRate: 1}, 3)
	for i := range off {
		var rest []series
		xids := 0
		total := 0.0
		for _, s := range on[i] {
			if s.name == "nvidia_kmsg.xid.errors" {
				xids++
				total += s.value
				continue
			}
			rest = append(rest, s)
		}
		if len(rest) != len(off[i]) {
			t.Fatalf("collection %d: %d points without XIDs, want %d", i, len(rest), len(off[i]))
		}
		for j := range rest {
			if rest[j].key() != off[i][j].key() || rest[j].value != off[i][j].value {
				t.Fatalf("collection %d point %s changed with XIDRate", i, rest[j].key())
			}
		}
		if xids == 0 {
			t.Fatalf("collection %d: no XID series with XIDRate 1", i)
		}
		// Every GPU VM logs one XID per collection: 45 VMs, counted on the hypervisor or the passthrough guest.
		if want := float64(45 * (i + 1)); total != want {
			t.Fatalf("collection %d: XID count %v, want %v", i, total, want)
		}
	}
	for _, s := range off[2] {
		if s.name == "nvidia_kmsg.xid.errors" {
			t.Fatal("XID series with XIDRate 0")
		}
	}
}

func TestGPUViews(t *testing.T) {
	top := topology.Default()
	pts := collect(t, top, Options{Seed: 1, XIDRate: 1}, 2)[1]
	byEntity := map[string][]series{}
	for _, s := range pts {
		byEntity[s.attrs[LabelEntityID]] = append(byEntity[s.attrs[LabelEntityID]], s)
	}
	find := func(entity, name string) []series {
		var out []series
		for _, s := range byEntity[entity] {
			if s.name == name {
				out = append(out, s)
			}
		}
		return out
	}
	hopper := map[string]bool{}
	for _, h := range top.Hypervisors {
		hid := topology.EntityID(h.Hostname, "")
		total := find(hid, "nvidia_smi.memory.total")
		switch h.Mode {
		case topology.ModePassthrough:
			// No host driver: the GPUs are bound to vfio-pci and nvidia-smi is not there.
			if len(total) != 0 {
				t.Errorf("%s: passthrough host reports %d GPUs", h.Hostname, len(total))
			}
		default:
			if len(total) != len(h.GPUs) {
				t.Errorf("%s: %d GPUs reported, want %d", h.Hostname, len(total), len(h.GPUs))
			}
		}
		if util := find(hid, "nvidia_smi.utilization.gpu"); h.Mode == topology.ModeMIG && len(util) != 0 {
			t.Errorf("%s: MIG enabled GPU reports utilization.gpu", h.Hostname)
		}
		gpmMIG := 0
		for _, s := range find(hid, "nvml.gpm.sm.utilization") {
			if s.attrs["mig"] == "true" {
				gpmMIG++
			}
		}
		wantMIG := 0
		for _, g := range h.GPUs {
			if g.Arch == "Hopper" {
				hopper[g.UUID] = true
				wantMIG += len(g.MIGInstances)
			}
		}
		if gpmMIG != wantMIG {
			t.Errorf("%s: %d MIG GPM points, want %d", h.Hostname, gpmMIG, wantMIG)
		}
		for _, g := range h.GPUs {
			for _, vm := range g.VMs {
				vid := topology.EntityID(vm.Name, "")
				vt := find(vid, "nvidia_smi.memory.total")
				if len(vt) != 1 {
					t.Fatalf("%s: %d GPUs, want 1", vm.Name, len(vt))
				}
				s := vt[0]
				temps := find(vid, "nvidia_smi.temperature.gpu")
				gpm := find(vid, "nvml.gpm.sm.utilization")
				switch h.Mode {
				case topology.ModePassthrough:
					if s.attrs["uuid"] != g.UUID || s.attrs["name"] != g.ModelName || s.value != float64(g.MemoryMiB) {
						t.Errorf("%s: passthrough GPU %v %v", vm.Name, s.attrs, s.value)
					}
					if len(temps) != 1 {
						t.Errorf("%s: passthrough guest has no temperature", vm.Name)
					}
				default:
					p := g.Profile
					if h.Mode == topology.ModeMIG {
						for _, mi := range g.MIGInstances {
							if mi.GPUInstanceID == vm.MIGGPUInstanceID {
								p = mi.VGPU
							}
						}
					}
					if s.attrs["uuid"] != "GPU-"+vm.VGPUUUID || s.attrs["name"] != p.Name || s.value != float64(p.FramebufferMB) || s.attrs["index"] != "0" {
						t.Errorf("%s: vGPU %v %v, want GPU-%s %s %d", vm.Name, s.attrs, s.value, vm.VGPUUUID, p.Name, p.FramebufferMB)
					}
					if len(temps) != 0 {
						t.Errorf("%s: vGPU guest reports temperature", vm.Name)
					}
				}
				wantGPM := 0
				if g.Arch == "Hopper" && h.Mode != topology.ModeVGPU {
					wantGPM = 1
				}
				if len(gpm) != wantGPM {
					t.Errorf("%s: %d GPM points, want %d", vm.Name, len(gpm), wantGPM)
				}
				if h.Mode == topology.ModeMIG && wantGPM == 1 && (gpm[0].attrs["mig"] != "true" || gpm[0].attrs["gpu_instance_id"] != "0") {
					t.Errorf("%s: MIG guest GPM attrs %v", vm.Name, gpm[0].attrs)
				}
			}
		}
	}
	for _, s := range pts {
		if s.name == "nvidia_kmsg.xid.errors" {
			if s.attrs["xid"] == "" || s.attrs["uuid"] == "" || len(s.attrs) != 3 {
				t.Errorf("XID point attrs %v, want xid, uuid and entity_id", s.attrs)
			}
		}
	}
	if len(hopper) == 0 {
		t.Fatal("default topology has no Hopper GPU; the GPM checks prove nothing")
	}
}
