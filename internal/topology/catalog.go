package topology

import (
	"fmt"
	"strconv"
	"strings"
)

// MaxMIGSlices is the number of compute slices one MIG GPU has.
const MaxMIGSlices = 7

// Catalog lists the building blocks the topology editor offers: GPU models with the vGPU and MIG
// profiles each one supports, the default flavors and the values Fill uses when a field is empty.
type Catalog struct {
	GPUModels []CatalogModel  `json:"gpu_models"`
	Flavors   []*Flavor       `json:"flavors"`
	Defaults  CatalogDefaults `json:"defaults"`
}

// CatalogModel is one physical GPU model.
type CatalogModel struct {
	Name      string `json:"name"`
	DeviceID  string `json:"device_id"`
	MemoryMiB int    `json:"memory_mib"`
	VBIOS     string `json:"vbios"`
	Arch      string `json:"arch"`
	// Modes lists the node modes this model can be placed in. Passthrough works for every model,
	// VGPU and MIG only when the catalog has a profile for the model.
	Modes             []GPUMode     `json:"modes"`
	VGPUProfiles      []CatalogVGPU `json:"vgpu_profiles"`
	MIGProfiles       []CatalogMIG  `json:"mig_profiles"`
	PassthroughFlavor string        `json:"passthrough_flavor"`
}

// CatalogVGPU is a vGPU type and the flavor name VMs on it get.
type CatalogVGPU struct {
	VGPUProfile
	Flavor string `json:"flavor"`
}

// CatalogMIG is a MIG GPU instance kind and the flavor name VMs on it get.
// GPUInstanceIDs is the order in which Fill hands out GI IDs for this kind.
type CatalogMIG struct {
	Name           string      `json:"name"`
	Slices         int         `json:"slices"`
	MemoryMiB      int         `json:"memory_mib"`
	GPUInstanceIDs []int       `json:"gpu_instance_ids"`
	VGPU           VGPUProfile `json:"vgpu_profile"`
	Flavor         string      `json:"flavor"`
}

// CatalogDefaults are the values Fill writes into empty fields.
type CatalogDefaults struct {
	Region             string `json:"region"`
	DomainID           string `json:"domain_id"`
	ProjectName        string `json:"project_name"`
	UserName           string `json:"user_name"`
	NodeVCPUs          int    `json:"node_vcpus"`
	NodeMemoryMB       int    `json:"node_memory_mb"`
	NodeLocalGB        int    `json:"node_local_gb"`
	HostDriver         string `json:"host_driver"`
	HostCUDA           string `json:"host_cuda"`
	VGPUGuestDriver    string `json:"vgpu_guest_driver"`
	PassthroughGuest   string `json:"passthrough_guest_driver"`
	GuestBDF           string `json:"guest_bdf"`
	MaxMIGSlices       int    `json:"max_mig_slices"`
	HostIPRange        string `json:"host_ip_range"`
	VMIPRanges         string `json:"vm_ip_ranges"`
	FirstHypervisorID  int    `json:"first_hypervisor_id"`
	InstanceNameFormat string `json:"instance_name_format"`
}

// Size of a node that Fill uses when vcpus, memory or disk is 0.
const (
	fillNodeVCPUs    = 128
	fillNodeMemoryMB = 1048576
	fillNodeLocalGB  = 3576
)

// catalogMIG lists the MIG kinds per model. The order is the order the editor shows them in.
var catalogMIG = map[string][]migProfile{
	modelA100SXM80.name: {migA100_1g, migA100_2g, migA100_3g, migA100_7g},
	modelH100HBM3.name:  {migH100_1g, migH100_3g},
}

// catalogBuses is the PCI bus order Fill picks from when a GPU has no bus id and its node is not a default node.
var catalogBuses = []string{"18", "2a", "3a", "5d", "9a", "ab", "ba", "db", "1b", "43", "4e", "8a", "ca", "e1", "f1", "f2"}

