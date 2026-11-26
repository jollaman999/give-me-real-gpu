// Light and dark theme of the settings UI. index.html loads this file without defer
// in <head>, so <html> has data-theme before the first paint and the page does not flash.
// The pure helper is also loaded by node in web_test.go. The DOM part only runs in a browser.
"use strict";

(function (root) {
  var THEMES = ["light", "dark"];
  var THEME_KEY = "gmrg.theme";
  var SWAP_MS = 700;

  // pickTheme returns the saved choice when it is a known theme, and otherwise
  // follows the operating system setting.
  function pickTheme(saved, osDark) {
    if (THEMES.indexOf(saved) >= 0) return saved;
    return osDark ? "dark" : "light";
  }

  if (typeof module !== "undefined" && module.exports) module.exports = { pickTheme: pickTheme };
  if (typeof document === "undefined") return;

  // ---- browser part ----

  var html = document.documentElement;
  var media = typeof root.matchMedia === "function" ? root.matchMedia("(prefers-color-scheme: dark)") : null;
  var chosen = null;
  var swapTimer = null;

  try { chosen = root.localStorage.getItem(THEME_KEY); } catch (e) { /* storage may be blocked */ }
  if (THEMES.indexOf(chosen) < 0) chosen = null;

  function osDark() { return !!(media && media.matches); }

  function current() { return html.getAttribute("data-theme") === "dark" ? "dark" : "light"; }

  function syncSwitch() {
    var b = document.getElementById("theme-toggle");
    if (b) b.setAttribute("aria-checked", String(current() === "dark"));
  }

  function apply(name) {
    html.setAttribute("data-theme", name);
    syncSwitch();
  }

  // swap is apply with the colors moving. The class is only on while the change is drawn.
  function swap(name) {
    if (name === current()) return;
    html.classList.add("theme-swapping");
    apply(name);
    clearTimeout(swapTimer);
    swapTimer = setTimeout(function () { html.classList.remove("theme-swapping"); }, SWAP_MS);
  }

  apply(pickTheme(chosen, osDark()));

  if (media) {
    var follow = function () { if (chosen === null) swap(pickTheme(null, osDark())); };
    if (typeof media.addEventListener === "function") media.addEventListener("change", follow);
    else if (typeof media.addListener === "function") media.addListener(follow);
  }

  function bind() {
    syncSwitch();
    var b = document.getElementById("theme-toggle");
    if (!b) return;
    b.addEventListener("click", function () {
      chosen = current() === "dark" ? "light" : "dark";
      swap(chosen);
      try { root.localStorage.setItem(THEME_KEY, chosen); } catch (e) { /* storage may be blocked */ }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", bind);
  } else {
    bind();
  }
})(typeof window !== "undefined" ? window : globalThis);
