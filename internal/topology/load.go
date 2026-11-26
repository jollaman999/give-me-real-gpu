package topology

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Load 는 토폴로지 JSON 파일을 읽고 Validate 한다. path 가 비어 있으면 Default() 를 돌려준다.
// 모르는 키가 있으면 오타로 보고 거부한다.
func Load(path string) (*Topology, error) {
	if path == "" {
		return Default(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("토폴로지 파일 열기: %w", err)
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var t Topology
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("토폴로지 파일 %s 해석: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("토폴로지 파일 %s: JSON 값 하나 뒤에 다른 내용이 있다", path)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("토폴로지 파일 %s 검증: %w", path, err)
	}
	return &t, nil
}

// Validate 는 토폴로지가 가짜 API·지표 생성기가 쓸 수 있는 상태인지 본다.
// 찾은 문제를 모두 모아 errors.Join 으로 돌려준다.
func (t *Topology) Validate() error {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf(format, a...))
	}
	// seen 은 종류별로 이미 나온 값을 기록하고, 두 번째로 나오면 중복으로 보고한다.
	seen := map[string]map[string]bool{}
	unique := func(kind, v string) {
		if seen[kind] == nil {
			seen[kind] = map[string]bool{}
		}
		if seen[kind][v] {
			fail("%s 중복: %q", kind, v)
		}
		seen[kind][v] = true
	}

	if t.ProjectID == "" {
		fail("project_id 가 비어 있다")
	}
	if t.UserName == "" {
		fail("user_name 이 비어 있다")
	}

	for i, f := range t.Flavors {
		if f == nil {
			fail("flavors[%d] 가 null 이다", i)
			continue
		}
		if f.ID == "" || f.Name == "" {
			fail("flavors[%d]: id 와 name 이 있어야 한다", i)
		}
		unique("flavor id", f.ID)
		unique("flavor name", f.Name)
	}

	for i, h := range t.Hypervisors {
		if h == nil {
			fail("hypervisors[%d] 가 null 이다", i)
			continue
		}
		if h.Hostname == "" {
			fail("hypervisors[%d]: hostname 이 비어 있다", i)
		}
		unique("hypervisor hostname", h.Hostname)
		unique("hypervisor id", fmt.Sprint(h.ID))
		unique("hypervisor uuid", h.UUID)
		switch h.Mode {
		case ModeVGPU, ModeMIG, ModePassthrough:
		default:
			fail("hypervisor %s: 모르는 mode %q", h.Hostname, h.Mode)
		}

		gpuIndex := map[int]bool{}
		for j, g := range h.GPUs {
			if g == nil {
				fail("hypervisor %s: gpus[%d] 가 null 이다", h.Hostname, j)
				continue
			}
			where := fmt.Sprintf("hypervisor %s gpu %d", h.Hostname, g.Index)
			if gpuIndex[g.Index] {
				fail("%s: gpu index 중복", where)
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

// validateGPU 는 GPU 한 장과 그 위의 VM 들을 모드별 제약으로 본다.
func (t *Topology) validateGPU(h *Hypervisor, g *GPU, where string,
	fail func(string, ...any), unique func(string, string)) {
	instances := map[int]*MIGInstance{}
	switch h.Mode {
	case ModeVGPU:
		if g.Profile == nil {
			fail("%s: VGPU 노드의 GPU 에 profile 이 없다", where)
		} else if len(g.VMs) > g.Profile.MaxInstances {
			fail("%s: VM %d 개가 vGPU 최대 %d 개를 넘는다", where, len(g.VMs), g.Profile.MaxInstances)
		}
	case ModeMIG:
		slices := 0
		for k, mi := range g.MIGInstances {
			if mi == nil {
				fail("%s: mig_instances[%d] 가 null 이다", where, k)
				continue
			}
			if instances[mi.GPUInstanceID] != nil {
				fail("%s: GPU instance id %d 중복", where, mi.GPUInstanceID)
			}
			instances[mi.GPUInstanceID] = mi
			slices += mi.Slices
		}
		if slices > 7 {
			fail("%s: MIG slice 합 %d 가 7 을 넘는다", where, slices)
		}
	case ModePassthrough:
		if len(g.VMs) > 1 {
			fail("%s: passthrough GPU 에 VM 이 %d 개다 (최대 1)", where, len(g.VMs))
		}
	}

	used := map[int]string{}
	for k, vm := range g.VMs {
		if vm == nil {
			fail("%s: vms[%d] 가 null 이다", where, k)
			continue
		}
		vw := fmt.Sprintf("vm %s", vm.Name)
		if vm.Name == "" || vm.ID == "" {
			fail("%s: vms[%d] 에 name 과 id 가 있어야 한다", where, k)
		}
		unique("vm name", vm.Name)
		unique("vm id", vm.ID)
		unique("vm instance_name", vm.InstanceName)
		if vm.Hypervisor != h.Hostname {
			fail("%s: hypervisor %q 가 실제로 올라간 %q 와 다르다", vw, vm.Hypervisor, h.Hostname)
		}
		if vm.GPUIndex != g.Index {
			fail("%s: gpu_index %d 가 실제로 붙은 GPU %d 와 다르다", vw, vm.GPUIndex, g.Index)
		}
		if t.FlavorByID(vm.FlavorID) == nil {
			fail("%s: flavor %q 가 없다", vw, vm.FlavorID)
		}
		if vm.Mode != h.Mode {
			fail("%s: mode %q 가 hypervisor mode %q 와 다르다", vw, vm.Mode, h.Mode)
		}
		if h.Mode == ModePassthrough {
			if vm.VGPUUUID != "" {
				fail("%s: passthrough VM 에 vgpu_uuid 가 있다", vw)
			}
		} else if vm.VGPUUUID == "" {
			fail("%s: vgpu_uuid 가 비어 있다", vw)
		} else {
			unique("vm vgpu_uuid", vm.VGPUUUID)
		}
		if h.Mode == ModeMIG {
			if instances[vm.MIGGPUInstanceID] == nil {
				fail("%s: GPU instance %d 가 %s 에 없다", vw, vm.MIGGPUInstanceID, where)
			} else if prev, ok := used[vm.MIGGPUInstanceID]; ok {
				fail("%s: GPU instance %d 를 %s 가 이미 쓴다", vw, vm.MIGGPUInstanceID, prev)
			} else {
				used[vm.MIGGPUInstanceID] = vm.Name
			}
		} else if vm.MIGGPUInstanceID != -1 {
			fail("%s: MIG 가 아닌데 mig_gpu_instance_id 가 %d 다 (-1 이어야 한다)", vw, vm.MIGGPUInstanceID)
		}
	}
}
