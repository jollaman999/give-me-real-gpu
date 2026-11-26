package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Load reads a topology JSON file and validates it. An empty path returns Default().
// Unknown keys are treated as typos and rejected.
func Load(path string) (*Topology, error) {
	if path == "" {
		return Default(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open topology file: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var t Topology
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parse topology file %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("topology file %s: unexpected data after the JSON value", path)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("validate topology file %s: %w", path, err)
	}
	return &t, nil
}

// Validate checks that the topology is usable by the fake API and the metrics generator.
// It collects every problem it finds and returns them combined with errors.Join.
func (t *Topology) Validate() error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}
	// seen records the values already observed per kind; a second occurrence is reported as a duplicate.
	seen := map[string]map[string]bool{}
	unique := func(kind, v string) {
		if seen[kind] == nil {
			seen[kind] = map[string]bool{}
		}
		if seen[kind][v] {
			fail("duplicate %s: %q", kind, v)
		}
		seen[kind][v] = true
	}

	if t.ProjectID == "" {
		fail("project_id is empty")
	}
	if t.UserName == "" {
		fail("user_name is empty")
	}

	for i, f := range t.Flavors {
		if f == nil {
			fail("flavors[%d] is null", i)
			continue
		}
		if f.ID == "" || f.Name == "" {
			fail("flavors[%d]: id and name are required", i)
		}
		unique("flavor id", f.ID)
		unique("flavor name", f.Name)
	}

	for i, h := range t.Hypervisors {
		if h == nil {
			fail("hypervisors[%d] is null", i)
			continue
		}
		if h.Hostname == "" {
			fail("hypervisors[%d]: hostname is empty", i)
		}
		unique("hypervisor hostname", h.Hostname)
		unique("hypervisor id", fmt.Sprint(h.ID))
		unique("hypervisor uuid", h.UUID)
		switch h.Mode {
		case ModeVGPU, ModeMIG, ModePassthrough:
		default:
			fail("hypervisor %s: unknown mode %q", h.Hostname, h.Mode)
		}

		gpuIndex := map[int]bool{}
		for j, g := range h.GPUs {
			if g == nil {
				fail("hypervisor %s: gpus[%d] is null", h.Hostname, j)
				continue
			}
			where := fmt.Sprintf("hypervisor %s gpu %d", h.Hostname, g.Index)
			if gpuIndex[g.Index] {
				fail("%s: duplicate gpu index", where)
			}
			gpuIndex[g.Index] = true
			unique("gpu uuid", g.UUID)
			unique("gpu bus_id", h.Hostname+"/"+g.BusID)
			t.validateGPU(h, g, where, fail, unique)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// validateGPU checks one GPU and the VMs on it against the constraints of its mode.
func (t *Topology) validateGPU(h *Hypervisor, g *GPU, where string,
	fail func(string, ...any), unique func(string, string)) {
	instances := map[int]*MIGInstance{}
	switch h.Mode {
	case ModeVGPU:
		if g.Profile == nil {
			fail("%s: GPU on a VGPU node has no profile", where)
		} else if len(g.VMs) > g.Profile.MaxInstances {
			fail("%s: %d VMs exceed the vGPU maximum of %d", where, len(g.VMs), g.Profile.MaxInstances)
		}
	case ModeMIG:
		slices := 0
		for k, mi := range g.MIGInstances {
			if mi == nil {
				fail("%s: mig_instances[%d] is null", where, k)
				continue
			}
			if instances[mi.GPUInstanceID] != nil {
				fail("%s: duplicate GPU instance id %d", where, mi.GPUInstanceID)
			}
			instances[mi.GPUInstanceID] = mi
			slices += mi.Slices
		}
		if slices > 7 {
			fail("%s: MIG slice total %d exceeds 7", where, slices)
		}
	case ModePassthrough:
		if len(g.VMs) > 1 {
			fail("%s: passthrough GPU has %d VMs (max 1)", where, len(g.VMs))
		}
	}

	used := map[int]string{}
	for k, vm := range g.VMs {
		if vm == nil {
			fail("%s: vms[%d] is null", where, k)
			continue
		}
		vw := fmt.Sprintf("vm %s", vm.Name)
		if vm.Name == "" || vm.ID == "" {
			fail("%s: vms[%d] needs both name and id", where, k)
		}
		unique("vm name", vm.Name)
		unique("vm id", vm.ID)
		unique("vm instance_name", vm.InstanceName)
		if vm.Hypervisor != h.Hostname {
			fail("%s: hypervisor %q does not match the host it runs on, %q", vw, vm.Hypervisor, h.Hostname)
		}
		if vm.GPUIndex != g.Index {
			fail("%s: gpu_index %d does not match the GPU it is attached to, %d", vw, vm.GPUIndex, g.Index)
		}
		if t.FlavorByID(vm.FlavorID) == nil {
			fail("%s: flavor %q not found", vw, vm.FlavorID)
		}
		if vm.Mode != h.Mode {
			fail("%s: mode %q does not match hypervisor mode %q", vw, vm.Mode, h.Mode)
		}
		if h.Mode == ModePassthrough {
			if vm.VGPUUUID != "" {
				fail("%s: passthrough VM has a vgpu_uuid", vw)
			}
		} else if vm.VGPUUUID == "" {
			fail("%s: vgpu_uuid is empty", vw)
		} else {
			unique("vm vgpu_uuid", vm.VGPUUUID)
		}
		if h.Mode == ModeMIG {
			if instances[vm.MIGGPUInstanceID] == nil {
				fail("%s: GPU instance %d not found on %s", vw, vm.MIGGPUInstanceID, where)
			} else if prev, ok := used[vm.MIGGPUInstanceID]; ok {
				fail("%s: GPU instance %d is already used by %s", vw, vm.MIGGPUInstanceID, prev)
			} else {
				used[vm.MIGGPUInstanceID] = vm.Name
			}
		} else if vm.MIGGPUInstanceID != -1 {
			fail("%s: mig_gpu_instance_id is %d on a non-MIG node (must be -1)", vw, vm.MIGGPUInstanceID)
		}
	}
}