// DefaultCatalog builds the catalog from the default topology's models, profiles and flavors.
func DefaultCatalog() Catalog {
	c := Catalog{
		Flavors: defaultFlavors(),
		Defaults: CatalogDefaults{
			Region:             defaultRegion,
			DomainID:           defaultDomainID,
			ProjectName:        defaultProjectName,
			UserName:           defaultUserName,
			NodeVCPUs:          fillNodeVCPUs,
			NodeMemoryMB:       fillNodeMemoryMB,
			NodeLocalGB:        fillNodeLocalGB,
			HostDriver:         hostDriver,
			HostCUDA:           hostCUDA,
			VGPUGuestDriver:    vgpuGuestDrv,
			PassthroughGuest:   passthruGuest,
			GuestBDF:           vmGuestBDF,
			MaxMIGSlices:       MaxMIGSlices,
			HostIPRange:        hostIPNet[0] + "." + hostIPNet[1] + "." + hostIPNet[2] + ".0/24",
			VMIPRanges:         ipRanges(vmIPNets),
			FirstHypervisorID:  1,
			InstanceNameFormat: "instance-%08x",
		},
	}
	index := map[string]int{}
	add := func(m gpuModel) *CatalogModel {
		if i, ok := index[m.name]; ok {
			return &c.GPUModels[i]
		}
		index[m.name] = len(c.GPUModels)
		c.GPUModels = append(c.GPUModels, CatalogModel{
			Name: m.name, DeviceID: m.deviceID, MemoryMiB: m.memoryMiB, VBIOS: m.vbios, Arch: m.arch,
			VGPUProfiles: []CatalogVGPU{}, MIGProfiles: []CatalogMIG{},
		})
		return &c.GPUModels[len(c.GPUModels)-1]
	}
	for _, n := range defaultNodes {
		m := add(n.model)
		switch n.mode {
		case ModeVGPU:
			m.VGPUProfiles = append(m.VGPUProfiles, CatalogVGPU{VGPUProfile: *n.profile, Flavor: n.flavor})
		case ModePassthrough:
			m.PassthroughFlavor = n.flavor
		case ModeMIG:
			for _, p := range catalogMIG[n.model.name] {
				m.MIGProfiles = append(m.MIGProfiles, CatalogMIG{
					Name: p.name, Slices: p.slices, MemoryMiB: p.memMiB,
					GPUInstanceIDs: append([]int(nil), p.giIDs...), VGPU: p.vgpu, Flavor: p.flavor,
				})
			}
		}
	}
	for i := range c.GPUModels {
		m := &c.GPUModels[i]
		if len(m.VGPUProfiles) > 0 {
			m.Modes = append(m.Modes, ModeVGPU)
		}
		if len(m.MIGProfiles) > 0 {
			m.Modes = append(m.Modes, ModeMIG)
		}
		m.Modes = append(m.Modes, ModePassthrough)
		if m.PassthroughFlavor == "" {
			m.PassthroughFlavor = passthroughFlavorName(m.Name)
		}
	}
	return c
}

func ipRanges(nets [][3]string) string {
	var out []string
	for _, n := range nets {
		out = append(out, n[0]+"."+n[1]+"."+n[2]+".0/24")
	}
	return strings.Join(out, ", ")
}

func (c *Catalog) model(name string) *CatalogModel {
	for i := range c.GPUModels {
		if c.GPUModels[i].Name == name {
			return &c.GPUModels[i]
		}
	}
	return nil
}

func (c *Catalog) mig(model, profile string) *CatalogMIG {
	m := c.model(model)
	if m == nil {
		return nil
	}
	for i := range m.MIGProfiles {
		if m.MIGProfiles[i].Name == profile {
			return &m.MIGProfiles[i]
		}
	}
	return nil
}

func (c *Catalog) vgpuFlavor(profile string) string {
	for _, m := range c.GPUModels {
		for _, p := range m.VGPUProfiles {
			if p.Name == profile {
				return p.Flavor
			}
		}
	}
	return ""
}

// slug lower-cases s and turns every run of other characters than letters and digits into one dash.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}

