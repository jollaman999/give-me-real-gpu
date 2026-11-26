package topology

import (
	"fmt"
	"strconv"
	"strings"
)

// Fixed values of the default topology.
const (
	defaultRegion      = "RegionOne"
	defaultDomainID    = "default"
	defaultProjectName = "emu-gpu"
	defaultUserName    = "emu-admin"

	vmGuestBDF    = "00:05.0"
	hostDriver    = "595.91.04"
	hostCUDA      = "13020"
	vgpuGuestDrv  = "595.91.07"
	passthruGuest = "580.95.05"
)

// Fake addresses come only from the documentation ranges (TEST-NET) and are never connected to.
// Hypervisors start at .11 of TEST-NET-1, VMs start at .11 of TEST-NET-2 and spill over into TEST-NET-3.
const (
	hostIPFirst = 11
	vmIPFirst   = 11
)

var (
	hostIPNet = [3]string{"192", "0", "2"}
	vmIPNets  = [][3]string{{"198", "51", "100"}, {"203", "0", "113"}}
)

func ipv4(net [3]string, host int) string {
	return strings.Join([]string{net[0], net[1], net[2], strconv.Itoa(host)}, ".")
}

// hostIP returns the host_ip of the i-th hypervisor (0-based).
func hostIP(i int) string {
	return ipv4(hostIPNet, hostIPFirst+i)
}

// vmIP returns the fixed ip of the seq-th VM (1-based). After .254 of the first range comes .1 of the second.
// It panics once both ranges are used up (the default topology is nowhere near that big).
func vmIP(seq int) string {
	n := vmIPFirst + seq - 1
	for _, net := range vmIPNets {
		if n <= 254 {
			return ipv4(net, n)
		}
		n -= 254
	}
	panic(fmt.Sprintf("VM address ranges exhausted (seq %d)", seq))
}

// vbios builds a VBIOS version string (five dot-separated 2-digit hex pairs).
// The parts are passed separately because a literal with four dots would be mistaken for an IP address.
func vbios(parts ...string) string {
	return strings.Join(parts, ".")
}

// gpuModel holds the fixed values of one physical GPU model.
// Only the T4 VBIOS was taken from a real node; the others are not measured.
type gpuModel struct {
	name      string
	deviceID  string
	memoryMiB int
	vbios     string
	arch      string
}

var (
	modelA10        = gpuModel{"NVIDIA A10", "2236", 23028, vbios("94", "02", "5C", "00", "0A"), "Ampere"}
	modelT4         = gpuModel{"Tesla T4", "1eb8", 15360, vbios("90", "04", "96", "00", "01"), "Turing"}
	modelA100SXM80  = gpuModel{"NVIDIA A100-SXM4-80GB", "20b2", 81920, vbios("92", "00", "45", "00", "03"), "Ampere"}
	modelH100HBM3   = gpuModel{"NVIDIA H100 80GB HBM3", "2330", 81559, vbios("96", "00", "74", "00", "0D"), "Hopper"}
	modelL40S       = gpuModel{"NVIDIA L40S", "26b9", 46068, vbios("95", "02", "3C", "00", "0E"), "Ada Lovelace"}
	modelA100PCIE40 = gpuModel{"NVIDIA A100-PCIE-40GB", "20f1", 40960, vbios("92", "00", "25", "00", "08"), "Ampere"}
)

// migProfile is one kind of MIG GPU instance.
// giIDs is the GI ID order nvidia-smi assigns when the GPU is filled with this kind (1g 7..13, 2g 3..5, 3g 1..2, 7g 0).
// vgpu is the MIG-backed vGPU type attached to the instance. The type numbers (nvidia-NNN) are not measured.
type migProfile struct {
	name   string
	slices int
	memMiB int
	giIDs  []int
	vgpu   VGPUProfile
	flavor string
}

var (
	migA100_1g = migProfile{"1g.10gb", 1, 9728, []int{7, 8, 9, 10, 11, 12, 13},
		VGPUProfile{"NVIDIA A100D-1-10C", "nvidia-699", 10240, 7}, "emu-mig-1g"}
	migA100_2g = migProfile{"2g.20gb", 2, 19968, []int{3, 4, 5},
		VGPUProfile{"NVIDIA A100D-2-20C", "nvidia-700", 20480, 3}, "emu-mig-2g"}
	migA100_3g = migProfile{"3g.40gb", 3, 40192, []int{1, 2},
		VGPUProfile{"NVIDIA A100D-3-40C", "nvidia-701", 40960, 2}, "emu-mig-3g"}
	migA100_7g = migProfile{"7g.80gb", 7, 80896, []int{0},
		VGPUProfile{"NVIDIA A100D-7-80C", "nvidia-702", 81920, 1}, "emu-mig-7g"}
	migH100_1g = migProfile{"1g.10gb", 1, 9984, []int{7, 8, 9, 10, 11, 12, 13},
		VGPUProfile{"NVIDIA H100XM-1-10C", "nvidia-1074", 10240, 7}, "emu-mig-1g"}
	migH100_3g = migProfile{"3g.40gb", 3, 40448, []int{1, 2},
		VGPUProfile{"NVIDIA H100XM-3-40C", "nvidia-1076", 40960, 2}, "emu-mig-3g"}
)

