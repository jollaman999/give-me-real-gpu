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
		t.Fatalf("같은 이름에서 다른 UUID: %s", a)
	}
	if !uuidRE.MatchString(a) {
		t.Fatalf("UUID v5 형식 아님: %s", a)
	}
	if StableUUID("ab", "c") == StableUUID("a", "bc") {
		t.Fatal("조각 경계가 다른데 같은 UUID")
	}
	if StableUUID("server", "x") == StableUUID("mdev", "x") {
		t.Fatal("종류가 다른데 같은 UUID")
	}
	h := StableHex32("project", "emu-gpu")
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(h) {
		t.Fatalf("32자 hex 아님: %s", h)
	}
}

func TestDefaultDeterministicAndValid(t *testing.T) {
	a, b := Default(), Default()
	if !reflect.DeepEqual(a, b) {
		t.Fatal("Default() 두 번 결과가 다르다")
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Default 가 Validate 실패: %v", err)
	}
}

// 설계 문서 표의 노드별 GPU 마다 VM 수.
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
		t.Fatalf("노드 수 %d, 기대 %d", len(top.Hypervisors), len(want))
	}
	total := 0
	for _, h := range top.Hypervisors {
		var got []int
		for _, g := range h.GPUs {
			got = append(got, len(g.VMs))
			total += len(g.VMs)
		}
		if !reflect.DeepEqual(got, want[h.Hostname]) {
			t.Errorf("%s GPU 별 VM 수 %v, 기대 %v", h.Hostname, got, want[h.Hostname])
		}
		t.Logf("%-12s %-11s GPU %d장 VM %d개 %v", h.Hostname, h.Mode, len(h.GPUs), sum(got), got)
	}
	if total != 45 || len(top.AllVMs()) != 45 {
		t.Errorf("총 VM %d (AllVMs %d), 기대 45", total, len(top.AllVMs()))
	}
	t.Logf("총 VM %d", total)

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
		t.Errorf("MIG instance 수 %v", migInst)
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
			t.Errorf("모드 %s 노드가 없다", m)
		}
	}
	if !multi[ModeVGPU] || !multi[ModeMIG] {
		t.Errorf("VM 2개 이상인 GPU: %v (VGPU·MIG 둘 다 있어야 한다)", multi)
	}
	if !unassignedGPU {
		t.Error("미할당 GPU 가 없다")
	}
	if !unassignedMIG {
		t.Error("미할당 MIG instance 가 없다")
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
					t.Errorf("%s flavor 불일치", vm.Name)
					continue
				}
				_, pt := f.ExtraSpecs["pci_passthrough:alias"]
				_, vg := f.ExtraSpecs["resources:VGPU"]
				if (vm.Mode == ModePassthrough) != pt || (vm.Mode != ModePassthrough) != vg {
					t.Errorf("%s mode %s 인데 flavor %s", vm.Name, vm.Mode, f.Name)
				}
				if vm.ID != StableUUID("server", vm.Name) {
					t.Errorf("%s id 가 이름에서 나온 값이 아니다", vm.Name)
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
		t.Errorf("첫 VM %s %s %s", v.Name, v.InstanceName, v.IP)
	}
}

