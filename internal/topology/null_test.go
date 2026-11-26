package topology

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A topology written by hand or in the web editor's JSON view can contain null entries.
// Validate must report them instead of panicking.
func TestValidateNullEntries(t *testing.T) {
	vm := `{"id":"v1","name":"v1","instance_name":"instance-00000001","flavor_id":"f1","hypervisor":"h1","gpu_index":0,"mode":"PASSTHROUGH","mig_gpu_instance_id":-1}`
	cases := []struct {
		name string
		json string
		want []string
	}{
		{"null flavor before the VM's flavor",
			`{"project_id":"p","user_name":"u","flavors":[null,{"id":"f1","name":"f1"}],"hypervisors":[{"id":1,"uuid":"u1","hostname":"h1","mode":"PASSTHROUGH","gpus":[{"index":0,"uuid":"g1","vms":[` + vm + `]}]}]}`,
			[]string{"flavors[0] is null"}},
		{"null flavor only",
			`{"project_id":"p","user_name":"u","flavors":[null],"hypervisors":[{"id":1,"uuid":"u1","hostname":"h1","mode":"PASSTHROUGH","gpus":[{"index":0,"uuid":"g1","vms":[` + vm + `]}]}]}`,
			[]string{"flavors[0] is null", `vm v1: flavor "f1" not found`}},
		{"null hypervisor",
			`{"project_id":"p","user_name":"u","flavors":[{"id":"f1","name":"f1"}],"hypervisors":[null,{"id":1,"uuid":"u1","hostname":"h1","mode":"PASSTHROUGH","gpus":[{"index":0,"uuid":"g1","vms":[` + vm + `]}]}]}`,
			[]string{"hypervisors[0] is null"}},
		{"null gpu and vm",
			`{"project_id":"p","user_name":"u","flavors":[{"id":"f1","name":"f1"}],"hypervisors":[{"id":1,"uuid":"u1","hostname":"h1","mode":"PASSTHROUGH","gpus":[null,{"index":0,"uuid":"g1","bus_id":"00000000:18:00.0","vms":[null]},{"index":1,"uuid":"g2","bus_id":"00000000:2A:00.0","vms":[` + strings.Replace(vm, `"gpu_index":0`, `"gpu_index":1`, 1) + `]}]}]}`,
			[]string{"hypervisor h1: gpus[0] is null", "hypervisor h1 gpu 0: vms[0] is null"}},
		{"null mig instance",
			`{"project_id":"p","user_name":"u","flavors":[{"id":"f1","name":"f1"}],"hypervisors":[{"id":1,"uuid":"u1","hostname":"h1","mode":"MIG","gpus":[{"index":0,"uuid":"g1","mig_instances":[null,{"gpu_instance_id":7,"profile":"1g.10gb","slices":1}],"vms":[]}]}]}`,
			[]string{"hypervisor h1 gpu 0: mig_instances[0] is null"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var top Topology
			if err := json.Unmarshal([]byte(c.json), &top); err != nil {
				t.Fatal(err)
			}
			err := top.Validate()
			if err == nil {
				t.Fatal("Validate accepted null entries")
			}
			got := strings.Split(err.Error(), "\n")
			if strings.Join(got, "|") != strings.Join(c.want, "|") {
				t.Fatalf("problems %q, want %q", got, c.want)
			}
		})
	}
}

func TestLoadNullFlavor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.json")
	top := Default()
	top.Flavors = append([]*Flavor{nil}, top.Flavors...)
	raw, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "flavors[0] is null") {
		t.Fatalf("Load: %v", err)
	}
}

func TestLookupsSkipNull(t *testing.T) {
	top := Default()
	want := len(top.AllVMs())
	first := top.Hypervisors[0]
	top.Flavors = append([]*Flavor{nil}, top.Flavors...)
	top.Hypervisors = append([]*Hypervisor{nil}, top.Hypervisors...)
	first.GPUs = append([]*GPU{nil}, first.GPUs...)
	first.GPUs[1].VMs = append([]*VM{nil}, first.GPUs[1].VMs...)

	if got := len(top.AllVMs()); got != want {
		t.Errorf("AllVMs returned %d VMs, want %d without the null one", got, want)
	}
	for _, vm := range top.AllVMs() {
		if vm == nil {
			t.Fatal("AllVMs returned a null VM")
		}
	}
	if h := top.HypervisorByName("emu-vgpu-01"); h != first {
		t.Errorf("HypervisorByName = %v", h)
	}
	if h := top.HypervisorByName("none"); h != nil {
		t.Errorf("HypervisorByName(none) = %v", h)
	}
	f := top.Flavors[1]
	if got := top.FlavorByID(f.ID); got != f {
		t.Errorf("FlavorByID = %v", got)
	}
	if got := top.FlavorByID("none"); got != nil {
		t.Errorf("FlavorByID(none) = %v", got)
	}
}
