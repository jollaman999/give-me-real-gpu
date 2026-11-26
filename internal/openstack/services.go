package openstack

import (
	"net/http"
	"regexp"
	"strings"
)

// ---- placement ----

func (s *server) placementRoutes() {
	m := s.mux
	m.HandleFunc("GET /placement", s.placementVersions)
	m.HandleFunc("GET /placement/{$}", s.placementVersions)
	m.HandleFunc("GET /placement/resource_providers", s.listResourceProviders)
	m.HandleFunc("GET /placement/resource_providers/{uuid}", s.getResourceProvider)
	m.HandleFunc("GET /placement/resource_providers/{uuid}/inventories", s.providerInventories)
	m.HandleFunc("GET /placement/resource_providers/{uuid}/usages", s.providerUsages)
}

func (s *server) placementVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"versions": []obj{{
		"id": "v1.0", "status": "CURRENT", "min_version": "1.0", "max_version": "1.39",
		"links": []obj{{"rel": "self", "href": ""}},
	}}})
}

func (s *server) resourceProviderJSON(uuid, name string) obj {
	self := "/placement/resource_providers/" + uuid
	return obj{
		"uuid":                 uuid,
		"name":                 name,
		"generation":           3,
		"parent_provider_uuid": nil,
		"root_provider_uuid":   uuid,
		"links": []obj{
			{"rel": "self", "href": self},
			{"rel": "inventories", "href": self + "/inventories"},
			{"rel": "usages", "href": self + "/usages"},
		},
	}
}

// listResourceProviders returns one compute node provider per hypervisor,
// named after the hostname with the hypervisor uuid, like nova creates them.
func (s *server) listResourceProviders(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	var out []obj
	for _, h := range s.topo.Hypervisors {
		if name == "" || name == h.Hostname {
			out = append(out, s.resourceProviderJSON(h.UUID, h.Hostname))
		}
	}
	writeJSON(w, http.StatusOK, obj{"resource_providers": list(out)})
}

func (s *server) providerOr404(w http.ResponseWriter, r *http.Request) (int, bool) {
	for i, h := range s.topo.Hypervisors {
		if h.UUID == r.PathValue("uuid") {
			return i, true
		}
	}
	writeJSON(w, http.StatusNotFound, obj{"errors": []obj{{
		"status": 404, "title": "Not Found", "code": "placement.undefined_code",
		"detail": "No resource provider with uuid " + r.PathValue("uuid") + " found",
	}}})
	return -1, false
}

func (s *server) getResourceProvider(w http.ResponseWriter, r *http.Request) {
	if i, ok := s.providerOr404(w, r); ok {
		h := s.topo.Hypervisors[i]
		writeJSON(w, http.StatusOK, s.resourceProviderJSON(h.UUID, h.Hostname))
	}
}

func (s *server) providerInventories(w http.ResponseWriter, r *http.Request) {
	i, ok := s.providerOr404(w, r)
	if !ok {
		return
	}
	h := s.topo.Hypervisors[i]
	inv := func(total, reserved int, ratio float64) obj {
		return obj{"total": total, "reserved": reserved, "min_unit": 1, "max_unit": total,
			"step_size": 1, "allocation_ratio": ratio}
	}
	writeJSON(w, http.StatusOK, obj{
		"resource_provider_generation": 3,
		"inventories": obj{
			"VCPU":      inv(h.VCPUs, 0, 4.0),
			"MEMORY_MB": inv(h.MemoryMB, reservedHostMemoryMB, 1.0),
			"DISK_GB":   inv(h.LocalGB, 0, 1.0),
		},
	})
}

func (s *server) providerUsages(w http.ResponseWriter, r *http.Request) {
	i, ok := s.providerOr404(w, r)
	if !ok {
		return
	}
	u := usageOf(s.topo.Hypervisors[i])
	writeJSON(w, http.StatusOK, obj{
		"resource_provider_generation": 3,
		"usages":                       obj{"VCPU": u.vcpus, "MEMORY_MB": u.memMB - reservedHostMemoryMB, "DISK_GB": u.diskGB},
	})
}

// ---- glance ----

func (s *server) glanceRoutes() {
	m := s.mux
	m.HandleFunc("GET /image", s.glanceVersions)
	m.HandleFunc("GET /image/{$}", s.glanceVersions)
	m.HandleFunc("GET /image/v2/images", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, obj{"images": []obj{}, "schema": "/v2/schemas/images", "first": "/v2/images"})
	})
	m.HandleFunc("GET /image/v2/images/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "itemNotFound", "No image found with ID "+r.PathValue("id"))
	})
	m.HandleFunc("GET /image/v2/metadefs/namespaces", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, obj{"namespaces": []obj{}, "schema": "/v2/schemas/metadefs/namespaces", "first": "/v2/metadefs/namespaces"})
	})
	m.HandleFunc("GET /image/v2/metadefs/resource_types", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, obj{"resource_types": []obj{}})
	})
}

