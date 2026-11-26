package editor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jollaman999/give-me-real-gpu/internal/topology"
)

// jsScript loads the pure helpers of topology.js in node and prints their answers for the cases below as JSON.
const jsScript = `
const h = require(process.argv[2]);
const cat = JSON.parse(process.argv[3]);
const topo = JSON.parse(process.argv[4]);
const out = {};
out.sha1 = h.sha1Hex("abc");
out.uuidLong = h.stableUUID(["server", "x".repeat(100)]);
out.entity = h.entityID("emu-vgpu-01", "");
out.entityKo = h.entityID("노드-1", "");
out.entityOverride = h.entityID("emu-vgpu-01", "mine");
out.counts = h.counts(topo);
out.nextVGPU = h.nextNodeName(topo, "VGPU");
out.nextMIG = h.nextNodeName(topo, "MIG");
const n0 = topo.hypervisors[0];
out.vmName = h.vmName(topo, n0, n0.gpus[3]);
const v = (t, p) => { const r = h.acceptVM(topo, cat, t, p); return r.ok ? (r.create ? "create:" + r.create.name : "ok" + (r.gi !== undefined ? ":" + r.gi : "")) : r.reason; };
out.vgpuFree = v({h: 0, g: 3}, {kind: "new-vm"});
out.vgpuFull = v({h: 0, g: 0}, {kind: "new-vm"});
out.vgpuMoveSame = v({h: 0, g: 1}, {kind: "vm", h: 0, g: 1, v: 0});
out.ptTaken = v({h: 4, g: 0}, {kind: "new-vm"});
out.ptFree = v({h: 4, g: 3}, {kind: "new-vm"});
out.ptMove = v({h: 4, g: 3}, {kind: "vm", h: 4, g: 0, v: 0});
out.migSlotUsed = v({h: 2, g: 0, slot: 7}, {kind: "new-vm"});
out.migSlotFree = v({h: 2, g: 0, slot: 12}, {kind: "new-vm"});
out.migGPU = v({h: 2, g: 0}, {kind: "new-vm"});
out.migFull = v({h: 2, g: 3}, {kind: "new-vm"});
out.migFreeArea = v({h: 3, g: 1, free: true}, {kind: "new-vm"});
out.gpuOnPT = h.acceptGPU(topo, cat, 4, "NVIDIA A10").ok;
out.gpuOnMIG = h.acceptGPU(topo, cat, 2, "NVIDIA A10").reason;
const h100 = cat.gpu_models.find(m => m.name === "NVIDIA H100 80GB HBM3");
const g = {model_name: h100.name, mig_instances: []};
for (const name of ["3g.40gb", "1g.10gb", "1g.10gb"]) g.mig_instances.push(h.newInstance(g, h100.mig_profiles.find(p => p.name === name)));
out.gi = g.mig_instances.map(i => i.gpu_instance_id);
out.slices = h.usedSlices(g);
const pm = h.mapProblems(topo, [
  "hypervisor emu-mig-01 gpu 1: MIG slice total 8 exceeds 7",
  "vm emu-pt01-g0-1: flavor \"x\" not found",
  "duplicate vm name: \"emu-vgpu02-g0-1\"",
  "hypervisors[5]: hostname is empty",
  "project_id is empty",
]);
out.problems = {global: pm.global, nodes: Object.keys(pm.nodes).sort(), gpus: Object.keys(pm.gpus), vms: Object.keys(pm.vms).sort()};
const top0 = topo.hypervisors[0], pt02 = topo.hypervisors[5];
const pm2 = h.mapProblems(topo, [
  "duplicate hypervisor uuid: \"" + top0.uuid + "\"",
  "duplicate hypervisor id: \"3\"",
  "duplicate gpu uuid: \"" + topo.hypervisors[1].gpus[1].uuid + "\"",
  "duplicate gpu bus_id: \"emu-pt-02/" + pt02.gpus[2].bus_id + "\"",
  "duplicate vm id: \"" + topo.hypervisors[2].gpus[1].vms[0].id + "\"",
  "duplicate vm instance_name: \"" + top0.gpus[0].vms[1].instance_name + "\"",
  "duplicate vm vgpu_uuid: \"" + top0.gpus[1].vms[0].vgpu_uuid + "\"",
  "duplicate flavor name: \"emu-pt-a100\"",
  "duplicate gpu uuid: \"GPU-none\"",
]);
out.problems2 = {global: pm2.global, nodes: Object.keys(pm2.nodes).sort(), gpus: Object.keys(pm2.gpus).sort(), vms: Object.keys(pm2.vms).sort()};

// A node made in the editor, renamed to a saved node's name and then to a new name, must not keep the saved node's uuid.
const fromServer = new WeakSet();
let t2 = JSON.parse(process.argv[4]);
h.markTree(t2, fromServer);
const fill = (t) => {
  const n = JSON.parse(JSON.stringify(t));
  h.carryMarks(t, n, fromServer);
  n.hypervisors.forEach((x) => {
    if (!x.uuid) x.uuid = h.stableUUID(["hypervisor", x.hostname]);
    x.gpus.forEach((g) => {
      if (!g.uuid) g.uuid = "GPU-" + h.stableUUID(["gpu", x.hostname, String(g.index)]);
      g.vms.forEach((v) => { if (!v.id) v.id = h.stableUUID(["server", v.name]); });
    });
  });
  return n;
};
t2.hypervisors.push({hostname: "emu-pt-03", mode: "PASSTHROUGH", gpus: [{index: 0, vms: [{name: "emu-pt03-g0-1", hypervisor: "emu-pt-03", mode: "PASSTHROUGH"}]}]});
t2 = fill(t2);
const nn = () => t2.hypervisors[6], nv = () => t2.hypervisors[6].gpus[0].vms[0];
h.renameNode(nn(), "emu-pt-01", fromServer.has(nn())); t2 = fill(t2);
h.renameNode(nn(), "emu-pt-09", fromServer.has(nn())); t2 = fill(t2);
h.renameVM(nv(), "emu-pt01-g0-1", fromServer.has(nv())); t2 = fill(t2);
h.renameVM(nv(), "my-new-vm", fromServer.has(nv())); t2 = fill(t2);
const saved = t2.hypervisors[4];
h.renameNode(saved, "emu-pt-01-renamed", fromServer.has(saved));
h.renameVM(saved.gpus[0].vms[0], "kept-id", fromServer.has(saved.gpus[0].vms[0]));
t2 = fill(t2);
out.rename = {
  newUUID: nn().uuid, newGPU: nn().gpus[0].uuid, vmHyp: nv().hypervisor, vmID: nv().id,
  savedUUID: t2.hypervisors[4].uuid, savedGPU: t2.hypervisors[4].gpus[0].uuid,
  savedVM: t2.hypervisors[4].gpus[0].vms[0].hypervisor + " " + t2.hypervisors[4].gpus[0].vms[0].id,
  uniqueNodes: new Set(t2.hypervisors.map((x) => x.uuid)).size,
};
// JSON view: objects whose uuid or id is in the saved topology count as saved, unless the applied JSON repeats it.
const j = JSON.parse(JSON.stringify(t2));
j.hypervisors.push(JSON.parse(JSON.stringify(j.hypervisors[0])));
const set2 = new WeakSet();
h.markByIDs(j, JSON.parse(process.argv[4]), set2);
out.jsonMarks = [set2.has(j.hypervisors[1]), set2.has(j.hypervisors[0]), set2.has(j.hypervisors[7]), set2.has(j.hypervisors[6]),
  set2.has(j.hypervisors[1].gpus[0].vms[0]), set2.has(j.hypervisors[6].gpus[0].vms[0]), set2.has(j.hypervisors[0].gpus[0].vms[0])];
console.log(JSON.stringify(out));
`

