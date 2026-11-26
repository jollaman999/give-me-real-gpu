// record.mjs drives the settings UI in a running headless Chrome over the DevTools protocol and records it with a
// screencast while it goes through the scenes: Chrome sends a frame whenever the page changes, at most fps frames a
// second are kept. It writes the PNG frames and an ffconcat list (frames.txt) with the real time between frames
// into the frames directory. No npm packages: it needs Node 20 with WebSocket
// (pass --experimental-websocket on Node 20) and fetch.
//
//   node record.mjs --devtools-port 9222 --url http://127.0.0.1:27780/ui/ --frames DIR [--fps 10]
//
// Elements are found by id, URL hash and the app's data attributes first and by visible text only where the page
// has nothing else, so the script keeps working when the look of the page changes. A caption bar at the bottom of
// the page says what each scene shows, in the page language.
//
// DEMO_SCENES picks the scenes to record, comma separated, in this order: status, general, sinks, topology.
// The default is all of them.

import { writeFile } from "node:fs/promises";
import { join } from "node:path";

const args = parseArgs(process.argv.slice(2));
const DEVTOOLS_PORT = need(args, "devtools-port");
const URL_BASE = need(args, "url");
const FRAMES = need(args, "frames");
const FPS = Number(args.fps || 10);
const WIDTH = Number(args.width || 1280);
const HEIGHT = Number(args.height || 800);
const LANG_KEY = process.env.DEMO_LANG_KEY || "gmrg.lang";
const THEME_KEY = process.env.DEMO_THEME_KEY || "gmrg.theme";
// LANG is the language of the page and the captions: "en" or "ko".
const LANG = process.env.DEMO_LANG || "en";
const LOCALE = LANG === "ko" ? "ko-KR" : "en-US";
const SCENES = ["status", "general", "sinks", "topology"];
const WANT = (process.env.DEMO_SCENES || SCENES.join(",")).split(",").map((s) => s.trim()).filter(Boolean);

// CAPTIONS are the lines of the caption bar. Keep each one short enough for two lines at most.
const CAPTIONS = {
  en: {
    status: "Status: what to register in the management platform, and lines per sink",
    general: "General: change a setting and save. It applies without a restart",
    sinks_add: "Add sink: the fields follow the type, InfluxDB v1, v2, v3 or OTLP",
    sinks_rules: "Tag rules: rename, drop or add tag keys per sink before writing",
    topo_drag: "Topology: drag a VM onto a GPU. A GPU with no room refuses it",
    topo_save: "Save: the fake OpenStack API serves the new topology right away"
  },
  ko: {
    status: "상태: 관리 플랫폼에 등록할 값과 sink 별 줄 수",
    general: "일반: 값을 바꿔 저장하면 재시작 없이 반영됩니다",
    sinks_add: "Sink 추가: 종류와 버전(InfluxDB v1, v2, v3, OTLP)에 맞춰 칸이 바뀝니다",
    sinks_rules: "태그 규칙: sink 마다 태그 키를 바꾸거나 빼거나 더해서 보냅니다",
    topo_drag: "토폴로지: VM 을 GPU 에 끌어다 놓습니다. 자리가 없는 GPU 는 거부합니다",
    topo_save: "저장: 가짜 OpenStack API 가 새 토폴로지를 바로 내보냅니다"
  }
};

function parseArgs(list) {
  const out = {};
  for (let i = 0; i < list.length; i++) {
    if (!list[i].startsWith("--")) throw new Error("unexpected argument " + list[i]);
    out[list[i].slice(2)] = list[i + 1];
    i++;
  }
  return out;
}