// migPlan is the layout of one MIG GPU: create instances GPU instances and attach a VM to the first vms of them.
type migPlan struct {
	profile   migProfile
	instances int
	vms       int
}

// nodeSpec is one node of the default topology.
// VGPU and PASSTHROUGH nodes use vmCounts (VMs per GPU), MIG nodes use mig (a plan per GPU).
type nodeSpec struct {
	hostname string
	short    string
	mode     GPUMode
	vcpus    int
	memoryMB int
	localGB  int
	model    gpuModel
	buses    []string
	profile  *VGPUProfile
	flavor   string
	vmCounts []int
	mig      []migPlan
}

var defaultNodes = []nodeSpec{
	{
		hostname: "emu-vgpu-01", short: "vgpu01", mode: ModeVGPU,
		vcpus: 128, memoryMB: 1048576, localGB: 3576,
		model: modelA10, buses: []string{"18", "3a", "9a", "ba"},
		profile: &VGPUProfile{"NVIDIA A10-4Q", "nvidia-592", 4096, 6},
		flavor:  "emu-vgpu-a10-4q", vmCounts: []int{6, 4, 3, 0},
	},
	{
		hostname: "emu-vgpu-02", short: "vgpu02", mode: ModeVGPU,
		vcpus: 64, memoryMB: 524288, localGB: 1788,
		model: modelT4, buses: []string{"5d", "ab"},
		profile: &VGPUProfile{"GRID T4-4Q", "nvidia-232", 4096, 4},
		flavor:  "emu-vgpu-t4-4q", vmCounts: []int{4, 2},
	},
	{
		hostname: "emu-mig-01", short: "mig01", mode: ModeMIG,
		vcpus: 256, memoryMB: 2097152, localGB: 7152,
		model: modelA100SXM80, buses: []string{"18", "2a", "3a", "5d"},
		mig: []migPlan{
			{migA100_1g, 7, 5},
			{migA100_2g, 3, 2},
			{migA100_3g, 2, 2},
			{migA100_7g, 1, 1},
		},
	},
	{
		hostname: "emu-mig-02", short: "mig02", mode: ModeMIG,
		vcpus: 224, memoryMB: 2097152, localGB: 7152,
		model: modelH100HBM3, buses: []string{"1b", "43"},
		mig: []migPlan{
			{migH100_1g, 7, 7},
			{migH100_3g, 2, 2},
		},
	},
	{
		hostname: "emu-pt-01", short: "pt01", mode: ModePassthrough,
		vcpus: 128, memoryMB: 1048576, localGB: 3576,
		model: modelL40S, buses: []string{"9a", "ab", "ba", "db"},
		flavor: "emu-pt-l40s", vmCounts: []int{1, 1, 1, 0},
	},
	{
		hostname: "emu-pt-02", short: "pt02", mode: ModePassthrough,
		vcpus: 128, memoryMB: 1048576, localGB: 3576,
		model: modelA100PCIE40, buses: []string{"18", "2a", "3a", "5d"},
		flavor: "emu-pt-a100", vmCounts: []int{1, 1, 1, 1},
	},
}

// defaultFlavors returns the GPU flavors. MIG flavors select the instance size through a trait.
func defaultFlavors() []*Flavor {
	mk := func(name string, vcpus, ramMB, diskGB int, specs map[string]string) *Flavor {
		return &Flavor{
			ID:         StableUUID("flavor", name),
			Name:       name,
			VCPUs:      vcpus,
			RAMMB:      ramMB,
			DiskGB:     diskGB,
			ExtraSpecs: specs,
		}
	}
	vgpu := func(trait string) map[string]string {
		return map[string]string{
			"resources:VGPU": "1",
			"trait:" + trait: "required",
		}
	}
	return []*Flavor{
		mk("emu-vgpu-a10-4q", 8, 32768, 100, vgpu("CUSTOM_NVIDIA_A10_4Q")),
		mk("emu-vgpu-t4-4q", 4, 16384, 80, vgpu("CUSTOM_GRID_T4_4Q")),
		mk("emu-mig-1g", 8, 32768, 100, vgpu("CUSTOM_MIG_1G_10GB")),
		mk("emu-mig-2g", 16, 65536, 200, vgpu("CUSTOM_MIG_2G_20GB")),
		mk("emu-mig-3g", 24, 131072, 300, vgpu("CUSTOM_MIG_3G_40GB")),
		mk("emu-mig-7g", 64, 262144, 500, vgpu("CUSTOM_MIG_7G_80GB")),
		mk("emu-pt-l40s", 32, 131072, 500, map[string]string{"pci_passthrough:alias": "l40s:1"}),
		mk("emu-pt-a100", 32, 131072, 500, map[string]string{"pci_passthrough:alias": "a100:1"}),
	}
}

