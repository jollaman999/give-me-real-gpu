package otelgen

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

func TestSharedMIGInstance(t *testing.T) {
	const raw = `{"hypervisors": [{"hostname": "bw-ts", "mode": "MIG", "gpus": [
	  {"index": 0, "model_name": "NVIDIA RTX PRO 6000 Blackwell Server Edition",
	   "mig_instances": [{"gpu_instance_id": -1, "profile": "4g.96gb",
	     "vgpu_profile": {"name": "NVIDIA RTX Pro 6000 Blackwell DC-4-32C", "type": "nvidia-1201", "framebuffer_mb": 32768, "max_instances": 3, "per_gpu_instance": 3}}],
	   "vms": [{"name": "s1", "mig_gpu_instance_id": -1}, {"name": "s2", "mig_gpu_instance_id": -1}, {"name": "s3", "mig_gpu_instance_id": -1}]}
	]}]}`
	cases := []struct {
		name           string
		demand         float64
		wantVM, wantGI float64
	}{
		{"demand over 100", 90, 33.3, 99.9},
		{"demand under 100", 20, 20, 60},
	}
	for _, c := range cases {
		var top topology.Topology
		if err := json.Unmarshal([]byte(raw), &top); err != nil {
			t.Fatal(err)
		}
		topology.Fill(&top)
		if err := top.Validate(); err != nil {
			t.Fatal(err)
		}
		g := NewGenerator(&top, Options{Seed: 2})
		if len(g.gpus) != 1 || len(g.gpus[0].vms) != 3 {
			t.Fatalf("%d GPUs", len(g.gpus))
		}
		pg := g.gpus[0]
		mi := top.Hypervisors[0].GPUs[0].MIGInstances[0]
		for _, vg := range pg.vms {
			vg.demand = &walk{v: c.demand, target: c.demand, lo: 0, hi: 100}
		}
		pg.step()

		sum := 0.0
		for _, vg := range pg.vms {
			if vg.util != c.wantVM {
				t.Errorf("%s: %s util %v, want %v", c.name, vg.vm.Name, vg.util, c.wantVM)
			}
			sum += vg.util
		}
		gi := pg.giUtil[mi]
		if math.Abs(gi-c.wantGI) > 1e-9 || math.Abs(sum-gi) > 1e-9 || math.Abs(pg.util-gi) > 1e-9 {
			t.Errorf("%s: VM util sum %v, GI util %v, device util %v, want %v", c.name, sum, gi, pg.util, c.wantGI)
		}
		// The instance memory counts once on the physical GPU.
		if pg.fbUsed != 95616 {
			t.Errorf("%s: physical fb used %v, want 95616", c.name, pg.fbUsed)
		}

		host := &gpuView{kind: viewHost, phys: pg}
		targets := host.gpmTargets()
		if len(targets) != 2 || targets[0].util != pg.util || !targets[1].mig || targets[1].gi != 0 || targets[1].util != gi {
			t.Errorf("%s: host GPM targets %+v, want device and GI 0 at %v", c.name, targets, gi)
		}

		for _, vg := range pg.vms {
			v := guestView(pg, vg)
			if v == nil || v.kind != viewMIGGuest {
				t.Fatalf("%s: %s guest view %+v", c.name, vg.vm.Name, v)
			}
			var total, used float64
			for _, sv := range v.smiValues() {
				switch sv.field {
				case "memory.total":
					total = sv.v
				case "memory.used":
					used = sv.v
				}
			}
			if total != 32768 || used <= 0 || used > 32768 {
				t.Errorf("%s: %s guest memory total %v used %v, want total 32768", c.name, vg.vm.Name, total, used)
			}
		}
	}
}
