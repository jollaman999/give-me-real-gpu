package metrics

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// mixedTopology has nodes whose GPUs are handed out in different modes:
// a VGPU node with passthrough GPUs, a MIG node with VGPU GPUs, a node whose GPUs are all
// passthrough although the node mode is VGPU, and a Blackwell MIG GPU with one 4-slice instance.
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
	    {"index": 1, "model_name": "Tesla T4", "mode": "VGPU", "vms": [{"name": "t1", "mig_gpu_instance_id": -1}]},
	    {"index": 2, "model_name": "Tesla T4", "mode": "VGPU", "vms": []}
	  ]},
	  {"hostname": "mx-pt", "mode": "VGPU", "gpus": [
	    {"index": 0, "model_name": "NVIDIA L40S", "mode": "PASSTHROUGH", "vms": [{"name": "p2", "mig_gpu_instance_id": -1}]}
	  ]},
	  {"hostname": "mx-bw", "mode": "VGPU", "gpus": [
	    {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition", "mode": "MIG",
	     "mig_instances": [{"gpu_instance_id": -1, "profile": "4g.96gb"}], "vms": [{"name": "b1", "mig_gpu_instance_id": -1}]}
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

func TestMixedNodeGPUType(t *testing.T) {
	ps := parseAll(t, NewGenerator(mixedTopology(t), Options{CredentialID: testCred, Seed: 3}).Collect(t0))
	want := map[string]string{
		"mx-vp": "BARE_METAL", "mx-mv": "MIG", "mx-pt": "NONE", "mx-bw": "MIG",
		"v1": "VGPU", "v2": "VGPU", "p1": "PASSTHROUGH", "m1": "MIG", "t1": "VGPU", "p2": "PASSTHROUGH", "b1": "MIG",
	}
	seen := map[string]bool{}
	for _, p := range filter(ps, func(p point) bool { return p.name == "cpu" }) {
		h := p.tags["host"]
		seen[h] = true
		if p.tags["gpu_type"] != want[h] {
			t.Errorf("%s gpu_type %q, want %q", h, p.tags["gpu_type"], want[h])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("cpu points from %v, want %d agents", seen, len(want))
	}
}

func TestMixedNodeMapping(t *testing.T) {
	top := mixedTopology(t)
	ps := parseAll(t, NewGenerator(top, Options{CredentialID: testCred, Seed: 3}).Collect(t0))
	rows := func(host string) []point {
		return filter(ps, func(p point) bool { return p.name == "gpu_vm_mapping" && p.tags["host"] == host })
	}
	type row struct{ vm, bdf, status, driver, gpuType string }
	check := func(host string, want []row) {
		t.Helper()
		got := rows(host)
		if len(got) != len(want) {
			t.Fatalf("%s: %d rows, want %d", host, len(got), len(want))
		}
		for i, w := range want {
			p := got[i]
			g := row{p.tags["openstack_vm_name"], p.tags["host_bdf"], p.tags["status"], p.tags["driver"], p.tags["gpu_type"]}
			if g != w {
				t.Errorf("%s row %d: %+v, want %+v", host, i, g, w)
			}
		}
	}
	vp, mv := top.Hypervisors[0], top.Hypervisors[1]
	check("mx-vp", []row{
		{"v1", vp.GPUs[0].HostBDF, "assigned", "nvidia", "vgpu"},
		{"v2", vp.GPUs[0].HostBDF, "assigned", "nvidia", "vgpu"},
		{"p1", vp.GPUs[1].HostBDF, "assigned", "vfio-pci", "passthrough"},
		{"N/A", vp.GPUs[2].HostBDF, "unassigned", "vfio-pci", "passthrough"},
	})
	check("mx-mv", []row{
		{"m1", mv.GPUs[0].HostBDF, "assigned", "nvidia", "mig"},
		{"t1", mv.GPUs[1].HostBDF, "assigned", "nvidia", "vgpu"},
		{"N/A", mv.GPUs[2].HostBDF, "unassigned", "nvidia", "vgpu"},
	})
	if r := rows("mx-vp"); r[2].fields["gpu_uuid"].s != "" || r[3].fields["gpu_uuid"].s != "" {
		t.Error("passthrough GPUs are bound to vfio-pci, their rows must not carry gpu_uuid")
	}
	if r := rows("mx-mv"); r[2].fields["gpu_uuid"].s != mv.GPUs[2].UUID {
		t.Errorf("unassigned VGPU GPU on a MIG node gpu_uuid %q", r[2].fields["gpu_uuid"].s)
	}

	// The per VM dcgm points the script writes: gpu_type and mig_mode follow the VM's GPU.
	exec := func(vm string) point {
		return exactlyOne(ps, func(p point) bool {
			return p.name == "dcgm" && p.tags["host"] == vm && p.tags["physical_gpu_uuid"] != ""
		})
	}
	if p := exec("t1"); p.tags["gpu_type"] != "VGPU" || p.fields["mig_mode"].f != 0 || p.tags["gpu_instance_id"] != "N/A" {
		t.Errorf("t1 exec point %s %v", seriesKey(p), p.fields)
	} else if _, ok := p.fields["gpu_util"]; !ok {
		t.Error("vGPU VM on a MIG node lacks gpu_util")
	}
	if p := exec("m1"); p.tags["gpu_type"] != "MIG" || p.fields["mig_mode"].f != 1 || p.tags["gpu_instance_id"] != "1" {
		t.Errorf("m1 exec point %s %v", seriesKey(p), p.fields)
	}
}

func TestMixedNodePhysicalGPUs(t *testing.T) {
	top := mixedTopology(t)
	ps := parseAll(t, NewGenerator(top, Options{CredentialID: testCred, Seed: 3}).Collect(t0))
	// nvidia-smi points of the hypervisor: they carry the index tag, the per VM points do not.
	smi := func(host string) map[string][]point {
		out := map[string][]point{}
		for _, p := range ps {
			if p.name == "dcgm" && p.tags["host"] == host && p.tags["index"] != "" {
				out[p.tags["UUID"]] = append(out[p.tags["UUID"]], p)
			}
		}
		return out
	}
	vp, mv, pt := top.Hypervisors[0], top.Hypervisors[1], top.Hypervisors[2]
	got := smi("mx-vp")
	if len(got) != 1 || len(got[vp.GPUs[0].UUID]) != 1 {
		t.Errorf("mx-vp physical GPUs %v, want only the VGPU GPU %s", keys(got), vp.GPUs[0].UUID)
	}
	if p := got[vp.GPUs[0].UUID]; len(p) == 1 && p[0].tags["DCGM_FI_DRIVER_VERSION"] != vp.DriverVersion {
		t.Errorf("mx-vp driver tag %q", p[0].tags["DCGM_FI_DRIVER_VERSION"])
	}
	got = smi("mx-mv")
	if len(got) != 3 || len(got[mv.GPUs[0].UUID]) != 2 || len(got[mv.GPUs[1].UUID]) != 1 || len(got[mv.GPUs[2].UUID]) != 1 {
		t.Errorf("mx-mv physical GPUs %v", keys(got))
	}
	for _, g := range mv.GPUs {
		p := got[g.UUID]
		if len(p) == 0 {
			continue
		}
		_, util := p[0].fields["gpu_util"]
		if wantUtil := mv.GPUMode(g) != topology.ModeMIG; util != wantUtil {
			t.Errorf("mx-mv gpu %d (%s): gpu_util present %v, want %v", g.Index, mv.GPUMode(g), util, wantUtil)
		}
	}
	if got := smi("mx-pt"); len(got) != 0 {
		t.Errorf("passthrough-only node reports physical GPUs %v", keys(got))
	}
	if pt.DriverVersion != "" {
		t.Errorf("passthrough-only node driver %q", pt.DriverVersion)
	}
	guest := filter(ps, func(p point) bool {
		return p.name == "dcgm" && p.tags["host"] == "p1" && p.tags["UUID"] == vp.GPUs[1].UUID
	})
	if len(guest) != 1 || guest[0].tags["gpu_type"] != "PASSTHROUGH" {
		t.Errorf("passthrough guest on a VGPU node: %d points", len(guest))
	}
	probe := filter(ps, func(p point) bool { return p.name == "dcgm" && p.tags["host"] == "t1" && p.fields["cuda_probe"].i == 1 })
	if len(probe) != 1 {
		t.Errorf("vGPU guest on a MIG node: %d CUDA probe points", len(probe))
	}
}

func TestBlackwellMIGUtilAndSpec(t *testing.T) {
	top := mixedTopology(t)
	bw := top.Hypervisors[3]
	g := NewGenerator(top, Options{CredentialID: testCred, Seed: 3})
	busy := false
	for i := 0; i < 5; i++ {
		ps := parseAll(t, g.Collect(t0.Add(time.Duration(i)*10*time.Second)))
		phys := filter(ps, func(p point) bool { return p.name == "dcgm" && p.tags["host"] == "mx-bw" && p.tags["index"] != "" })
		if len(phys) != 2 {
			t.Fatalf("Blackwell MIG GPU: %d host points, want nvidia-smi + GPM device", len(phys))
		}
		smiPt, dev := phys[0], phys[1]
		if smiPt.fields["fb_total"].f != 97887 || smiPt.fields["sm_clock"].f < 2430*0.92 || smiPt.fields["mem_clock"].f != 12481 {
			t.Errorf("Blackwell spec %v", smiPt.fields)
		}
		if _, ok := smiPt.fields["memory_temp"]; ok {
			t.Error("Blackwell has GDDR7, no memory_temp")
		}
		gpm := filter(ps, func(p point) bool {
			return p.name == "dcgm" && p.tags["host"] == "b1" && p.tags["physical_gpu_uuid"] != "" && p.fields["sm_occupancy"].kind != 0
		})
		if len(gpm) != 1 {
			t.Fatalf("collection %d: %d instance GPM points for b1, want 1", i, len(gpm))
		}
		vm := gpm[0]
		// One instance holding all 4 slices: the device util equals the instance util.
		if d, v := dev.fields["gpu_util"].f, vm.fields["gpu_util"].f; d != v {
			t.Errorf("collection %d: device GPM util %v, instance %v (4 of 4 slices)", i, d, v)
		} else if d > 0 {
			busy = true
		}
	}
	if !busy {
		t.Error("Blackwell instance stayed idle, the util comparison proved nothing")
	}
	if bw.GPUs[0].MIGInstances[0].GPUInstanceID != 0 {
		t.Errorf("4g.96gb GI %d", bw.GPUs[0].MIGInstances[0].GPUInstanceID)
	}
}

func TestMIGInstanceVGPUType(t *testing.T) {
	const raw = `{"hypervisors": [{"hostname": "bw-c", "mode": "MIG", "gpus": [
	  {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition",
	   "mig_instances": [
	     {"gpu_instance_id": -1, "profile": "2g.48gb",
	      "vgpu_profile": {"name": "NVIDIA RTX Pro 6000 Blackwell DC-2-48C", "type": "nvidia-1198", "framebuffer_mb": 49152, "max_instances": 2}},
	     {"gpu_instance_id": -1, "profile": "2g.48gb"}],
	   "vms": [{"name": "c1", "mig_gpu_instance_id": -1}, {"name": "q1", "mig_gpu_instance_id": -1}]}
	]}]}`
	var top topology.Topology
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	topology.Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatal(err)
	}
	ps := parseAll(t, NewGenerator(&top, Options{CredentialID: testCred, Seed: 3}).Collect(t0))
	want := map[string][2]string{
		"c1": {"NVIDIA RTX Pro 6000 Blackwell DC-2-48C", "nvidia-1198"},
		"q1": {"NVIDIA RTX Pro 6000 Blackwell DC-2-48Q", "nvidia-1188"},
	}
	rows := filter(ps, func(p point) bool { return p.name == "gpu_vm_mapping" })
	if len(rows) != 2 {
		t.Fatalf("%d mapping rows, want 2", len(rows))
	}
	for _, p := range rows {
		w := want[p.tags["openstack_vm_name"]]
		if p.fields["vgpu_model_name"].s != w[0] || p.fields["vgpu_profile"].s != w[1] || p.fields["vgpu_fb_mb"].i != 49152 ||
			p.fields["vgpu_max_instances"].i != 2 || p.tags["gpu_type"] != "mig" {
			t.Errorf("%s: %v", p.tags["openstack_vm_name"], p.fields)
		}
	}
	for vm, w := range want {
		p := exactlyOne(ps, func(p point) bool { return p.tags["host"] == vm && p.tags["physical_gpu_uuid"] != "" })
		if p.tags["modelName"] != w[0] || p.fields["fb_total"].f != 49152 {
			t.Errorf("%s exec point modelName %q fb_total %v", vm, p.tags["modelName"], p.fields["fb_total"].f)
		}
	}
}