// Default builds the six-node default topology from the design document.
// Every id is derived deterministically from names, so repeated calls return identical values.
func Default() *Topology {
	t := &Topology{
		Region:      defaultRegion,
		DomainID:    defaultDomainID,
		ProjectID:   StableHex32("project", defaultProjectName),
		ProjectName: defaultProjectName,
		UserID:      StableHex32("user", defaultUserName),
		UserName:    defaultUserName,
		Flavors:     defaultFlavors(),
	}
	flavorID := map[string]*Flavor{}
	for _, f := range t.Flavors {
		flavorID[f.Name] = f
	}

	vmSeq := 0
	newVM := func(n nodeSpec, gpuIndex, nth int, flavor string) *VM {
		vmSeq++
		f := flavorID[flavor]
		name := fmt.Sprintf("emu-%s-g%d-%d", n.short, gpuIndex, nth)
		vm := &VM{
			ID:               StableUUID("server", name),
			Name:             name,
			InstanceName:     fmt.Sprintf("instance-%08x", vmSeq),
			FlavorID:         f.ID,
			IP:               vmIP(vmSeq),
			Hypervisor:       n.hostname,
			GPUIndex:         gpuIndex,
			Mode:             n.mode,
			MIGGPUInstanceID: -1,
			GuestBDF:         vmGuestBDF,
			VCPUs:            f.VCPUs,
			MemoryMB:         f.RAMMB,
			DiskGB:           f.DiskGB,
		}
		if n.mode == ModePassthrough {
			vm.GuestDriverVersion = passthruGuest
		} else {
			vm.VGPUUUID = StableUUID("mdev", name)
			vm.GuestDriverVersion = vgpuGuestDrv
		}
		return vm
	}

	for i, n := range defaultNodes {
		h := &Hypervisor{
			ID:       i + 1,
			UUID:     StableUUID("hypervisor", n.hostname),
			Hostname: n.hostname,
			HostIP:   hostIP(i),
			Mode:     n.mode,
			VCPUs:    n.vcpus,
			MemoryMB: n.memoryMB,
			LocalGB:  n.localGB,
		}
		if n.mode != ModePassthrough {
			h.DriverVersion = hostDriver
			h.CUDAVersion = hostCUDA
		}
		for gi, bus := range n.buses {
			g := &GPU{
				Index:     gi,
				UUID:      "GPU-" + StableUUID("gpu", n.hostname, strconv.Itoa(gi)),
				BusID:     "00000000:" + strings.ToUpper(bus) + ":00.0",
				HostBDF:   strings.ToLower(bus) + ":00.0",
				ModelName: n.model.name,
				DeviceID:  n.model.deviceID,
				MemoryMiB: n.model.memoryMiB,
				VBIOS:     n.model.vbios,
				Arch:      n.model.arch,
				VMs:       []*VM{},
			}
			switch n.mode {
			case ModeVGPU:
				p := *n.profile
				g.Profile = &p
				for k := 0; k < n.vmCounts[gi]; k++ {
					g.VMs = append(g.VMs, newVM(n, gi, k+1, n.flavor))
				}
			case ModePassthrough:
				for k := 0; k < n.vmCounts[gi]; k++ {
					g.VMs = append(g.VMs, newVM(n, gi, k+1, n.flavor))
				}
			case ModeMIG:
				plan := n.mig[gi]
				for k := 0; k < plan.instances; k++ {
					v := plan.profile.vgpu
					g.MIGInstances = append(g.MIGInstances, &MIGInstance{
						GPUInstanceID: plan.profile.giIDs[k],
						Profile:       plan.profile.name,
						Slices:        plan.profile.slices,
						MemoryMiB:     plan.profile.memMiB,
						VGPU:          &v,
					})
				}
				for k := 0; k < plan.vms; k++ {
					vm := newVM(n, gi, k+1, plan.profile.flavor)
					vm.MIGGPUInstanceID = g.MIGInstances[k].GPUInstanceID
					g.VMs = append(g.VMs, vm)
				}
			}
			h.GPUs = append(h.GPUs, g)
		}
		t.Hypervisors = append(t.Hypervisors, h)
	}
	return t
}
