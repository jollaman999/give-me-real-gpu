// Package topology 는 emulator 가 흉내 내는 가짜 GPU 클라우드의 구성을 담는다.
// 가짜 OpenStack API 와 지표 생성기가 같은 Topology 를 읽으므로,
// nova 의 hypervisor_hostname·server id 와 InfluxDB 의 host·uuid 태그가 항상 같은 값이 된다.
package topology

// GPUMode 는 하이퍼바이저가 GPU 를 VM 에 나눠 주는 방식이다.
// 값은 지표의 gpu_type 태그(대문자) 와 같다.
type GPUMode string

const (
	ModeVGPU        GPUMode = "VGPU"
	ModeMIG         GPUMode = "MIG"
	ModePassthrough GPUMode = "PASSTHROUGH"
)

// Topology 는 가짜 클라우드 전체다.
type Topology struct {
	Region      string        `json:"region"`       // keystone catalog 의 region (예: RegionOne)
	DomainID    string        `json:"domain_id"`    // keystone domain id (예: default)
	ProjectID   string        `json:"project_id"`   // 모든 VM 이 속한 project id (32자 hex)
	ProjectName string        `json:"project_name"` // project 이름
	UserID      string        `json:"user_id"`      // keystone user id (32자 hex)
	UserName    string        `json:"user_name"`    // keystone user 이름 (관리 플랫폼에 등록할 계정)
	Hypervisors []*Hypervisor `json:"hypervisors"`
	Flavors     []*Flavor     `json:"flavors"`
}

// Hypervisor 는 nova compute 노드 하나다.
type Hypervisor struct {
	ID       int     `json:"id"`        // nova hypervisor 정수 id (1부터)
	UUID     string  `json:"uuid"`      // nova hypervisor uuid (microversion 2.53 이상에서 id 로 쓰임)
	Hostname string  `json:"hostname"`  // hypervisor_hostname = service host = InfluxDB host 태그
	HostIP   string  `json:"host_ip"`   // nova host_ip, net 지표의 ip 필드
	Mode     GPUMode `json:"mode"`      // 이 노드의 GPU 분할 방식
	VCPUs    int     `json:"vcpus"`     // 물리 vCPU 수
	MemoryMB int     `json:"memory_mb"` // 물리 메모리
	LocalGB  int     `json:"local_gb"`  // 로컬 디스크
	// 호스트 nvidia 드라이버. passthrough 노드는 호스트에 드라이버가 없으므로 빈 문자열이다.
	DriverVersion string `json:"driver_version"`
	CUDAVersion   string `json:"cuda_version"` // 호스트 CUDA 드라이버 버전 (DCGM_FI_CUDA_DRIVER_VERSION 형식, 예: 13020)
	GPUs          []*GPU `json:"gpus"`
}

// GPU 는 물리 GPU 한 장이다.
type GPU struct {
	Index     int    `json:"index"`      // nvidia-smi index (0부터)
	UUID      string `json:"uuid"`       // GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	BusID     string `json:"bus_id"`     // 00000000:3B:00.0 형식 (nvidia-smi pci.bus_id)
	HostBDF   string `json:"host_bdf"`   // 3b:00.0 형식 (gpu_vm_mapping host_bdf 태그, 소문자)
	ModelName string `json:"model_name"` // 예: NVIDIA A100-SXM4-80GB
	DeviceID  string `json:"device_id"`  // PCI device id 소문자 4자리 (예: 20b2). vendor 는 항상 10de
	MemoryMiB int    `json:"memory_mib"` // 프레임버퍼 전체
	VBIOS     string `json:"vbios"`      // 예: 92.00.45.00.03
	Arch      string `json:"arch"`       // 예: Ampere, Hopper, Ada Lovelace, Turing

	// VGPU 노드에서 이 GPU 에 만든 vGPU 종류. 다른 모드면 nil.
	Profile *VGPUProfile `json:"profile,omitempty"`
	// MIG 노드에서 이 GPU 를 나눈 GPU instance 목록. 다른 모드면 비어 있다.
	MIGInstances []*MIGInstance `json:"mig_instances,omitempty"`
	// 이 GPU 를 쓰는 VM. VGPU 는 여러 개, MIG 는 instance 마다 최대 하나, passthrough 는 최대 하나.
	// 비어 있으면 미할당 GPU 다.
	VMs []*VM `json:"vms"`
}

