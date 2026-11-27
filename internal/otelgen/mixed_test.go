package otelgen

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// mixedTopology has nodes whose GPUs are handed out in different modes, written the way the editor sends them.
func mixedTopology(t *testing.T) *topology.Topology {
	t.Helper()
	const raw = `{"hypervisors": [
	  {"hostname": "mx-vp", "mode": "VGPU", "gpus": [
	    {"index": 0, "model_name": "NVIDIA A10", "vms": [{"name": "v1", "mig_gpu_instance_id": -1}, {"name": "v2", "mig_gpu_instance_id": -1}]},
	    {"index": 1, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": [{"name": "p1", "mig_gpu_instance_id": -1}]},
	    {"index": 2, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": []}
	  ]},
	  {"hostname": "mx-mv", "mode": "MIG", "gpus": [
	    {"index": 0, "model_name": "NVIDIA A100-SXM4-80GB", "mig_instances": [{"gpu_instance_id": -1, "profile": "3g.40gb"}],
	     "vms": [{"name": "m1", "mig_gpu_instance_id": -1}]},
	    {"index": 1, "model_name": "Tesla T4", "mode": "VGPU", "vms": [{"name": "t1", "mig_gpu_instance_id": -1}]}
	  ]},
	  {"hostname": "mx-pt", "mode": "VGPU", "gpus": [
	    {"index": 0, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": [{"name": "p2", "mig_gpu_instance_id": -1}]}
	  ]},
	  {"hostname": "mx-bw", "mode": "VGPU", "gpus": [
	    {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition", "mode": "MIG",
	     "mig_instances": [{"gpu_instance_id": -1, "profile": "4g.96gb",
	       "vgpu_profile": {"name": "NVIDIA RTX Pro 6000 Blackwell DC-4-96C", "type": "nvidia-1199", "framebuffer_mb": 98304, "max_instances": 1}}],
	     "vms": [{"name": "b1", "mig_gpu_instance_id": -1}]}
	  ]}
	]}`
	var top topology.Topology
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	topology.Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatalf("mixed topology: %v", err)
	}
	return &top
}

func TestMixedNodeViews(t *testing.T) {
	top := mixedTopology(t)
	g := NewGenerator(top, Options{Seed: 2})
	agents := map[string]*agent{}
	for _, a := range g.agents {
		agents[a.name] = a
	}
	uuids := func(a *agent) []string {
		var out []string
		for _, v := range a.gpus {
			out = append(out, v.uuid)
		}
		return out
	}
	vp, mv := top.Hypervisors[0], top.Hypervisors[1]
	// Both passthrough GPUs, the one given to p1 and the free one, are bound to vfio-pci; the host driver sees only the VGPU GPU.
	if got, want := uuids(agents["mx-vp"]), []string{vp.GPUs[0].UUID}; !slices.Equal(got, want) {
		t.Errorf("mx-vp host GPUs %v, want %v", got, want)
	}
	if got := uuids(agents["mx-mv"]); len(got) != 2 {
		t.Errorf("mx-mv host GPUs %v", got)
	}
	if got := uuids(agents["mx-pt"]); len(got) != 0 {
		t.Errorf("passthrough-only node sees %v", got)
	}

	type guest struct {
		kind viewKind
		name string
	}
	wantGuest := map[string]guest{
		"v1": {viewVGPU, "NVIDIA A10-4Q"},
		"p1": {viewPassthrough, "NVIDIA L40S"},
		"m1": {viewMIGGuest, "NVIDIA A100D-3-40C"},
		"t1": {viewVGPU, "GRID T4-4Q"},
		"p2": {viewPassthrough, "NVIDIA L40S"},
		"b1": {viewMIGGuest, "NVIDIA RTX Pro 6000 Blackwell DC-4-96C"},
	}
	for vm, w := range wantGuest {
		a := agents[vm]
		if a == nil || len(a.gpus) != 1 || a.gpus[0].kind != w.kind || a.gpus[0].name != w.name {
			t.Errorf("%s guest view %+v, want %+v", vm, a.gpus, w)
		}
	}
	if v := agents["p1"].gpus[0]; v.uuid != vp.GPUs[1].UUID {
		t.Errorf("p1 sees %s, want the physical GPU %s", v.uuid, vp.GPUs[1].UUID)
	}

	// Host utilization is N/A on the MIG GPU only.
	for _, v := range agents["mx-mv"].gpus {
		hasUtil := false
		for _, sv := range v.smiValues() {
			if sv.field == "utilization.gpu" {
				hasUtil = true
			}
		}
		if want := v.phys.g == mv.GPUs[1]; hasUtil != want {
			t.Errorf("mx-mv %s: utilization.gpu %v, want %v", v.name, hasUtil, want)
		}
	}

	procs := func(host, name string) int {
		n := 0
		for _, p := range agents[host].sys.procs {
			if p.name == name {
				n++
			}
		}
		return n
	}
	cases := []struct {
		host                       string
		qemu, vgpuMgr, persistence int
	}{
		{"mx-vp", 3, 2, 1},
		{"mx-mv", 2, 2, 1},
		{"mx-pt", 1, 0, 0},
		{"mx-bw", 1, 1, 1},
	}
	for _, c := range cases {
		q, m, p := procs(c.host, "qemu-system-x86_64"), procs(c.host, "nvidia-vgpu-mgr"), procs(c.host, "nvidia-persistenced")
		if q != c.qemu || m != c.vgpuMgr || p != c.persistence {
			t.Errorf("%s: qemu %d vgpu-mgr %d persistenced %d, want %d %d %d", c.host, q, m, p, c.qemu, c.vgpuMgr, c.persistence)
		}
	}
}

func TestBlackwellOTel(t *testing.T) {
	top := mixedTopology(t)
	g := NewGenerator(top, Options{Seed: 2})
	var pg *physGPU
	for _, x := range g.gpus {
		if x.host.Hostname == "mx-bw" {
			pg = x
		}
	}
	if pg == nil {
		t.Fatal("no Blackwell GPU")
	}
	if s := pg.spec; s.tdp != 600 || s.fan || s.hbm || !s.gpm || s.smBoost != 2430 || s.memBusy != 12481 {
		t.Errorf("Blackwell spec %+v", s)
	}
	busy := false
	for i := 0; i < 5; i++ {
		pg.step()
		vg := pg.vms[0]
		// One instance holding all 4 slices: the device util equals the instance util.
		if pg.util != vg.util {
			t.Errorf("step %d: device util %v, instance %v", i, pg.util, vg.util)
		}
		if pg.util > 0 {
			busy = true
		}
	}
	if !busy {
		t.Error("Blackwell instance stayed idle, the util comparison proved nothing")
	}
}
