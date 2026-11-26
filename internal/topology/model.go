// Package topology describes the fake GPU cloud that the emulator imitates.
// The fake OpenStack API and the metrics generator read the same Topology,
// so nova's hypervisor_hostname and server id always match the host and uuid tags in InfluxDB.
package topology

// GPUMode is how a hypervisor hands its GPUs out to VMs.
// The value matches the gpu_type tag (upper case) in the metrics.
type GPUMode string

const (
	ModeVGPU        GPUMode = "VGPU"
	ModeMIG         GPUMode = "MIG"
	ModePassthrough GPUMode = "PASSTHROUGH"
)

// Topology is the whole fake cloud.
type Topology struct {
	Region      string        `json:"region"`       // region in the keystone catalog (e.g. RegionOne)
	DomainID    string        `json:"domain_id"`    // keystone domain id (e.g. default)
	ProjectID   string        `json:"project_id"`   // project id that every VM belongs to (32 hex chars)
	ProjectName string        `json:"project_name"` // project name
	UserID      string        `json:"user_id"`      // keystone user id (32 hex chars)
	UserName    string        `json:"user_name"`    // keystone user name (the account registered in the management platform)
	Hypervisors []*Hypervisor `json:"hypervisors"`
	Flavors     []*Flavor     `json:"flavors"`
}

// Hypervisor is a single nova compute node.
type Hypervisor struct {
	ID       int     `json:"id"`        // nova hypervisor integer id (starts at 1)
	UUID     string  `json:"uuid"`      // nova hypervisor uuid (used as the id from microversion 2.53 on)
	Hostname string  `json:"hostname"`  // hypervisor_hostname = service host = InfluxDB host tag
	HostIP   string  `json:"host_ip"`   // nova host_ip, the ip field of net metrics
	Mode     GPUMode `json:"mode"`      // how this node partitions its GPUs
	VCPUs    int     `json:"vcpus"`     // physical vCPU count
	MemoryMB int     `json:"memory_mb"` // physical memory
	LocalGB  int     `json:"local_gb"`  // local disk
	// Host nvidia driver. Empty on passthrough nodes, since the host has no driver there.
	DriverVersion string `json:"driver_version"`
	CUDAVersion   string `json:"cuda_version"` // host CUDA driver version (DCGM_FI_CUDA_DRIVER_VERSION format, e.g. 13020)
	GPUs          []*GPU `json:"gpus"`
	// entity_id the OpenTelemetry-based agent stamps on every data point. Empty means EntityID(Hostname, "").
	EntityID string `json:"entity_id,omitempty"`
}

// GPU is a single physical GPU.
type GPU struct {
	Index     int    `json:"index"`      // nvidia-smi index (starts at 0)
	UUID      string `json:"uuid"`       // GPU-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
	BusID     string `json:"bus_id"`     // 00000000:3B:00.0 format (nvidia-smi pci.bus_id)
	HostBDF   string `json:"host_bdf"`   // 3b:00.0 format (gpu_vm_mapping host_bdf tag, lower case)
	ModelName string `json:"model_name"` // e.g. NVIDIA A100-SXM4-80GB
	DeviceID  string `json:"device_id"`  // PCI device id, 4 lower-case hex digits (e.g. 20b2). The vendor is always 10de
	MemoryMiB int    `json:"memory_mib"` // total framebuffer
	VBIOS     string `json:"vbios"`      // five dot-separated hex pairs
	Arch      string `json:"arch"`       // e.g. Ampere, Hopper, Ada Lovelace, Turing

	// vGPU type created on this GPU on a VGPU node. nil in other modes.
	Profile *VGPUProfile `json:"profile,omitempty"`
	// GPU instances this GPU is split into on a MIG node. Empty in other modes.
	MIGInstances []*MIGInstance `json:"mig_instances,omitempty"`
	// VMs using this GPU. VGPU allows several, MIG at most one per instance, passthrough at most one.
	// An empty list means the GPU is unassigned.
	VMs []*VM `json:"vms"`
}

