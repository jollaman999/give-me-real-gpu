package metrics

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// sharedMIGTopology is one Blackwell GPU with a single 4g.96gb instance carrying the time-sliced DC-4-32C,
// shared by three VMs.
func sharedMIGTopology(t *testing.T) *topology.Topology {
	t.Helper()
	const raw = `{"hypervisors": [{"hostname": "bw-ts", "mode": "MIG", "gpus": [
	  {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition",
	   "mig_instances": [{"gpu_instance_id": -1, "profile": "4g.96gb",
	     "vgpu_profile": {"name": "NVIDIA RTX Pro 6000 Blackwell DC-4-32C", "type": "nvidia-1201", "framebuffer_mb": 32768, "max_instances": 3, "per_gpu_instance": 3}}],
	   "vms": [{"name": "s1", "mig_gpu_instance_id": -1}, {"name": "s2", "mig_gpu_instance_id": -1}, {"name": "s3", "mig_gpu_instance_id": -1}]}
	]}]}`
	var top topology.Topology
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatal(err)
	}
	topology.Fill(&top)
	if err := top.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, vm := range top.AllVMs() {
		if vm.MIGGPUInstanceID != 0 {
			t.Fatalf("%s on GI %d, want 0", vm.Name, vm.MIGGPUInstanceID)
		}
	}
	return &top
}

func TestSharedMIGInstance(t *testing.T) {
	cases := []struct {
		name           string
		demand         float64
		wantVM, wantGI float64
	}{
		{"demand over 100", 90, 33.3, 99.9},
		{"demand under 100", 20, 20, 60},
	}
	for _, c := range cases {
		top := sharedMIGTopology(t)
		g := NewGenerator(top, Options{CredentialID: testCred, Seed: 3})
		for _, vs := range g.vms {
			vs.demand = &walk{v: c.demand, target: c.demand, lo: 0, hi: 100}
		}
		ps := parseAll(t, g.Collect(t0))
		gs := g.hosts[0].gpus[0]
		mi := top.Hypervisors[0].GPUs[0].MIGInstances[0]

		sum := 0.0
		for _, vs := range gs.vms {
			if vs.util != c.wantVM {
				t.Errorf("%s: %s util %v, want %v", c.name, vs.vm.Name, vs.util, c.wantVM)
			}
			sum += vs.util
		}
		gi := gs.giUtil[mi]
		if math.Abs(gi-c.wantGI) > 1e-9 || math.Abs(sum-gi) > 1e-9 || gi > 100 {
			t.Errorf("%s: VM util sum %v, GI util %v, want %v", c.name, sum, gi, c.wantGI)
		}
		// One instance holding all 4 slices: the device util is the instance util.
		if math.Abs(gs.util-gi) > 1e-9 {
			t.Errorf("%s: device util %v, GI util %v", c.name, gs.util, gi)
		}

		// The hypervisor's nvidia-smi counts the instance memory once, not once per VM.
		phys := filter(ps, func(p point) bool { return p.name == "dcgm" && p.tags["host"] == "bw-ts" && p.tags["index"] != "" })
		if len(phys) != 2 {
			t.Fatalf("%s: %d host points, want nvidia-smi + GPM device", c.name, len(phys))
		}
		if phys[0].fields["fb_used"].f != 95616 || phys[1].fields["gpu_util"].f != c.wantGI {
			t.Errorf("%s: physical fb_used %v, device GPM util %v", c.name, phys[0].fields["fb_used"].f, phys[1].fields["gpu_util"].f)
		}

		rows := filter(ps, func(p point) bool { return p.name == "gpu_vm_mapping" })
		if len(rows) != 3 {
			t.Fatalf("%s: %d mapping rows, want 3", c.name, len(rows))
		}
		for _, p := range rows {
			if p.fields["vgpu_fb_mb"].i != 32768 || p.fields["vgpu_max_instances"].i != 3 || p.fields["vgpu_profile"].s != "nvidia-1201" ||
				p.fields["vgpu_active_instances"].i != 3 || p.tags["gpu_type"] != "mig" {
				t.Errorf("%s: mapping row %s %v", c.name, p.tags["openstack_vm_name"], p.fields)
			}
		}
		for _, vm := range []string{"s1", "s2", "s3"} {
			exec := exactlyOne(ps, func(p point) bool { return p.tags["host"] == vm && p.tags["physical_gpu_uuid"] != "" })
			if exec.fields["fb_total"].f != 32768 || exec.tags["gpu_instance_id"] != "0" {
				t.Errorf("%s: %s exec fb_total %v gi %s", c.name, vm, exec.fields["fb_total"].f, exec.tags["gpu_instance_id"])
			}
			// GPM measures the instance, so each VM on it gets the instance util.
			gpm := filter(ps, func(p point) bool {
				return p.name == "dcgm" && p.tags["host"] == vm && p.tags["physical_gpu_uuid"] != "" && p.fields["sm_occupancy"].kind != 0
			})
			if len(gpm) != 1 || gpm[0].fields["gpu_util"].f != c.wantGI {
				t.Errorf("%s: %s GPM points %v", c.name, vm, gpm)
			}
		}
	}
}