function need(a, k) {
  if (!a[k]) throw new Error("--" + k + " is required");
  return a[k];
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
// PACE stretches the pauses and cursor moves of the scenes so a viewer can follow them. 1 is the fastest that still works.
const PACE = Math.max(1, Number(process.env.DEMO_PACE || 2.2));
const hold = (ms) => sleep(ms * PACE);
// READ_MS is how long a new caption stays before the scene goes on, before PACE.
const READ_MS = 1100;
const log = (msg) => process.stderr.write("[record] " + msg + "\n");

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.next = 1;
    this.pending = new Map();
    this.handlers = new Map();
    ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id === undefined) {
        const h = this.handlers.get(msg.method);
        if (h) h(msg.params);
        return;
      }
      const p = this.pending.get(msg.id);
      if (!p) return;
      this.pending.delete(msg.id);
      if (msg.error) p.reject(new Error(p.method + ": " + msg.error.message));
      else p.resolve(msg.result);
    });
    ws.addEventListener("close", () => {
      for (const p of this.pending.values()) p.reject(new Error("DevTools connection closed"));
      this.pending.clear();
    });
  }

  static connect(url) {
    return new Promise((resolve, reject) => {
      const ws = new WebSocket(url);
      ws.addEventListener("open", () => resolve(new CDP(ws)), { once: true });
      ws.addEventListener("error", () => reject(new Error("cannot connect to " + url)), { once: true });
    });
  }

  send(method, params = {}) {
    const id = this.next++;
    this.ws.send(JSON.stringify({ id, method, params }));
    return new Promise((resolve, reject) => this.pending.set(id, { resolve, reject, method }));
  }

  on(method, fn) { this.handlers.set(method, fn); }

  close() { this.ws.close(); }
}

let cdp;

async function evaluate(expr) {
  const r = await cdp.send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) {
    const d = r.exceptionDetails;
    throw new Error("page: " + ((d.exception && d.exception.description) || d.text));
  }
  return r.result.value;
}

async function waitFor(what, expr, ms = 10000) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await evaluate(expr);
    if (v) return v;
    if (Date.now() > end) throw new Error("timed out waiting for " + what);
    await sleep(50);
  }
}

// waitForSinkLines waits until every sink in the status API reports a line count, so the Status tab shows numbers.
async function waitForSinkLines() {
  const statusURL = new URL("/api/v1/status", URL_BASE).href;
  const end = Date.now() + 30000;
  for (;;) {
    try {
      const st = await (await fetch(statusURL)).json();
      const sinks = st.sinks || [];
      if (sinks.length && sinks.every((s) => !s.enabled || s.lines > 0)) return;
    } catch (e) { /* not up yet */ }
    if (Date.now() > end) throw new Error("timed out waiting for the sinks to report lines");
    await sleep(200);
  }
}