func (s *server) glanceVersions(w http.ResponseWriter, r *http.Request) {
	var out []obj
	for i, v := range []string{"v2.16", "v2.15", "v2.9", "v2.0"} {
		status := "SUPPORTED"
		if i == 0 {
			status = "CURRENT"
		}
		out = append(out, obj{"id": v, "status": status,
			"links": []obj{{"rel": "self", "href": requestBase(r) + "/image/v2/"}}})
	}
	writeJSON(w, http.StatusOK, obj{"versions": out})
}

// ---- cinder ----

func (s *server) cinderRoutes() {
	s.mux.HandleFunc("/volume", s.cinder)
	s.mux.HandleFunc("/volume/", s.cinder)
}

var (
	versionSeg = regexp.MustCompile(`^v[0-9]+(\.[0-9]+)?$`)
	hex32Seg   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// cinderEmptyLists maps a cinder collection path to its JSON key.
var cinderEmptyLists = map[string]string{
	"volumes": "volumes", "volumes/detail": "volumes",
	"snapshots": "snapshots", "snapshots/detail": "snapshots",
	"backups": "backups", "backups/detail": "backups",
	"groups": "groups", "groups/detail": "groups",
	"group_snapshots": "group_snapshots", "group_snapshots/detail": "group_snapshots",
	"consistencygroups": "consistencygroups", "consistencygroups/detail": "consistencygroups",
	"cgsnapshots": "cgsnapshots", "cgsnapshots/detail": "cgsnapshots",
	"group_types": "group_types", "messages": "messages", "qos-specs": "qos_specs",
	"os-volume-transfer": "transfers", "os-volume-transfer/detail": "transfers",
	"volume-transfers": "transfers", "volume-transfers/detail": "transfers",
	"attachments": "attachments", "attachments/detail": "attachments",
	"scheduler-stats/get_pools": "pools", "os-hosts": "hosts",
}

// cinder routes every /volume path by hand. The base URL in the catalog
// already holds /v3/<project>, and some clients prepend /v3/<project> again,
// so leading version and project id segments are skipped before dispatch.
func (s *server) cinder(w http.ResponseWriter, r *http.Request) {
	segs := strings.FieldsFunc(strings.TrimPrefix(r.URL.Path, "/volume"), func(c rune) bool { return c == '/' })
	i, sawProject := 0, false
	for i < len(segs) && (versionSeg.MatchString(segs[i]) || hex32Seg.MatchString(segs[i]) || segs[i] == s.topo.ProjectID) {
		if !versionSeg.MatchString(segs[i]) {
			sawProject = true
		}
		i++
	}
	rest := segs[i:]
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.fallback(w, r)
		return
	}
	if len(rest) == 0 && !sawProject {
		s.cinderVersions(w, r)
		return
	}
	path := strings.Join(rest, "/")
	if key, ok := cinderEmptyLists[path]; ok {
		writeJSON(w, http.StatusOK, obj{key: []obj{}})
		return
	}
	switch {
	case path == "types":
		writeJSON(w, http.StatusOK, obj{"volume_types": []obj{s.defaultVolumeType()}})
	case len(rest) == 2 && rest[0] == "types":
		if rest[1] != s.defaultVolumeType()["id"] {
			writeError(w, http.StatusNotFound, "itemNotFound", "Volume type "+rest[1]+" could not be found.")
			return
		}
		writeJSON(w, http.StatusOK, obj{"volume_type": s.defaultVolumeType()})
	case len(rest) == 3 && rest[0] == "types" && rest[2] == "extra_specs":
		writeJSON(w, http.StatusOK, obj{"extra_specs": obj{}})
	case path == "os-services":
		s.cinderServices(w)
	case path == "limits":
		s.cinderLimits(w)
	case len(rest) >= 2 && rest[0] == "os-quota-sets":
		s.cinderQuota(w, r, rest[1])
	case path == "os-availability-zone":
		writeJSON(w, http.StatusOK, obj{"availabilityZoneInfo": []obj{{"zoneName": "nova", "zoneState": obj{"available": true}}}})
	default:
		notFound(w, r)
	}
}

