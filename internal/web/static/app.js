// Settings UI for give-me-real-gpu. Plain JavaScript, no build step.
// The pure helpers are also loaded by node in web_test.go. The DOM part only runs in a browser.
"use strict";

(function (root) {
  var LANGS = ["en", "ko"];
  var LANG_KEY = "gmrg.lang";
  var STATUS_EVERY_MS = 5000;

  // pickLanguage returns "ko" when a Korean entry comes before any English entry in the
  // browser's language list, and "en" otherwise.
  function pickLanguage(languages) {
    var list = languages || [];
    for (var i = 0; i < list.length; i++) {
      var l = String(list[i] || "").toLowerCase();
      if (l.indexOf("ko") === 0) return "ko";
      if (l.indexOf("en") === 0) return "en";
    }
    return "en";
  }

  // initialLanguage prefers a saved choice, then the browser's list.
  function initialLanguage(saved, languages) {
    if (LANGS.indexOf(saved) >= 0) return saved;
    return pickLanguage(languages);
  }

  // format replaces {name} in the template with params.name.
  function format(template, params) {
    if (!params) return template;
    return template.replace(/\{(\w+)\}/g, function (m, k) {
      return Object.prototype.hasOwnProperty.call(params, k) ? String(params[k]) : m;
    });
  }

  var INFLUX_VERSIONS = ["v1", "v2", "v3"];

  // influxVersion returns v1, v2 or v3. Anything else, including empty, is v1.
  function influxVersion(v) {
    var s = String(v || "").trim().toLowerCase();
    return INFLUX_VERSIONS.indexOf(s) >= 0 ? s : "v1";
  }

  // sinkVariant names the form layout: "influxdb-v1", "influxdb-v2", "influxdb-v3" or the type itself.
  function sinkVariant(type, version) {
    return type === "influxdb" ? "influxdb-" + influxVersion(version) : String(type || "");
  }

  var RULE_ACTIONS = ["rename", "drop", "add"];
  var MAX_TAG_RULES = 50;

  function ruleText(v) {
    return String(v === undefined || v === null ? "" : v).trim();
  }

  // tagRuleBlank tells whether a rule row has nothing typed in the fields its action uses.
  function tagRuleBlank(r) {
    if (!r || ruleText(r.key) !== "") return false;
    if (r.action === "rename") return ruleText(r.new_key) === "";
    if (r.action === "add") return ruleText(r.value) === "";
    return true;
  }

  // tagRules turns rule rows into the tag_rules list of the sink body. Blank rows are left out
  // and each rule only carries the fields its action uses.
  function tagRules(rows) {
    var out = [];
    (rows || []).forEach(function (r) {
      if (!r || tagRuleBlank(r)) return;
      var action = ruleText(r.action);
      var rule = { action: action, key: ruleText(r.key) };
      if (action === "rename") rule.new_key = ruleText(r.new_key);
      if (action === "add") rule.value = ruleText(r.value);
      out.push(rule);
    });
    return out;
  }

  // tagRuleProblem returns the i18n key of what is wrong with a rule row, or "" when it looks fine.
  // The server makes the final check.
  function tagRuleProblem(r) {
    if (tagRuleBlank(r)) return "";
    if (ruleText(r.key) === "") return "sinks.rule_key_missing";
    if (r.action === "rename") {
      if (ruleText(r.new_key) === "") return "sinks.rule_new_key_missing";
      if (ruleText(r.new_key) === ruleText(r.key)) return "sinks.rule_same_key";
    }
    if (r.action === "add" && ruleText(r.value) === "") return "sinks.rule_value_missing";
    return "";
  }

  // sinkPayload turns form values into the body of POST/PUT /api/v1/sinks.
  // Fields the chosen type and version do not use are sent empty. Secrets are only sent when typed.
  function sinkPayload(v) {
    var influx = v.type === "influxdb";
    var variant = sinkVariant(v.type, v.influx_version);
    var basic = variant === "otlp" || variant === "influxdb-v1";
    var bearer = variant === "otlp" || variant === "influxdb-v2" || variant === "influxdb-v3";
    var body = {
      name: String(v.name || "").trim(),
      type: v.type,
      url: String(v.url || "").trim(),
      influx_version: influx ? influxVersion(v.influx_version) : "",
      org: variant === "influxdb-v2" ? String(v.org || "").trim() : "",
      db: influx ? String(v.db || "").trim() : "",
      username: basic ? String(v.username || "").trim() : "",
      credential_id: influx ? String(v.credential_id || "").trim() : "",
      enabled: !!v.enabled,
      tag_rules: tagRules(v.tag_rules)
    };
    if (basic && v.password) body.password = v.password;
    if (bearer && v.token) body.token = v.token;
    return body;
  }

  // sinkValues turns a sink from GET /api/v1/sinks into form values. Secrets start empty.
  function sinkValues(s) {
    return {
      name: s.name, type: s.type, url: s.url, influx_version: influxVersion(s.influx_version), org: s.org,
      db: s.db, username: s.username, credential_id: s.credential_id, enabled: s.enabled, password: "", token: "",
      tag_rules: (s.tag_rules || []).map(function (r) {
        return { action: r.action, key: r.key || "", new_key: r.new_key || "", value: r.value || "" };
      })
    };
  }

  // secretKept tells whether a saved secret survives the save: the sink exists,
  // the secret is set and the URL is unchanged.
  function secretKept(original, setFlag, url) {
    if (!original || !original[setFlag]) return false;
    return String(url || "").trim() === original.url;
  }

  var helpers = {
    pickLanguage: pickLanguage,
    initialLanguage: initialLanguage,
    format: format,
    influxVersion: influxVersion,
    sinkVariant: sinkVariant,
    tagRules: tagRules,
    tagRuleProblem: tagRuleProblem,
    sinkPayload: sinkPayload,
    sinkValues: sinkValues,
    secretKept: secretKept
  };
  if (typeof module !== "undefined" && module.exports) module.exports = helpers;
  if (typeof document === "undefined") return;

  // ---- browser part ----

  var state = {
    lang: "en",
    dicts: {},
    tab: "",
    langListeners: [],
    topologyMounted: false,
    toastTimer: null,
    status: null,
    statusAt: null,
    statusBusy: false,
    settings: null,
    settingsLoaded: false,
    sinks: [],
    editing: null
  };

  function $(id) { return document.getElementById(id); }

  function t(key, params) {
    var d = state.dicts[state.lang] || {};
    var s = d[key];
    if (s === undefined) s = (state.dicts.en || {})[key];
    if (s === undefined) s = key;
    return format(s, params);
  }

  function el(tag, props, children) {
    var n = document.createElement(tag);
    props = props || {};
    if (props.className) n.className = props.className;
    if (props.text !== undefined) n.textContent = props.text;
    if (props.title) n.title = props.title;
    if (props.attrs) {
      Object.keys(props.attrs).forEach(function (k) { n.setAttribute(k, props.attrs[k]); });
    }
    if (props.on) {
      Object.keys(props.on).forEach(function (k) { n.addEventListener(k, props.on[k]); });
    }
    (children || []).forEach(function (c) {
      if (c === null || c === undefined) return;
      n.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    });
    return n;
  }

  function api(method, path, body) {
    var opt = { method: method, headers: { Accept: "application/json" }, cache: "no-store" };
    if (body !== undefined) {
      opt.headers["Content-Type"] = "application/json";
      opt.body = JSON.stringify(body);
    }
    return fetch(path, opt).then(function (res) {
      var ct = res.headers.get("Content-Type") || "";
      var parse = ct.indexOf("json") >= 0 ? res.json().catch(function () { return null; }) : Promise.resolve(null);
      return parse.then(function (data) { return { ok: res.ok, status: res.status, data: data }; });
    });
  }

  function errorText(res) {
    if (res && res.data && res.data.error) return res.data.error;
    return t("common.http_error", { status: res ? res.status : "?" });
  }

  // showResult fills a .result box. kind is "ok" or "error".
  function showResult(box, kind, message, problems) {
    box.textContent = "";
    box.className = "result result-" + kind;
    box.appendChild(el("p", { text: message }));
    if (problems && problems.length) {
      box.appendChild(el("ul", {}, problems.map(function (p) { return el("li", { text: p }); })));
    }
    box.hidden = false;
  }

  function showResponseError(box, res) {
    if (res.status === 400 && res.data && res.data.problems) {
      showResult(box, "error", t("common.validation_failed"), res.data.problems);
    } else {
      showResult(box, "error", t("common.save_failed", { error: errorText(res) }));
    }
  }

  function hideResult(box) {
    box.hidden = true;
    box.textContent = "";
  }

  // ---- i18n ----

  function loadDict(lang) {
    if (state.dicts[lang]) return Promise.resolve();
    return fetch("i18n/" + lang + ".json", { cache: "no-cache" })
      .then(function (r) { return r.ok ? r.json() : {}; })
      .catch(function () { return {}; })
      .then(function (d) { state.dicts[lang] = d; });
  }

  function applyI18n(scope) {
    scope = scope || document;
    scope.querySelectorAll("[data-i18n]").forEach(function (n) { n.textContent = t(n.getAttribute("data-i18n")); });
    scope.querySelectorAll("[data-i18n-placeholder]").forEach(function (n) { n.placeholder = t(n.getAttribute("data-i18n-placeholder")); });
    scope.querySelectorAll("[data-i18n-title]").forEach(function (n) { n.title = t(n.getAttribute("data-i18n-title")); });
    scope.querySelectorAll("[data-i18n-aria-label]").forEach(function (n) { n.setAttribute("aria-label", t(n.getAttribute("data-i18n-aria-label"))); });
  }

  function setLanguage(lang) {
    return Promise.all([loadDict("en"), loadDict(lang)]).then(function () {
      state.lang = lang;
      try { localStorage.setItem(LANG_KEY, lang); } catch (e) { /* storage may be blocked */ }
      document.documentElement.lang = lang;
      document.title = t("app.title");
      document.querySelectorAll(".lang-btn").forEach(function (b) {
        b.setAttribute("aria-pressed", String(b.getAttribute("data-lang") === lang));
      });
      applyI18n(document);
      renderStatus();
      renderSettingsHints();
      renderSinks();
      if (state.editing) renderSinkDialog();
      state.langListeners.forEach(function (cb) {
        try { cb(lang); } catch (e) { console.error(e); }
      });
    });
  }

  // ---- tabs ----

  function mountTopology() {
    if (state.topologyMounted) return;
    var editor = window.GMRG_TOPOLOGY;
    if (!editor || typeof editor.mount !== "function") return;
    state.topologyMounted = true;
    editor.mount($("topology-root"));
  }

  function showTab(name) {
    var panels = document.querySelectorAll("[data-panel]");
    var known = false;
    panels.forEach(function (p) { if (p.getAttribute("data-panel") === name) known = true; });
    if (!known) name = "status";
    state.tab = name;
    panels.forEach(function (p) { p.hidden = p.getAttribute("data-panel") !== name; });
    document.querySelectorAll(".tab").forEach(function (a) {
      var on = a.getAttribute("data-tab") === name;
      a.classList.toggle("active", on);
      a.setAttribute("aria-selected", String(on));
    });
    if (name === "status") refreshStatus();
    if (name === "general" && !state.settingsLoaded) loadSettings();
    if (name === "sinks") loadSinks();
    if (name === "topology") mountTopology();
  }

  function route() {
    showTab((location.hash || "#status").slice(1));
  }

  // ---- status ----

  function relativeTime(iso) {
    var d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    var sec = Math.min(0, Math.round((d.getTime() - Date.now()) / 1000));
    var abs = Math.abs(sec);
    try {
      var rtf = new Intl.RelativeTimeFormat(state.lang, { numeric: "auto" });
      if (abs < 60) return rtf.format(sec, "second");
      if (abs < 3600) return rtf.format(Math.round(sec / 60), "minute");
      if (abs < 86400) return rtf.format(Math.round(sec / 3600), "hour");
      return rtf.format(Math.round(sec / 86400), "day");
    } catch (e) {
      return d.toLocaleString(state.lang);
    }
  }

  function yesNo(v) {
    return el("span", { className: "badge " + (v ? "badge-ok" : "badge-off"), text: t(v ? "common.on" : "common.off") });
  }

  function typeLabel(type, version) {
    if (type === "influxdb") return t("sinks.type_influxdb_short") + (version === undefined ? "" : " " + influxVersion(version));
    if (type === "otlp") return t("sinks.type_otlp_short");
    return type || "-";
  }

  function refreshStatus() {
    if (state.statusBusy) return;
    state.statusBusy = true;
    api("GET", "/api/v1/status")
      .then(function (res) {
        if (!res.ok || !res.data) throw new Error(errorText(res));
        state.status = res.data;
        state.statusAt = new Date();
        $("conn-error").hidden = true;
        renderStatus();
      })
      .catch(function () { $("conn-error").hidden = false; })
      .then(function () { state.statusBusy = false; });
  }

  function renderStatus() {
    var s = state.status;
    if (!s) return;
    var os = s.openstack || {};
    var badge = $("os-ready");
    badge.className = "badge " + (os.ready ? "badge-ok" : "badge-warn");
    badge.textContent = t(os.ready ? "status.ready" : "status.not_ready");
    $("setup-banner").hidden = !!os.ready;
    $("setup-reason").textContent = os.ready || !os.reason ? "" : t("status.reason", { reason: os.reason });
    ["auth_url", "user", "project_id", "project_name", "domain_id", "region"].forEach(function (k) {
      $("os-" + k).textContent = os[k] || "-";
    });
    $("version").textContent = s.version ? t("app.version", { version: s.version }) : "";
    $("updated").textContent = state.statusAt ? t("app.updated", { time: state.statusAt.toLocaleTimeString(state.lang) }) : "";

    var sinks = s.sinks || [];
    $("status-no-sinks").hidden = sinks.length > 0;
    var tbody = $("status-sinks").querySelector("tbody");
    tbody.textContent = "";
    $("status-sinks").hidden = sinks.length === 0;
    sinks.forEach(function (k) {
      var last = k.last_write
        ? el("span", { text: relativeTime(k.last_write), title: new Date(k.last_write).toLocaleString(state.lang) })
        : el("span", { className: "muted", text: t("status.never") });
      var err = k.last_error
        ? el("span", { className: "error-text", text: k.last_error, title: k.last_error })
        : el("span", { className: "muted", text: "-" });
      tbody.appendChild(el("tr", {}, [
        el("td", { attrs: { "data-label": t("sinks.col_name") } }, [el("strong", { text: k.name })]),
        el("td", { attrs: { "data-label": t("sinks.col_type") }, text: typeLabel(k.type) }),
        el("td", { attrs: { "data-label": t("sinks.col_enabled") } }, [yesNo(k.enabled)]),
        el("td", { attrs: { "data-label": t("status.col_last_write") } }, [last]),
        el("td", { className: "num", attrs: { "data-label": t("status.col_lines") }, text: k.last_write ? Number(k.lines || 0).toLocaleString(state.lang) : "-" }),
        el("td", { className: "num", attrs: { "data-label": t("status.col_elapsed") }, text: k.last_write ? t("status.elapsed_ms", { n: Number(k.elapsed_ms || 0).toLocaleString(state.lang) }) : "-" }),
        el("td", { className: "wrap", attrs: { "data-label": t("status.col_last_error") } }, [err])
      ]));
    });
  }

  function copyFrom(id, btn) {
    var text = $(id).textContent;
    if (!text || text === "-") return;
    var done = function () {
      btn.textContent = t("common.copied");
      setTimeout(function () { btn.textContent = t("common.copy"); }, 1200);
    };
    var fallback = function () {
      var r = document.createRange();
      r.selectNodeContents($(id));
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(r);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, fallback);
    } else {
      fallback();
    }
  }

  // ---- general ----

  function loadSettings() {
    var box = $("settings-result");
    return api("GET", "/api/v1/settings").then(function (res) {
      if (!res.ok || !res.data) {
        showResult(box, "error", t("common.load_failed", { error: errorText(res) }));
        return;
      }
      var s = res.data;
      state.settings = s;
      state.settingsLoaded = true;
      $("f-advertise_host").value = s.advertise_host || "";
      $("f-openstack_password").value = "";
      $("f-token_ttl").value = s.token_ttl || "";
      $("f-interval").value = s.interval || "";
      $("f-seed").value = s.seed === undefined || s.seed === null ? "" : String(s.seed);
      $("f-xid_rate").value = s.xid_rate === undefined || s.xid_rate === null ? "" : String(s.xid_rate);
      $("f-dry_run").checked = !!s.dry_run;
      renderSettingsHints();
    }, function () {
      showResult(box, "error", t("common.load_failed", { error: t("app.conn_error") }));
    });
  }

  function renderSettingsHints() {
    var set = !!(state.settings && state.settings.openstack_password_set);
    $("f-openstack_password").placeholder = set ? t("common.unchanged") : "";
    $("f-openstack_password-hint").textContent = t(set ? "general.secret_set_hint" : "general.secret_unset_hint");
  }

  function saveSettings(ev) {
    ev.preventDefault();
    var box = $("settings-result");
    var problems = [];
    var seedRaw = $("f-seed").value.trim();
    var seed = Number(seedRaw);
    if (seedRaw === "" || !Number.isSafeInteger(seed)) problems.push(t("general.seed_invalid"));
    var rateRaw = $("f-xid_rate").value.trim();
    var rate = Number(rateRaw);
    if (rateRaw === "" || !isFinite(rate)) problems.push(t("general.xid_rate_invalid"));
    if (problems.length) {
      showResult(box, "error", t("common.validation_failed"), problems);
      return;
    }
    var body = {
      advertise_host: $("f-advertise_host").value.trim(),
      token_ttl: $("f-token_ttl").value.trim(),
      interval: $("f-interval").value.trim(),
      seed: seed,
      xid_rate: rate,
      dry_run: $("f-dry_run").checked
    };
    var pw = $("f-openstack_password").value;
    if (pw) body.openstack_password = pw;
    api("PUT", "/api/v1/settings", body).then(function (res) {
      if (!res.ok) {
        showResponseError(box, res);
        return;
      }
      showResult(box, "ok", t("general.saved"));
      loadSettings();
      refreshStatus();
    }, function () {
      showResult(box, "error", t("common.save_failed", { error: t("app.conn_error") }));
    });
  }

  // ---- sinks ----

  function loadSinks() {
    var box = $("sinks-result");
    return api("GET", "/api/v1/sinks").then(function (res) {
      if (!res.ok || !Array.isArray(res.data)) {
        showResult(box, "error", t("common.load_failed", { error: errorText(res) }));
        return;
      }
      state.sinks = res.data;
      renderSinks();
    }, function () {
      showResult(box, "error", t("common.load_failed", { error: t("app.conn_error") }));
    });
  }

  function toggleSink(s, on, input) {
    var box = $("sinks-result");
    hideResult(box);
    var v = sinkValues(s);
    v.enabled = on;
    input.disabled = true;
    api("PUT", "/api/v1/sinks/" + encodeURIComponent(s.id), sinkPayload(v)).then(function (res) {
      if (!res.ok) {
        input.checked = !on;
        showResponseError(box, res);
      }
      loadSinks();
      refreshStatus();
    }, function () {
      input.checked = !on;
      showResult(box, "error", t("common.save_failed", { error: t("app.conn_error") }));
    }).then(function () { input.disabled = false; });
  }

  function deleteSink(s) {
    var box = $("sinks-result");
    hideResult(box);
    if (!window.confirm(t("sinks.delete_confirm", { name: s.name }))) return;
    api("DELETE", "/api/v1/sinks/" + encodeURIComponent(s.id)).then(function (res) {
      if (!res.ok && res.status !== 404) {
        showResult(box, "error", t("sinks.delete_failed", { error: errorText(res) }));
      } else {
        showResult(box, "ok", t("sinks.deleted", { name: s.name }));
      }
      loadSinks();
      refreshStatus();
    }, function () {
      showResult(box, "error", t("sinks.delete_failed", { error: t("app.conn_error") }));
    });
  }

  function renderSinks() {
    var tbody = $("sinks-table").querySelector("tbody");
    tbody.textContent = "";
    $("sinks-empty").hidden = state.sinks.length > 0;
    $("sinks-table").hidden = state.sinks.length === 0;
    state.sinks.forEach(function (s) {
      var toggle = el("input", { attrs: { type: "checkbox", "aria-label": t("sinks.toggle_label", { name: s.name }) } });
      toggle.checked = !!s.enabled;
      toggle.addEventListener("change", function () { toggleSink(s, toggle.checked, toggle); });
      var nRules = (s.tag_rules || []).length;
      var rulesBadge = nRules ? el("span", {
        className: "badge rules-count",
        text: t(nRules === 1 ? "sinks.rules_count_one" : "sinks.rules_count", { n: nRules })
      }) : null;
      tbody.appendChild(el("tr", {}, [
        el("td", { attrs: { "data-label": t("sinks.col_name") } }, [el("strong", { text: s.name })]),
        el("td", { attrs: { "data-label": t("sinks.col_type") } }, [typeLabel(s.type, s.influx_version || ""), rulesBadge]),
        el("td", { className: "wrap", attrs: { "data-label": t("sinks.col_url") } }, [el("code", { text: s.url || "-" })]),
        el("td", { attrs: { "data-label": t("sinks.col_enabled") } }, [el("label", { className: "switch" }, [toggle, el("span", { className: "slider", attrs: { "aria-hidden": "true" } })])]),
        el("td", { className: "row-actions" }, [
          el("button", { className: "btn btn-small", text: t("common.edit"), attrs: { type: "button" }, on: { click: function () { openSinkDialog(s); } } }),
          el("button", { className: "btn btn-small btn-danger", text: t("common.delete"), attrs: { type: "button" }, on: { click: function () { deleteSink(s); } } })
        ])
      ]));
    });
  }

  function openSinkDialog(s) {
    var v = s ? sinkValues(s) : {
      name: "", type: "influxdb", url: "", influx_version: "v1", org: "", db: "", username: "",
      credential_id: "", enabled: true, password: "", token: "", tag_rules: []
    };
    state.editing = { original: s || null, rules: v.tag_rules };
    $("s-rules").open = v.tag_rules.length > 0;
    $("s-name").value = v.name || "";
    $("s-type").value = v.type === "otlp" ? "otlp" : "influxdb";
    $("s-url").value = v.url || "";
    $("s-influx_version").value = influxVersion(v.influx_version);
    $("s-org").value = v.org || "";
    $("s-db").value = v.db || "";
    $("s-username").value = v.username || "";
    $("s-password").value = "";
    $("s-token").value = "";
    $("s-credential_id").value = v.credential_id || "";
    $("s-enabled").checked = !!v.enabled;
    hideResult($("sink-form-result"));
    renderSinkDialog();
    var d = $("sink-dialog");
    if (typeof d.showModal === "function") d.showModal(); else d.setAttribute("open", "");
    $("s-name").focus();
  }

  function closeSinkDialog() {
    var d = $("sink-dialog");
    if (typeof d.close === "function") d.close(); else d.removeAttribute("open");
    state.editing = null;
  }

  function renderSinkDialog() {
    if (!state.editing) return;
    var orig = state.editing.original;
    var type = $("s-type").value;
    var variant = sinkVariant(type, $("s-influx_version").value);
    var url = $("s-url").value;
    $("sink-dialog-title").textContent = orig ? t("sinks.edit_title", { name: orig.name }) : t("sinks.add_title");
    document.querySelectorAll("#sink-form [data-for]").forEach(function (n) {
      var on = n.getAttribute("data-for").split(" ");
      n.hidden = on.indexOf(type) < 0 && on.indexOf(variant) < 0;
    });
    if (variant === "otlp") {
      $("s-url").placeholder = "http://192.0.2.20:8429/opentelemetry/v1/metrics";
      $("s-url-hint").textContent = t("sinks.url_hint_otlp");
      $("s-token-label").textContent = t("sinks.token");
      $("s-token-hint").textContent = "";
    } else if (variant === "influxdb-v2") {
      $("s-url").placeholder = "http://192.0.2.20:8086";
      $("s-url-hint").textContent = t("sinks.url_hint_influxdb_v2");
      $("s-token-label").textContent = t("sinks.api_token");
      $("s-token-hint").textContent = t("sinks.token_hint_v2");
    } else if (variant === "influxdb-v3") {
      $("s-url").placeholder = "http://192.0.2.20:8181";
      $("s-url-hint").textContent = t("sinks.url_hint_influxdb_v3");
      $("s-token-label").textContent = t("sinks.api_token");
      $("s-token-hint").textContent = t("sinks.token_hint_v3");
    } else {
      $("s-url").placeholder = "http://192.0.2.20:8086";
      $("s-url-hint").textContent = t("sinks.url_hint_influxdb_v1");
      $("s-token-label").textContent = t("sinks.token");
      $("s-token-hint").textContent = "";
    }
    $("s-token-hint").hidden = $("s-token-hint").textContent === "";
    $("s-db-label").textContent = t(variant === "influxdb-v2" ? "sinks.bucket" : "sinks.db");
    $("s-password").placeholder = secretKept(orig, "password_set", url) ? t("common.unchanged") : "";
    $("s-token").placeholder = secretKept(orig, "token_set", url) ? t("common.unchanged") : "";
    var hadSecret = !!(orig && (orig.password_set || orig.token_set));
    $("s-url-warning").hidden = !(hadSecret && url.trim() !== orig.url);
    renderRules(type);
  }

  // ---- tag and label rules ----

  var RULE_ACTION_LABELS = { rename: "sinks.rule_rename", drop: "sinks.rule_drop", add: "sinks.rule_add" };

  function renderRules(type) {
    var rules = state.editing.rules;
    var otlp = type === "otlp";
    var n = tagRules(rules).length;
    $("s-rules-title").textContent = t(otlp ? "sinks.label_rules" : "sinks.tag_rules");
    $("s-rules-count").textContent = n ? "(" + n + ")" : "";
    $("s-rules-hint").textContent = t(otlp ? "sinks.rules_hint_otlp" : "sinks.rules_hint_influxdb");
    $("s-rule-add").disabled = rules.length >= MAX_TAG_RULES;
    $("s-rules-max").hidden = rules.length < MAX_TAG_RULES;
    var list = $("s-rules-list");
    list.textContent = "";
    rules.forEach(function (r, i) { list.appendChild(ruleRow(r, i, rules.length)); });
  }

  function moveRule(i, delta) {
    var rules = state.editing.rules;
    var j = i + delta;
    if (j < 0 || j >= rules.length) return;
    var r = rules[i];
    rules[i] = rules[j];
    rules[j] = r;
    renderRules($("s-type").value);
    var row = $("s-rules-list").children[j];
    var btn = row && row.querySelector(delta < 0 ? ".rule-up" : ".rule-down");
    if (btn && !btn.disabled) btn.focus();
  }

  function ruleInput(r, field, label, placeholder, problemBox) {
    var input = el("input", { attrs: {
      type: "text", autocomplete: "off", spellcheck: "false", "aria-label": t(label), placeholder: placeholder
    } });
    input.className = "rule-" + field.replace("_", "-");
    input.value = r[field] || "";
    input.addEventListener("input", function () {
      r[field] = input.value;
      showRuleProblem(r, problemBox);
      var n = tagRules(state.editing.rules).length;
      $("s-rules-count").textContent = n ? "(" + n + ")" : "";
    });
    return input;
  }

  function showRuleProblem(r, box) {
    var key = tagRuleProblem(r);
    box.textContent = key ? t(key) : "";
    box.hidden = !key;
  }

  function ruleRow(r, i, count) {
    var problem = el("p", { className: "hint error-text rule-problem" });
    var action = el("select", { className: "rule-action", attrs: { "aria-label": t("sinks.rule_action") } },
      RULE_ACTIONS.map(function (a) { return el("option", { text: t(RULE_ACTION_LABELS[a]), attrs: { value: a } }); }));
    action.value = r.action;
    action.addEventListener("change", function () {
      r.action = action.value;
      renderRules($("s-type").value);
      var row = $("s-rules-list").children[i];
      if (row) row.querySelector(".rule-action").focus();
    });
    var second;
    if (r.action === "rename") second = ruleInput(r, "new_key", "sinks.rule_new_key", "hostname", problem);
    else if (r.action === "add") second = ruleInput(r, "value", "sinks.rule_value", "dev", problem);
    else second = el("span", { className: "rule-empty", attrs: { "aria-hidden": "true" } });
    var keyHolder = r.action === "add" ? "env" : "host";
    var row = el("div", { className: "rule-row" }, [
      action,
      ruleInput(r, "key", "sinks.rule_key", keyHolder, problem),
      second,
      el("span", { className: "rule-buttons" }, [
        el("button", { className: "btn btn-small rule-up", text: "\u2191", title: t("sinks.rule_up"),
          attrs: { type: "button", "aria-label": t("sinks.rule_up") }, on: { click: function () { moveRule(i, -1); } } }),
        el("button", { className: "btn btn-small rule-down", text: "\u2193", title: t("sinks.rule_down"),
          attrs: { type: "button", "aria-label": t("sinks.rule_down") }, on: { click: function () { moveRule(i, 1); } } }),
        el("button", { className: "btn btn-small btn-danger rule-remove", text: t("sinks.rule_remove"),
          attrs: { type: "button" }, on: { click: function () {
            state.editing.rules.splice(i, 1);
            renderRules($("s-type").value);
            $("s-rule-add").focus();
          } } })
      ]),
      problem
    ]);
    row.querySelector(".rule-up").disabled = i === 0;
    row.querySelector(".rule-down").disabled = i === count - 1;
    showRuleProblem(r, problem);
    return row;
  }

  function addRule() {
    if (!state.editing || state.editing.rules.length >= MAX_TAG_RULES) return;
    state.editing.rules.push({ action: "rename", key: "", new_key: "", value: "" });
    $("s-rules").open = true;
    renderRules($("s-type").value);
    var rows = $("s-rules-list").children;
    rows[rows.length - 1].querySelector(".rule-key").focus();
  }

  function saveSink(ev) {
    ev.preventDefault();
    var box = $("sink-form-result");
    var orig = state.editing && state.editing.original;
    var body = sinkPayload({
      name: $("s-name").value,
      type: $("s-type").value,
      url: $("s-url").value,
      influx_version: $("s-influx_version").value,
      org: $("s-org").value,
      db: $("s-db").value,
      username: $("s-username").value,
      password: $("s-password").value,
      token: $("s-token").value,
      credential_id: $("s-credential_id").value,
      enabled: $("s-enabled").checked,
      tag_rules: state.editing ? state.editing.rules : []
    });
    var req = orig
      ? api("PUT", "/api/v1/sinks/" + encodeURIComponent(orig.id), body)
      : api("POST", "/api/v1/sinks", body);
    req.then(function (res) {
      if (!res.ok) {
        showResponseError(box, res);
        return;
      }
      closeSinkDialog();
      showResult($("sinks-result"), "ok", t("sinks.saved", { name: body.name }));
      loadSinks();
      refreshStatus();
    }, function () {
      showResult(box, "error", t("common.save_failed", { error: t("app.conn_error") }));
    });
  }

  function toast(msg, kind) {
    var n = $("toast");
    n.textContent = String(msg);
    n.className = "toast toast-" + (kind === "error" ? "error" : kind === "warn" ? "warn" : "ok");
    n.hidden = false;
    clearTimeout(state.toastTimer);
    state.toastTimer = setTimeout(function () { n.hidden = true; }, kind === "error" ? 6000 : 3000);
  }

  // ---- start ----

  function bind() {
    window.addEventListener("hashchange", route);
    window.addEventListener("load", function () {
      if (state.tab === "topology") mountTopology();
    });
    document.querySelectorAll(".lang-btn").forEach(function (b) {
      b.addEventListener("click", function () { setLanguage(b.getAttribute("data-lang")); });
    });
    document.querySelectorAll("button.copy").forEach(function (b) {
      b.addEventListener("click", function () { copyFrom(b.getAttribute("data-copy"), b); });
    });
    $("settings-form").addEventListener("submit", saveSettings);
    $("settings-reload").addEventListener("click", function () {
      hideResult($("settings-result"));
      loadSettings();
    });
    $("sink-add").addEventListener("click", function () { openSinkDialog(null); });
    $("sink-form").addEventListener("submit", saveSink);
    $("sink-cancel").addEventListener("click", closeSinkDialog);
    $("sink-dialog").addEventListener("close", function () { state.editing = null; });
    $("s-type").addEventListener("change", renderSinkDialog);
    $("s-influx_version").addEventListener("change", renderSinkDialog);
    $("s-url").addEventListener("input", renderSinkDialog);
    $("s-rule-add").addEventListener("click", addRule);
    document.addEventListener("visibilitychange", function () {
      if (!document.hidden) refreshStatus();
    });
    setInterval(function () {
      if (!document.hidden) refreshStatus();
    }, STATUS_EVERY_MS);
  }

  function start() {
    var saved = null;
    try { saved = localStorage.getItem(LANG_KEY); } catch (e) { /* storage may be blocked */ }
    var lang = initialLanguage(saved, navigator.languages || [navigator.language]);
    bind();
    setLanguage(lang).then(function () {
      route();
      if (state.tab !== "status") refreshStatus();
    });
  }

  // GMRG is what topology.js uses. lang is read live.
  root.GMRG = {
    get lang() { return state.lang; },
    t: t,
    api: function (path, opt) {
      opt = opt || {};
      return api(opt.method || "GET", path, opt.body);
    },
    onLangChange: function (cb) {
      if (typeof cb === "function") state.langListeners.push(cb);
    },
    toast: toast
  };

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", start);
  } else {
    start();
  }
})(typeof window !== "undefined" ? window : globalThis);