// VGPUProfile is an NVIDIA vGPU type.
type VGPUProfile struct {
	Name          string `json:"name"`           // e.g. NVIDIA A10-4Q, GRID T4-4Q (vgpu_model_name)
	Type          string `json:"type"`           // mdev type name (e.g. nvidia-592), the vgpu_profile field
	FramebufferMB int    `json:"framebuffer_mb"` // vgpu_fb_mb
	MaxInstances  int    `json:"max_instances"`  // vgpu_max_instances
}

// MIGInstance is a single MIG GPU instance.
type MIGInstance struct {
	GPUInstanceID int    `json:"gpu_instance_id"` // GI ID as reported by nvidia-smi
	Profile       string `json:"profile"`         // e.g. 1g.10gb
	Slices        int    `json:"slices"`          // number of compute slices it occupies (1g=1 ... 7g=7)
	MemoryMiB     int    `json:"memory_mib"`
	// vGPU type of the MIG-backed vGPU (e.g. A100D-1-10C / nvidia-699). OpenStack attaches MIG as a vGPU.
	VGPU *VGPUProfile `json:"vgpu_profile,omitempty"`
}

// VM is a single nova server. Only VMs that use a GPU are modeled.
type VM struct {
	ID           string  `json:"id"`            // nova server id = libvirt domain uuid = openstack_vm_uuid tag
	Name         string  `json:"name"`          // server name = hostname inside the VM = host tag of VM metrics
	InstanceName string  `json:"instance_name"` // libvirt name (e.g. instance-0000001a)
	FlavorID     string  `json:"flavor_id"`
	IP           string  `json:"ip"`         // fixed ip
	Hypervisor   string  `json:"hypervisor"` // Hypervisor.Hostname it runs on
	GPUIndex     int     `json:"gpu_index"`  // GPU.Index it is attached to
	Mode         GPUMode `json:"mode"`
	// mdev uuid of the vGPU (including MIG-backed). Used as the vgpu_uuid tag and as the GPU UUID seen inside the VM. Empty for passthrough.
	VGPUUUID string `json:"vgpu_uuid,omitempty"`
	// GPU instance id attached on a MIG node. -1 when not MIG.
	MIGGPUInstanceID int `json:"mig_gpu_instance_id"`
	// PCI address seen inside the VM (e.g. 00:05.0). Used for the gpu_vm_mapping guest_bdf tag and the PCI_BUSID of VM dcgm.
	GuestBDF string `json:"guest_bdf"`
	// nvidia guest driver version inside the VM.
	GuestDriverVersion string `json:"guest_driver_version"`
	VCPUs              int    `json:"vcpus"`
	MemoryMB           int    `json:"memory_mb"`
	DiskGB             int    `json:"disk_gb"`
	// entity_id the OpenTelemetry-based agent stamps on every data point. Empty means EntityID(Name, "").
	EntityID string `json:"entity_id,omitempty"`
}

// Flavor is a nova flavor. GPU flavors carry resources:VGPU or pci_passthrough:alias in ExtraSpecs.
type Flavor struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	VCPUs      int               `json:"vcpus"`
	RAMMB      int               `json:"ram_mb"`
	DiskGB     int               `json:"disk_gb"`
	ExtraSpecs map[string]string `json:"extra_specs"`
}

// EntityID is the entity_id of a node: override when it is set, otherwise a UUID derived from the node name,
// so the id stays the same across restarts.
func EntityID(name, override string) string {
	if override != "" {
		return override
	}
	return StableUUID("entity", name)
}

// AllVMs returns every VM, ordered by hypervisor and then by GPU.
func (t *Topology) AllVMs() []*VM {
	var out []*VM
	for _, h := range t.Hypervisors {
		for _, g := range h.GPUs {
			out = append(out, g.VMs...)
		}
	}
	return out
}

// HypervisorByName finds a hypervisor by hostname. Returns nil if there is none.
func (t *Topology) HypervisorByName(name string) *Hypervisor {
	for _, h := range t.Hypervisors {
		if h.Hostname == name {
			return h
		}
	}
	return nil
}

// FlavorByID finds a flavor by id. Returns nil if there is none.
func (t *Topology) FlavorByID(id string) *Flavor {
	for _, f := range t.Flavors {
		if f.ID == id {
			return f
		}
	}
	return nil
}
