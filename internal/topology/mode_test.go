package topology

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestGPUModeAndModes(t *testing.T) {
	h := &Hypervisor{Mode: ModeVGPU}
	if got := h.Modes(); !reflect.DeepEqual(got, []GPUMode{ModeVGPU}) {
		t.Errorf("no GPUs: modes %v", got)
	}
	g0 := &GPU{Index: 0}
	g1 := &GPU{Index: 1, Mode: ModePassthrough}
	g2 := &GPU{Index: 2, Mode: ModeMIG}
	h.GPUs = []*GPU{g1, nil, g0, g2}
	if h.GPUMode(g0) != ModeVGPU || h.GPUMode(g1) != ModePassthrough || h.GPUMode(g2) != ModeMIG {
		t.Errorf("GPUMode %s %s %s", h.GPUMode(g0), h.GPUMode(g1), h.GPUMode(g2))
	}
	if got := h.Modes(); !reflect.DeepEqual(got, []GPUMode{ModeVGPU, ModeMIG, ModePassthrough}) {
		t.Errorf("mixed: modes %v", got)
	}
	h = &Hypervisor{Mode: ModeMIG, GPUs: []*GPU{{Mode: ModePassthrough}, {Mode: ModePassthrough}}}
	if got := h.Modes(); !reflect.DeepEqual(got, []GPUMode{ModePassthrough}) {
		t.Errorf("all passthrough: modes %v", got)
	}
}

// mixedNodes has a VGPU node with a passthrough GPU, a MIG node with a VGPU GPU and a passthrough node
// with a VGPU GPU, all written the way the editor sends them.
const mixedNodes = `{
  "hypervisors": [
    {"hostname": "mix-vgpu", "mode": "VGPU", "gpus": [
      {"index": 0, "model_name": "NVIDIA A10", "vms": [{"name": "v1", "mig_gpu_instance_id": -1}, {"name": "v2", "mig_gpu_instance_id": -1}]},
      {"index": 1, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": [{"name": "p1", "mig_gpu_instance_id": -1}]}
    ]},
    {"hostname": "mix-mig", "mode": "MIG", "gpus": [
      {"index": 0, "model_name": "NVIDIA A100-SXM4-80GB", "mig_instances": [{"gpu_instance_id": -1, "profile": "3g.40gb"}],
       "vms": [{"name": "m1", "mig_gpu_instance_id": -1}]},
      {"index": 1, "model_name": "Tesla T4", "mode": "VGPU", "vms": [{"name": "t1", "mig_gpu_instance_id": -1}]}
    ]},
    {"hostname": "mix-pt", "mode": "PASSTHROUGH", "gpus": [
      {"index": 0, "model_name": "NVIDIA L40S", "vms": [{"name": "p2", "mig_gpu_instance_id": -1}]},
      {"index": 1, "model_name": "NVIDIA L40S", "vms": []}
    ]},
    {"hostname": "pt-only", "mode": "VGPU", "gpus": [
      {"index": 0, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": []}
    ]}
  ]
}`

func loadMixed(t *testing.T) *Topology {
	t.Helper()
	var top Topology
	if err := json.Unmarshal([]byte(mixedNodes), &top); err != nil {
		t.Fatal(err)
	}
	Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatalf("filled mixed topology does not validate:\n%v", err)
	}
	return &top
}