func trimVendor(name string) string {
	for _, p := range []string{"NVIDIA ", "GRID ", "Tesla "} {
		name = strings.TrimPrefix(name, p)
	}
	return name
}

func passthroughFlavorName(model string) string { return "emu-pt-" + slug(trimVendor(model)) }
func vgpuFlavorName(profile string) string      { return "emu-vgpu-" + slug(trimVendor(profile)) }
func migFlavorName(profile string) string       { return "emu-mig-" + slug(profile) }

// traitName is CUSTOM_ plus the upper-cased name with every other character than letters and digits as _.
func traitName(name string) string {
	return "CUSTOM_" + strings.ToUpper(strings.ReplaceAll(slug(name), "-", "_"))
}

// derivedFlavor builds a flavor for a name no catalog flavor has.
func derivedFlavor(name string, vcpus, ramMB, diskGB int, specs map[string]string) *Flavor {
	return &Flavor{ID: StableUUID("flavor", name), Name: name, VCPUs: vcpus, RAMMB: ramMB, DiskGB: diskGB, ExtraSpecs: specs}
}

// filler carries the state of one Fill call.
type filler struct {
	t       *Topology
	cat     Catalog
	ips     map[string]bool
	insts   map[string]bool
	vmNames map[string]bool
}

// Fill writes values into the empty fields of t with the same deterministic rules Default uses,
// so a topology that only has names becomes complete. Fields that already have a value are left alone.
//
// "Empty" is "" for strings, 0 for sizes and ids, nil for profiles, and a negative number for
// MIG GPU instance ids (0 is a valid GI ID). A VM on a MIG node with a negative
// mig_gpu_instance_id gets the first GPU instance no other VM on that GPU uses.
// GPU indexes are not filled: 0 is a valid index. A VM's gpu_index of 0 is taken as empty and set to
// the index of the GPU it sits under.
func Fill(t *Topology) {
	if t == nil {
		return
	}
	f := &filler{t: t, cat: DefaultCatalog(), ips: map[string]bool{}, insts: map[string]bool{}, vmNames: map[string]bool{}}

	if t.Region == "" {
		t.Region = defaultRegion
	}
	if t.DomainID == "" {
		t.DomainID = defaultDomainID
	}
	if t.ProjectName == "" {
		t.ProjectName = defaultProjectName
	}
	if t.ProjectID == "" {
		t.ProjectID = StableHex32("project", t.ProjectName)
	}
	if t.UserName == "" {
		t.UserName = defaultUserName
	}
	if t.UserID == "" {
		t.UserID = StableHex32("user", t.UserName)
	}
	for _, fl := range t.Flavors {
		if fl != nil && fl.ID == "" && fl.Name != "" {
			fl.ID = StableUUID("flavor", fl.Name)
		}
	}

	f.collect()
	f.fillHypervisors()
	seq := 0
	for _, h := range t.Hypervisors {
		if h == nil {
			continue
		}
		for _, g := range h.GPUs {
			if g == nil {
				continue
			}
			f.fillGPU(h, g)
			for k, vm := range g.VMs {
				if vm == nil {
					continue
				}
				seq++
				f.fillVM(h, g, k, seq, vm)
			}
		}
	}
}

// collect records the addresses, libvirt names and VM names already taken, so filled values do not collide with them.
func (f *filler) collect() {
	for _, h := range f.t.Hypervisors {
		if h == nil {
			continue
		}
		if h.HostIP != "" {
			f.ips[h.HostIP] = true
		}
		for _, g := range h.GPUs {
			if g == nil {
				continue
			}
			for _, vm := range g.VMs {
				if vm == nil {
					continue
				}
				if vm.IP != "" {
					f.ips[vm.IP] = true
				}
				if vm.InstanceName != "" {
					f.insts[vm.InstanceName] = true
				}
				if vm.Name != "" {
					f.vmNames[vm.Name] = true
				}
			}
		}
	}
}