// VGPUProfile 은 NVIDIA vGPU type 이다.
type VGPUProfile struct {
	Name          string `json:"name"`           // 예: NVIDIA A10-4Q, GRID T4-4Q (vgpu_model_name)
	Type          string `json:"type"`           // mdev type 이름 (예: nvidia-592), vgpu_profile 필드
	FramebufferMB int    `json:"framebuffer_mb"` // vgpu_fb_mb
	MaxInstances  int    `json:"max_instances"`  // vgpu_max_instances
}

// MIGInstance 는 MIG GPU instance 하나다.
type MIGInstance struct {
	GPUInstanceID int    `json:"gpu_instance_id"` // nvidia-smi 의 GI ID
	Profile       string `json:"profile"`         // 예: 1g.10gb
	Slices        int    `json:"slices"`          // 차지하는 compute slice 수 (1g=1 ... 7g=7)
	MemoryMiB     int    `json:"memory_mib"`
	// MIG-backed vGPU 의 vGPU type (예: A100D-1-10C / nvidia-699). OpenStack 은 MIG 를 vGPU 로 붙인다.
	VGPU *VGPUProfile `json:"vgpu_profile,omitempty"`
}

// VM 은 nova server 하나다. GPU 를 쓰는 VM 만 다룬다.
type VM struct {
	ID           string  `json:"id"`            // nova server id = libvirt domain uuid = openstack_vm_uuid 태그
	Name         string  `json:"name"`          // server 이름 = VM 안 hostname = VM 지표의 host 태그
	InstanceName string  `json:"instance_name"` // libvirt 이름 (예: instance-0000001a)
	FlavorID     string  `json:"flavor_id"`
	IP           string  `json:"ip"`         // fixed ip
	Hypervisor   string  `json:"hypervisor"` // 올라간 Hypervisor.Hostname
	GPUIndex     int     `json:"gpu_index"`  // 올라간 GPU.Index
	Mode         GPUMode `json:"mode"`
	// vGPU(MIG-backed 포함) 의 mdev uuid. vgpu_uuid 태그와 VM 안에서 본 GPU UUID 로 쓰인다. passthrough 는 빈 문자열.
	VGPUUUID string `json:"vgpu_uuid,omitempty"`
	// MIG 노드에서 붙은 GPU instance id. MIG 가 아니면 -1.
	MIGGPUInstanceID int `json:"mig_gpu_instance_id"`
	// VM 안에서 본 PCI 주소 (예: 00:05.0). gpu_vm_mapping guest_bdf 태그와 VM dcgm 의 PCI_BUSID 에 쓰인다.
	GuestBDF string `json:"guest_bdf"`
	// VM 안 nvidia 게스트 드라이버 버전.
	GuestDriverVersion string `json:"guest_driver_version"`
	VCPUs              int    `json:"vcpus"`
	MemoryMB           int    `json:"memory_mb"`
	DiskGB             int    `json:"disk_gb"`
}

// Flavor 는 nova flavor 다. GPU flavor 는 ExtraSpecs 에 resources:VGPU 나 pci_passthrough:alias 를 갖는다.
type Flavor struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	VCPUs      int               `json:"vcpus"`
	RAMMB      int               `json:"ram_mb"`
	DiskGB     int               `json:"disk_gb"`
	ExtraSpecs map[string]string `json:"extra_specs"`
}

// AllVMs 는 하이퍼바이저·GPU 순서대로 모든 VM 을 돌려준다.
func (t *Topology) AllVMs() []*VM {
	var out []*VM
	for _, h := range t.Hypervisors {
		for _, g := range h.GPUs {
			out = append(out, g.VMs...)
		}
	}
	return out
}

// HypervisorByName 은 hostname 으로 하이퍼바이저를 찾는다. 없으면 nil.
func (t *Topology) HypervisorByName(name string) *Hypervisor {
	for _, h := range t.Hypervisors {
		if h.Hostname == name {
			return h
		}
	}
	return nil
}

// FlavorByID 는 id 로 flavor 를 찾는다. 없으면 nil.
func (t *Topology) FlavorByID(id string) *Flavor {
	for _, f := range t.Flavors {
		if f.ID == id {
			return f
		}
	}
	return nil
}