// PAGE_HELPERS runs in the page. It draws a cursor and a caption bar in the top layer (above open dialogs) and keeps
// the elements the scenes work on. The page's CSP blocks style attributes, so the styles are set through CSSOM.
const PAGE_HELPERS = `(() => {
  if (window.__demo) return true;
  const ns = "http://www.w3.org/2000/svg";
  const cur = document.createElement("div");
  cur.setAttribute("popover", "manual");
  cur.style.cssText = "position:fixed;inset:auto;left:0;top:0;margin:0;padding:0;border:0;background:transparent;" +
    "overflow:visible;pointer-events:none;width:24px;height:24px;transform:translate(-100px,-100px);";
  const svg = document.createElementNS(ns, "svg");
  svg.setAttribute("width", "22");
  svg.setAttribute("height", "26");
  svg.setAttribute("viewBox", "0 0 22 26");
  svg.style.cssText = "position:absolute;left:0;top:0;filter:drop-shadow(0 1px 2px rgba(0,0,0,.6))";
  const path = document.createElementNS(ns, "path");
  path.setAttribute("d", "M2 2 L2 21 L7 16.5 L10.5 24 L13.5 22.6 L10 15.2 L17 15.2 Z");
  path.setAttribute("fill", "#fff");
  path.setAttribute("stroke", "#111");
  path.setAttribute("stroke-width", "1.5");
  path.setAttribute("stroke-linejoin", "round");
  svg.appendChild(path);
  const ring = document.createElement("div");
  ring.style.cssText = "position:absolute;left:-14px;top:-14px;width:28px;height:28px;border-radius:50%;" +
    "border:3px solid #ffb020;opacity:0;transition:opacity .2s, transform .2s;transform:scale(.4)";
  const chip = document.createElement("div");
  chip.style.cssText = "position:absolute;left:18px;top:20px;padding:2px 8px;border-radius:6px;font:600 12px system-ui,sans-serif;" +
    "background:#ffb020;color:#111;white-space:nowrap;display:none;box-shadow:0 2px 6px rgba(0,0,0,.4)";
  cur.append(ring, svg, chip);
  const cap = document.createElement("div");
  cap.setAttribute("popover", "manual");
  cap.style.cssText = "position:fixed;inset:auto;left:50%;bottom:26px;top:auto;right:auto;transform:translateX(-50%);" +
    "margin:0;border:0;overflow:hidden;pointer-events:none;box-sizing:border-box;width:max-content;max-width:820px;" +
    "padding:10px 22px;border-radius:14px;background:rgba(10,12,18,.84);color:#fff;text-align:center;" +
    "font:500 18px/1.45 system-ui,'Noto Sans CJK KR','Noto Sans KR',sans-serif;letter-spacing:.01em;" +
    "box-shadow:0 6px 24px rgba(0,0,0,.45);text-wrap:balance;";
  document.documentElement.append(cap, cur);
  const visible = (el) => {
    if (!el || !el.isConnected) return false;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return false;
    const cs = getComputedStyle(el);
    return cs.visibility !== "hidden" && cs.display !== "none";
  };
  const norm = (s) => (s || "").replace(/\\s+/g, " ").trim().toLowerCase();
  const show = (p) => { if (p.matches(":popover-open")) p.hidePopover(); p.showPopover(); };
  window.__demo = {
    refs: {},
    cursor(x, y) {
      show(cur);
      cur.style.transform = "translate(" + x + "px," + y + "px)";
    },
    press(on) {
      ring.style.opacity = on ? "1" : "0";
      ring.style.transform = on ? "scale(1)" : "scale(.4)";
    },
    carry(text) {
      chip.textContent = text || "";
      chip.style.display = text ? "block" : "none";
    },
    // caption shows text in the caption bar, or hides the bar when text is empty.
    caption(text) {
      cap.textContent = text || "";
      if (text) show(cap); else if (cap.matches(":popover-open")) cap.hidePopover();
      if (cur.matches(":popover-open")) show(cur);
      const r = cap.getBoundingClientRect();
      return { lines: Math.round((r.height - 20) / 26), bg: getComputedStyle(cap).backgroundColor };
    },
    // raise puts the caption and the cursor back on top, after a dialog opened above them.
    raise() {
      if (cap.textContent) show(cap);
      if (cur.matches(":popover-open")) show(cur);
    },
    hasDialog() { return !!document.querySelector("dialog[open]"); },
    // set keeps el under name and tells whether it is on the page.
    set(name, el) {
      this.refs[name] = visible(el) ? el : null;
      return !!this.refs[name];
    },
    box(name) {
      const el = this.refs[name];
      if (!visible(el)) return null;
      const r = el.getBoundingClientRect();
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2), w: r.width, h: r.height };
    },
    // reveal scrolls smoothly, as little as it can, so the element is above the caption bar, and returns the
    // distance. It scrolls the page, or the open dialog, first; inner is for a pane that scrolls on its own and
    // stays in place (sticky) when the page scrolls.
    reveal(name, inner) {
      const el = this.refs[name];
      let box = null;
      for (let a = el.parentElement; a && a !== document.body; a = a.parentElement) {
        const cs = getComputedStyle(a);
        if (/(auto|scroll)/.test(cs.overflowY) && a.scrollHeight > a.clientHeight) {
          box = a;
          if (inner) break;
        }
      }
      if (!inner && box && !box.matches("dialog")) box = null;
      const lim = box ? box.getBoundingClientRect() : { top: 0, bottom: innerHeight };
      const top = Math.max(lim.top, 0) + 16, bottom = Math.min(lim.bottom, innerHeight - 120);
      const r = el.getBoundingClientRect();
      let dy = 0;
      if (r.top < top) dy = r.top - top;
      else if (r.bottom > bottom) dy = Math.min(r.bottom - bottom, r.top - top);
      if (!dy) return 0;
      box = box || document.scrollingElement;
      const goal = Math.max(0, Math.min(box.scrollTop + dy, box.scrollHeight - box.clientHeight));
      if (Math.abs(goal - box.scrollTop) < 1) return 0;
      this.goal = { box, top: goal };
      box.scrollTo({ top: goal, behavior: "smooth" });
      return dy;
    },
    // home scrolls the page smoothly to the top and returns the distance.
    home() {
      const box = document.scrollingElement;
      if (box.scrollTop < 1) return 0;
      this.goal = { box, top: 0 };
      box.scrollTo({ top: 0, behavior: "smooth" });
      return box.scrollTop;
    },
    // scrolled tells whether the last reveal reached its place.
    scrolled() {
      return !this.goal || Math.abs(this.goal.box.scrollTop - this.goal.top) < 1;
    },
    // rowButton finds the button whose text matches re in the table row that names the sink.
    rowButton(table, name, reSrc) {
      const re = new RegExp(reSrc, "i");
      for (const tr of document.querySelectorAll("#" + table + " tbody tr")) {
        const strong = tr.querySelector("strong");
        if (!strong || norm(strong.textContent) !== norm(name)) continue;
        return [...tr.querySelectorAll("button")].find((b) => re.test(norm(b.textContent))) || null;
      }
      return null;
    },
    // choose sets a select the way a user would and tells the page it changed.
    choose(name, value) {
      const el = this.refs[name];
      el.value = value;
      el.dispatchEvent(new Event("input", { bubbles: true }));
      el.dispatchEvent(new Event("change", { bubbles: true }));
      return el.value === value;
    },
    // gpuFor finds the first GPU, other than the one the item is on, that takes (want true) or refuses (want false)
    // the item under the name src, by asking the page's dragover handlers.
    gpuFor(src, want) {
      const s = this.refs[src];
      const dt = new DataTransfer();
      s.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      let found = null;
      for (const t of document.querySelectorAll('[data-drop="gpu"]')) {
        if (!visible(t) || t.contains(s)) continue;
        const r = t.getBoundingClientRect();
        const ev = new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer: dt,
          clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 });
        t.dispatchEvent(ev);
        if (ev.defaultPrevented === want) { found = t; break; }
      }
      s.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
      return found;
    },
    dragStart(name) {
      this.src = this.refs[name];
      this.dt = new DataTransfer();
      this.over = null;
      this.carry(this.src.textContent.trim());
      this.src.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: this.dt }));
    },
    dragOver(x, y) {
      const t = document.elementFromPoint(x, y);
      if (!t) return false;
      const init = { bubbles: true, cancelable: true, dataTransfer: this.dt, clientX: x, clientY: y };
      if (t !== this.over) {
        if (this.over) this.over.dispatchEvent(new DragEvent("dragleave", { ...init, relatedTarget: t }));
        t.dispatchEvent(new DragEvent("dragenter", init));
        this.over = t;
      }
      const ev = new DragEvent("dragover", init);
      t.dispatchEvent(ev);
      return ev.defaultPrevented;
    },
    // dragEnd drops at (x, y) when drop is true, the way a browser does on a target that took the drag, and ends the drag.
    dragEnd(x, y, drop) {
      const t = document.elementFromPoint(x, y);
      const init = { bubbles: true, cancelable: true, dataTransfer: this.dt, clientX: x, clientY: y };
      if (drop && t) t.dispatchEvent(new DragEvent("drop", init));
      this.src.dispatchEvent(new DragEvent("dragend", init));
      this.carry("");
    }
  };
  return true;
})()`;