func (f *filler) fillHypervisors() {
	ids := map[int]bool{}
	maxID := 0
	for _, h := range f.t.Hypervisors {
		if h != nil && h.ID != 0 {
			ids[h.ID] = true
			maxID = max(maxID, h.ID)
		}
	}
	for i, h := range f.t.Hypervisors {
		if h == nil {
			continue
		}
		if h.ID == 0 {
			id := i + 1
			if ids[id] {
				id = maxID + 1
			}
			h.ID = id
			ids[id] = true
			maxID = max(maxID, id)
		}
		if h.UUID == "" && h.Hostname != "" {
			h.UUID = StableUUID("hypervisor", h.Hostname)
		}
		if h.HostIP == "" {
			h.HostIP = f.freeIP([][3]string{hostIPNet}, hostIPFirst+i, hostIPFirst)
		}
		if h.VCPUs == 0 {
			h.VCPUs = fillNodeVCPUs
		}
		if h.MemoryMB == 0 {
			h.MemoryMB = fillNodeMemoryMB
		}
		if h.LocalGB == 0 {
			h.LocalGB = fillNodeLocalGB
		}
		if h.Mode == ModeVGPU || h.Mode == ModeMIG {
			if h.DriverVersion == "" {
				h.DriverVersion = hostDriver
			}
			if h.CUDAVersion == "" {
				h.CUDAVersion = hostCUDA
			}
		}
	}
}

// freeIP returns the address at position want (counting .first of the first range as first) when it is free,
// otherwise the first free address from .first on. It returns "" when the ranges are used up.
func (f *filler) freeIP(nets [][3]string, want, first int) string {
	at := func(n int) string {
		for _, net := range nets {
			if n <= 254 {
				return ipv4(net, n)
			}
			n -= 254
		}
		return ""
	}
	if ip := at(want); ip != "" && !f.ips[ip] {
		f.ips[ip] = true
		return ip
	}
	for n := first; ; n++ {
		ip := at(n)
		if ip == "" {
			return ""
		}
		if !f.ips[ip] {
			f.ips[ip] = true
			return ip
		}
	}
}

// defaultBus returns the bus of GPU index i of the default node with this hostname, or "".
func defaultBus(hostname string, i int) string {
	for _, n := range defaultNodes {
		if n.hostname == hostname && i >= 0 && i < len(n.buses) {
			return n.buses[i]
		}
	}
	return ""
}

func (f *filler) fillGPU(h *Hypervisor, g *GPU) {
	m := f.cat.model(g.ModelName)
	if m != nil {
		if g.DeviceID == "" {
			g.DeviceID = m.DeviceID
		}
		if g.MemoryMiB == 0 {
			g.MemoryMiB = m.MemoryMiB
		}
		if g.VBIOS == "" {
			g.VBIOS = m.VBIOS
		}
		if g.Arch == "" {
			g.Arch = m.Arch
		}
	}
	if g.UUID == "" && h.Hostname != "" {
		g.UUID = "GPU-" + StableUUID("gpu", h.Hostname, strconv.Itoa(g.Index))
	}
	f.fillBus(h, g)
	if g.VMs == nil {
		g.VMs = []*VM{}
	}

	switch h.Mode {
	case ModeVGPU:
		if g.Profile == nil && m != nil && len(m.VGPUProfiles) > 0 {
			p := m.VGPUProfiles[0].VGPUProfile
			g.Profile = &p
		}
	case ModeMIG:
		used := map[int]bool{}
		for _, mi := range g.MIGInstances {
			if mi != nil && mi.GPUInstanceID >= 0 {
				used[mi.GPUInstanceID] = true
			}
		}
		for _, mi := range g.MIGInstances {
			if mi == nil {
				continue
			}
			cm := f.cat.mig(g.ModelName, mi.Profile)
			if cm != nil {
				if mi.Slices == 0 {
					mi.Slices = cm.Slices
				}
				if mi.MemoryMiB == 0 {
					mi.MemoryMiB = cm.MemoryMiB
				}
				if mi.VGPU == nil {
					v := cm.VGPU
					mi.VGPU = &v
				}
			}
			if mi.GPUInstanceID < 0 {
				mi.GPUInstanceID = freeGI(cm, used)
				used[mi.GPUInstanceID] = true
			}
		}
	}
}

