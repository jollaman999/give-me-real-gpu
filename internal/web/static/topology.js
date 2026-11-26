// Topology editor for the settings UI. Plain JavaScript, no build step, no libraries.
// The pure helpers at the top are also loaded by node in internal/editor/js_test.go. The DOM part only runs in a browser.
// app.js calls window.GMRG_TOPOLOGY.mount(el) the first time the Topology tab opens.
"use strict";

(function (root) {
  var MODES = ["VGPU", "MIG", "PASSTHROUGH"];
  var MODE_PREFIX = { VGPU: "emu-vgpu-", MIG: "emu-mig-", PASSTHROUGH: "emu-pt-" };
  var MAX_SLICES = 7;

  // ---- pure helpers ----

  function arr(x) { return Array.isArray(x) ? x : []; }

  function format(template, params) {
    if (!params) return template;
    return template.replace(/\{(\w+)\}/g, function (m, k) {
      return Object.prototype.hasOwnProperty.call(params, k) ? String(params[k]) : m;
    });
  }

  function utf8(s) {
    return new TextEncoder().encode(s);
  }

  // sha1 returns the 20-byte SHA-1 of a byte array.
  function sha1(bytes) {
    var ml = bytes.length;
    var total = ((ml + 9 + 63) >> 6) << 6;
    var b = new Uint8Array(total);
    b.set(bytes);
    b[ml] = 0x80;
    var bits = ml * 8;
    var hi = Math.floor(bits / 0x100000000);
    var lo = bits >>> 0;
    for (var i = 0; i < 4; i++) {
      b[total - 8 + i] = (hi >>> (24 - 8 * i)) & 0xff;
      b[total - 4 + i] = (lo >>> (24 - 8 * i)) & 0xff;
    }
    var h0 = 0x67452301, h1 = 0xefcdab89, h2 = 0x98badcfe, h3 = 0x10325476, h4 = 0xc3d2e1f0;
    var w = new Int32Array(80);
    for (var off = 0; off < total; off += 64) {
      for (i = 0; i < 16; i++) {
        var p = off + 4 * i;
        w[i] = (b[p] << 24) | (b[p + 1] << 16) | (b[p + 2] << 8) | b[p + 3];
      }
      for (i = 16; i < 80; i++) {
        var x = w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16];
        w[i] = (x << 1) | (x >>> 31);
      }
      var a = h0, bb = h1, c = h2, d = h3, e = h4;
      for (i = 0; i < 80; i++) {
        var f, k;
        if (i < 20) { f = (bb & c) | (~bb & d); k = 0x5a827999; }
        else if (i < 40) { f = bb ^ c ^ d; k = 0x6ed9eba1; }
        else if (i < 60) { f = (bb & c) | (bb & d) | (c & d); k = 0x8f1bbcdc; }
        else { f = bb ^ c ^ d; k = 0xca62c1d6; }
        var tmp = (((a << 5) | (a >>> 27)) + f + e + k + w[i]) | 0;
        e = d; d = c; c = (bb << 30) | (bb >>> 2); bb = a; a = tmp;
      }
      h0 = (h0 + a) | 0; h1 = (h1 + bb) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0; h4 = (h4 + e) | 0;
    }
    var out = new Uint8Array(20);
    [h0, h1, h2, h3, h4].forEach(function (h, j) {
      out[4 * j] = (h >>> 24) & 0xff;
      out[4 * j + 1] = (h >>> 16) & 0xff;
      out[4 * j + 2] = (h >>> 8) & 0xff;
      out[4 * j + 3] = h & 0xff;
    });
    return out;
  }

  function hex(bytes) {
    var s = "";
    for (var i = 0; i < bytes.length; i++) s += (bytes[i] < 16 ? "0" : "") + bytes[i].toString(16);
    return s;
  }

  // stableUUID is topology.StableUUID: sha1 of the parts joined with NUL, version 5, variant 10.
  function stableUUID(parts) {
    var s = sha1(utf8(parts.join("\u0000"))).slice(0, 16);
    s[6] = (s[6] & 0x0f) | 0x50;
    s[8] = (s[8] & 0x3f) | 0x80;
    var h = hex(s);
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
  }

  // entityID is topology.EntityID: the override when set, otherwise a UUID derived from the name.
  function entityID(name, override) {
    return override ? override : stableUUID(["entity", name || ""]);
  }

  function counts(topo) {
    var n = { nodes: 0, gpus: 0, vms: 0 };
    arr(topo && topo.hypervisors).forEach(function (h) {
      if (!h) return;
      n.nodes++;
      arr(h.gpus).forEach(function (g) {
        if (!g) return;
        n.gpus++;
        n.vms += arr(g.vms).filter(Boolean).length;
      });
    });
    return n;
  }

  function allVMNames(topo) {
    var names = {};
    arr(topo && topo.hypervisors).forEach(function (h) {
      arr(h && h.gpus).forEach(function (g) {
        arr(g && g.vms).forEach(function (v) { if (v && v.name) names[v.name] = true; });
      });
    });
    return names;
  }

  // nextNodeName returns emu-vgpu-NN / emu-mig-NN / emu-pt-NN with the smallest unused NN.
  function nextNodeName(topo, mode) {
    var taken = {};
    arr(topo && topo.hypervisors).forEach(function (h) { if (h) taken[h.hostname] = true; });
    for (var n = 1; ; n++) {
      var name = MODE_PREFIX[mode] + (n < 10 ? "0" : "") + n;
      if (!taken[name]) return name;
    }
  }

  // shortName is the part of a node name that VM names carry, as in the default topology: emu-vgpu-01 -> vgpu01.
  function shortName(hostname) {
    return String(hostname || "node").replace(/^emu-/, "").replace(/[^A-Za-z0-9]/g, "").toLowerCase() || "node";
  }

  // vmName returns emu-<short>-g<index>-<n> with the smallest n no VM uses yet.
  function vmName(topo, node, gpu) {
    var taken = allVMNames(topo);
    var base = "emu-" + shortName(node.hostname) + "-g" + gpu.index + "-";
    for (var n = 1; ; n++) {
      if (!taken[base + n]) return base + n;
    }
  }

  function modelOf(cat, name) {
    return arr(cat && cat.gpu_models).filter(function (m) { return m.name === name; })[0] || null;
  }

  function usedSlices(gpu) {
    return arr(gpu && gpu.mig_instances).reduce(function (s, mi) { return s + (mi ? mi.slices || 0 : 0); }, 0);
  }

  // freeGI is the GI ID Fill would hand out: the first id of the kind's order not used yet.
  function freeGI(profile, used) {
    var ids = arr(profile && profile.gpu_instance_ids);
    for (var i = 0; i < ids.length; i++) if (!used[ids[i]]) return ids[i];
    var id = 0;
    while (used[id]) id++;
    return id;
  }

  // fittingProfiles returns the MIG kinds of the GPU's model that still fit, smallest first.
  function fittingProfiles(cat, gpu) {
    var m = modelOf(cat, gpu.model_name);
    var left = MAX_SLICES - usedSlices(gpu);
    return arr(m && m.mig_profiles)
      .filter(function (p) { return p.slices <= left; })
      .sort(function (a, b) { return a.slices - b.slices; });
  }

  function profileCopy(p) {
    if (!p) return null;
    return { name: p.name, type: p.type, framebuffer_mb: p.framebuffer_mb, max_instances: p.max_instances };
  }

  // newInstance builds a MIG instance of the kind with the next GI ID on the GPU.
  function newInstance(gpu, profile) {
    var used = {};
    arr(gpu.mig_instances).forEach(function (mi) { if (mi) used[mi.gpu_instance_id] = true; });
    return {
      gpu_instance_id: freeGI(profile, used),
      profile: profile.name,
      slices: profile.slices,
      memory_mib: profile.memory_mib,
      vgpu_profile: profileCopy(profile.vgpu_profile)
    };
  }

  function vmAt(topo, h, g, v) {
    var node = arr(topo.hypervisors)[h];
    var gpu = node && arr(node.gpus)[g];
    return gpu ? arr(gpu.vms)[v] || null : null;
  }

  // acceptGPU tells whether a GPU of this model may go on node h.
  function acceptGPU(topo, cat, h, model) {
    var node = arr(topo.hypervisors)[h];
    var m = modelOf(cat, model);
    if (!node || !m) return { ok: false, reason: "unknown" };
    if (arr(m.modes).indexOf(node.mode) < 0) {
      return { ok: false, reason: "mode_model", params: { model: m.name, mode: node.mode } };
    }
    return { ok: true };
  }

  // acceptVM tells whether a VM may go on GPU (t.h, t.g). t.slot is the GI ID of a MIG instance the VM is dropped on,
  // t.free means the free part of a MIG bar. p is {kind:"new-vm"} or {kind:"vm", h, g, v}.
  // The answer carries gi (attach to that instance) or create (the MIG kind of a new instance to make).
  function acceptVM(topo, cat, t, p) {
    var node = arr(topo.hypervisors)[t.h];
    var gpu = node && arr(node.gpus)[t.g];
    if (!gpu) return { ok: false, reason: "unknown" };
    var moving = p.kind === "vm" ? vmAt(topo, p.h, p.g, p.v) : null;
    var same = !!moving && p.h === t.h && p.g === t.g;
    var others = arr(gpu.vms).filter(function (v) { return v && v !== moving; });
    switch (node.mode) {
      case "PASSTHROUGH":
        if (same) return { ok: false, reason: "same" };
        if (others.length >= 1) return { ok: false, reason: "full_pt" };
        return { ok: true };
      case "VGPU":
        if (same) return { ok: false, reason: "same" };
        if (!gpu.profile) return { ok: false, reason: "no_profile" };
        if (others.length >= gpu.profile.max_instances) {
          return { ok: false, reason: "full_vgpu", params: { max: gpu.profile.max_instances } };
        }
        return { ok: true };
      case "MIG":
        var usedBy = {};
        others.forEach(function (v) { usedBy[v.mig_gpu_instance_id] = v; });
        if (t.slot !== undefined) {
          if (same && moving.mig_gpu_instance_id === t.slot) return { ok: false, reason: "same" };
          if (usedBy[t.slot]) return { ok: false, reason: "slot_used", params: { vm: usedBy[t.slot].name } };
          return { ok: true, gi: t.slot };
        }
        if (!t.free) {
          if (same) return { ok: false, reason: "same" };
          var insts = arr(gpu.mig_instances);
          for (var i = 0; i < insts.length; i++) {
            if (insts[i] && !usedBy[insts[i].gpu_instance_id]) return { ok: true, gi: insts[i].gpu_instance_id };
          }
        }
        var fit = fittingProfiles(cat, gpu);
        if (!fit.length) {
          if (!modelOf(cat, gpu.model_name)) return { ok: false, reason: "no_mig_profile" };
          return { ok: false, reason: "no_slices", params: { used: usedSlices(gpu), max: MAX_SLICES } };
        }
        return { ok: true, create: fit[0] };
    }
    return { ok: false, reason: "unknown" };
  }

  // mapProblems sorts Validate lines by what they are about, so each card can show its own.
  // Keys: nodes[h], gpus["h/g"], vms["h/g/v"]. Lines it cannot place go to global.
  function mapProblems(topo, problems) {
    var out = { global: [], nodes: {}, gpus: {}, vms: {} };
    var hosts = {}, vms = {};
    arr(topo && topo.hypervisors).forEach(function (h, hi) {
      if (!h) return;
      (hosts[h.hostname] = hosts[h.hostname] || []).push(hi);
      arr(h.gpus).forEach(function (g, gi) {
        arr(g && g.vms).forEach(function (v, vi) {
          if (v) (vms[v.name] = vms[v.name] || []).push([hi, gi, vi]);
        });
      });
    });
    function add(map, key, msg) { (map[key] = map[key] || []).push(msg); }
    // placeDuplicate puts a "duplicate <kind>: <value>" line on every node, GPU or VM that has the value.
    function placeDuplicate(kind, value, msg) {
      var nodeKey = { "hypervisor hostname": "hostname", "hypervisor id": "id", "hypervisor uuid": "uuid" }[kind];
      var vmKey = { "vm name": "name", "vm id": "id", "vm instance_name": "instance_name", "vm vgpu_uuid": "vgpu_uuid" }[kind];
      var found = false;
      arr(topo.hypervisors).forEach(function (h, hi) {
        if (!h) return;
        if (nodeKey && String(h[nodeKey]) === value) {
          add(out.nodes, hi, msg);
          found = true;
        }
        arr(h.gpus).forEach(function (g, gi) {
          if (!g) return;
          var gpuHit = (kind === "gpu uuid" && g.uuid === value) ||
            (kind === "gpu bus_id" && h.hostname + "/" + g.bus_id === value);
          if (gpuHit) {
            add(out.nodes, hi, msg);
            add(out.gpus, hi + "/" + gi, msg);
            found = true;
          }
          arr(g.vms).forEach(function (v, vi) {
            if (v && vmKey && v[vmKey] === value) {
              add(out.nodes, hi, msg);
              add(out.vms, hi + "/" + gi + "/" + vi, msg);
              found = true;
            }
          });
        });
      });
      return found;
    }
    function gpuPos(hi, index) {
      var gs = arr(topo.hypervisors[hi].gpus);
      for (var i = 0; i < gs.length; i++) if (gs[i] && gs[i].index === index) return i;
      return -1;
    }
    arr(problems).forEach(function (msg) {
      var m, placed = false;
      if ((m = /^hypervisors\[(\d+)\]/.exec(msg))) {
        add(out.nodes, +m[1], msg);
        placed = true;
      } else if ((m = /^hypervisor (.+?) gpu (-?\d+): /.exec(msg)) && hosts[m[1]]) {
        hosts[m[1]].forEach(function (hi) {
          add(out.nodes, hi, msg);
          var gi = gpuPos(hi, +m[2]);
          if (gi >= 0) add(out.gpus, hi + "/" + gi, msg);
        });
        placed = true;
      } else if ((m = /^hypervisor (.+?): /.exec(msg)) && hosts[m[1]]) {
        hosts[m[1]].forEach(function (hi) { add(out.nodes, hi, msg); });
        placed = true;
      } else if ((m = /^vm (.+?): /.exec(msg)) && vms[m[1]]) {
        vms[m[1]].forEach(function (p) {
          add(out.nodes, p[0], msg);
          add(out.vms, p.join("/"), msg);
        });
        placed = true;
      } else if ((m = /^duplicate (.+?): "(.*)"$/.exec(msg))) {
        placed = placeDuplicate(m[1], unquote(m[2]), msg);
      }
      if (!placed) out.global.push(msg);
    });
    return out;
  }

  // unquote reads a Go %q string body. Plain text, which every generated value is, comes back as it is.
  function unquote(s) {
    try { return JSON.parse('"' + s + '"'); } catch (e) { return s; }
  }

  // ---- which objects came from the server ----
  // Ids derived from a name follow the name only for nodes and VMs made in the editor; a saved one keeps its id on
  // rename, as nova does. Whether an object was loaded from the server is kept in a WeakSet, never as a JSON field.

  function eachObject(topo, fn) {
    arr(topo && topo.hypervisors).forEach(function (h, hi) {
      if (!h) return;
      fn(h, [hi]);
      arr(h.gpus).forEach(function (g, gi) {
        if (!g) return;
        fn(g, [hi, gi]);
        arr(g.vms).forEach(function (v, vi) { if (v) fn(v, [hi, gi, vi]); });
      });
    });
  }

  // markTree adds every node, GPU and VM of topo to set.
  function markTree(topo, set) {
    eachObject(topo, function (o) { set.add(o); });
  }

  // carryMarks marks the objects of to that sit where a marked object of from sits.
  // The fill endpoint answers with new objects in the same order it was sent, so position is identity there.
  function carryMarks(from, to, set) {
    eachObject(to, function (o, pos) {
      var h = arr(from && from.hypervisors)[pos[0]];
      var x = h;
      if (pos.length > 1) x = x ? arr(x.gpus)[pos[1]] : null;
      if (pos.length > 2) x = x ? arr(x.vms)[pos[2]] : null;
      if (x && set.has(x)) set.add(o);
    });
  }

  // markByIDs marks nodes and VMs whose uuid or id is in the saved topology and appears once in topo.
  // It is for JSON typed by hand, where positions mean nothing.
  function markByIDs(topo, saved, set) {
    var savedIDs = {}, seen = {};
    eachObject(saved, function (o, pos) {
      if (pos.length === 1 && o.uuid) savedIDs["h:" + o.uuid] = true;
      if (pos.length === 3 && o.id) savedIDs["v:" + o.id] = true;
    });
    var key = function (o, pos) {
      if (pos.length === 1 && o.uuid) return "h:" + o.uuid;
      if (pos.length === 3 && o.id) return "v:" + o.id;
      return "";
    };
    eachObject(topo, function (o, pos) { var k = key(o, pos); if (k) seen[k] = (seen[k] || 0) + 1; });
    eachObject(topo, function (o, pos) {
      var k = key(o, pos);
      if (k && savedIDs[k] && seen[k] === 1) set.add(o);
    });
  }

  // renameNode renames a node and the hypervisor field of its VMs. A node that did not come from the server
  // drops its uuid and GPU uuids so Fill derives them from the new name.
  function renameNode(n, name, fromServer) {
    n.hostname = name;
    arr(n.gpus).forEach(function (g) {
      arr(g && g.vms).forEach(function (v) { if (v) v.hypervisor = name; });
    });
    if (!fromServer) {
      n.uuid = "";
      arr(n.gpus).forEach(function (g) { if (g) g.uuid = ""; });
    }
  }

  // renameVM renames a VM. A VM that did not come from the server drops the ids Fill derives from the name.
  function renameVM(vm, name, fromServer) {
    vm.name = name;
    if (!fromServer) {
      vm.id = "";
      if (vm.mode !== "PASSTHROUGH") vm.vgpu_uuid = "";
    }
  }

  var helpers = {
    format: format,
    sha1Hex: function (s) { return hex(sha1(utf8(s))); },
    stableUUID: stableUUID,
    entityID: entityID,
    counts: counts,
    nextNodeName: nextNodeName,
    vmName: vmName,
    freeGI: freeGI,
    usedSlices: usedSlices,
    newInstance: newInstance,
    acceptGPU: acceptGPU,
    acceptVM: acceptVM,
    mapProblems: mapProblems,
    markTree: markTree,
    carryMarks: carryMarks,
    markByIDs: markByIDs,
    renameNode: renameNode,
    renameVM: renameVM
  };
  if (typeof module !== "undefined" && module.exports) module.exports = helpers;
  if (typeof document === "undefined") return;

  // ---- browser part ----

  var CATALOG = "/api/v1/topology/catalog";
  var FILL = "/api/v1/topology/fill";
  var TOPOLOGY = "/api/v1/topology";
  var RESET = "/api/v1/topology/reset";
  var FILL_DELAY_MS = 400;

  var DICT = {
    en: {
      save: "Save",
      revert: "Revert",
      reset: "Reset to default",
      view_canvas: "Editor",
      view_json: "JSON",
      view_entities: "Entity IDs",
      palette: "Palette",
      properties: "Properties",
      summary: "{nodes} nodes, {gpus} GPUs, {vms} VMs",
      st_loading: "Loading...",
      st_checking: "Checking...",
      st_saving: "Saving...",
      st_ok: "No problems",
      st_problems: "Problems: {n}",
      st_dirty: "Unsaved changes",
      st_saved: "Saved",
      load_failed: "Could not load the topology: {error}",
      retry: "Retry",
      conn_error: "Cannot reach the server.",
      pal_nodes: "Nodes",
      pal_gpus: "GPU models",
      pal_vm: "VM",
      pal_hint: "Drag a node onto the canvas, a GPU model onto a node and a VM onto a GPU.",
      node_VGPU: "vGPU node",
      node_MIG: "MIG node",
      node_PASSTHROUGH: "Passthrough node",
      mode_VGPU: "vGPU",
      mode_MIG: "MIG",
      mode_PASSTHROUGH: "Passthrough",
      vm_item: "VM",
      trash: "Drop here to delete",
      canvas_empty: "Drag a node here from the palette.",
      node_empty: "Drag a GPU model here.",
      seat_empty: "free",
      mig_free: "{n} free",
      mig_none: "No MIG instances. Drop a VM here or add instances in Properties.",
      problems_global: "Problems",
      r_same: "Already here",
      r_full_pt: "A passthrough GPU takes one VM",
      r_full_vgpu: "All {max} vGPU slots are used",
      r_no_profile: "Pick a vGPU profile first",
      r_slot_used: "This GPU instance is used by {vm}",
      r_no_slices: "No MIG slices left ({used}/{max})",
      r_no_mig_profile: "No MIG profiles for this GPU model",
      r_mode_model: "{model} cannot go on a {mode} node",
      r_unknown: "Cannot drop here",
      p_none: "Select a node, GPU or VM on the canvas.",
      p_project: "Project",
      p_node: "Node",
      p_gpu: "GPU",
      p_vm: "VM",
      f_region: "Region",
      f_domain_id: "Domain ID",
      f_project_name: "Project name",
      f_user_name: "User name",
      f_hostname: "Host name",
      f_mode: "Mode",
      f_mode_hint: "Set when the node is created. Delete and create the node to change it.",
      f_id: "ID",
      f_uuid: "UUID",
      f_host_ip: "Host IP",
      f_vcpus: "vCPUs",
      f_memory_mb: "Memory (MB)",
      f_local_gb: "Local disk (GB)",
      f_disk_gb: "Disk (GB)",
      f_entity_id: "OTel entity_id",
      f_entity_hint: "Empty means the default derived from the name: {id}",
      f_index: "Index",
      f_model: "Model",
      f_bus_id: "Bus ID",
      f_memory_mib: "Memory (MiB)",
      f_vgpu_profile: "vGPU profile",
      f_max_instances: "Max VMs",
      f_mig_instances: "MIG instances ({used}/{max} slices)",
      f_name: "Name",
      f_instance_name: "Instance name",
      f_ip: "IP",
      f_flavor: "Flavor",
      f_mig_instance: "MIG instance",
      auto: "filled automatically",
      custom_profile: "{name} (custom)",
      col_gi: "GI",
      col_profile: "Profile",
      col_vm: "VM",
      add_gpu: "Add GPU",
      add_vm: "Add VM",
      add_instance: "Add instance",
      remove: "Remove",
      delete_node: "Delete node",
      delete_gpu: "Delete GPU",
      delete_vm: "Delete VM",
      instance_used: "Used by a VM. Move or delete the VM first.",
      json_help: "Edit the topology as JSON. Empty ids, addresses and flavors are filled in when you apply.",
      json_apply: "Apply",
      json_invalid: "Invalid JSON: {error}",
      json_applied: "Applied. Not saved yet.",
      ent_help: "entity_id each node sends with OpenTelemetry metrics. Edit it in Properties.",
      ent_kind: "Kind",
      ent_name: "Name",
      ent_id: "entity_id",
      ent_source: "Source",
      ent_default: "default",
      ent_custom: "custom",
      ent_node: "node",
      ent_vm: "VM",
      ent_copy: "Copy all",
      copied: "Copied",
      saved: "Topology saved",
      not_saved_problems: "Not saved. Fix the problems first ({n}).",
      save_failed: "Save failed: {error}",
      revert_confirm: "Discard unsaved changes and reload the saved topology?",
      reverted: "Reloaded the saved topology",
      reset_confirm: "Replace the saved topology with the six-node default? This saves immediately.",
      reset_done: "Topology reset to the default",
      leave_confirm: "The topology has unsaved changes."
    },
    ko: {
      save: "저장",
      revert: "되돌리기",
      reset: "기본값으로",
      view_canvas: "편집기",
      view_json: "JSON",
      view_entities: "Entity ID",
      palette: "팔레트",
      properties: "속성",
      summary: "노드 {nodes}개, GPU {gpus}개, VM {vms}개",
      st_loading: "불러오는 중...",
      st_checking: "검사 중...",
      st_saving: "저장 중...",
      st_ok: "문제 없음",
      st_problems: "문제 {n}개",
      st_dirty: "저장 안 한 변경 있음",
      st_saved: "저장됨",
      load_failed: "토폴로지를 불러오지 못했습니다: {error}",
      retry: "다시 시도",
      conn_error: "서버에 연결할 수 없습니다.",
      pal_nodes: "노드",
      pal_gpus: "GPU 모델",
      pal_vm: "VM",
      pal_hint: "노드는 캔버스에, GPU 모델은 노드에, VM 은 GPU 에 끌어다 놓습니다.",
      node_VGPU: "vGPU 노드",
      node_MIG: "MIG 노드",
      node_PASSTHROUGH: "Passthrough 노드",
      mode_VGPU: "vGPU",
      mode_MIG: "MIG",
      mode_PASSTHROUGH: "Passthrough",
      vm_item: "VM",
      trash: "여기에 놓으면 삭제",
      canvas_empty: "팔레트에서 노드를 끌어다 놓으세요.",
      node_empty: "GPU 모델을 끌어다 놓으세요.",
      seat_empty: "빈 자리",
      mig_free: "{n} 남음",
      mig_none: "MIG 인스턴스가 없습니다. VM 을 놓거나 속성에서 인스턴스를 추가하세요.",
      problems_global: "문제",
      r_same: "이미 여기에 있습니다",
      r_full_pt: "Passthrough GPU 에는 VM 하나만 붙습니다",
      r_full_vgpu: "vGPU 자리 {max}개가 다 찼습니다",
      r_no_profile: "vGPU 프로필을 먼저 고르세요",
      r_slot_used: "이 GPU 인스턴스는 {vm} 이(가) 쓰고 있습니다",
      r_no_slices: "남은 MIG slice 가 없습니다 ({used}/{max})",
      r_no_mig_profile: "이 GPU 모델의 MIG 프로필이 없습니다",
      r_mode_model: "{model} 은(는) {mode} 노드에 넣을 수 없습니다",
      r_unknown: "여기에는 놓을 수 없습니다",
      p_none: "캔버스에서 노드, GPU, VM 을 고르세요.",
      p_project: "프로젝트",
      p_node: "노드",
      p_gpu: "GPU",
      p_vm: "VM",
      f_region: "리전",
      f_domain_id: "도메인 ID",
      f_project_name: "프로젝트 이름",
      f_user_name: "사용자 이름",
      f_hostname: "호스트 이름",
      f_mode: "모드",
      f_mode_hint: "노드를 만들 때 정합니다. 바꾸려면 노드를 지우고 다시 만드세요.",
      f_id: "ID",
      f_uuid: "UUID",
      f_host_ip: "호스트 IP",
      f_vcpus: "vCPU",
      f_memory_mb: "메모리 (MB)",
      f_local_gb: "로컬 디스크 (GB)",
      f_disk_gb: "디스크 (GB)",
      f_entity_id: "OTel entity_id",
      f_entity_hint: "비우면 이름에서 만든 기본값을 씁니다: {id}",
      f_index: "인덱스",
      f_model: "모델",
      f_bus_id: "Bus ID",
      f_memory_mib: "메모리 (MiB)",
      f_vgpu_profile: "vGPU 프로필",
      f_max_instances: "최대 VM 수",
      f_mig_instances: "MIG 인스턴스 ({used}/{max} slice)",
      f_name: "이름",
      f_instance_name: "인스턴스 이름",
      f_ip: "IP",
      f_flavor: "Flavor",
      f_mig_instance: "MIG 인스턴스",
      auto: "자동으로 채움",
      custom_profile: "{name} (직접 지정)",
      col_gi: "GI",
      col_profile: "프로필",
      col_vm: "VM",
      add_gpu: "GPU 추가",
      add_vm: "VM 추가",
      add_instance: "인스턴스 추가",
      remove: "제거",
      delete_node: "노드 삭제",
      delete_gpu: "GPU 삭제",
      delete_vm: "VM 삭제",
      instance_used: "VM 이 쓰고 있습니다. VM 을 먼저 옮기거나 지우세요.",
      json_help: "토폴로지를 JSON 으로 고칩니다. 적용하면 비어 있는 id, 주소, flavor 를 채웁니다.",
      json_apply: "적용",
      json_invalid: "JSON 이 잘못됐습니다: {error}",
      json_applied: "적용했습니다. 아직 저장하지 않았습니다.",
      ent_help: "노드마다 OpenTelemetry 지표에 붙는 entity_id 입니다. 속성 칸에서 바꿉니다.",
      ent_kind: "종류",
      ent_name: "이름",
      ent_id: "entity_id",
      ent_source: "출처",
      ent_default: "기본값",
      ent_custom: "직접 지정",
      ent_node: "노드",
      ent_vm: "VM",
      ent_copy: "모두 복사",
      copied: "복사했습니다",
      saved: "토폴로지를 저장했습니다",
      not_saved_problems: "저장하지 않았습니다: 문제 {n}개를 먼저 고치세요",
      save_failed: "저장 실패: {error}",
      revert_confirm: "저장 안 한 변경을 버리고 저장된 토폴로지를 다시 불러올까요?",
      reverted: "저장된 토폴로지를 다시 불러왔습니다",
      reset_confirm: "저장된 토폴로지를 6노드 기본값으로 바꿀까요? 바로 저장됩니다.",
      reset_done: "토폴로지를 기본값으로 되돌렸습니다",
      leave_confirm: "토폴로지에 저장 안 한 변경이 있습니다."
    }
  };

  // Which payloads each kind of drop target takes.
  var HANDLES = {
    canvas: ["new-node"],
    node: ["new-gpu"],
    gpu: ["new-vm", "vm"],
    slot: ["new-vm", "vm"],
    free: ["new-vm", "vm"],
    trash: ["vm", "gpu", "node"]
  };

  var S = {
    root: null,
    els: {},
    catalog: null,
    topo: null,
    baseline: "",
    fromServer: new WeakSet(),
    sel: null,
    view: "canvas",
    problems: [],
    rev: 0,
    fillTimer: null,
    filling: false,
    busy: "",
    loadError: "",
    jsonEdited: false,
    jsonError: "",
    drag: null,
    pdrag: null,
    open: { palette: false, props: false }
  };

  function gm() { return root.GMRG || null; }
  function lang() { var g = gm(); return g && g.lang === "ko" ? "ko" : "en"; }
  function tr(key, params) {
    var s = DICT[lang()][key];
    if (s === undefined) s = DICT.en[key];
    if (s === undefined) s = key;
    return format(s, params);
  }
  function toast(msg, kind) { var g = gm(); if (g && g.toast) g.toast(msg, kind); }
  function api(path, opt) {
    var g = gm();
    if (g && g.api) return g.api(path, opt);
    return Promise.reject(new Error("GMRG.api missing"));
  }
  function errText(res) {
    if (res && res.data && res.data.error) return res.data.error;
    return "HTTP " + (res ? res.status : "?");
  }

  function el(tag, cls, text, attrs) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    if (attrs) Object.keys(attrs).forEach(function (k) {
      if (attrs[k] !== undefined && attrs[k] !== null) n.setAttribute(k, attrs[k]);
    });
    return n;
  }
  function btn(text, cls, onClick) {
    var b = el("button", "btn tp-btn" + (cls ? " " + cls : ""), text, { type: "button" });
    if (onClick) b.addEventListener("click", onClick);
    return b;
  }
  function clear(n) { while (n.firstChild) n.removeChild(n.firstChild); }
  function modeLabel(mode) { return DICT.en["mode_" + mode] ? tr("mode_" + mode) : String(mode || "?"); }
  function shortModel(name) { return String(name || "?").replace(/^(NVIDIA|Tesla|GRID) /, ""); }

  function nodes() { return arr(S.topo && S.topo.hypervisors); }
  function node(h) { return nodes()[h] || null; }
  function gpu(h, g) { var n = node(h); return n ? arr(n.gpus)[g] || null : null; }
  function flavorByID(id) {
    return arr(S.topo && S.topo.flavors).filter(function (f) { return f && f.id === id; })[0] || null;
  }
  function dirty() { return !!S.topo && JSON.stringify(S.topo) !== S.baseline; }

  // ---- loading and saving ----

  function setSaved(t) {
    S.topo = t;
    S.baseline = JSON.stringify(t);
    S.fromServer = new WeakSet();
    markTree(t, S.fromServer);
    S.jsonEdited = false;
    S.jsonError = "";
  }

  function load() {
    S.loadError = "";
    S.busy = "load";
    renderAll();
    var cat = S.catalog ? Promise.resolve({ ok: true, data: S.catalog }) : api(CATALOG);
    return Promise.all([cat, api(TOPOLOGY)]).then(function (rs) {
      S.busy = "";
      var bad = rs.filter(function (r) { return !r.ok; })[0];
      if (bad) {
        S.loadError = errText(bad);
        renderAll();
        return;
      }
      S.catalog = rs[0].data;
      setSaved(rs[1].data);
      S.sel = null;
      S.problems = [];
      S.rev++;
      renderAll();
      scheduleFill(0);
    }, function () {
      S.busy = "";
      S.loadError = tr("conn_error");
      renderAll();
    });
  }

  function scheduleFill(delay) {
    clearTimeout(S.fillTimer);
    S.fillTimer = setTimeout(runFill, delay === undefined ? FILL_DELAY_MS : delay);
    renderStatus();
  }

  // runFill sends the working copy to the fill endpoint and takes the filled copy back,
  // unless the user changed something meanwhile (then the newer change's own fill wins).
  function runFill() {
    S.fillTimer = null;
    if (!S.topo) return Promise.resolve(false);
    var rev = S.rev;
    S.filling = true;
    renderStatus();
    return api(FILL, { method: "POST", body: S.topo }).then(function (res) {
      if (rev !== S.rev) return false;
      S.filling = false;
      if (res.ok && res.data && res.data.topology) {
        carryMarks(S.topo, res.data.topology, S.fromServer);
        S.topo = res.data.topology;
        S.problems = arr(res.data.problems);
      } else {
        S.problems = res.data && res.data.problems ? [errText(res)].concat(res.data.problems) : [errText(res)];
      }
      renderAfterData();
      return res.ok;
    }, function () {
      if (rev !== S.rev) return false;
      S.filling = false;
      S.problems = [tr("conn_error")];
      renderAfterData();
      return false;
    });
  }

  function save() {
    if (!S.topo || S.busy) return;
    clearTimeout(S.fillTimer);
    S.busy = "save";
    renderStatus();
    runFill().then(function (ok) {
      if (!ok || S.problems.length) {
        S.busy = "";
        renderStatus();
        toast(tr("not_saved_problems", { n: S.problems.length || 1 }), "error");
        return;
      }
      var rev = S.rev;
      var sent = new WeakSet();
      markTree(S.topo, sent);
      return api(TOPOLOGY, { method: "PUT", body: S.topo }).then(function (res) {
        S.busy = "";
        if (res.ok) {
          if (rev === S.rev) {
            setSaved(res.data);
            S.problems = [];
          } else {
            S.baseline = JSON.stringify(res.data);
            eachObject(S.topo, function (o) { if (sent.has(o)) S.fromServer.add(o); });
          }
          toast(tr("saved"), "ok");
        } else {
          if (res.status === 400 && res.data && res.data.problems) S.problems = res.data.problems;
          toast(tr("save_failed", { error: errText(res) }), "error");
        }
        renderAfterData();
      });
    }).catch(function () {
      S.busy = "";
      toast(tr("save_failed", { error: tr("conn_error") }), "error");
      renderStatus();
    });
  }

  function revert() {
    if (S.busy) return;
    if (dirty() && !window.confirm(tr("revert_confirm"))) return;
    clearTimeout(S.fillTimer);
    load().then(function () { if (!S.loadError) toast(tr("reverted"), "ok"); });
  }

  function reset() {
    if (S.busy || !window.confirm(tr("reset_confirm"))) return;
    clearTimeout(S.fillTimer);
    S.busy = "save";
    renderStatus();
    api(RESET, { method: "POST" }).then(function (res) {
      S.busy = "";
      if (!res.ok) {
        toast(tr("save_failed", { error: errText(res) }), "error");
        renderStatus();
        return;
      }
      setSaved(res.data);
      S.sel = null;
      S.problems = [];
      S.rev++;
      renderAll();
      toast(tr("reset_done"), "ok");
    }, function () {
      S.busy = "";
      toast(tr("save_failed", { error: tr("conn_error") }), "error");
      renderStatus();
    });
  }

  // changed is called after every edit: redraw, then fill and check after a short pause.
  function changed(keepProps) {
    S.rev++;
    S.jsonEdited = false;
    renderMain();
    if (!keepProps) renderProps();
    renderSummary();
    scheduleFill();
  }

  // ---- edits ----

  function addNode(mode) {
    if (!S.topo.hypervisors) S.topo.hypervisors = [];
    S.topo.hypervisors.push({ hostname: nextNodeName(S.topo, mode), mode: mode, gpus: [] });
    S.sel = { kind: "node", h: S.topo.hypervisors.length - 1 };
  }

  function addGPU(h, modelName) {
    var n = node(h);
    var m = modelOf(S.catalog, modelName);
    var index = 0;
    arr(n.gpus).forEach(function (g) { if (g && g.index >= index) index = g.index + 1; });
    var g = { index: index, model_name: modelName, vms: [] };
    if (n.mode === "VGPU") g.profile = profileCopy(arr(m && m.vgpu_profiles)[0]);
    if (n.mode === "MIG") g.mig_instances = [];
    if (!n.gpus) n.gpus = [];
    n.gpus.push(g);
    S.sel = { kind: "gpu", h: h, g: n.gpus.length - 1 };
  }

  function place(gp, verdict, vm) {
    if (verdict.create) {
      var mi = newInstance(gp, verdict.create);
      if (!gp.mig_instances) gp.mig_instances = [];
      gp.mig_instances.push(mi);
      vm.mig_gpu_instance_id = mi.gpu_instance_id;
    } else if (verdict.gi !== undefined) {
      vm.mig_gpu_instance_id = verdict.gi;
    }
  }

  function addVM(h, g, verdict) {
    var n = node(h), gp = gpu(h, g);
    var vm = { name: vmName(S.topo, n, gp), hypervisor: n.hostname, gpu_index: gp.index, mode: n.mode, mig_gpu_instance_id: -1 };
    place(gp, verdict, vm);
    if (!gp.vms) gp.vms = [];
    gp.vms.push(vm);
    S.sel = { kind: "vm", h: h, g: g, v: gp.vms.length - 1 };
  }

  // resetFlavor drops the flavor so Fill picks the one that matches the new GPU. Sizes that came from the old
  // flavor go with it; sizes the user changed stay.
  function resetFlavor(vm) {
    var f = flavorByID(vm.flavor_id);
    if (f && vm.vcpus === f.vcpus && vm.memory_mb === f.ram_mb && vm.disk_gb === f.disk_gb) {
      vm.vcpus = 0;
      vm.memory_mb = 0;
      vm.disk_gb = 0;
    }
    vm.flavor_id = "";
  }

  function moveVM(p, h, g, verdict) {
    var src = gpu(p.h, p.g), srcNode = node(p.h);
    var dst = gpu(h, g), dstNode = node(h);
    var vm = src.vms.splice(p.v, 1)[0];
    var profName = function (x) { return x.profile ? x.profile.name : ""; };
    if (srcNode.mode !== dstNode.mode || src.model_name !== dst.model_name ||
        profName(src) !== profName(dst) || dstNode.mode === "MIG") {
      resetFlavor(vm);
    }
    if ((srcNode.mode === "PASSTHROUGH") !== (dstNode.mode === "PASSTHROUGH")) {
      vm.guest_driver_version = "";
      vm.vgpu_uuid = "";
    }
    vm.hypervisor = dstNode.hostname;
    vm.gpu_index = dst.index;
    vm.mode = dstNode.mode;
    vm.mig_gpu_instance_id = -1;
    place(dst, verdict, vm);
    if (!dst.vms) dst.vms = [];
    dst.vms.push(vm);
    S.sel = { kind: "vm", h: h, g: g, v: dst.vms.length - 1 };
  }

  function remove(p) {
    if (p.kind === "vm") gpu(p.h, p.g).vms.splice(p.v, 1);
    if (p.kind === "gpu") node(p.h).gpus.splice(p.g, 1);
    if (p.kind === "node") S.topo.hypervisors.splice(p.h, 1);
    S.sel = null;
  }

  // ---- drag and drop ----

  function payloadOf(n) {
    var d = n.dataset;
    var p = { kind: d.drag };
    if (d.mode) p.mode = d.mode;
    if (d.model) p.model = d.model;
    if (d.h !== undefined) p.h = +d.h;
    if (d.g !== undefined) p.g = +d.g;
    if (d.v !== undefined) p.v = +d.v;
    return p;
  }

  function dropTarget(n, p) {
    var cur = n && n.closest ? n.closest("[data-drop]") : null;
    while (cur && S.root.contains(cur)) {
      if ((HANDLES[cur.dataset.drop] || []).indexOf(p.kind) >= 0) return cur;
      cur = cur.parentElement ? cur.parentElement.closest("[data-drop]") : null;
    }
    return null;
  }

  function evaluate(t, p) {
    var d = t.dataset;
    switch (d.drop) {
      case "canvas":
      case "trash":
        return { ok: true };
      case "node":
        return acceptGPU(S.topo, S.catalog, +d.h, p.model);
      case "gpu":
      case "slot":
      case "free":
        return acceptVM(S.topo, S.catalog, {
          h: +d.h, g: +d.g,
          slot: d.drop === "slot" ? +d.gi : undefined,
          free: d.drop === "free"
        }, p);
    }
    return { ok: false, reason: "unknown" };
  }

  function perform(t, p, verdict) {
    var d = t.dataset;
    switch (d.drop) {
      case "canvas": addNode(p.mode); break;
      case "node": addGPU(+d.h, p.model); break;
      case "trash": remove(p); break;
      default:
        if (p.kind === "new-vm") addVM(+d.h, +d.g, verdict);
        else moveVM(p, +d.h, +d.g, verdict);
    }
    changed();
  }

  var hl = null;
  function highlight(t, verdict, x, y) {
    if (hl && hl !== t) hl.classList.remove("tp-drop-ok", "tp-drop-no");
    hl = t;
    var hint = S.els.hint;
    if (!t) {
      hint.hidden = true;
      return;
    }
    t.classList.toggle("tp-drop-ok", verdict.ok);
    t.classList.toggle("tp-drop-no", !verdict.ok);
    if (!verdict.ok && verdict.reason !== "same") {
      hint.textContent = tr("r_" + verdict.reason, verdict.params);
      hint.style.left = (x + 14) + "px";
      hint.style.top = (y + 14) + "px";
      hint.hidden = false;
    } else {
      hint.hidden = true;
    }
  }

  function endDrag() {
    highlight(null);
    S.drag = null;
    S.root.classList.remove("tp-dragging");
  }

  function bindDrag(rootEl) {
    rootEl.addEventListener("dragstart", function (e) {
      var src = e.target.closest ? e.target.closest("[data-drag]") : null;
      if (!src || S.pdrag) { e.preventDefault(); return; }
      S.drag = payloadOf(src);
      e.dataTransfer.effectAllowed = "copyMove";
      e.dataTransfer.setData("text/plain", src.textContent || S.drag.kind);
      rootEl.classList.add("tp-dragging");
    });
    rootEl.addEventListener("dragover", function (e) {
      if (!S.drag) return;
      var t = dropTarget(e.target, S.drag);
      if (!t) { highlight(null); return; }
      var v = evaluate(t, S.drag);
      highlight(t, v, e.clientX, e.clientY);
      if (v.ok) {
        e.preventDefault();
        e.dataTransfer.dropEffect = S.drag.kind.indexOf("new-") === 0 ? "copy" : "move";
      }
    });
    rootEl.addEventListener("dragleave", function (e) {
      if (S.drag && (!e.relatedTarget || !rootEl.contains(e.relatedTarget))) highlight(null);
    });
    rootEl.addEventListener("drop", function (e) {
      if (!S.drag) return;
      e.preventDefault();
      var p = S.drag;
      var t = dropTarget(e.target, p);
      var v = t ? evaluate(t, p) : null;
      endDrag();
      if (v && v.ok) perform(t, p, v);
    });
    rootEl.addEventListener("dragend", endDrag);

    // Touch and pen: native drag and drop is missing or needs a long press there, so follow the pointer instead.
    rootEl.addEventListener("pointerdown", function (e) {
      if (e.pointerType === "mouse" || e.button !== 0) return;
      var src = e.target.closest ? e.target.closest("[data-drag]") : null;
      if (!src) return;
      S.pdrag = { src: src, id: e.pointerId, x: e.clientX, y: e.clientY, ghost: null, p: payloadOf(src) };
    });
    window.addEventListener("pointermove", function (e) {
      var d = S.pdrag;
      if (!d || e.pointerId !== d.id) return;
      if (!d.ghost) {
        if (Math.abs(e.clientX - d.x) + Math.abs(e.clientY - d.y) < 8) return;
        d.ghost = el("div", "tp-ghost", d.src.textContent);
        document.body.appendChild(d.ghost);
        S.drag = d.p;
        rootEl.classList.add("tp-dragging");
      }
      e.preventDefault();
      d.ghost.style.left = e.clientX + "px";
      d.ghost.style.top = e.clientY + "px";
      var t = dropTarget(document.elementFromPoint(e.clientX, e.clientY), d.p);
      highlight(t, t ? evaluate(t, d.p) : null, e.clientX, e.clientY);
    }, { passive: false });
    var finish = function (e, drop) {
      var d = S.pdrag;
      if (!d || e.pointerId !== d.id) return;
      S.pdrag = null;
      if (!d.ghost) return;
      d.ghost.remove();
      var t = drop ? dropTarget(document.elementFromPoint(e.clientX, e.clientY), d.p) : null;
      var v = t ? evaluate(t, d.p) : null;
      endDrag();
      if (v && v.ok) perform(t, d.p, v);
    };
    window.addEventListener("pointerup", function (e) { finish(e, true); });
    window.addEventListener("pointercancel", function (e) { finish(e, false); });
  }

  // ---- rendering ----

  function renderAll() {
    if (!S.root) return;
    renderToolbar();
    renderPalette();
    renderMain();
    renderProps();
    renderSummary();
    renderStatus();
  }

  function renderAfterData() {
    renderMain();
    renderProps(true);
    renderSummary();
    renderStatus();
  }

  function renderToolbar() {
    var bar = S.els.toolbar;
    clear(bar);
    var left = el("div", "tp-tools");
    var ready = !!S.topo && !S.loadError;
    var b;
    left.appendChild(b = btn(tr("save"), "btn-primary", save));
    b.disabled = !ready;
    left.appendChild(b = btn(tr("revert"), "", revert));
    b.disabled = !ready;
    left.appendChild(b = btn(tr("reset"), "", reset));
    b.disabled = !ready;
    var views = el("div", "tp-views", null, { role: "group" });
    ["canvas", "json", "entities"].forEach(function (v) {
      var vb = btn(tr("view_" + v), "tp-view" + (S.view === v ? " tp-on" : ""), function () {
        if (S.view === v) return;
        S.view = v;
        S.jsonEdited = false;
        S.jsonError = "";
        renderToolbar();
        renderMain();
      });
      vb.setAttribute("aria-pressed", String(S.view === v));
      vb.disabled = !ready;
      views.appendChild(vb);
    });
    left.appendChild(views);
    bar.appendChild(left);
    var info = el("div", "tp-info");
    S.els.summary = el("span", "tp-summary");
    S.els.status = el("span", "tp-status", null, { role: "status" });
    info.appendChild(S.els.summary);
    info.appendChild(S.els.status);
    bar.appendChild(info);
    renderSummary();
    renderStatus();
  }

  function renderSummary() {
    if (!S.els.summary) return;
    S.els.summary.textContent = S.topo ? tr("summary", counts(S.topo)) : "";
  }

  function renderStatus() {
    var st = S.els.status;
    if (!st) return;
    var parts = [], cls = "tp-status";
    if (S.busy === "load") parts.push(tr("st_loading"));
    else if (S.busy === "save") parts.push(tr("st_saving"));
    else if (S.topo) {
      if (S.fillTimer || S.filling) parts.push(tr("st_checking"));
      else if (S.problems.length) { parts.push(tr("st_problems", { n: S.problems.length })); cls += " tp-bad-text"; }
      else parts.push(tr("st_ok"));
      parts.push(dirty() ? tr("st_dirty") : tr("st_saved"));
      if (dirty()) cls += " tp-dirty";
    }
    st.className = cls;
    st.textContent = parts.join(" / ");
  }

  function section(key, extra) {
    var s = el("div", "tp-sec" + (extra ? " " + extra : ""));
    s.appendChild(el("h3", "tp-sec-title", tr(key)));
    return s;
  }

  function renderPalette() {
    var pal = S.els.paletteBody;
    clear(pal);
    pal.appendChild(el("p", "tp-hint", tr("pal_hint")));
    var ns = section("pal_nodes");
    MODES.forEach(function (m) {
      var it = el("div", "tp-pal-item tp-mode-" + m, tr("node_" + m),
        { draggable: "true", "data-drag": "new-node", "data-mode": m, title: tr("node_" + m) });
      ns.appendChild(it);
    });
    pal.appendChild(ns);
    var gs = section("pal_gpus");
    arr(S.catalog && S.catalog.gpu_models).forEach(function (m) {
      var it = el("div", "tp-pal-item tp-pal-gpu", null,
        { draggable: "true", "data-drag": "new-gpu", "data-model": m.name, title: m.name });
      it.appendChild(el("span", "tp-pal-name", shortModel(m.name)));
      var meta = el("span", "tp-pal-meta");
      meta.appendChild(el("span", null, Math.round(m.memory_mib / 1024) + " GiB"));
      arr(m.modes).forEach(function (mode) { meta.appendChild(el("span", "tp-tag tp-mode-" + mode, modeLabel(mode))); });
      it.appendChild(meta);
      gs.appendChild(it);
    });
    pal.appendChild(gs);
    var vs = section("pal_vm");
    vs.appendChild(el("div", "tp-pal-item tp-pal-vm", tr("vm_item"), { draggable: "true", "data-drag": "new-vm" }));
    pal.appendChild(vs);
    pal.appendChild(el("div", "tp-trash", tr("trash"), { "data-drop": "trash" }));
  }

  function renderMain() {
    var main = S.els.main;
    if (!main) return;
    if (S.view === "json" && S.jsonEdited && S.els.json && main.contains(S.els.json)) return;
    clear(main);
    if (S.loadError) {
      var box = el("div", "tp-problems");
      box.appendChild(el("p", null, tr("load_failed", { error: S.loadError })));
      box.appendChild(btn(tr("retry"), "", load));
      main.appendChild(box);
      return;
    }
    if (!S.topo) {
      main.appendChild(el("p", "tp-hint", tr("st_loading")));
      return;
    }
    var mapped = mapProblems(S.topo, S.problems);
    if (mapped.global.length) {
      var gp = el("div", "tp-problems");
      gp.appendChild(el("strong", null, tr("problems_global")));
      var ul = el("ul");
      mapped.global.forEach(function (m) { ul.appendChild(el("li", null, m)); });
      gp.appendChild(ul);
      main.appendChild(gp);
    }
    if (S.view === "json") renderJSON(main);
    else if (S.view === "entities") renderEntities(main);
    else renderCanvas(main, mapped);
  }

  function isSel(kind, h, g, v) {
    var s = S.sel;
    return !!s && s.kind === kind && s.h === h && (g === undefined || s.g === g) && (v === undefined || s.v === v);
  }

  function select(sel) {
    S.sel = sel;
    if (S.view === "canvas") renderMain();
    renderProps();
    if (window.matchMedia && window.matchMedia("(max-width: 900px)").matches) {
      S.open.props = true;
      S.els.props.classList.add("tp-open");
    }
  }

  function clickable(n, sel) {
    n.addEventListener("click", function (e) {
      e.stopPropagation();
      select(sel);
    });
    n.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        e.stopPropagation();
        select(sel);
      }
    });
    n.tabIndex = 0;
  }

  // vmChip draws a VM. compact drops the emu-<node>- prefix the default names carry, for the narrow MIG slots.
  function vmChip(h, g, v, vm, mapped, compact) {
    var key = h + "/" + g + "/" + v;
    var label = vm.name || "?";
    var prefix = "emu-" + shortName(node(h).hostname) + "-";
    if (compact && label.indexOf(prefix) === 0 && label.length > prefix.length) label = label.slice(prefix.length);
    var c = el("div", "tp-vm" + (isSel("vm", h, g, v) ? " tp-sel" : "") + (mapped.vms[key] ? " tp-bad" : ""),
      label, { draggable: "true", "data-drag": "vm", "data-h": h, "data-g": g, "data-v": v });
    c.title = mapped.vms[key] ? mapped.vms[key].join("\n") : (vm.name || "") + (vm.ip ? " " + vm.ip : "");
    clickable(c, { kind: "vm", h: h, g: g, v: v });
    return c;
  }

  function renderGPU(h, g, n, gp, mapped) {
    var key = h + "/" + g;
    var cell = el("div", "tp-gpu" + (isSel("gpu", h, g) ? " tp-sel" : "") + (mapped.gpus[key] ? " tp-bad" : ""), null,
      { "data-drop": "gpu", "data-h": h, "data-g": g });
    var head = el("div", "tp-gpu-head", null, { draggable: "true", "data-drag": "gpu", "data-h": h, "data-g": g });
    head.appendChild(el("span", "tp-gpu-idx", "GPU " + gp.index));
    head.appendChild(el("span", "tp-gpu-model", shortModel(gp.model_name)));
    var vms = arr(gp.vms);
    if (n.mode === "VGPU") {
      var max = gp.profile ? gp.profile.max_instances : 0;
      head.appendChild(el("span", "tp-gpu-meta", (gp.profile ? shortModel(gp.profile.name) : "-") + " " + vms.length + "/" + max));
    } else if (n.mode === "MIG") {
      head.appendChild(el("span", "tp-gpu-meta", usedSlices(gp) + "/" + MAX_SLICES));
    }
    clickable(head, { kind: "gpu", h: h, g: g });
    cell.appendChild(head);

    var body = el("div", "tp-gpu-body");
    if (n.mode === "MIG") {
      var bar = el("div", "tp-mig");
      var byGI = {};
      vms.forEach(function (vm, v) { if (vm) byGI[vm.mig_gpu_instance_id] = { vm: vm, v: v }; });
      arr(gp.mig_instances).forEach(function (mi) {
        if (!mi) return;
        var slot = el("div", "tp-slot", null, { "data-drop": "slot", "data-h": h, "data-g": g, "data-gi": mi.gpu_instance_id });
        slot.style.gridColumn = "span " + Math.max(1, Math.min(MAX_SLICES, mi.slices || 1));
        slot.appendChild(el("span", "tp-slot-label", mi.profile + " #" + mi.gpu_instance_id));
        var hit = byGI[mi.gpu_instance_id];
        if (hit) slot.appendChild(vmChip(h, g, hit.v, hit.vm, mapped, true));
        else slot.appendChild(el("span", "tp-seat", tr("seat_empty")));
        bar.appendChild(slot);
      });
      var left = MAX_SLICES - usedSlices(gp);
      if (left > 0) {
        var free = el("div", "tp-slot tp-free", tr("mig_free", { n: left }), { "data-drop": "free", "data-h": h, "data-g": g });
        free.style.gridColumn = "span " + left;
        bar.appendChild(free);
      }
      body.appendChild(bar);
      if (!arr(gp.mig_instances).length) body.appendChild(el("p", "tp-hint", tr("mig_none")));
      vms.forEach(function (vm, v) {
        if (!vm) return;
        var ok = arr(gp.mig_instances).some(function (mi) { return mi && mi.gpu_instance_id === vm.mig_gpu_instance_id; });
        if (!ok) body.appendChild(vmChip(h, g, v, vm, mapped));
      });
    } else {
      var seats = el("div", "tp-seats");
      var total = n.mode === "VGPU" ? Math.max(gp.profile ? gp.profile.max_instances : 0, vms.length) : Math.max(1, vms.length);
      for (var i = 0; i < total; i++) {
        if (vms[i]) seats.appendChild(vmChip(h, g, i, vms[i], mapped));
        else seats.appendChild(el("div", "tp-seat", tr("seat_empty")));
      }
      body.appendChild(seats);
    }
    cell.appendChild(body);
    return cell;
  }

  function renderCanvas(main, mapped) {
    var canvas = el("div", "tp-canvas", null, { "data-drop": "canvas" });
    canvas.addEventListener("click", function (e) { if (e.target === canvas) select(null); });
    var list = nodes();
    if (!list.length) canvas.appendChild(el("p", "tp-hint tp-canvas-empty", tr("canvas_empty")));
    list.forEach(function (n, h) {
      if (!n) {
        canvas.appendChild(el("div", "tp-node tp-bad", "null"));
        return;
      }
      var card = el("div", "tp-node tp-mode-" + n.mode + (isSel("node", h) ? " tp-sel" : "") + (mapped.nodes[h] ? " tp-bad" : ""),
        null, { "data-drop": "node", "data-h": h });
      var head = el("div", "tp-node-head", null, { draggable: "true", "data-drag": "node", "data-h": h });
      head.appendChild(el("span", "tp-tag tp-mode-" + n.mode, modeLabel(n.mode)));
      head.appendChild(el("span", "tp-node-name", n.hostname || "?"));
      head.appendChild(el("span", "tp-node-meta", n.host_ip || ""));
      clickable(head, { kind: "node", h: h });
      card.appendChild(head);
      var gpus = el("div", "tp-gpus");
      arr(n.gpus).forEach(function (gp, g) {
        if (gp) gpus.appendChild(renderGPU(h, g, n, gp, mapped));
      });
      if (!arr(n.gpus).length) gpus.appendChild(el("p", "tp-hint", tr("node_empty")));
      card.appendChild(gpus);
      if (mapped.nodes[h]) {
        var ul = el("ul", "tp-node-problems");
        mapped.nodes[h].forEach(function (m) { ul.appendChild(el("li", null, m)); });
        card.appendChild(ul);
      }
      canvas.appendChild(card);
    });
    main.appendChild(canvas);
  }

  function renderJSON(main) {
    var wrap = el("div", "tp-jsonview");
    wrap.appendChild(el("p", "tp-hint", tr("json_help")));
    var ta = el("textarea", "tp-json", null, { spellcheck: "false", "aria-label": "JSON" });
    ta.value = JSON.stringify(S.topo, null, 2);
    ta.addEventListener("input", function () { S.jsonEdited = true; });
    S.els.json = wrap;
    wrap.appendChild(ta);
    var err = el("p", "tp-bad-text", S.jsonError);
    err.hidden = !S.jsonError;
    wrap.appendChild(err);
    var actions = el("div", "tp-actions");
    actions.appendChild(btn(tr("json_apply"), "btn-primary", function () {
      var parsed;
      try {
        parsed = JSON.parse(ta.value);
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("not an object");
      } catch (e) {
        S.jsonError = tr("json_invalid", { error: e.message });
        err.textContent = S.jsonError;
        err.hidden = false;
        return;
      }
      S.jsonError = "";
      markByIDs(parsed, JSON.parse(S.baseline), S.fromServer);
      S.topo = parsed;
      S.sel = null;
      changed();
      toast(tr("json_applied"), "ok");
    }));
    wrap.appendChild(actions);
    main.appendChild(wrap);
  }

  function entityRows() {
    var rows = [];
    nodes().forEach(function (n) {
      if (!n) return;
      rows.push({ kind: tr("ent_node"), name: n.hostname, id: entityID(n.hostname, n.entity_id), custom: !!n.entity_id });
      arr(n.gpus).forEach(function (g) {
        arr(g && g.vms).forEach(function (v) {
          if (v) rows.push({ kind: tr("ent_vm"), name: v.name, id: entityID(v.name, v.entity_id), custom: !!v.entity_id });
        });
      });
    });
    return rows;
  }

  function renderEntities(main) {
    var wrap = el("div", "tp-entities");
    var rows = entityRows();
    var head = el("div", "tp-actions");
    head.appendChild(el("p", "tp-hint", tr("ent_help")));
    head.appendChild(btn(tr("ent_copy"), "btn-small", function () {
      var text = rows.map(function (r) { return r.name + "\t" + r.id; }).join("\n");
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { toast(tr("copied"), "ok"); }, function () {});
      }
    }));
    wrap.appendChild(head);
    var tw = el("div", "table-wrap");
    var table = el("table", "grid tp-table");
    var thr = el("tr");
    ["ent_kind", "ent_name", "ent_id", "ent_source"].forEach(function (k) { thr.appendChild(el("th", null, tr(k))); });
    var thead = el("thead");
    thead.appendChild(thr);
    table.appendChild(thead);
    var tb = el("tbody");
    rows.forEach(function (r) {
      var tr_ = el("tr");
      tr_.appendChild(el("td", null, r.kind));
      tr_.appendChild(el("td", null, r.name));
      var td = el("td");
      td.appendChild(el("code", null, r.id));
      tr_.appendChild(td);
      tr_.appendChild(el("td", r.custom ? "tp-custom" : "tp-muted", tr(r.custom ? "ent_custom" : "ent_default")));
      tb.appendChild(tr_);
    });
    table.appendChild(tb);
    tw.appendChild(table);
    wrap.appendChild(tw);
    main.appendChild(wrap);
  }

  // ---- properties ----

  // field adds one labelled input. get/set read and write the value; set gets a number for type "number".
  function field(box, o) {
    var id = "tp-f-" + o.key;
    var f = el("div", "field tp-field");
    f.appendChild(el("label", null, o.label, { for: id }));
    var input = el("input", null, null, {
      id: id, type: o.type || "text", autocomplete: "off", spellcheck: "false",
      "data-key": o.key, placeholder: o.placeholder || ""
    });
    var v = o.get();
    input.value = v === undefined || v === null ? "" : String(v);
    if (o.ro) input.readOnly = true;
    else input.addEventListener("input", function () {
      var val = input.value;
      if (o.type === "number") {
        val = input.value.trim() === "" ? 0 : Number(input.value);
        if (!isFinite(val)) return;
        val = Math.trunc(val);
      }
      o.set(val);
      changed(true);
    });
    f.appendChild(input);
    if (o.hint) f.appendChild(el("p", "hint", o.hint));
    box.appendChild(f);
    return input;
  }

  function selectField(box, key, label, options, current, onChange) {
    var id = "tp-f-" + key;
    var f = el("div", "field tp-field");
    f.appendChild(el("label", null, label, { for: id }));
    var s = el("select", null, null, { id: id, "data-key": key });
    options.forEach(function (o) {
      var op = el("option", null, o.label, { value: o.value });
      if (o.value === current) op.selected = true;
      s.appendChild(op);
    });
    s.addEventListener("change", function () { onChange(s.value); });
    f.appendChild(s);
    box.appendChild(f);
    return s;
  }

  function entityField(box, name, obj) {
    field(box, {
      key: "entity_id", label: tr("f_entity_id"),
      placeholder: entityID(name, ""),
      hint: tr("f_entity_hint", { id: entityID(name, "") }),
      get: function () { return obj.entity_id || ""; },
      set: function (v) {
        if (v.trim()) obj.entity_id = v.trim();
        else delete obj.entity_id;
      }
    });
  }

  function renderProps(keepFocus) {
    var box = S.els.propsBody;
    if (!box) return;
    var focus = null;
    var ae = document.activeElement;
    if (keepFocus && ae && box.contains(ae) && ae.dataset.key) {
      focus = { key: ae.dataset.key, start: ae.selectionStart, end: ae.selectionEnd };
    }
    clear(box);
    if (!S.topo) return;
    var s = S.sel;
    var n = s ? node(s.h) : null;
    var gp = s && s.g !== undefined ? gpu(s.h, s.g) : null;
    var vm = s && s.kind === "vm" && gp ? arr(gp.vms)[s.v] : null;
    if (!s || !n || (s.kind !== "node" && !gp) || (s.kind === "vm" && !vm)) {
      S.sel = null;
      renderProject(box);
    } else if (s.kind === "node") renderNodeProps(box, s.h, n);
    else if (s.kind === "gpu") renderGPUProps(box, s.h, s.g, n, gp);
    else renderVMProps(box, s, n, gp, vm);
    if (focus) {
      var f = box.querySelector('[data-key="' + focus.key + '"]');
      if (f) {
        f.focus();
        try { f.setSelectionRange(focus.start, focus.end); } catch (e) { /* not a text input */ }
      }
    }
  }

  function renderProject(box) {
    box.appendChild(el("p", "tp-hint", tr("p_none")));
    box.appendChild(el("h3", "tp-sec-title", tr("p_project")));
    var t = S.topo;
    [["region", "f_region"], ["domain_id", "f_domain_id"], ["project_name", "f_project_name"], ["user_name", "f_user_name"]]
      .forEach(function (k) {
        field(box, { key: k[0], label: tr(k[1]), get: function () { return t[k[0]]; }, set: function (v) { t[k[0]] = v; } });
      });
  }

  function auto(v) { return v ? v : undefined; }

  function renderNodeProps(box, h, n) {
    box.appendChild(el("h3", "tp-sec-title", tr("p_node")));
    field(box, { key: "hostname", label: tr("f_hostname"), get: function () { return n.hostname; }, set: function (v) { renameNode(n, v, S.fromServer.has(n)); } });
    field(box, { key: "mode", label: tr("f_mode"), ro: true, hint: tr("f_mode_hint"), get: function () { return modeLabel(n.mode); } });
    field(box, { key: "id", label: tr("f_id"), ro: true, placeholder: tr("auto"), get: function () { return auto(n.id); } });
    field(box, { key: "uuid", label: tr("f_uuid"), ro: true, placeholder: tr("auto"), get: function () { return n.uuid; } });
    field(box, { key: "host_ip", label: tr("f_host_ip"), placeholder: tr("auto"), get: function () { return n.host_ip; }, set: function (v) { n.host_ip = v.trim(); } });
    [["vcpus", "f_vcpus"], ["memory_mb", "f_memory_mb"], ["local_gb", "f_local_gb"]].forEach(function (k) {
      field(box, { key: k[0], label: tr(k[1]), type: "number", placeholder: tr("auto"), get: function () { return auto(n[k[0]]); }, set: function (v) { n[k[0]] = v; } });
    });
    entityField(box, n.hostname, n);

    var models = arr(S.catalog && S.catalog.gpu_models).filter(function (m) { return arr(m.modes).indexOf(n.mode) >= 0; });
    if (models.length) {
      var row = el("div", "tp-actions");
      var sel = el("select", null, null, { "aria-label": tr("f_model") });
      models.forEach(function (m) { sel.appendChild(el("option", null, shortModel(m.name), { value: m.name })); });
      row.appendChild(sel);
      row.appendChild(btn(tr("add_gpu"), "btn-small", function () {
        addGPU(h, sel.value);
        changed();
      }));
      box.appendChild(row);
    }
    var del = el("div", "tp-actions");
    del.appendChild(btn(tr("delete_node"), "btn-small tp-danger", function () { remove({ kind: "node", h: h }); changed(); }));
    box.appendChild(del);
  }

  function renderGPUProps(box, h, g, n, gp) {
    box.appendChild(el("h3", "tp-sec-title", tr("p_gpu") + " " + gp.index + " (" + n.hostname + ")"));
    field(box, { key: "model", label: tr("f_model"), ro: true, get: function () { return gp.model_name; } });
    field(box, { key: "uuid", label: tr("f_uuid"), ro: true, placeholder: tr("auto"), get: function () { return gp.uuid; } });
    field(box, { key: "bus_id", label: tr("f_bus_id"), placeholder: tr("auto"), get: function () { return gp.bus_id; },
      set: function (v) { gp.bus_id = v.trim(); gp.host_bdf = ""; } });
    field(box, { key: "memory_mib", label: tr("f_memory_mib"), ro: true, placeholder: tr("auto"), get: function () { return auto(gp.memory_mib); } });
    var m = modelOf(S.catalog, gp.model_name);

    if (n.mode === "VGPU") {
      var profs = arr(m && m.vgpu_profiles);
      var cur = gp.profile ? gp.profile.name : "";
      var opts = profs.map(function (p) { return { value: p.name, label: shortModel(p.name) + " (" + p.framebuffer_mb + " MB, " + p.max_instances + ")" }; });
      if (cur && !profs.some(function (p) { return p.name === cur; })) opts.unshift({ value: cur, label: tr("custom_profile", { name: cur }) });
      if (!cur) opts.unshift({ value: "", label: "-" });
      selectField(box, "vgpu_profile", tr("f_vgpu_profile"), opts, cur, function (val) {
        var p = profs.filter(function (x) { return x.name === val; })[0];
        if (!p) return;
        gp.profile = profileCopy(p);
        arr(gp.vms).forEach(function (v) { if (v) resetFlavor(v); });
        changed();
      });
      if (gp.profile) {
        field(box, { key: "max_instances", label: tr("f_max_instances"), type: "number",
          get: function () { return gp.profile.max_instances; }, set: function (v) { gp.profile.max_instances = v; } });
      }
    }

    if (n.mode === "MIG") {
      box.appendChild(el("h4", "tp-sub", tr("f_mig_instances", { used: usedSlices(gp), max: MAX_SLICES })));
      var table = el("table", "grid tp-table");
      var thr = el("tr");
      ["col_gi", "col_profile", "col_vm", ""].forEach(function (k) { thr.appendChild(el("th", null, k ? tr(k) : "")); });
      var thead = el("thead");
      thead.appendChild(thr);
      table.appendChild(thead);
      var tb = el("tbody");
      arr(gp.mig_instances).forEach(function (mi, i) {
        if (!mi) return;
        var user = arr(gp.vms).filter(function (v) { return v && v.mig_gpu_instance_id === mi.gpu_instance_id; })[0];
        var row = el("tr");
        row.appendChild(el("td", null, String(mi.gpu_instance_id)));
        row.appendChild(el("td", null, mi.profile));
        row.appendChild(el("td", null, user ? user.name : "-"));
        var td = el("td");
        var rb = btn(tr("remove"), "btn-small", function () {
          gp.mig_instances.splice(i, 1);
          changed();
        });
        if (user) {
          rb.disabled = true;
          rb.title = tr("instance_used");
        }
        td.appendChild(rb);
        row.appendChild(td);
        tb.appendChild(row);
      });
      table.appendChild(tb);
      box.appendChild(table);
      var fit = fittingProfiles(S.catalog, gp);
      if (fit.length) {
        var row = el("div", "tp-actions");
        var sel = el("select", null, null, { "aria-label": tr("col_profile") });
        fit.forEach(function (p) { sel.appendChild(el("option", null, p.name, { value: p.name })); });
        row.appendChild(sel);
        row.appendChild(btn(tr("add_instance"), "btn-small", function () {
          var p = fit.filter(function (x) { return x.name === sel.value; })[0];
          if (!gp.mig_instances) gp.mig_instances = [];
          gp.mig_instances.push(newInstance(gp, p));
          changed();
        }));
        box.appendChild(row);
      }
    }

    var actions = el("div", "tp-actions");
    var verdict = acceptVM(S.topo, S.catalog, { h: h, g: g }, { kind: "new-vm" });
    var ab = btn(tr("add_vm"), "btn-small", function () {
      addVM(h, g, verdict);
      changed();
    });
    if (!verdict.ok) {
      ab.disabled = true;
      ab.title = tr("r_" + verdict.reason, verdict.params);
    }
    actions.appendChild(ab);
    actions.appendChild(btn(tr("delete_gpu"), "btn-small tp-danger", function () { remove({ kind: "gpu", h: h, g: g }); changed(); }));
    box.appendChild(actions);
  }

  function renderVMProps(box, s, n, gp, vm) {
    box.appendChild(el("h3", "tp-sec-title", tr("p_vm")));
    field(box, { key: "name", label: tr("f_name"), get: function () { return vm.name; }, set: function (v) { renameVM(vm, v, S.fromServer.has(vm)); } });
    field(box, { key: "id", label: tr("f_id"), ro: true, placeholder: tr("auto"), get: function () { return vm.id; } });
    field(box, { key: "instance_name", label: tr("f_instance_name"), ro: true, placeholder: tr("auto"), get: function () { return vm.instance_name; } });
    field(box, { key: "ip", label: tr("f_ip"), placeholder: tr("auto"), get: function () { return vm.ip; }, set: function (v) { vm.ip = v.trim(); } });
    var fl = flavorByID(vm.flavor_id);
    field(box, { key: "flavor", label: tr("f_flavor"), ro: true, placeholder: tr("auto"), get: function () { return fl ? fl.name : vm.flavor_id; } });
    [["vcpus", "f_vcpus"], ["memory_mb", "f_memory_mb"], ["disk_gb", "f_disk_gb"]].forEach(function (k) {
      field(box, { key: k[0], label: tr(k[1]), type: "number", placeholder: tr("auto"), get: function () { return auto(vm[k[0]]); }, set: function (v) { vm[k[0]] = v; } });
    });
    if (n.mode === "MIG") {
      var taken = {};
      arr(gp.vms).forEach(function (v) { if (v && v !== vm) taken[v.mig_gpu_instance_id] = true; });
      var opts = arr(gp.mig_instances).filter(function (mi) { return mi && !taken[mi.gpu_instance_id]; })
        .map(function (mi) { return { value: String(mi.gpu_instance_id), label: mi.profile + " #" + mi.gpu_instance_id }; });
      if (!opts.some(function (o) { return o.value === String(vm.mig_gpu_instance_id); })) {
        opts.unshift({ value: String(vm.mig_gpu_instance_id), label: "-" });
      }
      selectField(box, "mig", tr("f_mig_instance"), opts, String(vm.mig_gpu_instance_id), function (val) {
        vm.mig_gpu_instance_id = +val;
        resetFlavor(vm);
        changed();
      });
    }
    entityField(box, vm.name, vm);
    var actions = el("div", "tp-actions");
    actions.appendChild(btn(tr("delete_vm"), "btn-small tp-danger", function () { remove({ kind: "vm", h: s.h, g: s.g, v: s.v }); changed(); }));
    box.appendChild(actions);
  }

  // ---- mount ----

  function pane(cls, titleKey, openKey) {
    var p = el("aside", "tp-pane " + cls);
    var head = el("div", "tp-pane-head");
    var title = el("h2", "tp-pane-title", tr(titleKey));
    var toggle = btn(tr(titleKey), "btn-small tp-toggle", function () {
      S.open[openKey] = !S.open[openKey];
      p.classList.toggle("tp-open", S.open[openKey]);
      toggle.setAttribute("aria-expanded", String(S.open[openKey]));
    });
    toggle.setAttribute("aria-expanded", "false");
    head.appendChild(title);
    head.appendChild(toggle);
    p.appendChild(head);
    var body = el("div", "tp-pane-body");
    p.appendChild(body);
    p._title = title;
    p._toggle = toggle;
    p._key = titleKey;
    return p;
  }

  function relabelPanes() {
    [S.els.palette, S.els.props].forEach(function (p) {
      p._title.textContent = tr(p._key);
      p._toggle.textContent = tr(p._key);
    });
  }

  function mount(rootEl) {
    if (!rootEl || S.root) return;
    S.root = rootEl;
    rootEl.classList.add("tp");
    clear(rootEl);
    S.els.toolbar = el("div", "tp-toolbar");
    var body = el("div", "tp-body");
    S.els.palette = pane("tp-palette", "palette", "palette");
    S.els.paletteBody = S.els.palette.lastChild;
    S.els.main = el("div", "tp-main");
    S.els.props = pane("tp-props", "properties", "props");
    S.els.propsBody = S.els.props.lastChild;
    body.appendChild(S.els.palette);
    body.appendChild(S.els.main);
    body.appendChild(S.els.props);
    S.els.hint = el("div", "tp-drag-hint", null, { role: "status" });
    S.els.hint.hidden = true;
    rootEl.appendChild(S.els.toolbar);
    rootEl.appendChild(body);
    rootEl.appendChild(S.els.hint);
    bindDrag(rootEl);
    var g = gm();
    if (g && g.onLangChange) g.onLangChange(function () {
      relabelPanes();
      renderAll();
    });
    window.addEventListener("beforeunload", function (e) {
      if (dirty()) {
        e.preventDefault();
        e.returnValue = tr("leave_confirm");
      }
    });
    load();
  }

  root.GMRG_TOPOLOGY = { mount: mount };
})(typeof window !== "undefined" ? window : globalThis);
