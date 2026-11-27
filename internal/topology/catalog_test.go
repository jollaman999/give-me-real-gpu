package topology

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultCatalog(t *testing.T) {
	c := DefaultCatalog()
	if !reflect.DeepEqual(c, DefaultCatalog()) {
		t.Fatal("two DefaultCatalog() calls differ")
	}
	want := map[string][]GPUMode{
		"NVIDIA A10":            {ModeVGPU, ModePassthrough},
		"Tesla T4":              {ModeVGPU, ModePassthrough},
		"NVIDIA A100-SXM4-80GB": {ModeMIG, ModePassthrough},
		"NVIDIA H100 80GB HBM3": {ModeMIG, ModePassthrough},
		"NVIDIA L40S":           {ModePassthrough},
		"NVIDIA A100-PCIE-40GB": {ModePassthrough},
		"NVIDIA RTX PRO 6000 Blackwell Server Edition": {ModeVGPU, ModeMIG, ModePassthrough},
	}
	if len(c.GPUModels) != len(want) {
		t.Fatalf("got %d models, want %d", len(c.GPUModels), len(want))
	}
	for _, m := range c.GPUModels {
		if !reflect.DeepEqual(m.Modes, want[m.Name]) {
			t.Errorf("%s: modes %v, want %v", m.Name, m.Modes, want[m.Name])
		}
	}
	a100 := c.model("NVIDIA A100-SXM4-80GB")
	var names []string
	for _, p := range a100.MIGProfiles {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "1g.10gb,2g.20gb,3g.40gb,7g.80gb" {
		t.Errorf("A100 MIG profiles %s", got)
	}
	if p := c.mig("NVIDIA A100-SXM4-80GB", "1g.10gb"); p == nil || p.Flavor != "emu-mig-1g" || p.GPUInstanceIDs[0] != 7 {
		t.Errorf("A100 1g profile %+v", p)
	}
	if f := c.model("NVIDIA A10").VGPUProfiles[0]; f.Name != "NVIDIA A10-4Q" || f.Flavor != "emu-vgpu-a10-4q" || f.MaxInstances != 6 {
		t.Errorf("A10 vGPU profile %+v", f)
	}
	if f := c.model("NVIDIA L40S").PassthroughFlavor; f != "emu-pt-l40s" {
		t.Errorf("L40S passthrough flavor %s", f)
	}
	if f := c.model("Tesla T4").PassthroughFlavor; f != "emu-pt-t4" {
		t.Errorf("T4 derived passthrough flavor %s", f)
	}
	for _, f := range c.Flavors {
		if c.Defaults.MaxMIGSlices != 7 || f.ID != StableUUID("flavor", f.Name) {
			t.Errorf("flavor %s id %s", f.Name, f.ID)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"gpu_models"`, `"vgpu_profiles"`, `"mig_profiles"`, `"gpu_instance_ids"`, `"framebuffer_mb"`, `"flavor":"emu-vgpu-a10-4q"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("catalog JSON lacks %s", key)
		}
	}
}

// clearFilled empties every field Fill knows how to fill.
func clearFilled(top *Topology) {
	top.Region, top.DomainID, top.ProjectID, top.UserID = "", "", "", ""
	for _, f := range top.Flavors {
		f.ID = ""
	}
	for _, h := range top.Hypervisors {
		h.ID, h.UUID, h.HostIP = 0, "", ""
		h.VCPUs, h.MemoryMB, h.LocalGB = 0, 0, 0
		h.DriverVersion, h.CUDAVersion = "", ""
		for _, g := range h.GPUs {
			g.UUID, g.BusID, g.HostBDF = "", "", ""
			g.DeviceID, g.MemoryMiB, g.VBIOS, g.Arch = "", 0, "", ""
			if h.GPUMode(g) == ModeVGPU {
				g.Profile = nil
			}
			for _, mi := range g.MIGInstances {
				mi.GPUInstanceID, mi.Slices, mi.MemoryMiB, mi.VGPU = -1, 0, 0, nil
			}
			for _, vm := range g.VMs {
				vm.ID, vm.InstanceName, vm.FlavorID, vm.IP, vm.VGPUUUID = "", "", "", "", ""
				vm.Hypervisor, vm.Mode, vm.GuestBDF, vm.GuestDriverVersion = "", "", "", ""
				vm.VCPUs, vm.MemoryMB, vm.DiskGB = 0, 0, 0
				if h.GPUMode(g) == ModeMIG {
					vm.MIGGPUInstanceID = -1
				}
			}
		}
	}
}

func TestFillRestoresDefault(t *testing.T) {
	want := Default()
	// Node sizes of the default differ per node; Fill writes one fixed size, so only the default-sized ones round-trip.
	top := Default()
	clearFilled(top)
	Fill(top)
	for i, h := range top.Hypervisors {
		w := want.Hypervisors[i]
		if h.VCPUs != fillNodeVCPUs || h.MemoryMB != fillNodeMemoryMB || h.LocalGB != fillNodeLocalGB {
			t.Errorf("%s: filled size %d/%d/%d", h.Hostname, h.VCPUs, h.MemoryMB, h.LocalGB)
		}
		h.VCPUs, h.MemoryMB, h.LocalGB = w.VCPUs, w.MemoryMB, w.LocalGB
	}
	if !reflect.DeepEqual(top, want) {
		a, _ := json.MarshalIndent(top, "", " ")
		b, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("Fill after clearing differs from Default:\n%s", firstDiff(string(a), string(b)))
	}
}

func firstDiff(a, b string) string {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) && i < len(lb); i++ {
		if la[i] != lb[i] {
			return "line " + la[i] + "\nwant " + lb[i]
		}
	}
	return "length differs"
}

