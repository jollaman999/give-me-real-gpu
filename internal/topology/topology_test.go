package topology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-5[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestStableUUID(t *testing.T) {
	a := StableUUID("server", "emu-vgpu01-g0-1")
	if a != StableUUID("server", "emu-vgpu01-g0-1") {
		t.Fatalf("same name gave different UUIDs: %s", a)
	}
	if !uuidRE.MatchString(a) {
		t.Fatalf("not in UUID v5 format: %s", a)
	}
	if StableUUID("ab", "c") == StableUUID("a", "bc") {
		t.Fatal("different part boundaries gave the same UUID")
	}
	if StableUUID("server", "x") == StableUUID("mdev", "x") {
		t.Fatal("different kinds gave the same UUID")
	}
	h := StableHex32("project", "emu-gpu")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h) {
		t.Fatalf("not 32 hex chars: %s", h)
	}
}

func TestDefaultDeterministicAndValid(t *testing.T) {
	a, b := Default(), Default()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two Default() calls returned different results")
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Default failed Validate: %v", err)
	}
}

// VMs per GPU for each node, as listed in the design document table.
func TestDefaultMatchesDesign(t *testing.T) {
	want := map[string][]int{
		"emu-vgpu-01": {6, 4, 3, 0},
		"emu-vgpu-02": {4, 2},
		"emu-mig-01":  {5, 2, 2, 1},
		"emu-mig-02":  {7, 2},
		"emu-pt-01":   {1, 1, 1, 0},
		"emu-pt-02":   {1, 1, 1, 1},
	}
	top := Default()
	if len(top.Hypervisors) != len(want) {
		t.Fatalf("got %d nodes, want %d", len(top.Hypervisors), len(want))
	}
	total := 0
	for _, h := range top.Hypervisors {
		var got []int
		for _, g := range h.GPUs {
			got = append(got, len(g.VMs))
			total += len(g.VMs)
		}
		if !reflect.DeepEqual(got, want[h.Hostname]) {
			t.Errorf("%s VMs per GPU %v, want %v", h.Hostname, got, want[h.Hostname])
		}
		t.Logf("%-12s %-11s %d GPUs %d VMs %v", h.Hostname, h.Mode, len(h.GPUs), sum(got), got)
	}
	if total != 45 || len(top.AllVMs()) != 45 {
		t.Errorf("total VMs %d (AllVMs %d), want 45", total, len(top.AllVMs()))
	}
	t.Logf("total VMs %d", total)

	migInst := map[string][]int{}
	for _, h := range top.Hypervisors {
		if h.Mode != ModeMIG {
			continue
		}
		for _, g := range h.GPUs {
			migInst[h.Hostname] = append(migInst[h.Hostname], len(g.MIGInstances))
		}
	}
	if !reflect.DeepEqual(migInst["emu-mig-01"], []int{7, 3, 2, 1}) ||
		!reflect.DeepEqual(migInst["emu-mig-02"], []int{7, 2}) {
		t.Errorf("MIG instance counts %v", migInst)
	}
}

