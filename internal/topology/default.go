package topology

import (
	"fmt"
	"strconv"
	"strings"
)

// 기본 토폴로지의 고정 값.
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

// 가짜 주소는 문서용 대역(TEST-NET)만 쓴다. 실제로 접속하지 않는 값이다.
// 하이퍼바이저는 TEST-NET-1 의 .11 부터, VM 은 TEST-NET-2 의 .11 부터 쓰고 모자라면 TEST-NET-3 으로 넘어간다.
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

// hostIP 는 i 번째(0부터) 하이퍼바이저의 host_ip 다.
func hostIP(i int) string {
	return ipv4(hostIPNet, hostIPFirst+i)
}

// vmIP 는 seq 번째(1부터) VM 의 fixed ip 다. 첫 대역의 .254 다음은 둘째 대역의 .1 이다.
// 두 대역을 다 쓰면 panic 한다 (기본 토폴로지는 그만큼 크지 않다).
func vmIP(seq int) string {
	n := vmIPFirst + seq - 1
	for _, net := range vmIPNets {
		if n <= 254 {
			return ipv4(net, n)
		}
		n -= 254
	}
	panic(fmt.Sprintf("VM 주소 대역을 다 썼다 (seq %d)", seq))
}

// vbios 는 VBIOS 버전 문자열(점으로 이은 2자리 hex 다섯 개)을 만든다.
// 소스에 점 네 개짜리 숫자를 그대로 두면 IP 로 오인되므로 조각으로 적는다.
func vbios(parts ...string) string {
	return strings.Join(parts, ".")
}

// gpuModel 은 물리 GPU 종류 하나의 고정 값이다.
// VBIOS 는 T4 만 실제 노드에서 본 값이고 나머지는 실측 아님.
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

// migProfile 은 MIG GPU instance 종류 하나다.
// giIDs 는 그 종류를 꽉 채워 만들었을 때 nvidia-smi 가 주는 GI ID 순서다 (1g 7..13, 2g 3..5, 3g 1..2, 7g 0).
// vgpu 는 그 instance 에 붙는 MIG-backed vGPU type 이다. type 번호(nvidia-NNN)는 실측 아님.
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

// migPlan 은 MIG GPU 한 장의 계획이다. instances 개를 만들고 앞에서부터 vms 개에 VM 을 붙인다.
type migPlan struct {
	profile   migProfile
	instances int
	vms       int
}

// nodeSpec 은 기본 토폴로지의 노드 하나다.
// VGPU·PASSTHROUGH 는 vmCounts(GPU 마다 VM 수), MIG 는 mig(GPU 마다 계획)를 쓴다.
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

// defaultFlavors 는 GPU flavor 목록이다. MIG flavor 는 trait 로 instance 크기를 고른다.
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

// Default 는 설계 문서의 6노드 기본 토폴로지를 만든다.
// 모든 id 는 이름에서 결정적으로 나오므로 몇 번 불러도 같은 값이다.
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
