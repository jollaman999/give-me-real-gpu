// record.mjs drives the settings UI in a running headless Chrome over the DevTools protocol and saves a screenshot
// every 1/fps seconds while it goes through the scenes. It writes the PNG frames and an ffconcat list (frames.txt)
// with the real time between frames into the frames directory. No npm packages: it needs Node 20 with WebSocket
// (pass --experimental-websocket on Node 20) and fetch.
//
//   node record.mjs --devtools-port 9222 --url http://127.0.0.1:27780/ui/ --frames DIR [--fps 10]
//
// Elements are found by URL hash and visible text first and by the app's data attributes where it has them, so
// the script keeps working when the look of the page changes.

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
const log = (msg) => process.stderr.write("[record] " + msg + "\n");

class CDP {
  constructor(ws) {
    this.ws = ws;
    this.next = 1;
    this.pending = new Map();
    ws.addEventListener("message", (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id === undefined) return;
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
    await sleep(100);
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

// PAGE_HELPERS runs in the page. It draws a cursor in the top layer (above open dialogs) and finds elements.
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
    "border:3px solid #ffb020;opacity:0;transition:opacity .35s, transform .35s;transform:scale(.4)";
  const chip = document.createElement("div");
  chip.style.cssText = "position:absolute;left:18px;top:20px;padding:2px 8px;border-radius:6px;font:600 12px system-ui,sans-serif;" +
    "background:#ffb020;color:#111;white-space:nowrap;display:none;box-shadow:0 2px 6px rgba(0,0,0,.4)";
  cur.append(ring, svg, chip);
  document.documentElement.appendChild(cur);
  const visible = (el) => {
    if (!el || !el.isConnected) return false;
    const r = el.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) return false;
    const cs = getComputedStyle(el);
    return cs.visibility !== "hidden" && cs.display !== "none";
  };
  const norm = (s) => (s || "").replace(/\\s+/g, " ").trim().toLowerCase();
  const label = (el) => norm(el.textContent) || norm(el.getAttribute("aria-label")) || norm(el.getAttribute("title"));
  const scope = () => {
    const dlgs = [...document.querySelectorAll("dialog[open], [role=dialog]:not([hidden]), [aria-modal=true]")].filter(visible);
    return dlgs.length ? dlgs[dlgs.length - 1] : document;
  };
  window.__demo = {
    el: null,
    cursor(x, y) {
      if (cur.matches(":popover-open")) cur.hidePopover();
      cur.showPopover();
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
    hasDialog() { return scope() !== document; },
    keep(el, noScroll) {
      if (!el || !visible(el)) return null;
      if (!noScroll) el.scrollIntoView({ block: "nearest", inline: "nearest" });
      this.el = el;
      const r = el.getBoundingClientRect();
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2), w: r.width, h: r.height };
    },
    // tab finds a tab by its URL hash, then by the visible text of a link or tab.
    tab(name) {
      const byHash = [...document.querySelectorAll('a[href="#' + name + '"], [data-tab="' + name + '"]')].filter(visible);
      if (byHash.length) return this.keep(byHash[0]);
      const want = norm(name);
      const byText = [...document.querySelectorAll("[role=tab], nav a, nav button")].filter((e) => visible(e) && label(e) === want);
      return this.keep(byText[0]);
    },
    // button finds a button or link whose text, aria-label or title matches re, in the open dialog when there is one.
    button(reSrc, flags) {
      const re = new RegExp(reSrc, flags);
      const all = [...scope().querySelectorAll("button, a, [role=button], summary")].filter(visible);
      return this.keep(all.find((e) => re.test(label(e))));
    },
    // rowAction finds the button matching re in the row that shows text and no other row.
    rowAction(text, others, reSrc, flags) {
      const re = new RegExp(reSrc, flags);
      const want = norm(text);
      const cands = [...document.querySelectorAll("button, a, [role=button]")].filter((e) => visible(e) && re.test(label(e)));
      for (const b of cands) {
        for (let a = b.parentElement; a && a !== document.body; a = a.parentElement) {
          const t = norm(a.textContent);
          if (!t.includes(want)) continue;
          if (others.some((o) => t.includes(norm(o)))) break;
          return this.keep(b);
        }
      }
      return null;
    },
    // closedDetails finds the summary of a closed details element in the open dialog.
    closedDetails() {
      const d = [...scope().querySelectorAll("details:not([open]) > summary")].filter(visible);
      return this.keep(d[0]);
    },
    count(sel) { return document.querySelectorAll(sel).length; },
    q(sel) { return this.keep([...document.querySelectorAll(sel)].find(visible)); },
    // dragSource finds the palette item for a new VM.
    dragSource() {
      const byAttr = [...document.querySelectorAll('[data-drag="new-vm"]')].find(visible);
      const el = byAttr || [...document.querySelectorAll("[draggable=true]")].find((e) => visible(e) && /^(new )?vm$/.test(label(e)));
      this.source = el;
      return this.keep(el, true);
    },
    // dropTarget picks the first GPU that takes the dragged item, by asking the page's dragover handlers.
    dropTarget(sel) {
      const src = this.el;
      const dt = new DataTransfer();
      src.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      let found = null;
      for (const t of document.querySelectorAll(sel)) {
        if (!visible(t)) continue;
        const r = t.getBoundingClientRect();
        const ev = new DragEvent("dragover", { bubbles: true, cancelable: true, dataTransfer: dt,
          clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 });
        t.dispatchEvent(ev);
        if (ev.defaultPrevented) { found = t; break; }
      }
      src.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
      if (!found) return null;
      this.target = found;
      return this.keep(found, true);
    },
    // fit scrolls smoothly, as little as it can, so the drag source and the target are both on screen.
    fit() {
      const s = this.source.getBoundingClientRect();
      const t = this.target.getBoundingClientRect();
      const need = Math.max(s.bottom, t.bottom) - innerHeight + 16;
      if (need <= 0) return 0;
      const dy = Math.min(need, Math.max(Math.min(s.top, t.top) - 16, 0));
      scrollBy({ top: dy, behavior: "smooth" });
      return dy;
    },
    targetBox() {
      const r = this.target.getBoundingClientRect();
      if (r.bottom < 0 || r.top > innerHeight || r.right < 0 || r.left > innerWidth) return null;
      return { x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2), w: r.width, h: r.height };
    },
    dragStart() {
      this.src = this.el;
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
    drop(x, y) {
      const t = document.elementFromPoint(x, y);
      const init = { bubbles: true, cancelable: true, dataTransfer: this.dt, clientX: x, clientY: y };
      if (t) t.dispatchEvent(new DragEvent("drop", init));
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

async function moveTo(pt, ms = 450, onStep) {
  const steps = Math.max(1, Math.round(ms / 30));
  const from = { ...mouse };
  for (let i = 1; i <= steps; i++) {
    const k = i / steps;
    const e = k < 0.5 ? 2 * k * k : 1 - Math.pow(-2 * k + 2, 2) / 2;
    const x = Math.round(from.x + (pt.x - from.x) * e);
    const y = Math.round(from.y + (pt.y - from.y) * e);
    if (onStep) await onStep(x, y);
    else await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x, y });
    await cursor(x, y);
    await sleep(30);
  }
  mouse = { x: pt.x, y: pt.y };
}

async function click(pt) {
  await moveTo(pt);
  await evaluate("window.__demo.press(true)");
  await cdp.send("Input.dispatchMouseEvent", { type: "mousePressed", x: pt.x, y: pt.y, button: "left", clickCount: 1 });
  await sleep(90);
  await cdp.send("Input.dispatchMouseEvent", { type: "mouseReleased", x: pt.x, y: pt.y, button: "left", clickCount: 1 });
  await sleep(250);
  await evaluate("window.__demo.press(false)");
  await cursor(pt.x, pt.y);
}

async function wheel(dy, times) {
  for (let i = 0; i < times; i++) {
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseWheel", x: mouse.x, y: mouse.y, deltaX: 0, deltaY: dy });
    await sleep(120);
  }
}

async function openTab(name) {
  const pt = await evaluate(`window.__demo.tab(${JSON.stringify(name)})`);
  if (pt) {
    await click(pt);
  } else {
    log(`no visible tab for #${name}; switching by URL hash`);
    await evaluate(`location.hash = ${JSON.stringify("#" + name)}`);
  }
  await waitFor(`#${name}`, `location.hash === ${JSON.stringify("#" + name)}`, 3000).catch(() => {});
}

const frames = [];
let capturing = false;

async function captureLoop() {
  const period = 1000 / FPS;
  let next = performance.now();
  while (capturing) {
    const t = performance.now();
    const { data } = await cdp.send("Page.captureScreenshot", { format: "png", optimizeForSpeed: true });
    const name = "f" + String(frames.length).padStart(5, "0") + ".png";
    await writeFile(join(FRAMES, name), Buffer.from(data, "base64"));
    frames.push({ name, t });
    next += period;
    const wait = next - performance.now();
    if (wait > 0) await sleep(wait);
    else next = performance.now();
  }
}

async function writeConcat() {
  const lines = ["ffconcat version 1.0"];
  frames.forEach((f, i) => {
    const d = i + 1 < frames.length ? (frames[i + 1].t - f.t) / 1000 : 1 / FPS;
    lines.push(`file '${f.name}'`, `duration ${d.toFixed(3)}`);
  });
  lines.push(`file '${frames[frames.length - 1].name}'`);
  await writeFile(join(FRAMES, "frames.txt"), lines.join("\n") + "\n");
}

async function scenes() {
  // Status: the OpenStack values and the line counts of the dry-run sinks.
  await sleep(2200);

  // General settings.
  await openTab("general");
  await sleep(2200);

  // Sinks list, then the site-b sink: InfluxDB v2 with tag rules.
  await openTab("sinks");
  await sleep(1600);
  const edit = await evaluate(`window.__demo.rowAction("site-b", ["site-a", "site-c"], "edit", "i")`);
  if (!edit) throw new Error("no Edit button in the site-b row");
  await click(edit);
  await waitFor("the sink dialog", "window.__demo.hasDialog()", 3000);
  await sleep(1400);
  const rules = await evaluate("window.__demo.closedDetails()");
  if (rules) {
    await click(rules);
    await sleep(300);
  }
  await moveTo({ x: WIDTH / 2, y: HEIGHT / 2 }, 300);
  await wheel(160, 6);
  await sleep(2000);
  const cancel = await evaluate(`window.__demo.button("^(cancel|close)$", "i")`);
  if (cancel) {
    await click(cancel);
  } else {
    log("no Cancel button in the dialog; pressing Escape");
    await cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
    await cdp.send("Input.dispatchKeyEvent", { type: "keyUp", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
  }
  await waitFor("the dialog to close", "!window.__demo.hasDialog()", 3000);
  await sleep(700);

  // Topology: drag a new VM from the palette onto a GPU that has room. Nothing is saved.
  await openTab("topology");
  await waitFor("the topology palette", 'document.querySelector("[data-drag], [draggable=true]") !== null', 10000);
  await sleep(1800);
  const vmsBefore = await evaluate('window.__demo.count("[data-drag=\\"vm\\"]")');
  let src = await evaluate("window.__demo.dragSource()");
  if (!src) throw new Error("no VM item in the topology palette");
  const dst = await evaluate(`window.__demo.dropTarget('[data-drop="gpu"], [data-drop="free"], [data-drop="slot"]')`);
  if (!dst) throw new Error("no GPU takes a new VM");
  if (await evaluate("window.__demo.fit()")) await sleep(900);
  src = await evaluate("window.__demo.dragSource()");
  const to = await evaluate("window.__demo.targetBox()");
  if (!src || !to) throw new Error("the VM item and the GPU do not fit on the screen together");
  await moveTo(src, 600);
  await evaluate("window.__demo.press(true)");
  await sleep(200);
  await evaluate("window.__demo.dragStart()");
  await moveTo(to, 1400, async (x, y) => { await evaluate(`window.__demo.dragOver(${x}, ${y})`); });
  const ok = await evaluate(`window.__demo.dragOver(${to.x}, ${to.y})`);
  await sleep(500);
  await evaluate(`window.__demo.drop(${to.x}, ${to.y})`);
  await evaluate("window.__demo.press(false)");
  await sleep(300);
  const vmsAfter = await evaluate('window.__demo.count("[data-drag=\\"vm\\"]")');
  if (!ok || vmsAfter !== vmsBefore + 1) {
    throw new Error(`the drop did not add a VM (accepted ${ok}, VMs ${vmsBefore} -> ${vmsAfter})`);
  }
  await moveTo({ x: to.x + 140, y: Math.min(HEIGHT - 40, to.y + 90) }, 500);
  await sleep(2400);
}

async function main() {
  await waitForSinkLines();
  const list = await (await fetch(`http://127.0.0.1:${DEVTOOLS_PORT}/json/list`)).json();
  const page = list.find((t) => t.type === "page");
  if (!page) throw new Error("Chrome has no page target");
  cdp = await CDP.connect(page.webSocketDebuggerUrl);

  await cdp.send("Page.enable");
  await cdp.send("Runtime.enable");
  await cdp.send("Emulation.setDeviceMetricsOverride", { width: WIDTH, height: HEIGHT, deviceScaleFactor: 1, mobile: false });
  await cdp.send("Emulation.setEmulatedMedia", { features: [{ name: "prefers-color-scheme", value: "dark" }] });
  await cdp.send("Emulation.setLocaleOverride", { locale: "en-US" }).catch(() => {});
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", {
    source: `try {
      localStorage.setItem(${JSON.stringify(LANG_KEY)}, "en");
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
  log(`page lang=${look.lang} data-theme=${look.theme} prefers-dark=${look.dark}`);

  capturing = true;
  const loop = captureLoop();
  let err = null;
  try {
    await scenes();
  } catch (e) {
    err = e;
  }
  capturing = false;
  await loop;
  if (frames.length) await writeConcat();
  const secs = frames.length ? (frames[frames.length - 1].t - frames[0].t) / 1000 : 0;
  log(`${frames.length} frames over ${secs.toFixed(1)}s (${(frames.length / Math.max(secs, 0.001)).toFixed(1)} fps)`);
  cdp.close();
  if (err) throw err;
}

main().catch((e) => {
  log("error: " + (e && e.message ? e.message : e));
  process.exit(1);
});