func TestFillKeepsSetValues(t *testing.T) {
	want := Default()
	top := Default()
	Fill(top)
	if !reflect.DeepEqual(top, want) {
		t.Fatal("Fill changed a complete topology")
	}

	top = Default()
	h := top.Hypervisors[0]
	h.HostIP = "192.0.2.200"
	h.ID = 42
	vm := h.GPUs[0].VMs[0]
	vm.ID = "my-id"
	vm.IP = "203.0.113.250"
	vm.VCPUs = 3
	vm.InstanceName = ""
	Fill(top)
	if h.HostIP != "192.0.2.200" || h.ID != 42 || vm.ID != "my-id" || vm.IP != "203.0.113.250" || vm.VCPUs != 3 {
		t.Fatalf("Fill overwrote set values: %+v %+v", h, vm)
	}
	if vm.InstanceName != "instance-00000001" {
		t.Fatalf("instance name %q", vm.InstanceName)
	}
}

// nameOnly is what the editor sends for new things: names, mode, model and placement, nothing else.
const nameOnly = `{
  "hypervisors": [
    {"hostname": "emu-vgpu-01", "mode": "VGPU", "gpus": [
      {"index": 0, "model_name": "NVIDIA A10", "vms": [{"name": "a"}, {"name": "b", "mig_gpu_instance_id": -1}]}
    ]},
    {"hostname": "new-mig", "mode": "MIG", "gpus": [
      {"index": 0, "model_name": "NVIDIA H100 80GB HBM3", "mig_instances": [
        {"gpu_instance_id": -1, "profile": "3g.40gb"}, {"gpu_instance_id": -1, "profile": "1g.10gb"}, {"gpu_instance_id": -1, "profile": "1g.10gb"}
      ], "vms": [{"name": "m1", "mig_gpu_instance_id": -1}, {"name": "m2", "mig_gpu_instance_id": -1}]}
    ]},
    {"hostname": "new-pt", "mode": "PASSTHROUGH", "gpus": [
      {"index": 0, "model_name": "Tesla T4", "vms": [{"name": "p1", "mig_gpu_instance_id": -1}]},
      {"index": 1, "model_name": "Some Future GPU", "vms": [{"name": "p2", "mig_gpu_instance_id": -1}]}
    ]}
  ]
}`