let mouse = { x: WIDTH / 2, y: HEIGHT / 2 };

async function cursor(x, y) {
  await evaluate(`window.__demo.cursor(${x}, ${y})`);
}

// moveTo moves the cursor to pt in about ms milliseconds. The steps follow the clock, so a slow page makes fewer
// steps and not a longer move.
async function moveTo(pt, ms = 380, onStep) {
  ms *= Math.sqrt(PACE);
  const from = { ...mouse };
  const start = performance.now();
  for (let done = false; !done;) {
    const k = Math.min(1, (performance.now() - start + 30) / ms);
    done = k >= 1;
    const e = k < 0.5 ? 2 * k * k : 1 - Math.pow(-2 * k + 2, 2) / 2;
    const x = Math.round(from.x + (pt.x - from.x) * e);
    const y = Math.round(from.y + (pt.y - from.y) * e);
    if (onStep) await onStep(x, y);
    else await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
    await cursor(x, y);
    if (!done) await sleep(20);
  }
  mouse = { x: pt.x, y: pt.y };
}

// press shows the click ring at pt. real sends the mouse button too; selects get only the ring, because a real click
// opens the native list, which a screenshot does not show.
async function press(pt, real = true) {
  await moveTo(pt);
  await evaluate("window.__demo.press(true)");
  if (real) {
    await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: pt.x, y: pt.y, button: "left", clickCount: 1 });
    await sleep(70);
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: pt.x, y: pt.y, button: "left", clickCount: 1 });
  }
  await sleep(180);
  await evaluate("window.__demo.press(false)");
  await cursor(pt.x, pt.y);
}