func TestAddresses(t *testing.T) {
	cases := map[int]string{1: "198.51.100.11", 244: "198.51.100.254", 245: "203.0.113.1", 498: "203.0.113.254"}
	for seq, want := range cases {
		if got := vmIP(seq); got != want {
			t.Errorf("vmIP(%d) = %s, 기대 %s", seq, got, want)
		}
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("vmIP(499) 가 panic 하지 않았다")
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
			t.Errorf("%s 의 주소 %q 가 문서용 대역의 호스트 주소가 아니다", who, ip)
		}
		if ips[ip] {
			t.Errorf("주소 중복: %s", ip)
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
		t.Fatal("JSON 으로 쓰고 다시 읽은 토폴로지가 Default 와 다르다")
	}

	empty, err := Load("")
	if err != nil || !reflect.DeepEqual(empty, want) {
		t.Fatalf(`Load("") 가 Default 와 다르다: %v`, err)
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
			t.Errorf("%s 를 받아들였다", name)
		}
	}
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("없는 파일을 받아들였다")
	}

	// 형식은 맞지만 Validate 에 걸리는 파일.
	bad := Default()
	bad.AllVMs()[1].Name = bad.AllVMs()[0].Name
	data, _ := json.Marshal(bad)
	_, err := Load(write("dup.json", string(data)))
	if err == nil || !strings.Contains(err.Error(), "vm name 중복") {
		t.Errorf("중복 VM 이름 파일: %v", err)
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
		{"VM 이름 중복", func(tp *Topology) {
			vms := tp.AllVMs()
			vms[3].Name = vms[2].Name
		}, "vm name 중복"},
		{"VM id 중복", func(tp *Topology) {
			vms := tp.AllVMs()
			vms[3].ID = vms[2].ID
		}, "vm id 중복"},
		{"hypervisor 이름 중복", func(tp *Topology) {
			tp.Hypervisors[1].Hostname = tp.Hypervisors[0].Hostname
		}, "hypervisor hostname 중복"},
		{"flavor 이름 중복", func(tp *Topology) {
			tp.Flavors[1].Name = tp.Flavors[0].Name
		}, "flavor name 중복"},
		{"없는 flavor", func(tp *Topology) {
			tp.AllVMs()[0].FlavorID = "no-such-flavor"
		}, `flavor "no-such-flavor" 가 없다`},
		{"없는 hypervisor", func(tp *Topology) {
			tp.AllVMs()[0].Hypervisor = "emu-nowhere"
		}, `hypervisor "emu-nowhere"`},
		{"틀린 gpu_index", func(tp *Topology) {
			tp.AllVMs()[0].GPUIndex = 9
		}, "gpu_index 9"},
		{"passthrough GPU 에 VM 2개", func(tp *Topology) {
			g0 := findGPU(tp, "emu-pt-02", 0)
			g1 := findGPU(tp, "emu-pt-02", 1)
			vm := g1.VMs[0]
			vm.GPUIndex = 0
			g0.VMs = append(g0.VMs, vm)
			g1.VMs = nil
		}, "passthrough GPU 에 VM 이 2 개다"},
		{"모드 불일치", func(tp *Topology) {
			findGPU(tp, "emu-vgpu-01", 0).VMs[0].Mode = ModePassthrough
		}, "hypervisor mode"},
		{"vGPU 최대 초과", func(tp *Topology) {
			findGPU(tp, "emu-vgpu-02", 0).Profile.MaxInstances = 3
		}, "vGPU 최대 3 개를 넘는다"},
		{"없는 MIG instance", func(tp *Topology) {
			findGPU(tp, "emu-mig-01", 0).VMs[0].MIGGPUInstanceID = 99
		}, "GPU instance 99 가"},
		{"MIG instance 이중 할당", func(tp *Topology) {
			g := findGPU(tp, "emu-mig-01", 0)
			g.VMs[1].MIGGPUInstanceID = g.VMs[0].MIGGPUInstanceID
		}, "이미 쓴다"},
		{"MIG slice 초과", func(tp *Topology) {
			g := findGPU(tp, "emu-mig-01", 3)
			g.MIGInstances = append(g.MIGInstances, &MIGInstance{GPUInstanceID: 7, Profile: "1g.10gb", Slices: 1})
		}, "slice 합 8"},
		{"MIG 아닌데 instance id", func(tp *Topology) {
			findGPU(tp, "emu-pt-01", 0).VMs[0].MIGGPUInstanceID = 0
		}, "-1 이어야 한다"},
	}
	for _, c := range cases {
		tp := Default()
		c.mutate(tp)
		err := tp.Validate()
		if err == nil {
			t.Errorf("%s: Validate 가 통과시켰다", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: 오류에 %q 가 없다: %v", c.name, c.want, err)
		}
	}
}