func sum(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func TestDefaultCoverage(t *testing.T) {
	top := Default()
	modes := map[GPUMode]bool{}
	multi := map[GPUMode]bool{}
	unassignedGPU := false
	unassignedMIG := false
	for _, h := range top.Hypervisors {
		modes[h.Mode] = true
		for _, g := range h.GPUs {
			if len(g.VMs) >= 2 {
				multi[h.Mode] = true
			}
			if len(g.VMs) == 0 {
				unassignedGPU = true
			}
			used := map[int]bool{}
			for _, vm := range g.VMs {
				used[vm.MIGGPUInstanceID] = true
			}
			for _, mi := range g.MIGInstances {
				if !used[mi.GPUInstanceID] {
					unassignedMIG = true
				}
			}
		}
	}
	for _, m := range []GPUMode{ModeVGPU, ModeMIG, ModePassthrough} {
		if !modes[m] {
			t.Errorf("no node with mode %s", m)
		}
	}
	if !multi[ModeVGPU] || !multi[ModeMIG] {
		t.Errorf("modes with a GPU hosting 2+ VMs: %v (need both VGPU and MIG)", multi)
	}
	if !unassignedGPU {
		t.Error("no unassigned GPU")
	}
	if !unassignedMIG {
		t.Error("no unassigned MIG instance")
	}
}

func TestDefaultFields(t *testing.T) {
	top := Default()
	for _, h := range top.Hypervisors {
		if !uuidRE.MatchString(h.UUID) {
			t.Errorf("%s uuid %s", h.Hostname, h.UUID)
		}
		if (h.Mode == ModePassthrough) != (h.DriverVersion == "") {
			t.Errorf("%s mode %s driver %q", h.Hostname, h.Mode, h.DriverVersion)
		}
		for _, g := range h.GPUs {
			if !strings.HasPrefix(g.UUID, "GPU-") || !uuidRE.MatchString(strings.TrimPrefix(g.UUID, "GPU-")) {
				t.Errorf("gpu uuid %s", g.UUID)
			}
			if !strings.HasSuffix(strings.ToLower(g.BusID), g.HostBDF) || g.HostBDF != strings.ToLower(g.HostBDF) {
				t.Errorf("bus_id %s host_bdf %s", g.BusID, g.HostBDF)
			}
			for _, vm := range g.VMs {
				f := top.FlavorByID(vm.FlavorID)
				if f == nil || f.VCPUs != vm.VCPUs || f.RAMMB != vm.MemoryMB {
					t.Errorf("%s flavor mismatch", vm.Name)
					continue
				}
				_, pt := f.ExtraSpecs["pci_passthrough:alias"]
				_, vg := f.ExtraSpecs["resources:VGPU"]
				if (vm.Mode == ModePassthrough) != pt || (vm.Mode != ModePassthrough) != vg {
					t.Errorf("%s has mode %s but flavor %s", vm.Name, vm.Mode, f.Name)
				}
				if vm.ID != StableUUID("server", vm.Name) {
					t.Errorf("%s id is not derived from its name", vm.Name)
				}
			}
			if h.Mode == ModeMIG {
				for i, vm := range g.VMs {
					mi := g.MIGInstances[i]
					if vm.MIGGPUInstanceID != mi.GPUInstanceID || top.FlavorByID(vm.FlavorID).Name[len("emu-mig-"):] != mi.Profile[:2] {
						t.Errorf("%s instance %d flavor %s profile %s", vm.Name, vm.MIGGPUInstanceID, top.FlavorByID(vm.FlavorID).Name, mi.Profile)
					}
				}
			}
		}
	}
	v := top.AllVMs()[0]
	if v.Name != "emu-vgpu01-g0-1" || v.InstanceName != "instance-00000001" || v.IP != "198.51.100.11" {
		t.Errorf("first VM %s %s %s", v.Name, v.InstanceName, v.IP)
	}
}

func TestAddresses(t *testing.T) {
	cases := map[int]string{1: "198.51.100.11", 244: "198.51.100.254", 245: "203.0.113.1", 498: "203.0.113.254"}
	for seq, want := range cases {
		if got := vmIP(seq); got != want {
			t.Errorf("vmIP(%d) = %s, want %s", seq, got, want)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("vmIP(499) did not panic")
			}
		}()
		vmIP(499)
	}()
	if got := hostIP(0); got != "192.0.2.11" {
		t.Errorf("hostIP(0) = %s", got)
	}

	testNet := regexp.MustCompile(`^(192\.0\.2|198\.51\.100|203\.0\.113)\.([0-9]+)$`)
	top := Default()
	ips := map[string]bool{}
	check := func(who, ip string) {
		m := testNet.FindStringSubmatch(ip)
		if m == nil || m[2] == "0" || m[2] == "255" {
			t.Errorf("%s has address %q, which is not a host address in a documentation range", who, ip)
		}
		if ips[ip] {
			t.Errorf("duplicate address: %s", ip)
		}
		ips[ip] = true
	}
	for _, h := range top.Hypervisors {
		check(h.Hostname, h.HostIP)
	}
	for _, vm := range top.AllVMs() {
		check(vm.Name, vm.IP)
	}
	if v := top.HypervisorByName("emu-vgpu-02").GPUs[0].VBIOS; v != vbios("90", "04", "96", "00", "01") || len(v) != 14 {
		t.Errorf("T4 VBIOS %q", v)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	want := Default()
	data, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "topology.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("topology written to JSON and read back differs from Default")
	}

	empty, err := Load("")
	if err != nil || !reflect.DeepEqual(empty, want) {
		t.Fatalf(`Load("") differs from Default: %v`, err)
	}
}