// pick keeps the element that expr finds in the page under name. It fails with what when there is none.
async function pick(name, expr, what) {
  if (!(await evaluate(`window.__demo.set(${JSON.stringify(name)}, ${expr})`))) throw new Error("not on the page: " + what);
}

// show scrolls the element under name into view and returns its center. innerFirst scrolls the pane the element is
// in before the page.
async function show(name, innerFirst = false) {
  for (const inner of innerFirst ? [true, false] : [false, true]) {
    if (await evaluate(`window.__demo.reveal(${JSON.stringify(name)}, ${inner})`)) {
      await waitFor("the scroll to " + name, "window.__demo.scrolled()", 3000);
      await sleep(60);
    }
  }
  const b = await evaluate(`window.__demo.box(${JSON.stringify(name)})`);
  if (!b) throw new Error(name + " is not on the screen");
  return b;
}

async function clickOn(name, real = true) {
  await press(await show(name), real);
}

async function typeText(text) {
  for (const ch of text) {
    await cdp.send("Input.insertText", { text: ch });
    await sleep(70);
  }
}

async function caption(key) {
  const text = CAPTIONS[LANG][key];
  const r = await evaluate(`window.__demo.caption(${JSON.stringify(text)})`);
  if (r.lines > 2) log(`caption ${key} takes ${r.lines} lines`);
  if (r.bg !== "rgba(10, 12, 18, 0.84)") log(`caption ${key} has background ${r.bg}; the page did not take its style`);
  await hold(READ_MS);
}

async function openTab(name) {
  await evaluate("window.__demo.caption('')");
  await pick("tab", `document.querySelector('[data-tab="${name}"]')`, "the " + name + " tab");
  await clickOn("tab");
  await waitFor(`#${name}`, `location.hash === ${JSON.stringify("#" + name)}`, 3000);
}

const frames = [];
const writes = [];
let capturing = false;
let held = null;
let endT = 0;

// keep writes a screencast frame. A frame that comes sooner than 1/fps after the last kept one waits in held and
// replaces the one waiting before it, so the last look of the page before a pause is never lost.
function keep(f) {
  const name = "f" + String(frames.length).padStart(5, "0") + ".png";
  writes.push(writeFile(join(FRAMES, name), Buffer.from(f.data, "base64")));
  frames.push({ name, t: f.t });
}

