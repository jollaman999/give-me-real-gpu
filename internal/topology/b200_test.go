package topology

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// migGPU builds a filled MIG node "mg" with one GPU of the model carrying one instance per profile.
func migGPU(model string, profiles ...string) *Topology {
	g := map[string]any{"index": 0, "model_name": model, "vms": []any{}}
	var mi []any
	for _, p := range profiles {
		mi = append(mi, map[string]any{"gpu_instance_id": -1, "profile": p})
	}
	g["mig_instances"] = mi
	raw, _ := json.Marshal(map[string]any{"hypervisors": []any{map[string]any{"hostname": "mg", "mode": "MIG", "gpus": []any{g}}}})
	var top Topology
	if err := json.Unmarshal(raw, &top); err != nil {
		panic(err)
	}
	Fill(&top)
	return &top
}

func repeat(p string, n int) []string {
	var out []string
	for range n {
		out = append(out, p)
	}
	return out
}

func TestB200Catalog(t *testing.T) {
	c := DefaultCatalog()
	if len(c.GPUModels) != 8 || c.GPUModels[7].Name != "NVIDIA B200" {
		t.Fatalf("models %d, last %q", len(c.GPUModels), c.GPUModels[len(c.GPUModels)-1].Name)
	}
	b := c.GPUModels[7]
	if b.DeviceID != "2901" || b.MemoryMiB != 184320 || b.VBIOS != vbios("97", "00", "B8", "00", "01") || b.Arch != "Blackwell" ||
		b.MaxMIGSlices != 7 || MIGSlices(b.Name) != 7 || b.PassthroughFlavor != "emu-pt-b200" {
		t.Errorf("B200 model %+v", b)
	}
	if len(b.VGPUProfiles) != 1 {
		t.Fatalf("B200 vGPU types %+v", b.VGPUProfiles)
	}
	if v := b.VGPUProfiles[0]; v.Name != "NVIDIA B200X-180C" || v.Type != "nvidia-1210" || v.FramebufferMB != 184320 ||
		v.MaxInstances != 1 || v.PerGPUInstance != 0 || v.Flavor != "emu-vgpu-b200x-180c" {
		t.Errorf("B200 vGPU type %+v", v)
	}
	var mig []string
	for _, p := range b.MIGProfiles {
		if len(p.VGPUProfiles) != 1 || p.VGPU != p.VGPUProfiles[0] || p.VGPU.PerGPUInstance != 0 {
			t.Errorf("%s: vGPU types %+v", p.Name, p.VGPUProfiles)
		}
		if p.VGPU.MaxInstances != p.MaxPerGPU || p.MaxPerGPU*p.Slices > 7 || p.VGPU.FramebufferMB != p.MemoryMiB {
			t.Errorf("%s: %d slices x %d, vGPU %+v", p.Name, p.Slices, p.MaxPerGPU, p.VGPU)
		}
		mig = append(mig, fmt.Sprintf("%s %d %d %v %s %d %s", p.Name, p.Slices, p.MemoryMiB, p.GPUInstanceIDs,
			strings.TrimPrefix(p.VGPU.Name, "NVIDIA "), p.MaxPerGPU, p.Flavor))
	}
	want := "1g.23gb 1 23040 [7 8 9 10 11 12 13] B200X-1-23C 7 emu-mig-1g-23gb," +
		"1g.23gb+me 1 23040 [7] B200X-1-23CME 1 emu-mig-1g-23gb-me," +
		"1g.45gb 1 46080 [7 8 9 10] B200X-1-45C 4 emu-mig-1g-45gb," +
		"2g.45gb 2 46080 [3 4 5] B200X-2-45C 3 emu-mig-2g-45gb," +
		"3g.90gb 3 92160 [1 2] B200X-3-90C 2 emu-mig-3g-90gb," +
		"4g.90gb 4 92160 [1] B200X-4-90C 1 emu-mig-4g-90gb," +
		"7g.180gb 7 184320 [0] B200X-7-180C 1 emu-mig-7g-180gb"
	if got := strings.Join(mig, ","); got != want {
		t.Errorf("B200 MIG kinds\n got %s\nwant %s", got, want)
	}

	maxPer := map[string]int{}
	for _, m := range c.GPUModels {
		for _, p := range m.MIGProfiles {
			maxPer[m.Name+" "+p.Name] = p.MaxPerGPU
			if p.MaxPerGPU != MIGMaxPerGPU(m.Name, p.Name) {
				t.Errorf("%s %s: catalog %d, MIGMaxPerGPU %d", m.Name, p.Name, p.MaxPerGPU, MIGMaxPerGPU(m.Name, p.Name))
			}
		}
	}
	bw := modelRTXPro6000BW.name
	if maxPer[bw+" 1g.24gb"] != 4 || maxPer[bw+" 2g.48gb"] != 2 || maxPer[bw+" 4g.96gb"] != 1 ||
		maxPer["NVIDIA A100-SXM4-80GB 1g.10gb"] != 0 || maxPer["NVIDIA A100-SXM4-80GB 7g.80gb"] != 0 || maxPer["NVIDIA H100 80GB HBM3 3g.40gb"] != 0 {
		t.Errorf("max_per_gpu %v", maxPer)
	}
	if MIGMaxPerGPU("Some Future GPU", "1g.10gb") != 0 || MIGMaxPerGPU("NVIDIA B200", "9g.999gb") != 0 {
		t.Error("MIGMaxPerGPU of an unknown model or kind is not 0")
	}

	// A MIG flavor name belongs to one kind name, and no vGPU flavor or mdev type is used twice.
	owner, vgpuFlavor, types := map[string]string{}, map[string]string{}, map[string]string{}
	for _, m := range c.GPUModels {
		for _, p := range m.MIGProfiles {
			if o, ok := owner[p.Flavor]; ok && o != p.Name {
				t.Errorf("flavor %s used by %s and %s", p.Flavor, o, p.Name)
			}
			owner[p.Flavor] = p.Name
			for _, v := range p.VGPUProfiles {
				if o, ok := types[v.Type]; ok {
					t.Errorf("mdev type %s used by %s and %s", v.Type, o, v.Name)
				}
				types[v.Type] = v.Name
			}
		}
		for _, p := range m.VGPUProfiles {
			if o, ok := vgpuFlavor[p.Flavor]; ok {
				t.Errorf("flavor %s used by %s and %s", p.Flavor, o, p.Name)
			}
			if o, ok := types[p.Type]; ok {
				t.Errorf("mdev type %s used by %s and %s", p.Type, o, p.Name)
			}
			vgpuFlavor[p.Flavor], types[p.Type] = p.Name, p.Name
		}
	}
	for _, n := range defaultNodes {
		if n.model.name == b.Name {
			t.Errorf("default node %s uses the catalog-only model", n.hostname)
		}
	}

	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"1g.45gb"`) || !strings.Contains(string(raw), `"max_per_gpu":4`) {
		t.Error("catalog JSON lacks the B200 1g.45gb kind or max_per_gpu")
	}
	a100, _ := json.Marshal(c.model("NVIDIA A100-SXM4-80GB").MIGProfiles)
	if strings.Contains(string(a100), "max_per_gpu") {
		t.Errorf("A100 MIG kinds carry max_per_gpu: %s", a100)
	}
}

func TestValidateMIGMaxPerGPU(t *testing.T) {
	if err := migGPU("NVIDIA B200", repeat("1g.45gb", 4)...).Validate(); err != nil {
		t.Errorf("4 1g.45gb on a B200: %v", err)
	}
	err := migGPU("NVIDIA B200", repeat("1g.45gb", 5)...).Validate()
	if want := "hypervisor mg gpu 0: 5 1g.45gb instances, more than the 4 one GPU allows"; err == nil || err.Error() != want {
		t.Errorf("5 1g.45gb on a B200:\n got %v\nwant %s", err, want)
	}
	err = migGPU("NVIDIA B200", "1g.23gb+me", "1g.23gb+me").Validate()
	if want := "hypervisor mg gpu 0: 2 1g.23gb+me instances, more than the 1 one GPU allows"; err == nil || err.Error() != want {
		t.Errorf("2 1g.23gb+me on a B200:\n got %v\nwant %s", err, want)
	}
	if err := migGPU("NVIDIA B200", "3g.90gb", "2g.45gb", "1g.23gb", "1g.23gb").Validate(); err != nil {
		t.Errorf("mixed 7 slices on a B200: %v", err)
	}

	// Three 2g.48gb on an RTX PRO 6000 break both the slice total (6 > 4) and the kind maximum (3 > 2).
	err = migGPU(modelRTXPro6000BW.name, repeat("2g.48gb", 3)...).Validate()
	want := "hypervisor mg gpu 0: MIG slice total 6 exceeds 4\nhypervisor mg gpu 0: 3 2g.48gb instances, more than the 2 one GPU allows"
	if err == nil || err.Error() != want {
		t.Errorf("3 2g.48gb on an RTX PRO 6000:\n got %v\nwant %s", err, want)
	}

	if err := migGPU("NVIDIA A100-SXM4-80GB", repeat("1g.10gb", 7)...).Validate(); err != nil {
		t.Errorf("7 1g.10gb on an A100: %v", err)
	}
	// A model the catalog does not know gets only the slice total check.
	if err := migGPU("Some Future GPU", repeat("1g.45gb", 7)...).Validate(); err != nil {
		t.Errorf("7 1g.45gb on an unknown model: %v", err)
	}
}

func TestB200FillFlavors(t *testing.T) {
	top := migGPU("NVIDIA B200", "1g.45gb", "3g.90gb")
	g := top.Hypervisors[0].GPUs[0]
	g.VMs = []*VM{{MIGGPUInstanceID: -1}, {MIGGPUInstanceID: -1}}
	Fill(top)
	if err := top.Validate(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, mi := range g.MIGInstances {
		got = append(got, fmt.Sprintf("%s gi%d %d %s", mi.Profile, mi.GPUInstanceID, mi.MemoryMiB, mi.VGPU.Name))
	}
	for _, vm := range g.VMs {
		got = append(got, fmt.Sprintf("%s gi%d %s", vm.Name, vm.MIGGPUInstanceID, top.FlavorByID(vm.FlavorID).Name))
	}
	want := "1g.45gb gi7 46080 NVIDIA B200X-1-45C,3g.90gb gi1 92160 NVIDIA B200X-3-90C,mg-g0-1 gi7 emu-mig-1g-45gb,mg-g0-2 gi1 emu-mig-3g-90gb"
	if s := strings.Join(got, ","); s != want {
		t.Errorf("filled B200\n got %s\nwant %s", s, want)
	}
}
