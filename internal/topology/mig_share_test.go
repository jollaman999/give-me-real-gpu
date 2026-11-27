package topology

import (
	"reflect"
	"strings"
	"testing"
)

// catalogMIGVGPU returns the MIG-backed vGPU type name of the Blackwell MIG kind profile.
func catalogMIGVGPU(t *testing.T, profile, name string) *VGPUProfile {
	t.Helper()
	c := DefaultCatalog()
	cm := c.mig(modelRTXPro6000BW.name, profile)
	if cm == nil {
		t.Fatalf("no MIG kind %s", profile)
	}
	for _, v := range cm.VGPUProfiles {
		if v.Name == "NVIDIA RTX Pro 6000 Blackwell "+name {
			return &v
		}
	}
	t.Fatalf("%s has no vGPU type %s", profile, name)
	return nil
}

func TestTimeSlicedMIGCatalog(t *testing.T) {
	c := DefaultCatalog()
	cm := c.mig(modelRTXPro6000BW.name, "4g.96gb")
	if cm == nil || len(cm.VGPUProfiles) != 5 || cm.VGPU.Name != "NVIDIA RTX Pro 6000 Blackwell DC-4-96Q" || cm.VGPU.PerInstance() != 1 {
		t.Fatalf("4g.96gb %+v", cm)
	}
	v := catalogMIGVGPU(t, "4g.96gb", "DC-4-32C")
	if v.PerGPUInstance != 3 || v.PerInstance() != 3 || v.MaxInstances != 3 || v.FramebufferMB != 32768 || v.Type != "nvidia-1201" {
		t.Errorf("DC-4-32C %+v", v)
	}
	// A time-sliced type's maximum per GPU is its count per instance times the instances of its kind one GPU holds.
	for _, p := range c.model(modelRTXPro6000BW.name).MIGProfiles {
		for _, v := range p.VGPUProfiles {
			if v.PerGPUInstance == 0 {
				continue
			}
			if want := v.PerGPUInstance * (MIGSlices(modelRTXPro6000BW.name) / p.Slices); v.MaxInstances != want {
				t.Errorf("%s %s: max %d, want %d", p.Name, v.Name, v.MaxInstances, want)
			}
		}
	}
	// No other model gets a time-sliced MIG type.
	for _, m := range c.GPUModels {
		for _, p := range m.MIGProfiles {
			for _, v := range p.VGPUProfiles {
				if m.Name != modelRTXPro6000BW.name && v.PerGPUInstance != 0 {
					t.Errorf("%s %s %s: per_gpu_instance %d", m.Name, p.Name, v.Name, v.PerGPUInstance)
				}
			}
		}
	}
	var nilProfile *VGPUProfile
	if nilProfile.PerInstance() != 1 || (&VGPUProfile{PerGPUInstance: -2}).PerInstance() != 1 {
		t.Error("PerInstance of nil or a negative count is not 1")
	}
}

// sharedMIG builds a Blackwell MIG GPU with one 2g.48gb instance carrying vGPU type name and n VMs on it.
func sharedMIG(t *testing.T, name string, n int) *Topology {
	t.Helper()
	top := blackwellMIG("2g.48gb")
	g := top.Hypervisors[0].GPUs[0]
	g.MIGInstances[0].VGPU = catalogMIGVGPU(t, "2g.48gb", name)
	for range n {
		g.VMs = append(g.VMs, &VM{MIGGPUInstanceID: g.MIGInstances[0].GPUInstanceID})
	}
	Fill(top)
	return top
}

func TestValidateTimeSlicedMIG(t *testing.T) {
	if err := sharedMIG(t, "DC-2-12C", 4).Validate(); err != nil {
		t.Errorf("4 VMs on a DC-2-12C instance: %v", err)
	}
	err := sharedMIG(t, "DC-2-12C", 5).Validate()
	want := "hypervisor bw gpu 0: GPU instance 1 holds 5 VMs (bw-g0-1, bw-g0-2, bw-g0-3, bw-g0-4, bw-g0-5), more than the 4 its vGPU type allows"
	if err == nil || err.Error() != want {
		t.Errorf("5 VMs on a DC-2-12C instance:\n got %v\nwant %s", err, want)
	}
	err = sharedMIG(t, "DC-2-48C", 2).Validate()
	if err == nil || !strings.Contains(err.Error(), "GPU instance 1 holds 2 VMs (bw-g0-1, bw-g0-2), more than the 1 its vGPU type allows") {
		t.Errorf("2 VMs on a whole-instance type: %v", err)
	}
	if err := sharedMIG(t, "DC-2-48C", 1).Validate(); err != nil {
		t.Errorf("1 VM on a whole-instance type: %v", err)
	}
}

func TestFillSharesMIGInstance(t *testing.T) {
	top := blackwellMIG("2g.48gb", "2g.48gb")
	g := top.Hypervisors[0].GPUs[0]
	g.MIGInstances[0].VGPU = catalogMIGVGPU(t, "2g.48gb", "DC-2-16C")
	for range 6 {
		g.VMs = append(g.VMs, &VM{MIGGPUInstanceID: -1})
	}
	Fill(top)
	var gi []int
	for _, vm := range g.VMs {
		gi = append(gi, vm.MIGGPUInstanceID)
	}
	// Empty instances first (1, then the whole-instance 2), then the room left on the DC-2-16C instance 1, then none.
	if want := []int{1, 2, 1, 1, -1, -1}; !reflect.DeepEqual(gi, want) {
		t.Errorf("GI ids %v, want %v", gi, want)
	}
	g.VMs = g.VMs[:4]
	if err := top.Validate(); err != nil {
		t.Errorf("filled topology: %v", err)
	}
	for _, vm := range g.VMs {
		if fl := top.FlavorByID(vm.FlavorID); fl == nil || fl.Name != "emu-mig-2g-48gb" {
			t.Errorf("%s flavor %+v", vm.Name, fl)
		}
	}
}