async function startCapture() {
  const period = 1000 / FPS;
  cdp.on("Page.screencastFrame", (p) => {
    cdp.send("Page.screencastFrameAck", { sessionId: p.sessionId }).catch(() => {});
    if (!capturing) return;
    const f = { data: p.data, t: p.metadata.timestamp * 1000 };
    const last = frames.length ? frames[frames.length - 1].t : -Infinity;
    if (f.t - last >= period) {
      held = null;
      keep(f);
    } else {
      held = f;
      setTimeout(() => {
        if (held === f && capturing) {
          held = null;
          keep({ data: f.data, t: last + period });
        }
      }, period);
    }
  });
  capturing = true;
  await cdp.send("Page.startScreencast", { format: "png", maxWidth: WIDTH, maxHeight: HEIGHT, everyNthFrame: 1 });
  for (let i = 0; i < 50 && !frames.length; i++) await sleep(50);
  if (!frames.length) throw new Error("the screencast sent no frame");
}

async function stopCapture() {
  await sleep(1000 / FPS);
  if (held) keep(held);
  endT = Math.max(frames[frames.length - 1].t + 1000 / FPS, (await evaluate("Date.now()")));
  capturing = false;
  await cdp.send("Page.stopScreencast");
  await Promise.all(writes);
}

async function writeConcat() {
  const lines = ["ffconcat version 1.0"];
  frames.forEach((f, i) => {
    const d = ((i + 1 < frames.length ? frames[i + 1].t : endT) - f.t) / 1000;
    lines.push(`file '${f.name}'`, `duration ${d.toFixed(3)}`);
  });
  // ffmpeg ignores the duration of the last entry and gives the one before it to the end of the video, so the last
  // frame is repeated twice with a short duration in between.
  const last = `file '${frames[frames.length - 1].name}'`;
  lines.push(last, `duration ${(1 / FPS).toFixed(3)}`, last);
  await writeFile(join(FRAMES, "frames.txt"), lines.join("\n") + "\n");
}

// Status: the values to register in the management platform and the dry-run sinks with their line counts.
async function sceneStatus() {
  if ((await evaluate("location.hash")) !== "#status") await openTab("status");
  await caption("status");
  await pick("v", 'document.getElementById("os-auth_url")', "the Auth URL value");
  await moveTo(await show("v"), 450);
  await pick("v", 'document.getElementById("os-region")', "the Region value");
  await moveTo(await show("v"), 450);
  await waitFor("the sink status rows", 'document.querySelectorAll("#status-sinks tbody tr").length >= 3 && [...document.querySelectorAll("#status-sinks tbody tr")].pop().offsetHeight > 0', 10000);
  await pick("v", '[...document.querySelectorAll("#status-sinks tbody tr")].pop()', "the sink table");
  await moveTo(await show("v"), 500);
  await hold(1600);
}

// General: change the write interval and save. The page says the settings are in effect.
async function sceneGeneral() {
  await openTab("general");
  await waitFor("the settings", 'document.getElementById("f-interval").value !== ""', 5000);
  await caption("general");
  for (const id of ["f-advertise_host", "f-openstack_password", "f-token_ttl"]) {
    await pick("f", `document.getElementById(${JSON.stringify(id)})`, id);
    await moveTo(await show("f"), 260);
  }
  await pick("f", 'document.getElementById("f-interval")', "the interval field");
  await clickOn("f");
  await evaluate('document.getElementById("f-interval").select()');
  await typeText("15s");
  for (const id of ["f-seed", "f-xid_rate", "f-dry_run"]) {
    await pick("f", `document.getElementById(${JSON.stringify(id)})`, id);
    await moveTo(await show("f"), 260);
  }
  await pick("save", `document.querySelector('#settings-form button[type="submit"]')`, "the Save button");
  await clickOn("save");
  await waitFor("the saved notice", 'document.querySelector("#settings-result.result-ok:not([hidden])") !== null', 5000);
  await pick("v", 'document.getElementById("settings-result")', "the saved notice");
  await show("v");
  await hold(1700);
}