// freeGI returns the first GI ID of the kind's order that is not used yet, or the smallest unused id
// when the kind is unknown or its ids are all taken.
func freeGI(cm *CatalogMIG, used map[int]bool) int {
	if cm != nil {
		for _, id := range cm.GPUInstanceIDs {
			if !used[id] {
				return id
			}
		}
	}
	id := 0
	for used[id] {
		id++
	}
	return id
}

func (f *filler) fillBus(h *Hypervisor, g *GPU) {
	switch {
	case g.BusID != "" && g.HostBDF == "":
		if _, rest, ok := strings.Cut(g.BusID, ":"); ok && strings.Count(g.BusID, ":") == 2 {
			g.HostBDF = strings.ToLower(rest)
		}
		return
	case g.BusID == "" && g.HostBDF != "":
		g.BusID = "00000000:" + strings.ToUpper(g.HostBDF)
		return
	case g.BusID != "":
		return
	}
	taken := map[string]bool{}
	for _, o := range h.GPUs {
		if o != nil && o.BusID != "" {
			taken[strings.ToLower(o.BusID)] = true
		}
	}
	busID := func(bus string) string { return "00000000:" + strings.ToUpper(bus) + ":00.0" }
	free := func(bus string) bool { return bus != "" && !taken[strings.ToLower(busID(bus))] }
	bus := defaultBus(h.Hostname, g.Index)
	if !free(bus) {
		bus = ""
		for _, b := range catalogBuses {
			if free(b) {
				bus = b
				break
			}
		}
	}
	for n := 0x01; bus == "" && n <= 0xff; n++ {
		if b := fmt.Sprintf("%02x", n); free(b) {
			bus = b
		}
	}
	if bus == "" {
		return
	}
	g.BusID = busID(bus)
	g.HostBDF = strings.ToLower(bus) + ":00.0"
}

