/* omni-metrics console — dependency-free SPA.
   Talks to the Prometheus-compatible API and renders a hand-rolled SVG chart.
   Theming is via the data-theme attribute; chart colors are read from CSS vars
   so the graph adapts to the active theme.

   Note: all label/metric strings from the API are inserted via textContent (DOM
   text nodes), never as markup. The only string->DOM conversion is the chart
   SVG, which is built solely from numbers, colors, and time labels we compute —
   no untrusted content — and is parsed with DOMParser rather than innerHTML. */
(function () {
  "use strict";

  var SERIES_VARS = ["--c1", "--c2", "--c3", "--c4", "--c5", "--c6", "--c7", "--c8"];
  var DEFAULT_QUERY = "rate(omni_http_requests_total[1m])";
  var RANGES = [
    { label: "Last 15 min", seconds: 900 },
    { label: "Last 1 hour", seconds: 3600 },
    { label: "Last 6 hours", seconds: 21600 },
    { label: "Last 24 hours", seconds: 86400 },
  ];

  var state = { query: DEFAULT_QUERY, rangeSeconds: 3600 };
  var lastResult = null;

  /* ---------- dom helpers ---------- */
  function el(tag, attrs, children) {
    var e = document.createElement(tag);
    if (attrs) Object.keys(attrs).forEach(function (k) {
      if (k === "class") e.className = attrs[k];
      else e.setAttribute(k, attrs[k]);
    });
    (children || []).forEach(function (c) {
      e.appendChild(typeof c === "string" ? document.createTextNode(c) : c);
    });
    return e;
  }

  function clear(node) { while (node && node.firstChild) node.removeChild(node.firstChild); }

  function svgFromString(str) {
    var doc = new DOMParser().parseFromString(str, "image/svg+xml");
    return document.importNode(doc.documentElement, true);
  }

  function icon(markup) {
    var span = el("span", {});
    span.style.display = "inline-flex";
    span.appendChild(svgFromString(markup));
    return span;
  }

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  /* ---------- theme ---------- */
  function initTheme() {
    var saved = null;
    try { saved = localStorage.getItem("omni-theme"); } catch (e) {}
    if (!saved) {
      saved = window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
    }
    document.documentElement.setAttribute("data-theme", saved);
    var toggle = document.getElementById("theme-toggle");
    if (toggle) {
      toggle.addEventListener("click", function () {
        var cur = document.documentElement.getAttribute("data-theme");
        var next = cur === "dark" ? "light" : "dark";
        document.documentElement.setAttribute("data-theme", next);
        try { localStorage.setItem("omni-theme", next); } catch (e) {}
        if (location.hash.indexOf("graph") >= 0 && lastResult) renderChartInto(lastResult);
      });
    }
  }

  /* ---------- api ---------- */
  function api(path) {
    return fetch(path, { headers: { Accept: "application/json" } }).then(function (r) {
      return r.json().then(function (body) {
        if (body.status !== "success") throw new Error(body.error || ("HTTP " + r.status));
        return body.data;
      });
    });
  }

  function fmt(v) {
    var n = parseFloat(v);
    if (!isFinite(n)) return String(v);
    if (Math.abs(n) >= 100000 || (n !== 0 && Math.abs(n) < 0.001)) return n.toExponential(2);
    return (Math.round(n * 1000) / 1000).toString();
  }

  function labelText(metric) {
    var name = metric.__name__ || "";
    var parts = [];
    Object.keys(metric).sort().forEach(function (k) {
      if (k === "__name__") return;
      parts.push(k + '="' + metric[k] + '"');
    });
    return name + (parts.length ? "{" + parts.join(", ") + "}" : "");
  }

  function shortLabels(metric) {
    var parts = [];
    Object.keys(metric).sort().forEach(function (k) {
      if (k !== "__name__") parts.push(k + '="' + metric[k] + '"');
    });
    return "{" + parts.join(",") + "}";
  }

  /* ---------- router ---------- */
  function route() {
    var name = (location.hash.replace(/^#\/?/, "") || "graph").split("/")[0];
    document.querySelectorAll("#nav a").forEach(function (a) {
      a.classList.toggle("active", a.getAttribute("data-route") === name);
    });
    var view = document.getElementById("view");
    clear(view);
    if (name === "targets") renderTargets(view);
    else if (name === "status") renderStatus(view);
    else renderGraph(view);
  }

  /* ---------- graph view ---------- */
  function renderGraph(view) {
    view.appendChild(el("div", { class: "page-head" }, [
      el("div", {}, [el("div", { class: "eyebrow" }, ["Explore"]), el("h1", {}, ["Query & graph"])]),
      buildRangeControl(),
    ]));

    var input = el("input", { type: "text", value: state.query, spellcheck: "false", "aria-label": "PromQL query" });
    input.addEventListener("keydown", function (e) { if (e.key === "Enter") { state.query = input.value; runQuery(); } });

    var runBtn = el("button", { class: "run" }, [
      icon('<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M7 5v14l11-7z"/></svg>'),
      "Run",
    ]);
    runBtn.addEventListener("click", function () { state.query = input.value; runQuery(); });

    view.appendChild(el("div", { class: "querybar" }, [
      el("div", { class: "query-input" }, [
        icon('<svg xmlns="http://www.w3.org/2000/svg" width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h4l3 8 4-16 3 8h4"/></svg>'),
        input,
        el("span", { class: "kbd" }, ["⏎"]),
      ]),
      runBtn,
    ]));

    view.appendChild(el("div", { class: "panel" }, [
      el("div", { class: "panel-head" }, [
        el("div", { class: "tabs" }, [el("button", { class: "active" }, ["Graph"]), el("button", {}, ["Table"])]),
        el("div", { class: "meta", id: "chart-meta" }, []),
      ]),
      el("div", { class: "chart-area", id: "chart-area" }, []),
      el("div", { class: "legend", id: "legend" }, []),
    ]));

    view.appendChild(el("div", { class: "panel" }, [
      el("div", { class: "panel-head" }, [
        el("div", { class: "panel-title" }, ["Instant vector"]),
        el("span", { class: "mono", id: "instant-ts" }, []),
      ]),
      el("div", { class: "col-head" }, [
        el("span", { style: "flex:1" }, ["Series"]),
        el("span", { style: "width:140px;text-align:right" }, ["Value"]),
      ]),
      el("div", { class: "table", id: "instant-table" }, []),
    ]));

    runQuery();
  }

  function buildRangeControl() {
    var sel = el("select", {}, RANGES.map(function (r) {
      var o = el("option", { value: String(r.seconds) }, [r.label]);
      if (r.seconds === state.rangeSeconds) o.setAttribute("selected", "selected");
      return o;
    }));
    sel.addEventListener("change", function () { state.rangeSeconds = parseInt(sel.value, 10); runQuery(); });
    return el("div", { class: "controls" }, [el("label", { class: "select" }, [sel])]);
  }

  function runQuery() {
    var end = Math.floor(Date.now() / 1000);
    var start = end - state.rangeSeconds;
    var step = Math.max(1, Math.floor(state.rangeSeconds / 120));
    var q = encodeURIComponent(state.query);
    var meta = document.getElementById("chart-meta");
    if (meta) { clear(meta); meta.appendChild(el("span", { class: "mono" }, ["evaluating…"])); }

    var t0 = performance.now();
    Promise.all([
      api("/api/v1/query_range?query=" + q + "&start=" + start + "&end=" + end + "&step=" + step),
      api("/api/v1/query?query=" + q + "&time=" + end),
    ]).then(function (res) {
      var elapsed = Math.round(performance.now() - t0);
      lastResult = { matrix: res[0].result || [], start: start, end: end };
      renderChartInto(lastResult);
      renderMeta(lastResult.matrix, elapsed);
      renderInstant(res[1].result || [], end);
    }).catch(showError);
  }

  function showError(err) {
    var area = document.getElementById("chart-area");
    if (area) { clear(area); area.appendChild(el("div", { class: "error-banner" }, [String(err.message || err)])); }
    ["legend", "chart-meta", "instant-table"].forEach(function (id) { var n = document.getElementById(id); if (n) clear(n); });
  }

  function renderMeta(matrix, elapsed) {
    var meta = document.getElementById("chart-meta");
    if (!meta) return;
    var pts = matrix.reduce(function (a, s) { return a + (s.values ? s.values.length : 0); }, 0);
    clear(meta);
    meta.appendChild(el("span", { class: "mono" }, [matrix.length + " series"]));
    meta.appendChild(el("span", { class: "sep" }, []));
    meta.appendChild(el("span", { class: "mono" }, [pts + " pts"]));
    meta.appendChild(el("span", { class: "sep" }, []));
    meta.appendChild(el("span", { class: "mono ok" }, [elapsed + " ms"]));
  }

  function renderInstant(vector, endSec) {
    var tbl = document.getElementById("instant-table");
    var ts = document.getElementById("instant-ts");
    if (ts) ts.textContent = "@ " + new Date(endSec * 1000).toISOString().replace("T", " ").slice(0, 19);
    if (!tbl) return;
    clear(tbl);
    if (!vector.length) { tbl.appendChild(el("div", { class: "empty" }, ["No data for this query."])); return; }
    vector.forEach(function (s, i) {
      var color = cssVar(SERIES_VARS[i % SERIES_VARS.length]);
      tbl.appendChild(el("div", { class: "row" }, [
        el("div", { class: "series" }, [el("span", { class: "sdot", style: "background:" + color }, []), el("span", {}, [labelText(s.metric)])]),
        el("span", { class: "value" }, [fmt(s.value[1])]),
      ]));
    });
  }

  /* ---------- hand-rolled SVG chart ---------- */
  function renderChartInto(data) {
    var area = document.getElementById("chart-area");
    var legend = document.getElementById("legend");
    if (!area) return;
    clear(area);
    if (legend) clear(legend);

    var matrix = data.matrix;
    if (!matrix.length) {
      area.appendChild(el("div", { class: "empty" }, ["No data — try rate(omni_http_requests_total[1m])"]));
      return;
    }

    var W = 1358, H = 340, padL = 56, padR = 16, padT = 16, padB = 28;
    var x0 = padL, x1 = W - padR, y0 = padT, y1 = H - padB;
    var tMin = data.start, tMax = data.end;
    var vMax = 0, vMin = 0;
    matrix.forEach(function (s) {
      (s.values || []).forEach(function (p) {
        var v = parseFloat(p[1]);
        if (isFinite(v)) { if (v > vMax) vMax = v; if (v < vMin) vMin = v; }
      });
    });
    if (vMax === vMin) vMax = vMin + 1;
    vMax = vMax + (vMax - vMin) * 0.1;

    function sx(t) { return x0 + (t - tMin) / (tMax - tMin) * (x1 - x0); }
    function sy(v) { return y1 - (v - vMin) / (vMax - vMin) * (y1 - y0); }

    var grid = cssVar("--grid"), axis = cssVar("--axis"), baseline = cssVar("--border");
    var p = ['<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ' + W + ' ' + H + '" preserveAspectRatio="none">'];

    var yTicks = 4;
    for (var i = 0; i <= yTicks; i++) {
      var v = vMin + (vMax - vMin) * (i / yTicks), y = sy(v);
      p.push('<line x1="' + x0 + '" y1="' + y.toFixed(1) + '" x2="' + x1 + '" y2="' + y.toFixed(1) + '" stroke="' + (i === 0 ? baseline : grid) + '" stroke-width="1"/>');
      p.push('<text x="' + (x0 - 10) + '" y="' + (y + 4).toFixed(1) + '" text-anchor="end" font-family="IBM Plex Mono" font-size="11" fill="' + axis + '">' + esc(fmt(v)) + '</text>');
    }
    var xTicks = 6;
    for (var j = 0; j <= xTicks; j++) {
      var t = tMin + (tMax - tMin) * (j / xTicks), x = sx(t);
      var anchor = j === 0 ? "start" : j === xTicks ? "end" : "middle";
      p.push('<text x="' + x.toFixed(1) + '" y="' + (H - 8) + '" text-anchor="' + anchor + '" font-family="IBM Plex Mono" font-size="11" fill="' + axis + '">' + esc(hhmm(t)) + '</text>');
    }
    matrix.forEach(function (s, idx) {
      var color = cssVar(SERIES_VARS[idx % SERIES_VARS.length]);
      var coords = (s.values || []).map(function (pt) { return sx(pt[0]).toFixed(1) + "," + sy(parseFloat(pt[1])).toFixed(1); });
      var line = coords.join(" ");
      if (idx === 0 && coords.length) {
        var areaPoly = line + " " + sx(s.values[s.values.length - 1][0]).toFixed(1) + "," + y1 + " " + sx(s.values[0][0]).toFixed(1) + "," + y1;
        p.push('<polygon points="' + areaPoly + '" fill="' + color + '" fill-opacity="0.08"/>');
      }
      p.push('<polyline points="' + line + '" fill="none" stroke="' + color + '" stroke-width="' + (idx === 0 ? 2.5 : 2) + '" stroke-linejoin="round" stroke-linecap="round"/>');
    });
    p.push("</svg>");
    area.appendChild(svgFromString(p.join("")));

    if (legend) {
      matrix.forEach(function (s, idx) {
        var color = cssVar(SERIES_VARS[idx % SERIES_VARS.length]);
        var vals = s.values || [];
        legend.appendChild(el("div", { class: "item" }, [
          el("span", { class: "swatch", style: "background:" + color }, []),
          el("span", { class: "name" }, [shortLabels(s.metric)]),
          el("span", { class: "val", style: "color:" + color }, [vals.length ? fmt(vals[vals.length - 1][1]) : "–"]),
        ]));
      });
    }
  }

  function esc(s) { return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;"); }

  function hhmm(epochSec) {
    var d = new Date(epochSec * 1000);
    function pad(n) { return (n < 10 ? "0" : "") + n; }
    return pad(d.getHours()) + ":" + pad(d.getMinutes());
  }

  /* ---------- targets view ---------- */
  function renderTargets(view) {
    api("/api/v1/targets").then(function (targets) {
      targets = targets || [];
      var up = targets.filter(function (t) { return t.up; }).length;
      view.appendChild(el("div", { class: "page-head" }, [
        el("div", {}, [
          el("div", { class: "eyebrow" }, ["Scrape"]),
          el("div", { class: "summary" }, [
            el("h1", {}, ["Targets"]),
            el("span", { class: "mono up" }, [up + " up"]),
            el("span", { class: "mono down" }, [(targets.length - up) + " down"]),
          ]),
        ]),
      ]));
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { class: "tcol-ep" }, ["Endpoint"]),
          el("span", { class: "tcol-state" }, ["State"]),
          el("span", { class: "tcol-labels" }, ["Labels"]),
          el("span", { class: "tcol-scrape" }, ["Last scrape"]),
          el("span", { class: "tcol-dur" }, ["Duration"]),
          el("span", { class: "tcol-err", style: "color:var(--text-dim)" }, ["Error"]),
        ]),
      ]);
      if (!targets.length) panel.appendChild(el("div", { class: "empty" }, ["No scrape targets configured."]));
      targets.forEach(function (t) {
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { class: "tcol-ep" }, [stripScheme(t.scrapeUrl)]),
          el("span", { class: "tcol-state" }, [statePill(t.up)]),
          el("span", { class: "tcol-labels" }, ['job="' + t.job + '" instance="' + t.instance + '"']),
          el("span", { class: "tcol-scrape" }, [t.lastScrape && new Date(t.lastScrape).getTime() ? ago(t.lastScrape) : "–"]),
          el("span", { class: "tcol-dur" }, [t.up ? (t.lastScrapeDuration * 1000).toFixed(1) + " ms" : "–"]),
          el("span", { class: "tcol-err" }, [t.lastError || ""]),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (err) { view.appendChild(el("div", { class: "error-banner" }, [String(err.message || err)])); });
  }

  function statePill(up) {
    var pill = el("span", { class: "pill " + (up ? "up" : "down") }, [el("span", { class: "dot " + (up ? "ok" : "err") }, [])]);
    pill.appendChild(document.createTextNode(up ? "UP" : "DOWN"));
    return pill;
  }

  function stripScheme(u) { return (u || "").replace(/^https?:\/\//, ""); }

  function ago(iso) {
    var secs = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
    if (secs < 60) return secs + "s ago";
    if (secs < 3600) return Math.floor(secs / 60) + "m ago";
    return Math.floor(secs / 3600) + "h ago";
  }

  /* ---------- status view ---------- */
  function renderStatus(view) {
    view.appendChild(el("div", { class: "page-head" }, [
      el("div", {}, [el("div", { class: "eyebrow" }, ["Runtime"]), el("h1", {}, ["Status"])]),
    ]));
    var cards = el("div", { class: "cards" }, []);
    view.appendChild(cards);
    function card(k, v) { cards.appendChild(el("div", { class: "card" }, [el("div", { class: "k" }, [k]), el("div", { class: "v" }, [v])])); }

    api("/api/v1/query?query=omni_build_info").then(function (d) {
      card("Version", d.result && d.result[0] ? (d.result[0].metric.version || "?") : "?");
    }).catch(function () { card("Version", "?"); });
    api("/api/v1/query?query=omni_head_series").then(function (d) {
      card("Head series", d.result && d.result[0] ? fmt(d.result[0].value[1]) : "0");
    }).catch(function () {});
    api("/api/v1/query?query=omni_queries_total").then(function (d) {
      card("Queries served", d.result && d.result[0] ? fmt(d.result[0].value[1]) : "0");
    }).catch(function () {});
    api("/api/v1/query?query=omni_start_time_seconds").then(function (d) {
      if (d.result && d.result[0]) {
        var up = Math.max(0, Math.round(Date.now() / 1000 - parseFloat(d.result[0].value[1])));
        card("Uptime", up < 3600 ? Math.floor(up / 60) + "m" : (up / 3600).toFixed(1) + "h");
      }
    }).catch(function () {});
  }

  /* ---------- boot ---------- */
  initTheme();
  window.addEventListener("hashchange", route);
  if (!location.hash) location.hash = "#/graph";
  route();
})();