async function closeDialog() {
  await pick("cancel", 'document.getElementById("sink-cancel")', "the Cancel button");
  await clickOn("cancel");
  await waitFor("the dialog to close", "!window.__demo.hasDialog()", 3000);
}

// Sinks: the Add sink form for each InfluxDB version and OTLP, then the tag rules of site-b.
async function sceneSinks() {
  await openTab("sinks");
  await waitFor("the sink list", 'document.querySelectorAll("#sinks-table tbody tr").length > 0', 5000);
  await pick("add", 'document.getElementById("sink-add")', "the Add sink button");
  await clickOn("add");
  await waitFor("the sink dialog", "window.__demo.hasDialog()", 3000);
  await caption("sinks_add");
  await evaluate("window.__demo.raise()");
  await hold(700);
  await pick("ver", 'document.getElementById("s-influx_version")', "the InfluxDB version select");
  for (const v of ["v2", "v3"]) {
    await clickOn("ver", false);
    await evaluate(`window.__demo.choose("ver", ${JSON.stringify(v)})`);
    await hold(1000);
  }
  await pick("type", 'document.getElementById("s-type")', "the sink type select");
  await clickOn("type", false);
  await evaluate('window.__demo.choose("type", "otlp")');
  await hold(1300);
  await closeDialog();

  await pick("edit", 'window.__demo.rowButton("sinks-table", "site-b", "^(edit|수정)$")', "the Edit button of site-b");
  await clickOn("edit");
  await waitFor("the sink dialog", "window.__demo.hasDialog()", 3000);
  await caption("sinks_rules");
  await evaluate("window.__demo.raise()");
  await pick("rules", 'document.getElementById("s-rules")', "the tag rules");
  if (!(await evaluate('document.getElementById("s-rules").open'))) {
    await pick("sum", 'document.querySelector("#s-rules > summary")', "the tag rules summary");
    await clickOn("sum");
  }
  await pick("rule", 'document.querySelector("#s-rules-list .rule-row:last-child")', "a tag rule row");
  await show("rule");
  for (const sel of [":first-child", ":nth-child(2)", ":last-child"]) {
    await pick("rule", `document.querySelector("#s-rules-list .rule-row${sel} .rule-key")`, "a tag rule key");
    await moveTo(await show("rule"), 320);
    await hold(250);
  }
  await hold(1300);
  await closeDialog();
}

// Topology: a new VM from the palette goes onto a GPU with room, the same VM dragged onto a full GPU is refused, then
// Save. The refusal is shown at the top of the page: the page draws the refusal note relative to the page box, so it
// would sit off screen while the page is scrolled.
async function dragItem(item, target, drop, innerFirst) {
  const from = await show(item, innerFirst);
  const to = await show(target);
  const src = await evaluate(`window.__demo.box(${JSON.stringify(item)})`);
  await moveTo(src.y === from.y ? from : src, 380);
  await evaluate("window.__demo.press(true)");
  await hold(120);
  await evaluate(`window.__demo.dragStart(${JSON.stringify(item)})`);
  await moveTo(to, 750, async (x, y) => { await evaluate(`window.__demo.dragOver(${x}, ${y})`); });
  const ok = await evaluate(`window.__demo.dragOver(${to.x}, ${to.y})`);
  await hold(drop ? 300 : 1800);
  await evaluate(`window.__demo.dragEnd(${to.x}, ${to.y}, ${drop})`);
  await evaluate("window.__demo.press(false)");
  return ok;
}