func TestTopologyJSHelpers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := filepath.Abs("../web/static/topology.js")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "check.js")
	if err := os.WriteFile(script, []byte(jsScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, _ := json.Marshal(topology.DefaultCatalog())
	def := topology.Default()
	topo, _ := json.Marshal(def)
	raw, err := exec.Command(node, script, src, string(cat), string(topo)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	long := ""
	for range 100 {
		long += "x"
	}
	want := map[string]any{
		"sha1":           "a9993e364706816aba3e25717850c26c9cd0d89d",
		"uuidLong":       topology.StableUUID("server", long),
		"entity":         topology.EntityID("emu-vgpu-01", ""),
		"entityKo":       topology.EntityID("노드-1", ""),
		"entityOverride": "mine",
		"counts":         map[string]any{"nodes": 6.0, "gpus": 20.0, "vms": 45.0},
		"nextVGPU":       "emu-vgpu-03",
		"nextMIG":        "emu-mig-03",
		"vmName":         "emu-vgpu01-g3-1",
		"vgpuFree":       "ok",
		"vgpuFull":       "full_vgpu",
		"vgpuMoveSame":   "same",
		"ptTaken":        "full_pt",
		"ptFree":         "ok",
		"ptMove":         "ok",
		"migSlotUsed":    "slot_used",
		"migSlotFree":    "ok:12",
		"migGPU":         "ok:12",
		"migFull":        "no_slices",
		"migFreeArea":    "create:1g.10gb",
		"gpuOnPT":        true,
		"gpuOnMIG":       "mode_model",
		"gi":             []any{1.0, 7.0, 8.0},
		"slices":         5.0,
		"problems": map[string]any{
			"global": []any{"project_id is empty"},
			"nodes":  []any{"1", "2", "4", "5"},
			"gpus":   []any{"2/1"},
			"vms":    []any{"1/0/0", "4/0/0"},
		},
		"problems2": map[string]any{
			"global": []any{`duplicate flavor name: "emu-pt-a100"`, `duplicate gpu uuid: "GPU-none"`},
			"nodes":  []any{"0", "1", "2", "5"},
			"gpus":   []any{"1/1", "5/2"},
			"vms":    []any{"0/0/1", "0/1/0", "2/1/0"},
		},
		"rename": map[string]any{
			"newUUID":     topology.StableUUID("hypervisor", "emu-pt-09"),
			"newGPU":      "GPU-" + topology.StableUUID("gpu", "emu-pt-09", "0"),
			"vmHyp":       "emu-pt-09",
			"vmID":        topology.StableUUID("server", "my-new-vm"),
			"savedUUID":   def.Hypervisors[4].UUID,
			"savedGPU":    def.Hypervisors[4].GPUs[0].UUID,
			"savedVM":     "emu-pt-01-renamed " + def.Hypervisors[4].GPUs[0].VMs[0].ID,
			"uniqueNodes": 7.0,
		},
		"jsonMarks": []any{true, false, false, false, true, false, false},
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
}