func TestMixedNodeFillAndValidate(t *testing.T) {
	top := loadMixed(t)
	hv, hm, hp, ho := top.Hypervisors[0], top.Hypervisors[1], top.Hypervisors[2], top.Hypervisors[3]
	for _, h := range []*Hypervisor{hv, hm} {
		if h.DriverVersion != hostDriver || h.CUDAVersion != hostCUDA {
			t.Errorf("%s: driver %q cuda %q, want the host driver", h.Hostname, h.DriverVersion, h.CUDAVersion)
		}
	}
	for _, h := range []*Hypervisor{hp, ho} {
		if h.DriverVersion != "" || h.CUDAVersion != "" {
			t.Errorf("%s: passthrough-only node got driver %q cuda %q", h.Hostname, h.DriverVersion, h.CUDAVersion)
		}
	}
	if hv.GPUs[0].Mode != "" || hv.GPUs[1].Mode != ModePassthrough {
		t.Errorf("Fill changed GPU modes: %q %q", hv.GPUs[0].Mode, hv.GPUs[1].Mode)
	}
	if hv.GPUs[1].Profile != nil || hv.GPUs[0].Profile == nil || hv.GPUs[0].Profile.Name != "NVIDIA A10-4Q" {
		t.Errorf("profiles %+v %+v", hv.GPUs[0].Profile, hv.GPUs[1].Profile)
	}
	if g := hm.GPUs[1]; g.Profile == nil || g.Profile.Name != "GRID T4-4Q" || len(g.MIGInstances) != 0 {
		t.Errorf("VGPU GPU on a MIG node: profile %+v instances %d", g.Profile, len(g.MIGInstances))
	}

	flavor := func(vm *VM) string {
		if f := top.FlavorByID(vm.FlavorID); f != nil {
			return f.Name
		}
		return ""
	}
	cases := []struct {
		vm     *VM
		mode   GPUMode
		flavor string
		mdev   bool
		gi     int
	}{
		{hv.GPUs[0].VMs[0], ModeVGPU, "emu-vgpu-a10-4q", true, -1},
		{hv.GPUs[1].VMs[0], ModePassthrough, "emu-pt-l40s", false, -1},
		{hm.GPUs[0].VMs[0], ModeMIG, "emu-mig-3g", true, 1},
		{hm.GPUs[1].VMs[0], ModeVGPU, "emu-vgpu-t4-4q", true, -1},
		{hp.GPUs[0].VMs[0], ModePassthrough, "emu-pt-l40s", false, -1},
	}
	for _, c := range cases {
		vm := c.vm
		if vm.Mode != c.mode || flavor(vm) != c.flavor || (vm.VGPUUUID != "") != c.mdev || vm.MIGGPUInstanceID != c.gi {
			t.Errorf("%s: mode %s flavor %s vgpu_uuid %q gi %d, want %s %s mdev=%v gi %d",
				vm.Name, vm.Mode, flavor(vm), vm.VGPUUUID, vm.MIGGPUInstanceID, c.mode, c.flavor, c.mdev, c.gi)
		}
	}
	if d := hv.GPUs[1].VMs[0].GuestDriverVersion; d != passthruGuest {
		t.Errorf("passthrough VM on a VGPU node guest driver %q", d)
	}
	if d := hm.GPUs[1].VMs[0].GuestDriverVersion; d != vgpuGuestDrv {
		t.Errorf("vGPU VM on a MIG node guest driver %q", d)
	}
}

func TestMixedNodeRejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Topology)
		want   string
	}{
		{"VM mode is the node mode, not the GPU mode", func(tp *Topology) {
			tp.Hypervisors[0].GPUs[1].VMs[0].Mode = ModeVGPU
		}, `vm p1: mode "VGPU" does not match GPU mode "PASSTHROUGH"`},
		{"two VMs on a passthrough GPU of a VGPU node", func(tp *Topology) {
			g := tp.Hypervisors[0].GPUs[1]
			vm := *g.VMs[0]
			vm.Name, vm.ID, vm.InstanceName = "p9", "p9-id", "instance-p9"
			g.VMs = append(g.VMs, &vm)
		}, "hypervisor mix-vgpu gpu 1: passthrough GPU has 2 VMs (max 1)"},
		{"VGPU GPU on a MIG node without profile", func(tp *Topology) {
			tp.Hypervisors[1].GPUs[1].Profile = nil
		}, "hypervisor mix-mig gpu 1: VGPU GPU has no profile"},
		{"instance id on the VGPU GPU of a MIG node", func(tp *Topology) {
			tp.Hypervisors[1].GPUs[1].VMs[0].MIGGPUInstanceID = 1
		}, "vm t1: mig_gpu_instance_id is 1 on a non-MIG GPU (must be -1)"},
		{"unknown GPU mode", func(tp *Topology) {
			tp.Hypervisors[2].GPUs[1].Mode = "SRIOV"
		}, `hypervisor mix-pt gpu 1: unknown mode "SRIOV"`},
	}
	for _, c := range cases {
		tp := loadMixed(t)
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

func TestBlackwellCatalog(t *testing.T) {
	c := DefaultCatalog()
	if len(c.GPUModels) != 7 || c.GPUModels[6].Name != modelRTXPro6000BW.name {
		t.Fatalf("models %d, last %q", len(c.GPUModels), c.GPUModels[len(c.GPUModels)-1].Name)
	}
	bw := c.GPUModels[6]
	if bw.DeviceID != "2bb5" || bw.MemoryMiB != 97887 || bw.VBIOS != vbios("98", "02", "67", "00", "0A") || bw.Arch != "Blackwell" || bw.MaxMIGSlices != 4 {
		t.Errorf("Blackwell model %+v", bw)
	}
	if len(bw.VGPUProfiles) != 14 {
		t.Errorf("Blackwell vGPU profiles %d, want 14 (7 Q + 7 C)", len(bw.VGPUProfiles))
	}
	// Every vGPU type of every model has its own flavor and mdev type.
	flavors, types := map[string]string{}, map[string]string{}
	for _, m := range c.GPUModels {
		for _, p := range m.VGPUProfiles {
			if o, ok := flavors[p.Flavor]; ok {
				t.Errorf("flavor %s used by %s and %s", p.Flavor, o, p.Name)
			}
			if o, ok := types[p.Type]; ok {
				t.Errorf("mdev type %s used by %s and %s", p.Type, o, p.Name)
			}
			flavors[p.Flavor], types[p.Type] = p.Name, p.Name
		}
		for _, p := range m.MIGProfiles {
			if len(p.VGPUProfiles) == 0 || p.VGPU != p.VGPUProfiles[0] {
				t.Errorf("%s %s: vgpu_profile %+v is not the first of %+v", m.Name, p.Name, p.VGPU, p.VGPUProfiles)
			}
			for _, v := range p.VGPUProfiles {
				if o, ok := types[v.Type]; ok {
					t.Errorf("mdev type %s used by %s and %s", v.Type, o, v.Name)
				}
				types[v.Type] = v.Name
			}
			if m.Name != bw.Name && len(p.VGPUProfiles) != 1 {
				t.Errorf("%s %s: %d vGPU types, want 1", m.Name, p.Name, len(p.VGPUProfiles))
			}
		}
	}
	var vgpu []string
	for _, p := range bw.VGPUProfiles {
		vgpu = append(vgpu, strings.TrimPrefix(p.Name, "NVIDIA RTX Pro 6000 Blackwell ")+"/"+p.Flavor)
	}
	want := "DC-24Q/emu-vgpu-rtx-pro-6000-blackwell-dc-24q,DC-8Q/emu-vgpu-rtx-pro-6000-blackwell-dc-8q," +
		"DC-12Q/emu-vgpu-rtx-pro-6000-blackwell-dc-12q,DC-16Q/emu-vgpu-rtx-pro-6000-blackwell-dc-16q," +
		"DC-32Q/emu-vgpu-rtx-pro-6000-blackwell-dc-32q,DC-48Q/emu-vgpu-rtx-pro-6000-blackwell-dc-48q," +
		"DC-96Q/emu-vgpu-rtx-pro-6000-blackwell-dc-96q," +
		"DC-8C/emu-vgpu-rtx-pro-6000-blackwell-dc-8c,DC-12C/emu-vgpu-rtx-pro-6000-blackwell-dc-12c," +
		"DC-16C/emu-vgpu-rtx-pro-6000-blackwell-dc-16c,DC-24C/emu-vgpu-rtx-pro-6000-blackwell-dc-24c," +
		"DC-32C/emu-vgpu-rtx-pro-6000-blackwell-dc-32c,DC-48C/emu-vgpu-rtx-pro-6000-blackwell-dc-48c," +
		"DC-96C/emu-vgpu-rtx-pro-6000-blackwell-dc-96c"
	if got := strings.Join(vgpu, ","); got != want {
		t.Errorf("Blackwell vGPU profiles\n got %s\nwant %s", got, want)
	}
	// Each vGPU type fills the GPU's framebuffer at its maximum instance count.
	for _, p := range bw.VGPUProfiles {
		if p.FramebufferMB*p.MaxInstances > 98304 || p.FramebufferMB*(p.MaxInstances+1) <= 98304 {
			t.Errorf("%s: %d MiB x %d", p.Name, p.FramebufferMB, p.MaxInstances)
		}
	}
	var mig []string
	for _, p := range bw.MIGProfiles {
		var vs []string
		for _, v := range p.VGPUProfiles {
			vs = append(vs, fmt.Sprintf("%s %d %d %d", strings.TrimPrefix(v.Name, "NVIDIA RTX Pro 6000 Blackwell "), v.FramebufferMB, v.MaxInstances, v.PerGPUInstance))
		}
		mig = append(mig, p.Name+"/"+p.Flavor+"/"+strings.Join(vs, "+"))
	}
	wantMIG := "1g.24gb/emu-mig-1g-24gb/DC-1-24Q 24576 4 0+DC-1-24C 24576 4 0+DC-1-12C 12288 8 2+DC-1-8C 8192 12 3," +
		"2g.48gb/emu-mig-2g-48gb/DC-2-48Q 49152 2 0+DC-2-48C 49152 2 0+DC-2-24C 24576 4 2+DC-2-16C 16384 6 3+DC-2-12C 12288 8 4," +
		"4g.96gb/emu-mig-4g-96gb/DC-4-96Q 98304 1 0+DC-4-96C 98304 1 0+DC-4-48C 49152 2 2+DC-4-32C 32768 3 3+DC-4-24C 24576 4 4"
	if got := strings.Join(mig, ","); got != wantMIG {
		t.Errorf("Blackwell MIG profiles\n got %s\nwant %s", got, wantMIG)
	}
	if p := c.mig(bw.Name, "2g.48gb"); p == nil || p.MemoryMiB != 47744 || !reflect.DeepEqual(p.GPUInstanceIDs, []int{1, 2}) || p.Slices != 2 {
		t.Errorf("2g.48gb %+v", p)
	}
	// Blackwell MIG flavors must not reuse a flavor name another model's MIG kind already uses.
	owner := map[string]string{}
	for _, m := range c.GPUModels {
		for _, p := range m.MIGProfiles {
			if o, ok := owner[p.Flavor]; ok && o != p.Name {
				t.Errorf("flavor %s used by %s and %s", p.Flavor, o, p.Name)
			}
			owner[p.Flavor] = p.Name
		}
	}
	slicesOf := map[string]int{}
	for _, m := range c.GPUModels {
		slicesOf[m.Name] = m.MaxMIGSlices
	}
	if slicesOf["NVIDIA A100-SXM4-80GB"] != 7 || slicesOf["NVIDIA H100 80GB HBM3"] != 7 || slicesOf["NVIDIA L40S"] != 0 || c.Defaults.MaxMIGSlices != 7 {
		t.Errorf("max_mig_slices %v defaults %d", slicesOf, c.Defaults.MaxMIGSlices)
	}
	if MIGSlices("NVIDIA L40S") != 7 || MIGSlices("Some Future GPU") != 7 || MIGSlices(bw.Name) != 4 {
		t.Errorf("MIGSlices %d %d %d", MIGSlices("NVIDIA L40S"), MIGSlices("Some Future GPU"), MIGSlices(bw.Name))
	}
	if _, ok := catalogMIG[bw.Name]; !ok {
		t.Error("Blackwell MIG kinds missing")
	}
	for _, n := range defaultNodes {
		if n.model.name == bw.Name {
			t.Errorf("default node %s uses the catalog-only model", n.hostname)
		}
	}
}

func blackwellMIG(profiles ...string) *Topology {
	g := map[string]any{"index": 0, "model_name": modelRTXPro6000BW.name, "vms": []any{}}
	var mi []any
	for _, p := range profiles {
		mi = append(mi, map[string]any{"gpu_instance_id": -1, "profile": p})
	}
	g["mig_instances"] = mi
	raw, _ := json.Marshal(map[string]any{"hypervisors": []any{map[string]any{"hostname": "bw", "mode": "MIG", "gpus": []any{g}}}})
	var top Topology
	if err := json.Unmarshal(raw, &top); err != nil {
		panic(err)
	}
	Fill(&top)
	return &top
}

func TestBlackwellMIGSlices(t *testing.T) {
	ok := blackwellMIG("2g.48gb", "1g.24gb", "1g.24gb")
	if err := ok.Validate(); err != nil {
		t.Fatalf("4 slices rejected: %v", err)
	}
	var gi []int
	for _, mi := range ok.Hypervisors[0].GPUs[0].MIGInstances {
		gi = append(gi, mi.GPUInstanceID)
	}
	if !reflect.DeepEqual(gi, []int{1, 3, 4}) {
		t.Errorf("GI ids %v", gi)
	}
	if mi := ok.Hypervisors[0].GPUs[0].MIGInstances[0]; mi.Slices != 2 || mi.MemoryMiB != 47744 || mi.VGPU == nil || mi.VGPU.MaxInstances != 2 {
		t.Errorf("2g instance %+v", mi)
	}

	bad := blackwellMIG("2g.48gb", "1g.24gb", "1g.24gb", "1g.24gb")
	err := bad.Validate()
	if err == nil || !strings.Contains(err.Error(), "hypervisor bw gpu 0: MIG slice total 5 exceeds 4") {
		t.Fatalf("5 slices: %v", err)
	}
	// The same 5 slices on an A100 stay under its 7.
	bad.Hypervisors[0].GPUs[0].ModelName = "NVIDIA A100-SXM4-80GB"
	if err := bad.Validate(); err != nil {
		t.Errorf("5 slices on A100: %v", err)
	}
}

func TestFillKeepsInstanceVGPU(t *testing.T) {
	top := blackwellMIG("2g.48gb", "2g.48gb")
	g := top.Hypervisors[0].GPUs[0]
	c := &VGPUProfile{"NVIDIA RTX Pro 6000 Blackwell DC-2-48C", "nvidia-1198", 49152, 2, 0}
	g.MIGInstances[1].VGPU = c
	Fill(top)
	if err := top.Validate(); err != nil {
		t.Fatal(err)
	}
	if v := g.MIGInstances[0].VGPU; v == nil || v.Name != "NVIDIA RTX Pro 6000 Blackwell DC-2-48Q" {
		t.Errorf("default instance vGPU %+v", v)
	}
	if v := g.MIGInstances[1].VGPU; v != c || v.Name != "NVIDIA RTX Pro 6000 Blackwell DC-2-48C" {
		t.Errorf("chosen instance vGPU %+v", v)
	}
}

func TestBlackwellFillFlavors(t *testing.T) {
	raw := `{"hypervisors": [{"hostname": "bw", "mode": "VGPU", "gpus": [
	  {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition", "vms": [{"name": "q1", "mig_gpu_instance_id": -1}]},
	  {"index": 1, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition", "mode": "MIG",
	   "mig_instances": [{"gpu_instance_id": -1, "profile": "1g.24gb"}], "vms": [{"name": "g1", "mig_gpu_instance_id": -1}]},
	  {"index": 2, "model_name": "NVIDIA A100-SXM4-80GB", "mode": "MIG",
	   "mig_instances": [{"gpu_instance_id": -1, "profile": "1g.10gb"}], "vms": [{"name": "a1", "mig_gpu_instance_id": -1}]}
	]}]}`
	var top Topology
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	h := top.Hypervisors[0]
	if p := h.GPUs[0].Profile; p == nil || p.Name != "NVIDIA RTX Pro 6000 Blackwell DC-24Q" || p.MaxInstances != 4 {
		t.Errorf("default Blackwell profile %+v", p)
	}
	q1, g1, a1 := h.GPUs[0].VMs[0], h.GPUs[1].VMs[0], h.GPUs[2].VMs[0]
	fq, fg, fa := top.FlavorByID(q1.FlavorID), top.FlavorByID(g1.FlavorID), top.FlavorByID(a1.FlavorID)
	if fq == nil || fq.Name != "emu-vgpu-rtx-pro-6000-blackwell-dc-24q" || fq.ExtraSpecs["trait:CUSTOM_NVIDIA_RTX_PRO_6000_BLACKWELL_DC_24Q"] != "required" {
		t.Errorf("q1 flavor %+v", fq)
	}
	if fg == nil || fg.Name != "emu-mig-1g-24gb" || fg.ExtraSpecs["trait:CUSTOM_MIG_1G_24GB"] != "required" || g1.MIGGPUInstanceID != 3 {
		t.Errorf("g1 flavor %+v gi %d", fg, g1.MIGGPUInstanceID)
	}
	if fa == nil || fa.Name != "emu-mig-1g" || fa.ExtraSpecs["trait:CUSTOM_MIG_1G_10GB"] != "required" || fa == fg {
		t.Errorf("a1 flavor %+v", fa)
	}
}