func (f *filler) fillVM(h *Hypervisor, g *GPU, k, seq int, vm *VM) {
	if vm.Name == "" && h.Hostname != "" {
		base := fmt.Sprintf("%s-g%d-%d", h.Hostname, g.Index, k+1)
		name := base
		for n := 2; f.vmNames[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		vm.Name = name
		f.vmNames[name] = true
	}
	if vm.Hypervisor == "" {
		vm.Hypervisor = h.Hostname
	}
	if vm.Mode == "" {
		vm.Mode = h.Mode
	}
	if vm.GPUIndex == 0 {
		vm.GPUIndex = g.Index
	}
	if vm.ID == "" && vm.Name != "" {
		vm.ID = StableUUID("server", vm.Name)
	}
	if vm.InstanceName == "" {
		for n := seq; ; n++ {
			name := fmt.Sprintf("instance-%08x", n)
			if !f.insts[name] {
				vm.InstanceName = name
				f.insts[name] = true
				break
			}
		}
	}
	if vm.IP == "" {
		vm.IP = f.freeIP(vmIPNets, vmIPFirst+seq-1, vmIPFirst)
	}
	if vm.GuestBDF == "" {
		vm.GuestBDF = vmGuestBDF
	}
	if h.Mode == ModePassthrough {
		if vm.GuestDriverVersion == "" {
			vm.GuestDriverVersion = passthruGuest
		}
	} else if h.Mode == ModeVGPU || h.Mode == ModeMIG {
		if vm.VGPUUUID == "" && vm.Name != "" {
			vm.VGPUUUID = StableUUID("mdev", vm.Name)
		}
		if vm.GuestDriverVersion == "" {
			vm.GuestDriverVersion = vgpuGuestDrv
		}
	}
	if h.Mode == ModeMIG && vm.MIGGPUInstanceID < 0 {
		vm.MIGGPUInstanceID = freeInstance(g, vm)
	}
	if vm.FlavorID == "" {
		if fl := f.flavorFor(h, g, vm); fl != nil {
			vm.FlavorID = fl.ID
		}
	}
	if fl := f.flavorByID(vm.FlavorID); fl != nil {
		if vm.VCPUs == 0 {
			vm.VCPUs = fl.VCPUs
		}
		if vm.MemoryMB == 0 {
			vm.MemoryMB = fl.RAMMB
		}
		if vm.DiskGB == 0 {
			vm.DiskGB = fl.DiskGB
		}
	}
}

// flavorByID is FlavorByID that skips null entries, which Validate reports separately.
func (f *filler) flavorByID(id string) *Flavor {
	for _, fl := range f.t.Flavors {
		if fl != nil && fl.ID == id {
			return fl
		}
	}
	return nil
}

// freeInstance returns the GI ID of the first instance on g no other VM uses, or -1 when every one is taken.
func freeInstance(g *GPU, self *VM) int {
	used := map[int]bool{}
	for _, o := range g.VMs {
		if o != nil && o != self && o.MIGGPUInstanceID >= 0 {
			used[o.MIGGPUInstanceID] = true
		}
	}
	for _, mi := range g.MIGInstances {
		if mi != nil && mi.GPUInstanceID >= 0 && !used[mi.GPUInstanceID] {
			return mi.GPUInstanceID
		}
	}
	return -1
}

// flavorFor picks the flavor the VM should use and adds it to the topology when it is missing.
// VGPU VMs get the flavor of the GPU's vGPU type, MIG VMs the flavor of their instance kind,
// passthrough VMs the flavor of the GPU model. Names the catalog does not know get a derived flavor.
func (f *filler) flavorFor(h *Hypervisor, g *GPU, vm *VM) *Flavor {
	var name string
	var derive func() *Flavor
	switch h.Mode {
	case ModeVGPU:
		if g.Profile == nil || g.Profile.Name == "" {
			return nil
		}
		p := g.Profile.Name
		name = f.cat.vgpuFlavor(p)
		if name == "" {
			name = vgpuFlavorName(p)
		}
		derive = func() *Flavor {
			return derivedFlavor(name, 8, 32768, 100, map[string]string{
				"resources:VGPU": "1", "trait:" + traitName(p): "required",
			})
		}
	case ModeMIG:
		var mi *MIGInstance
		for _, x := range g.MIGInstances {
			if x != nil && x.GPUInstanceID == vm.MIGGPUInstanceID {
				mi = x
			}
		}
		if mi == nil || mi.Profile == "" {
			return nil
		}
		if cm := f.cat.mig(g.ModelName, mi.Profile); cm != nil {
			name = cm.Flavor
		} else {
			name = migFlavorName(mi.Profile)
		}
		s := max(mi.Slices, 1)
		derive = func() *Flavor {
			return derivedFlavor(name, 8*s, 32768*s, 100*s, map[string]string{
				"resources:VGPU": "1", "trait:" + traitName("MIG "+mi.Profile): "required",
			})
		}
	case ModePassthrough:
		if g.ModelName == "" {
			return nil
		}
		if m := f.cat.model(g.ModelName); m != nil {
			name = m.PassthroughFlavor
		} else {
			name = passthroughFlavorName(g.ModelName)
		}
		alias := strings.TrimPrefix(name, "emu-pt-")
		derive = func() *Flavor {
			return derivedFlavor(name, 32, 131072, 500, map[string]string{"pci_passthrough:alias": alias + ":1"})
		}
	default:
		return nil
	}
	for _, fl := range f.t.Flavors {
		if fl != nil && fl.Name == name {
			return fl
		}
	}
	var fl *Flavor
	for _, d := range f.cat.Flavors {
		if d.Name == name {
			fl = d
		}
	}
	if fl == nil {
		fl = derive()
	}
	f.t.Flavors = append(f.t.Flavors, fl)
	return fl
}