func TestLoadRejects(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cases := map[string]string{
		"unknown.json":  `{"region":"RegionOne","bogus":1}`,
		"trailing.json": `{"project_id":"p","user_name":"u"} {}`,
		"broken.json":   `{"region":`,
	}
	for name, body := range cases {
		if _, err := Load(write(name, body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file was accepted")
	}

	// A well-formed file that fails Validate.
	bad := Default()
	bad.AllVMs()[1].Name = bad.AllVMs()[0].Name
	data, _ := json.Marshal(bad)
	_, err := Load(write("dup.json", string(data)))
	if err == nil || !strings.Contains(err.Error(), "duplicate vm name") {
		t.Errorf("file with duplicate VM names: %v", err)
	}
}

func findGPU(top *Topology, host string, idx int) *GPU {
	return top.HypervisorByName(host).GPUs[idx]
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Topology)
		want   string
	}{
		{"duplicate VM name", func(tp *Topology) {
			vms := tp.AllVMs()
			vms[3].Name = vms[2].Name
		}, "duplicate vm name"},
		{"duplicate VM id", func(tp *Topology) {
			vms := tp.AllVMs()
			vms[3].ID = vms[2].ID
		}, "duplicate vm id"},
		{"duplicate hypervisor name", func(tp *Topology) {
			tp.Hypervisors[1].Hostname = tp.Hypervisors[0].Hostname
		}, "duplicate hypervisor hostname"},
		{"duplicate flavor name", func(tp *Topology) {
			tp.Flavors[1].Name = tp.Flavors[0].Name
		}, "duplicate flavor name"},
		{"missing flavor", func(tp *Topology) {
			tp.AllVMs()[0].FlavorID = "no-such-flavor"
		}, `flavor "no-such-flavor" not found`},
		{"missing hypervisor", func(tp *Topology) {
			tp.AllVMs()[0].Hypervisor = "emu-nowhere"
		}, `hypervisor "emu-nowhere"`},
		{"wrong gpu_index", func(tp *Topology) {
			tp.AllVMs()[0].GPUIndex = 9
		}, "gpu_index 9"},
		{"two VMs on a passthrough GPU", func(tp *Topology) {
			g0 := findGPU(tp, "emu-pt-02", 0)
			g1 := findGPU(tp, "emu-pt-02", 1)
			vm := g1.VMs[0]
			vm.GPUIndex = 0
			g0.VMs = append(g0.VMs, vm)
			g1.VMs = nil
		}, "passthrough GPU has 2 VMs"},
		{"mode mismatch", func(tp *Topology) {
			findGPU(tp, "emu-vgpu-01", 0).VMs[0].Mode = ModePassthrough
		}, "does not match GPU mode"},
		{"vGPU maximum exceeded", func(tp *Topology) {
			findGPU(tp, "emu-vgpu-02", 0).Profile.MaxInstances = 3
		}, "exceed the vGPU maximum of 3"},
		{"missing MIG instance", func(tp *Topology) {
			findGPU(tp, "emu-mig-01", 0).VMs[0].MIGGPUInstanceID = 99
		}, "GPU instance 99 not found"},
		{"MIG instance assigned twice", func(tp *Topology) {
			g := findGPU(tp, "emu-mig-01", 0)
			g.VMs[1].MIGGPUInstanceID = g.VMs[0].MIGGPUInstanceID
		}, "is already used by"},
		{"MIG slices exceeded", func(tp *Topology) {
			g := findGPU(tp, "emu-mig-01", 3)
			g.MIGInstances = append(g.MIGInstances, &MIGInstance{GPUInstanceID: 7, Profile: "1g.10gb", Slices: 1})
		}, "slice total 8"},
		{"instance id on a non-MIG node", func(tp *Topology) {
			findGPU(tp, "emu-pt-01", 0).VMs[0].MIGGPUInstanceID = 0
		}, "must be -1"},
	}
	for _, c := range cases {
		tp := Default()
		c.mutate(tp)
		err := tp.Validate()
		if err == nil {
			t.Errorf("%s: Validate accepted it", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error does not contain %q: %v", c.name, c.want, err)
		}
	}
}