func (s *server) cinderVersions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, obj{"versions": []obj{{
		"id": "v3.0", "status": "CURRENT", "version": "3.71", "min_version": "3.0",
		"updated": "2023-08-31T00:00:00Z",
		"links":   []obj{{"rel": "self", "href": requestBase(r) + "/volume/v3/"}},
		"media-types": []obj{{"base": "application/json",
			"type": "application/vnd.openstack.volume+json;version=3"}},
	}}})
}

func (s *server) defaultVolumeType() obj {
	return obj{
		"id":                              stableUUID("volume-type", "__DEFAULT__"),
		"name":                            "__DEFAULT__",
		"description":                     "Default Volume Type",
		"is_public":                       true,
		"os-volume-type-access:is_public": true,
		"extra_specs":                     obj{},
		"qos_specs_id":                    nil,
	}
}

func (s *server) cinderServices(w http.ResponseWriter) {
	updated := fmtTime(s.now(), layoutMicro)
	svc := func(binary, host string) obj {
		return obj{"binary": binary, "host": host, "zone": "nova", "status": "enabled",
			"state": "up", "updated_at": updated, "disabled_reason": nil}
	}
	writeJSON(w, http.StatusOK, obj{"services": []obj{
		svc("cinder-scheduler", controllerHost),
		svc("cinder-volume", controllerHost+"@lvm"),
	}})
}

func (s *server) cinderLimits(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, obj{"limits": obj{"rate": []obj{}, "absolute": obj{
		"maxTotalVolumes": 100, "maxTotalSnapshots": 100, "maxTotalVolumeGigabytes": 10000,
		"maxTotalBackups": 10, "maxTotalBackupGigabytes": 1000,
		"totalVolumesUsed": 0, "totalGigabytesUsed": 0, "totalSnapshotsUsed": 0,
		"totalBackupsUsed": 0, "totalBackupGigabytesUsed": 0,
	}}})
}

var cinderQuotaLimits = []struct {
	key   string
	limit int
}{
	{"volumes", 100}, {"snapshots", 100}, {"gigabytes", 10000}, {"backups", 10},
	{"backup_gigabytes", 1000}, {"per_volume_gigabytes", -1}, {"groups", 10},
}

func (s *server) cinderQuota(w http.ResponseWriter, r *http.Request, project string) {
	usage := r.URL.Query().Get("usage") == "true"
	q := obj{"id": project}
	for _, l := range cinderQuotaLimits {
		if usage {
			q[l.key] = obj{"limit": l.limit, "in_use": 0, "reserved": 0, "allocated": 0}
		} else {
			q[l.key] = l.limit
		}
	}
	writeJSON(w, http.StatusOK, obj{"quota_set": q})
}

// ---- octavia ----

var octaviaLists = map[string]string{
	"lbaas/loadbalancers": "loadbalancers", "lbaas/listeners": "listeners",
	"lbaas/pools": "pools", "lbaas/healthmonitors": "healthmonitors",
	"lbaas/l7policies": "l7policies", "lbaas/flavors": "flavors",
	"lbaas/flavorprofiles": "flavorprofiles", "lbaas/availabilityzones": "availability_zones",
	"lbaas/availabilityzoneprofiles": "availability_zone_profiles",
	"lbaas/quotas":                   "quotas", "lbaas/providers": "providers", "octavia/amphorae": "amphorae",
}

func (s *server) octaviaRoutes() {
	m := s.mux
	m.HandleFunc("GET /load-balancer", s.octaviaVersions)
	m.HandleFunc("GET /load-balancer/{$}", s.octaviaVersions)
	m.HandleFunc("GET /load-balancer/{ver}/{rest...}", func(w http.ResponseWriter, r *http.Request) {
		ver := r.PathValue("ver")
		if ver != "v2" && ver != "v2.0" {
			notFound(w, r)
			return
		}
		rest := strings.TrimSuffix(r.PathValue("rest"), "/")
		if key, ok := octaviaLists[rest]; ok {
			writeJSON(w, http.StatusOK, obj{key: []obj{}, key + "_links": []obj{}})
			return
		}
		writeJSON(w, http.StatusNotFound, obj{"faultcode": "Client", "faultstring": "Resource not found: " + rest, "debuginfo": nil})
	})
}

func (s *server) octaviaVersions(w http.ResponseWriter, r *http.Request) {
	var out []obj
	for _, v := range []struct{ id, status, updated string }{
		{"v2.0", "SUPPORTED", "2016-12-11T00:00:00Z"},
		{"v2.27", "CURRENT", "2023-05-17T00:00:00Z"},
	} {
		out = append(out, obj{"id": v.id, "status": v.status, "updated": v.updated,
			"links": []obj{{"rel": "self", "href": requestBase(r) + "/load-balancer/v2"}}})
	}
	writeJSON(w, http.StatusOK, obj{"versions": out})
}
