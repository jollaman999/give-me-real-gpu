package editor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
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
out.gpuOnMIG = h.acceptGPU(topo, cat, 2, "NVIDIA A10").mode;
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

// runTopologyJS runs script in node with topology.js, the default catalog and the default topology as arguments
// and decodes the JSON it prints.
func runTopologyJS(t *testing.T, script string) map[string]any {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src, err := filepath.Abs("../web/static/topology.js")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "check.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cat, _ := json.Marshal(topology.DefaultCatalog())
	topo, _ := json.Marshal(topology.Default())
	raw, err := exec.Command(node, file, src, string(cat), string(topo)).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, raw)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return got
}

func TestTopologyJSHelpers(t *testing.T) {
	got := runTopologyJS(t, jsScript)
	def := topology.Default()
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
		"gpuOnMIG":       "VGPU",
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

// jsModeScript checks the per-GPU type helpers: type changes that keep and fit the VMs, slice counts per model,
// GPUs of any known model on any node and the type tags of a node.
const jsModeScript = `
const h = require(process.argv[2]);
const cat = JSON.parse(process.argv[3]);
const topo = JSON.parse(process.argv[4]);
const copy = (x) => JSON.parse(JSON.stringify(x));
const out = {};
const a10 = cat.gpu_models.find(m => m.name === "NVIDIA A10");
const a100 = cat.gpu_models.find(m => m.name === "NVIDIA A100-SXM4-80GB");
const vp = (name) => ({name: name, type: "nvidia-1", framebuffer_mb: 1024, max_instances: 1});
// A model cut into four MIG slices, as RTX PRO 6000 Blackwell: 4g once, 2g twice, 1g four times.
const four = {name: "Test 4-slice GPU", memory_mib: 97887, modes: ["VGPU", "MIG", "PASSTHROUGH"], max_mig_slices: 4,
  vgpu_profiles: [], mig_profiles: [
    {name: "4g.96gb", slices: 4, memory_mib: 95000, gpu_instance_ids: [0], vgpu_profile: vp("x-4")},
    {name: "2g.48gb", slices: 2, memory_mib: 47744, gpu_instance_ids: [1, 2], vgpu_profile: vp("x-2")},
    {name: "1g.24gb", slices: 1, memory_mib: 23000, gpu_instance_ids: [3, 4, 5, 6], vgpu_profile: vp("x-1")}]};
cat.gpu_models.push(four);
const vms = (node, n, mode) => Array.from({length: n}, (_, i) => ({name: "vm" + (i + 1), hypervisor: node, gpu_index: 0,
  mode: mode, mig_gpu_instance_id: -1, guest_driver_version: "550", vgpu_uuid: "u" + i, flavor_id: "f"}));
const view = (t, hi, gi, r) => {
  const n = t.hypervisors[hi], g = n.gpus[gi];
  return {
    changed: r.changed, removed: r.removed, mode: g.mode, eff: h.gpuMode(n, g),
    profile: g.profile ? g.profile.name : null,
    mig: g.mig_instances === undefined ? null : g.mig_instances.map(mi => mi.profile + "#" + mi.gpu_instance_id),
    vms: g.vms.map(v => [v.name, v.mode, v.mig_gpu_instance_id, v.guest_driver_version, v.vgpu_uuid, v.flavor_id].join(" ")),
  };
};

// vGPU GPU with six VMs to passthrough: one VM stays, five go.
const t1 = copy(topo);
out.vgpuToPT = view(t1, 0, 0, h.convertGPU(t1, cat, 0, 0, "PASSTHROUGH"));
// And back to vGPU: the VM stays and the GPU gets the model's first vGPU profile.
out.ptToVGPU = view(t1, 0, 0, h.convertGPU(t1, cat, 0, 0, "VGPU"));
out.same = h.convertGPU(t1, cat, 0, 0, "VGPU").changed;

// vGPU GPU with three VMs on a seven-slice model to MIG: one 1g instance per VM.
const t2 = {flavors: [], hypervisors: [{hostname: "n1", mode: "VGPU", gpus: [
  {index: 0, model_name: a100.name, profile: vp("p"), vms: vms("n1", 3, "VGPU")}]}]};
t2.hypervisors[0].gpus[0].profile.max_instances = 4;
out.vgpuToMIG = view(t2, 0, 0, h.convertGPU(t2, cat, 0, 0, "MIG"));
out.mixedTags = h.nodeModes(t2.hypervisors[0]);

// Five VMs on a four-slice model to MIG: four get a 1g instance, the fifth is deleted.
const t3 = {flavors: [], hypervisors: [{hostname: "n2", mode: "PASSTHROUGH", gpus: [
  {index: 0, model_name: four.name, mode: "VGPU", profile: vp("q"), vms: vms("n2", 5, "VGPU")}]}]};
t3.hypervisors[0].gpus[0].profile.max_instances = 8;
out.fourToMIG = view(t3, 0, 0, h.convertGPU(t3, cat, 0, 0, "MIG"));
out.fourFull = (() => { const r = h.acceptVM(t3, cat, {h: 0, g: 0, free: true}, {kind: "new-vm"}); return r.reason + " " + r.params.used + "/" + r.params.max; })();

// Slices per model.
const fg = {model_name: four.name, mig_instances: []};
fg.mig_instances.push(h.newInstance(fg, four.mig_profiles[1]));
const ag = {model_name: a100.name, mig_instances: []};
ag.mig_instances.push(h.newInstance(ag, a100.mig_profiles.find(p => p.name === "3g.40gb")));
out.fitFour = h.fittingProfiles(cat, fg).map(p => p.name);
out.fitA100 = h.fittingProfiles(cat, ag).map(p => p.name);
out.slices = [h.maxSlices(cat, fg), h.maxSlices(cat, ag), h.maxSlices(cat, {model_name: "unknown"}), h.maxSlices({gpu_models: []}, fg)];

// Any known model goes on any node, in the node's default type when the model has it.
const ag2 = (hi, model) => { const r = h.acceptGPU(topo, cat, hi, model); return r.ok ? r.mode : r.reason; };
out.acceptGPU = [ag2(2, a10.name), ag2(0, a100.name), ag2(4, a10.name), ag2(0, a10.name), ag2(0, "no such model")];
const ng = (hi, model) => { const g = h.newGPU(topo, cat, hi, model); return [g.index, g.mode, g.profile ? g.profile.name : null, g.mig_instances ? g.mig_instances.length : null]; };
out.newGPU = [ng(2, a10.name), ng(0, a100.name)];

// acceptVM goes by the GPU's own type on a node of another default type.
const t4 = copy(topo);
h.convertGPU(t4, cat, 0, 3, "PASSTHROUGH");
const pt = () => { const r = h.acceptVM(t4, cat, {h: 0, g: 3}, {kind: "new-vm"}); return r.ok ? "ok" : r.reason; };
out.ptOnVGPUNode = [pt()];
t4.hypervisors[0].gpus[3].vms.push({name: "x", mode: "PASSTHROUGH", mig_gpu_instance_id: -1});
out.ptOnVGPUNode.push(pt());

// vGPU type per MIG instance. A kind from an older catalog without vgpu_profiles offers its vgpu_profile only.
const old = copy(a100.mig_profiles.find(p => p.name === "1g.10gb"));
delete old.vgpu_profiles;
out.oldKind = h.migVGPUs(old).map(p => p.name);
const og = {model_name: a100.name, mig_instances: []};
og.mig_instances.push(h.newInstance(og, old));
out.oldInst = og.mig_instances[0].vgpu_profile.name === old.vgpu_profile.name;
const oldCat = copy(cat);
oldCat.gpu_models.forEach(m => m.mig_profiles.forEach(p => delete p.vgpu_profiles));
out.oldSet = [h.setInstanceVGPU(oldCat, og, og.mig_instances[0].gpu_instance_id, old.vgpu_profile.name).changed,
  h.setInstanceVGPU(oldCat, og, og.mig_instances[0].gpu_instance_id, "other").changed];
// A kind with two types: a new instance takes the first, and the second can be picked without touching the VM.
const k2 = four.mig_profiles[1];
k2.vgpu_profiles = [vp("x-2-48Q"), vp("x-2-48C")];
delete k2.vgpu_profile;
const mg = {model_name: four.name, mig_instances: [], vms: [{name: "m1", mode: "MIG", flavor_id: "f2", vcpus: 8}]};
mg.mig_instances.push(h.newInstance(mg, k2));
mg.vms[0].mig_gpu_instance_id = mg.mig_instances[0].gpu_instance_id;
const before = mg.mig_instances[0].vgpu_profile.name;
out.pickVGPU = [before, h.setInstanceVGPU(cat, mg, 1, "x-2-48C").changed, mg.mig_instances[0].vgpu_profile.name,
  h.setInstanceVGPU(cat, mg, 1, "nope").changed, mg.mig_instances[0].vgpu_profile.name, h.setInstanceVGPU(cat, mg, 9, "x-2-48Q").changed,
  mg.vms[0].flavor_id + " " + mg.vms[0].vcpus];
out.pickCopy = mg.mig_instances[0].vgpu_profile !== k2.vgpu_profiles[1];

out.nodeModes = [
  h.nodeModes({mode: "PASSTHROUGH", gpus: [{}, {mode: "MIG"}, {mode: "VGPU"}]}),
  h.nodeModes({mode: "MIG", gpus: []}),
  h.nodeModes({mode: "VGPU", gpus: [{mode: "PASSTHROUGH"}, {mode: "PASSTHROUGH"}]}),
  h.nodeModes(topo.hypervisors[0]),
];
console.log(JSON.stringify(out));
`

func TestTopologyJSModes(t *testing.T) {
	got := runTopologyJS(t, jsModeScript)
	def := topology.Default()
	cat := topology.DefaultCatalog()
	var a10First string
	var a100GI []int
	for _, m := range cat.GPUModels {
		if m.Name == "NVIDIA A10" {
			a10First = m.VGPUProfiles[0].Name
		}
		if m.Name == "NVIDIA A100-SXM4-80GB" {
			for _, p := range m.MIGProfiles {
				if p.Name == "1g.10gb" {
					a100GI = p.GPUInstanceIDs
				}
			}
		}
	}
	if a10First == "" || len(a100GI) < 3 {
		t.Fatalf("catalog lacks the A10 vGPU profile or the A100 1g kind: %q %v", a10First, a100GI)
	}
	g0 := def.Hypervisors[0].GPUs[0]
	var gone []any
	for _, v := range g0.VMs[1:] {
		gone = append(gone, v.Name)
	}
	keep := g0.VMs[0].Name
	gi := func(i int) string { return strconv.Itoa(a100GI[i]) }
	want := map[string]any{
		"vgpuToPT": map[string]any{
			"changed": true, "removed": gone, "mode": "PASSTHROUGH", "eff": "PASSTHROUGH", "profile": nil, "mig": nil,
			"vms": []any{keep + " PASSTHROUGH -1   "},
		},
		"ptToVGPU": map[string]any{
			"changed": true, "removed": []any{}, "mode": "VGPU", "eff": "VGPU", "profile": a10First, "mig": nil,
			"vms": []any{keep + " VGPU -1   "},
		},
		"same": false,
		"vgpuToMIG": map[string]any{
			"changed": true, "removed": []any{}, "mode": "MIG", "eff": "MIG", "profile": nil,
			"mig": []any{"1g.10gb#" + gi(0), "1g.10gb#" + gi(1), "1g.10gb#" + gi(2)},
			"vms": []any{"vm1 MIG " + gi(0) + " 550 u0 ", "vm2 MIG " + gi(1) + " 550 u1 ", "vm3 MIG " + gi(2) + " 550 u2 "},
		},
		"mixedTags": []any{"MIG"},
		"fourToMIG": map[string]any{
			"changed": true, "removed": []any{"vm5"}, "mode": "MIG", "eff": "MIG", "profile": nil,
			"mig": []any{"1g.24gb#3", "1g.24gb#4", "1g.24gb#5", "1g.24gb#6"},
			"vms": []any{"vm1 MIG 3 550 u0 ", "vm2 MIG 4 550 u1 ", "vm3 MIG 5 550 u2 ", "vm4 MIG 6 550 u3 "},
		},
		"fourFull":     "no_slices 4/4",
		"fitFour":      []any{"1g.24gb", "2g.48gb"},
		"fitA100":      []any{"1g.10gb", "2g.20gb", "3g.40gb"},
		"slices":       []any{4.0, 7.0, 7.0, 7.0},
		"acceptGPU":    []any{"VGPU", "MIG", "PASSTHROUGH", "VGPU", "unknown"},
		"newGPU":       []any{[]any{4.0, "VGPU", a10First, nil}, []any{4.0, "MIG", nil, 0.0}},
		"ptOnVGPUNode": []any{"ok", "full_pt"},
		"oldKind":      []any{"NVIDIA A100D-1-10C"},
		"oldInst":      true,
		"oldSet":       []any{true, false},
		"pickVGPU":     []any{"x-2-48Q", true, "x-2-48C", false, "x-2-48C", false, "f2 8"},
		"pickCopy":     true,
		"nodeModes": []any{
			[]any{"VGPU", "MIG", "PASSTHROUGH"},
			[]any{"MIG"},
			[]any{"PASSTHROUGH"},
			[]any{"VGPU"},
		},
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
}

// jsSharedScript checks MIG instances that several VMs share in time slices: seats per instance, drops on a shared
// instance, the instance a drop on the GPU picks, VMs deleted when the vGPU type seats fewer and the instances a VM
// may move to in its properties.
const jsSharedScript = `
const h = require(process.argv[2]);
const cat = JSON.parse(process.argv[3]);
const out = {};
const vp = (name, per) => { const p = {name: name, type: "nvidia-1", framebuffer_mb: 1024, max_instances: 4}; if (per) p.per_gpu_instance = per; return p; };
const four = {name: "Test 4-slice GPU", memory_mib: 97887, modes: ["MIG"], max_mig_slices: 4, vgpu_profiles: [], mig_profiles: [
  {name: "2g.48gb", slices: 2, memory_mib: 47744, gpu_instance_ids: [1, 2],
   vgpu_profiles: [vp("x-2-48Q"), vp("x-2-48C"), vp("x-2-24C", 2), vp("x-2-12C", 4)]},
  {name: "1g.24gb", slices: 1, memory_mib: 23000, gpu_instance_ids: [3, 4, 5, 6], vgpu_profiles: [vp("x-1-24Q"), vp("x-1-8C", 3)]}]};
cat.gpu_models.push(four);

out.seats = [h.instanceSeats(null), h.instanceSeats({}), h.instanceSeats({vgpu_profile: vp("a")}), h.instanceSeats({vgpu_profile: vp("b", 4)})];

const g = {index: 0, model_name: four.name, mode: "MIG", mig_instances: [], vms: []};
const t = {flavors: [], hypervisors: [{hostname: "n1", mode: "MIG", gpus: [g]}]};
g.mig_instances.push(h.newInstance(g, four.mig_profiles[0]));
g.mig_instances.push(h.newInstance(g, four.mig_profiles[0]));
out.newDefault = g.mig_instances.map(mi => mi.vgpu_profile.name + " " + h.instanceSeats(mi));
out.set12C = h.setInstanceVGPU(cat, g, 1, "x-2-12C");
out.copied = g.mig_instances[0].vgpu_profile.per_gpu_instance;
let n = 0;
const show = (r) => r.ok ? (r.gi !== undefined ? "gi:" + r.gi : "create:" + r.create.name) : r.reason + (r.params ? " " + JSON.stringify(r.params) : "");
const put = (target) => {
  const r = h.acceptVM(t, cat, Object.assign({h: 0, g: 0}, target), {kind: "new-vm"});
  if (r.ok && r.gi !== undefined) g.vms.push({name: "vm" + (++n), mode: "MIG", mig_gpu_instance_id: r.gi});
  return show(r);
};
// Four VMs on the shared instance, the fifth is refused with the seat count. GI 2 seats one VM and names it when taken.
out.shared = [put({slot: 1}), put({slot: 1}), put({slot: 2}), put({slot: 1}), put({slot: 1}), put({slot: 1}), put({slot: 2})];
// A VM already on GI 1 dropped on GI 1 again is "same"; vm3 from GI 2 onto the full GI 1 is refused.
const idx = (name) => g.vms.findIndex(v => v.name === name);
out.move = [show(h.acceptVM(t, cat, {h: 0, g: 0, slot: 1}, {kind: "vm", h: 0, g: 0, v: idx("vm1")})),
  show(h.acceptVM(t, cat, {h: 0, g: 0, slot: 1}, {kind: "vm", h: 0, g: 0, v: idx("vm3")}))];

// A drop on the GPU picks an empty instance before a shared one with room, and a new instance last.
const g2 = {index: 0, model_name: four.name, mode: "MIG", mig_instances: [], vms: []};
const t2 = {flavors: [], hypervisors: [{hostname: "n2", mode: "MIG", gpus: [g2]}]};
g2.mig_instances.push(h.newInstance(g2, four.mig_profiles[0]));
h.setInstanceVGPU(cat, g2, 1, "x-2-24C");
g2.vms.push({name: "a", mode: "MIG", mig_gpu_instance_id: 1});
g2.mig_instances.push(h.newInstance(g2, four.mig_profiles[1]));
const onGPU = () => show(h.acceptVM(t2, cat, {h: 0, g: 0}, {kind: "new-vm"}));
out.pick = [onGPU()];
g2.vms.push({name: "b", mode: "MIG", mig_gpu_instance_id: 3});
out.pick.push(onGPU());
g2.vms.push({name: "c", mode: "MIG", mig_gpu_instance_id: 1});
out.pick.push(onGPU());
g2.mig_instances.push(h.newInstance(g2, four.mig_profiles[1]));
g2.vms.push({name: "d", mode: "MIG", mig_gpu_instance_id: 4});
out.pick.push(onGPU());

// The VM properties offer the instances with a seat for the VM: its own, and GI 1 only while it has room.
const choices = (gp, name) => h.instanceChoices(gp, gp.vms.find(v => v.name === name)).map(mi => mi.gpu_instance_id);
out.choices = [choices(g, "vm3")];
g.vms.splice(idx("vm2"), 1);
out.choices.push(choices(g, "vm3"), choices(g, "vm1"));

// Fewer seats delete the last VMs of the instance and leave the VMs of other instances alone.
g.vms = [{name: "p1", mig_gpu_instance_id: 1}, {name: "q1", mig_gpu_instance_id: 2}, {name: "p2", mig_gpu_instance_id: 1},
  {name: "p3", mig_gpu_instance_id: 1}, {name: "p4", mig_gpu_instance_id: 1}];
const r24 = h.setInstanceVGPU(cat, g, 1, "x-2-24C");
out.to24C = [r24.changed, r24.removed, g.vms.map(v => v.name), h.instanceSeats(g.mig_instances[0])];
const rQ = h.setInstanceVGPU(cat, g, 1, "x-2-48Q");
out.toQ = [rQ.changed, rQ.removed, g.vms.map(v => v.name), g.mig_instances[0].vgpu_profile.per_gpu_instance === undefined];
const rMore = h.setInstanceVGPU(cat, g, 1, "x-2-12C");
out.toMore = [rMore.changed, rMore.removed, g.vms.map(v => v.name)];
out.bad = h.setInstanceVGPU(cat, g, 1, "nope");
console.log(JSON.stringify(out));
`

func TestTopologyJSSharedInstances(t *testing.T) {
	got := runTopologyJS(t, jsSharedScript)
	want := map[string]any{
		"seats":      []any{1.0, 1.0, 1.0, 4.0},
		"newDefault": []any{"x-2-48Q 1", "x-2-48Q 1"},
		"set12C":     map[string]any{"changed": true, "removed": []any{}},
		"copied":     4.0,
		"shared": []any{"gi:1", "gi:1", "gi:2", "gi:1", "gi:1", `gi_full {"max":4}`,
			`slot_used {"vm":"vm3"}`},
		"move": []any{"same", `gi_full {"max":4}`},
		"pick": []any{"gi:3", "gi:1", "create:1g.24gb", "no_slices {\"used\":4,\"max\":4}"},
		"choices": []any{
			[]any{2.0},
			[]any{1.0, 2.0},
			[]any{1.0},
		},
		"to24C":  []any{true, []any{"p3", "p4"}, []any{"p1", "q1", "p2"}, 2.0},
		"toQ":    []any{true, []any{"p2"}, []any{"p1", "q1"}, true},
		"toMore": []any{true, []any{}, []any{"p1", "q1"}},
		"bad":    map[string]any{"changed": false, "removed": []any{}},
	}
	for k, w := range want {
		if !reflect.DeepEqual(got[k], w) {
			t.Errorf("%s = %#v, want %#v", k, got[k], w)
		}
	}
}