func TestFillNameOnly(t *testing.T) {
	var top Topology
	if err := json.Unmarshal([]byte(nameOnly), &top); err != nil {
		t.Fatal(err)
	}
	// "a" has no mig_gpu_instance_id, so it decodes as 0 on a VGPU node: Validate must still report that.
	top.Hypervisors[0].GPUs[0].VMs[0].MIGGPUInstanceID = -1
	Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatalf("filled topology does not validate:\n%v", err)
	}
	if top.ProjectID != StableHex32("project", "emu-gpu") || top.Region != "RegionOne" {
		t.Errorf("project %s region %s", top.ProjectID, top.Region)
	}

	h0, h1, h2 := top.Hypervisors[0], top.Hypervisors[1], top.Hypervisors[2]
	if h0.ID != 1 || h1.ID != 2 || h2.ID != 3 {
		t.Errorf("hypervisor ids %d %d %d", h0.ID, h1.ID, h2.ID)
	}
	if h0.HostIP != "192.0.2.11" || h2.HostIP != "192.0.2.13" {
		t.Errorf("host ips %s %s", h0.HostIP, h2.HostIP)
	}
	if h2.DriverVersion != "" || h0.DriverVersion != hostDriver {
		t.Errorf("driver %q %q", h0.DriverVersion, h2.DriverVersion)
	}
	// default node name: its bus list is used.
	if g := h0.GPUs[0]; g.BusID != "00000000:18:00.0" || g.HostBDF != "18:00.0" || g.Profile == nil || g.Profile.Name != "NVIDIA A10-4Q" || g.DeviceID != "2236" {
		t.Errorf("vgpu gpu %+v profile %+v", g, g.Profile)
	}
	if g := h2.GPUs[1]; g.BusID != "00000000:2A:00.0" {
		t.Errorf("second passthrough gpu bus %s", g.BusID)
	}

	mig := h1.GPUs[0]
	var gi []int
	for _, mi := range mig.MIGInstances {
		gi = append(gi, mi.GPUInstanceID)
	}
	if !reflect.DeepEqual(gi, []int{1, 7, 8}) {
		t.Errorf("GI ids %v", gi)
	}
	if mi := mig.MIGInstances[0]; mi.Slices != 3 || mi.MemoryMiB != 40448 || mi.VGPU == nil || mi.VGPU.Type != "nvidia-1076" {
		t.Errorf("3g instance %+v", mi)
	}
	m1, m2 := mig.VMs[0], mig.VMs[1]
	if m1.MIGGPUInstanceID != 1 || m2.MIGGPUInstanceID != 7 {
		t.Errorf("vm instances %d %d", m1.MIGGPUInstanceID, m2.MIGGPUInstanceID)
	}
	if f := top.FlavorByID(m1.FlavorID); f == nil || f.Name != "emu-mig-3g" || m1.VCPUs != 24 {
		t.Errorf("m1 flavor %+v vcpus %d", f, m1.VCPUs)
	}
	if f := top.FlavorByID(m2.FlavorID); f == nil || f.Name != "emu-mig-1g" {
		t.Errorf("m2 flavor %+v", f)
	}

	a := h0.GPUs[0].VMs[0]
	if a.ID != StableUUID("server", "a") || a.VGPUUUID != StableUUID("mdev", "a") || a.InstanceName != "instance-00000001" ||
		a.IP != "198.51.100.11" || a.Hypervisor != "emu-vgpu-01" || a.Mode != ModeVGPU || a.GuestBDF != "00:05.0" {
		t.Errorf("vm a %+v", a)
	}
	if f := top.FlavorByID(a.FlavorID); f == nil || f.Name != "emu-vgpu-a10-4q" {
		t.Errorf("vm a flavor %+v", f)
	}

	p1, p2 := h2.GPUs[0].VMs[0], h2.GPUs[1].VMs[0]
	if p1.VGPUUUID != "" || p1.GuestDriverVersion != passthruGuest || p1.InstanceName != "instance-00000005" || p1.IP != "198.51.100.15" {
		t.Errorf("vm p1 %+v", p1)
	}
	f1, f2 := top.FlavorByID(p1.FlavorID), top.FlavorByID(p2.FlavorID)
	if f1 == nil || f1.Name != "emu-pt-t4" || f1.ExtraSpecs["pci_passthrough:alias"] != "t4:1" {
		t.Errorf("derived flavor %+v", f1)
	}
	if f2 == nil || f2.Name != "emu-pt-some-future-gpu" || f2.ID != StableUUID("flavor", "emu-pt-some-future-gpu") {
		t.Errorf("derived flavor %+v", f2)
	}
	var fnames []string
	for _, f := range top.Flavors {
		fnames = append(fnames, f.Name)
	}
	if got := strings.Join(fnames, ","); got != "emu-vgpu-a10-4q,emu-mig-3g,emu-mig-1g,emu-pt-t4,emu-pt-some-future-gpu" {
		t.Errorf("flavors added %s", got)
	}

	again, _ := json.Marshal(&top)
	Fill(&top)
	if after, _ := json.Marshal(&top); string(after) != string(again) {
		t.Error("second Fill changed the topology")
	}
}

func TestFillAvoidsTakenValues(t *testing.T) {
	top := Default()
	h := top.Hypervisors[0]
	// The next VM ip and instance name by position are taken by a later VM, and the node's first bus by another GPU.
	h.GPUs[3].VMs = []*VM{{Name: "late", MIGGPUInstanceID: -1}}
	last := top.Hypervisors[5].GPUs[3].VMs[0]
	h.GPUs = append(h.GPUs, &GPU{Index: 4, ModelName: "NVIDIA A10"})
	Fill(top)
	late := h.GPUs[3].VMs[0]
	if late.InstanceName == "" || late.IP == "" {
		t.Fatalf("late not filled %+v", late)
	}
	seen := map[string]string{}
	for _, vm := range top.AllVMs() {
		for _, v := range []string{vm.IP, vm.InstanceName} {
			if o, ok := seen[v]; ok {
				t.Errorf("%s used by %s and %s", v, o, vm.Name)
			}
			seen[v] = vm.Name
		}
	}
	if last.IP == late.IP {
		t.Errorf("late got the ip of %s", last.Name)
	}
	if g := h.GPUs[4]; g.BusID != "00000000:2A:00.0" || g.UUID != "GPU-"+StableUUID("gpu", h.Hostname, "4") {
		t.Errorf("new gpu %+v", g)
	}
	if err := top.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestFillNil(t *testing.T) {
	Fill(nil)
	top := &Topology{Hypervisors: []*Hypervisor{nil, {Hostname: "x", Mode: ModeVGPU, GPUs: []*GPU{nil, {Index: 1, VMs: []*VM{nil}}}}}}
	Fill(top)
	if h := top.Hypervisors[1]; h.ID != 2 || h.UUID == "" {
		t.Fatalf("hypervisor after nil entries %+v", h)
	}
}