async function sceneTopology() {
  await openTab("topology");
  await waitFor("the topology palette", 'document.querySelector("[data-drag=new-vm]") !== null', 10000);
  await waitFor("the topology check", 'document.querySelector("[data-drop=gpu]") !== null', 10000);
  await caption("topo_drag");
  await pick("vm", 'document.querySelector("[data-drag=new-vm]")', "the VM in the palette");
  await show("vm", true);
  await pick("room", 'window.__demo.gpuFor("vm", true)', "a GPU that takes a VM");
  const count = 'document.querySelectorAll("[data-drag=vm]").length';
  const before = await evaluate(count);
  const ok = await dragItem("vm", "room", true, true);
  const after = await evaluate(count);
  if (!ok || after !== before + 1) throw new Error(`the drop did not add a VM (accepted ${ok}, VMs ${before} -> ${after})`);
  await hold(700);

  if (await evaluate("window.__demo.home()")) await waitFor("the scroll to the top", "window.__demo.scrolled()", 3000);
  await pick("chip", 'document.querySelector(".tp-vm.tp-sel[data-drag=vm]")', "the new VM on the canvas");
  await pick("full", 'window.__demo.gpuFor("chip", false)', "a GPU that refuses the VM");
  if (await dragItem("chip", "full", false)) throw new Error("the full GPU took the VM");
  await hold(400);

  await caption("topo_save");
  await pick("save", 'document.querySelector(".tp-toolbar .btn-primary")', "the topology Save button");
  await clickOn("save");
  await waitFor("the saved toast", 'document.querySelector("#toast.toast-ok:not([hidden])") !== null', 8000);
  await hold(2000);
}

const SCENE_FUNCS = { status: sceneStatus, general: sceneGeneral, sinks: sceneSinks, topology: sceneTopology };

async function scenes() {
  for (const name of SCENES) {
    if (!WANT.includes(name)) continue;
    const t = performance.now();
    await SCENE_FUNCS[name]();
    log(`${name}: ${((performance.now() - t) / 1000).toFixed(1)}s`);
  }
}

async function main() {
  const unknown = WANT.filter((s) => !SCENES.includes(s));
  if (unknown.length) throw new Error("unknown DEMO_SCENES entry: " + unknown.join(", "));
  await waitForSinkLines();
  const list = await (await fetch(`http://127.0.0.1:${DEVTOOLS_PORT}/json/list`)).json();
  const page = list.find((t) => t.type === "page");
  if (!page) throw new Error("Chrome has no page target");
  cdp = await CDP.connect(page.webSocketDebuggerUrl);

  await cdp.send("Page.enable");
  await cdp.send("Runtime.enable");
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: WIDTH, height: HEIGHT, deviceScaleFactor: 1, mobile: false });
  await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
  await cdp.send("Emulation.setLocaleOverride", { locale: LOCALE }).catch(() => {});
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
    source: `try {
      localStorage.setItem(${JSON.stringify(LANG_KEY)}, ${JSON.stringify(LANG)});
      localStorage.setItem(${JSON.stringify(THEME_KEY)}, "dark");
    } catch (e) {}
    document.documentElement.setAttribute("data-theme", "dark");`
  });

  await cdp.send("Page.navigate", { url: new URL("#status", URL_BASE).href });
  await waitFor("the page to load", 'document.readyState === "complete"');
  await waitFor("the sinks on the Status tab", 'document.body.innerText.includes("site-b")');
  await evaluate(PAGE_HELPERS);
  await sleep(500);
  const look = await evaluate(`({ lang: document.documentElement.lang, theme: document.documentElement.getAttribute("data-theme"),
    dark: matchMedia("(prefers-color-scheme: dark)").matches })`);
  log(`page lang=${look.lang} data-theme=${look.theme} prefers-dark=${look.dark} scenes=${WANT.join(",")}`);

  await startCapture();
  let err = null;
  try {
    await scenes();
  } catch (e) {
    err = e;
  }
  await stopCapture();
  await writeConcat();
  const secs = (endT - frames[0].t) / 1000;
  log(`${frames.length} frames over ${secs.toFixed(1)}s (${(frames.length / Math.max(secs, 0.001)).toFixed(1)} fps)`);
  cdp.close();
  if (err) throw err;
}

main().catch((e) => {
  log("error: " + (e && e.message ? e.message : e));
  process.exit(1);
});
