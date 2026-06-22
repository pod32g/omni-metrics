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

  // Autocomplete vocabulary: the supported PromQL functions/aggregations/keywords
  // (the engine is a documented subset) plus metric and label names fetched live.
  var PROMQL_FUNCS = [
    { text: "rate", kind: "func" }, { text: "irate", kind: "func" }, { text: "increase", kind: "func" },
    { text: "sum_over_time", kind: "func" }, { text: "avg_over_time", kind: "func" },
    { text: "min_over_time", kind: "func" }, { text: "max_over_time", kind: "func" },
    { text: "count_over_time", kind: "func" },
    { text: "sum", kind: "agg" }, { text: "avg", kind: "agg" }, { text: "min", kind: "agg" },
    { text: "max", kind: "agg" }, { text: "count", kind: "agg" },
  ];
  // by/without are only valid as a grouping clause on an aggregation, so they are
  // suggested separately (see computeSuggestions), not in the default pool.
  var PROMQL_KEYWORDS = [{ text: "by", kind: "kw" }, { text: "without", kind: "kw" }];
  var AGG_BEFORE = /\b(sum|avg|min|max|count)\b\s*(\([^)]*\)\s*)?$/;
  var acData = { metrics: [], labels: [] };

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
    var parts = (location.hash.replace(/^#\/?/, "") || "graph").split("/");
    var name = parts[0];
    document.querySelectorAll("#nav a").forEach(function (a) {
      a.classList.toggle("active", a.getAttribute("data-route") === name);
    });
    var view = document.getElementById("view");
    clear(view);
    if (name === "targets") renderTargets(view);
    else if (name === "pushers") renderPushers(view);
    else if (name === "status") renderStatus(view);
    else if (name === "alerts") renderAlerts(view, parts);
    else renderGraph(view);
  }

  /* ---------- api write helper ---------- */
  function apiSend(method, path, body) {
    return fetch(path, {
      method: method,
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (r) {
      return r.json().then(function (b) {
        if (b.status !== "success") throw new Error(b.error || ("HTTP " + r.status));
        return b.data;
      });
    });
  }

  /* ---------- graph view ---------- */
  function renderGraph(view) {
    view.appendChild(el("div", { class: "page-head" }, [
      el("div", {}, [el("div", { class: "eyebrow" }, ["Explore"]), el("h1", {}, ["Query & graph"])]),
      buildRangeControl(),
    ]));

    var input = el("input", { type: "text", value: state.query, spellcheck: "false", autocomplete: "off", "aria-label": "PromQL query" });

    var runBtn = el("button", { class: "run" }, [
      icon('<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><path d="M7 5v14l11-7z"/></svg>'),
      "Run",
    ]);
    runBtn.addEventListener("click", function () { state.query = input.value; runQuery(); });

    var dropdown = el("div", { class: "ac-dropdown", role: "listbox" }, []);
    dropdown.style.display = "none";
    var queryWrap = el("div", { class: "query-wrap" }, [
      el("div", { class: "query-input" }, [
        icon('<svg xmlns="http://www.w3.org/2000/svg" width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h4l3 8 4-16 3 8h4"/></svg>'),
        input,
        el("span", { class: "kbd" }, ["⏎"]),
      ]),
      dropdown,
    ]);

    view.appendChild(el("div", { class: "querybar" }, [queryWrap, runBtn]));

    renderAutocomplete(input, dropdown, function () { state.query = input.value; runQuery(); });
    loadAutocompleteData();

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
    if (!state.query || !state.query.trim()) { renderEmptyQuery(); return; }
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

  /* ---------- empty query state ---------- */
  function renderEmptyQuery() {
    ["legend", "chart-meta", "instant-table", "instant-ts"].forEach(function (id) {
      var n = document.getElementById(id); if (n) clear(n);
    });
    var area = document.getElementById("chart-area");
    if (!area) return;
    clear(area);
    var chips = ["up", DEFAULT_QUERY, "omni_head_series"].map(function (q) {
      var chip = el("button", { class: "example-chip", type: "button" }, [q]);
      chip.addEventListener("click", function () {
        var inp = document.querySelector(".query-input input");
        if (inp) inp.value = q;
        state.query = q;
        runQuery();
      });
      return chip;
    });
    area.appendChild(el("div", { class: "empty" }, [
      el("div", {}, ["Enter a PromQL query to graph it — or pick an example:"]),
      el("div", { class: "examples" }, chips),
    ]));
  }

  /* ---------- query autocomplete ---------- */
  function loadAutocompleteData() {
    api("/api/v1/label/__name__/values").then(function (v) { acData.metrics = v || []; }).catch(function () {});
    api("/api/v1/labels").then(function (v) {
      acData.labels = (v || []).filter(function (n) { return n !== "__name__"; });
    }).catch(function () {});
  }

  // tokenAt returns the identifier being typed (the run of name characters ending
  // at the caret) and where it starts.
  function tokenAt(value, caret) {
    var start = caret;
    while (start > 0 && /[A-Za-z0-9_:]/.test(value.charAt(start - 1))) start--;
    return { start: start, prefix: value.slice(start, caret) };
  }

  // tokenEnd returns the index just past the identifier the caret sits in, scanning
  // right over name characters — so accepting a completion replaces the whole token,
  // not just the part left of the caret.
  function tokenEnd(value, caret) {
    var end = caret;
    while (end < value.length && /[A-Za-z0-9_:]/.test(value.charAt(end))) end++;
    return end;
  }

  // labelContext scans value[0..caret) to classify the caret: inside a {…} label
  // set, inside a quoted value, or just after '=' (a label-value position).
  function labelContext(value, caret) {
    var depth = 0, inQuote = false, afterEq = false;
    for (var i = 0; i < caret; i++) {
      var ch = value.charAt(i);
      if (inQuote) {
        if (ch === '"') {
          // A quote terminates the string only if preceded by an even number of
          // backslashes (an odd count means the quote itself is escaped).
          var bs = 0, j = i - 1;
          while (j >= 0 && value.charAt(j) === "\\") { bs++; j--; }
          if (bs % 2 === 0) inQuote = false;
        }
        continue;
      }
      if (ch === '"') { inQuote = true; afterEq = false; }
      else if (ch === "{") { depth++; afterEq = false; }
      else if (ch === "}") { if (depth > 0) depth--; afterEq = false; }
      else if (ch === "=") { afterEq = true; }
      else if (ch === ",") { afterEq = false; }
    }
    return { inBraces: depth > 0, inQuote: inQuote, afterEq: afterEq };
  }

  // computeSuggestions picks the candidate list for the caret: label names inside
  // {…}, otherwise metric names + PromQL funcs/keywords. Returns null when nothing
  // should be shown (typing a value, or no prefix outside a fresh label set).
  function computeSuggestions(value, caret) {
    var ctx = labelContext(value, caret);
    if (ctx.inQuote || ctx.afterEq) return null;
    var tok = tokenAt(value, caret);
    var pool;
    if (ctx.inBraces) {
      pool = acData.labels.map(function (n) { return { text: n, kind: "label" }; });
    } else {
      pool = acData.metrics.map(function (n) { return { text: n, kind: "metric" }; }).concat(PROMQL_FUNCS);
      if (AGG_BEFORE.test(value.slice(0, tok.start))) pool = pool.concat(PROMQL_KEYWORDS);
    }
    if (!tok.prefix) {
      // Skip whitespace so the label list still opens after "comma + space".
      var pi = tok.start - 1;
      while (pi >= 0 && (value.charAt(pi) === " " || value.charAt(pi) === "\t")) pi--;
      var prev = pi >= 0 ? value.charAt(pi) : "";
      if (!(ctx.inBraces && (prev === "{" || prev === ","))) return null;
    }
    var pl = tok.prefix.toLowerCase();
    var matches = pool.filter(function (s) { return s.text.toLowerCase().indexOf(pl) === 0; });
    matches.sort(function (a, b) { return a.text.length - b.text.length || (a.text < b.text ? -1 : 1); });
    if (!matches.length) return null;
    return { items: matches.slice(0, 12), prefix: tok.prefix };
  }

  function renderAutocomplete(input, dropdown, onRun) {
    var items = [], active = -1, prefixLower = "";

    function close() { items = []; active = -1; clear(dropdown); dropdown.style.display = "none"; }

    function paintActive() {
      var rows = dropdown.childNodes;
      for (var i = 0; i < rows.length; i++) rows[i].className = "ac-item" + (i === active ? " active" : "");
      if (active >= 0 && rows[active] && rows[active].scrollIntoView) rows[active].scrollIntoView({ block: "nearest" });
    }

    // nameNode bolds the matched prefix using text nodes only (never innerHTML),
    // so metric/label names from the API can never inject markup.
    function nameNode(text) {
      var name = el("span", { class: "ac-name" }, []);
      if (prefixLower && text.toLowerCase().indexOf(prefixLower) === 0) {
        name.appendChild(el("b", {}, [text.slice(0, prefixLower.length)]));
        name.appendChild(document.createTextNode(text.slice(prefixLower.length)));
      } else {
        name.appendChild(document.createTextNode(text));
      }
      return name;
    }

    function render(matches) {
      clear(dropdown);
      items = matches;
      active = matches.length ? 0 : -1;
      matches.forEach(function (s, i) {
        var row = el("div", { class: "ac-item" + (i === 0 ? " active" : ""), role: "option" }, [
          nameNode(s.text),
          el("span", { class: "ac-kind" }, [s.kind]),
        ]);
        // mousedown (not click) so it fires before the input's blur closes us.
        row.addEventListener("mousedown", function (e) { e.preventDefault(); accept(s); });
        row.addEventListener("mouseenter", function () { active = i; paintActive(); });
        dropdown.appendChild(row);
      });
      dropdown.style.display = "block";
    }

    function update() {
      var res = computeSuggestions(input.value, input.selectionStart);
      if (!res) { close(); return; }
      prefixLower = res.prefix.toLowerCase();
      render(res.items);
    }

    // wouldChange reports whether accepting choice would actually edit the query —
    // used so Enter runs (rather than re-accepting) when the highlighted item is
    // already fully typed.
    function wouldChange(choice) {
      var value = input.value, caret = input.selectionStart;
      var tok = tokenAt(value, caret), end = tokenEnd(value, caret);
      var insert = choice.text;
      if ((choice.kind === "func" || choice.kind === "agg") && value.charAt(end) !== "(") insert += "(";
      return value.slice(tok.start, end) !== insert;
    }

    function accept(choice) {
      var value = input.value, caret = input.selectionStart;
      var tok = tokenAt(value, caret), end = tokenEnd(value, caret);
      var before = value.slice(0, tok.start);
      var after = value.slice(end); // suffix after the whole token, not just the caret
      var insert = choice.text;
      if ((choice.kind === "func" || choice.kind === "agg") && after.charAt(0) !== "(") insert += "(";
      input.value = before + insert + after;
      state.query = input.value;
      var pos = before.length + insert.length;
      close();
      input.focus();
      input.setSelectionRange(pos, pos);
    }

    input.addEventListener("input", update);
    input.addEventListener("focus", update);
    input.addEventListener("blur", function () { setTimeout(close, 150); });
    input.addEventListener("keydown", function (e) {
      if (items.length) {
        if (e.key === "ArrowDown") { active = (active + 1) % items.length; paintActive(); e.preventDefault(); return; }
        if (e.key === "ArrowUp") { active = (active - 1 + items.length) % items.length; paintActive(); e.preventDefault(); return; }
        if (e.key === "Escape") { close(); e.preventDefault(); return; }
        if (e.key === "Tab" && active >= 0) { accept(items[active]); e.preventDefault(); return; }
        // Enter accepts a suggestion only when it would change the query; if the
        // highlighted item is already fully typed, fall through and run instead.
        if (e.key === "Enter" && active >= 0 && wouldChange(items[active])) { accept(items[active]); e.preventDefault(); return; }
      }
      if (e.key === "Enter") { close(); onRun(); }
    });
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

  /* ---------- pushers view ---------- */
  function renderPushers(view) {
    api("/api/v1/push/sources").then(function (sources) {
      sources = sources || [];
      var ok = sources.filter(function (s) { return !s.lastError; }).length;
      view.appendChild(el("div", { class: "page-head" }, [
        el("div", {}, [
          el("div", { class: "eyebrow" }, ["Push"]),
          el("div", { class: "summary" }, [
            el("h1", {}, ["Pushers"]),
            el("span", { class: "mono up" }, [ok + " ok"]),
            el("span", { class: "mono down" }, [(sources.length - ok) + " err"]),
          ]),
        ]),
      ]));
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "flex:1.4" }, ["Source"]),
          el("span", { style: "width:90px" }, ["State"]),
          el("span", { style: "width:120px" }, ["Last push"]),
          el("span", { style: "width:90px;text-align:right" }, ["Pushes"]),
          el("span", { style: "width:110px;text-align:right;padding-right:24px" }, ["Samples"]),
          el("span", { style: "flex:1", "data-dim": "1" }, ["Error"]),
        ]),
      ]);
      if (!sources.length) panel.appendChild(el("div", { class: "empty" }, ["No pushers have reported yet."]));
      sources.forEach(function (s) {
        var fresh = !s.lastError;
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { style: "flex:1.4" }, ['job="' + s.job + '" instance="' + s.instance + '"']),
          el("span", { style: "width:90px" }, [okPill(fresh)]),
          el("span", { style: "width:120px" }, [s.lastPush && new Date(s.lastPush).getTime() ? ago(s.lastPush) : "–"]),
          el("span", { class: "mono", style: "width:90px;text-align:right;display:inline-block" }, [String(s.pushesTotal || 0)]),
          el("span", { class: "mono", style: "width:110px;text-align:right;padding-right:24px;display:inline-block" }, [String(s.samplesTotal || 0)]),
          el("span", { style: "flex:1" }, [s.lastError || ""]),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (err) { view.appendChild(el("div", { class: "error-banner" }, [String(err.message || err)])); });
  }

  function okPill(ok) {
    var pill = el("span", { class: "pill " + (ok ? "up" : "down") }, [el("span", { class: "dot " + (ok ? "ok" : "err") }, [])]);
    pill.appendChild(document.createTextNode(ok ? "OK" : "ERR"));
    return pill;
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

  /* ---------- alerting ---------- */
  var SEVERITIES = ["critical", "warning", "info"];

  function severityChip(sev) {
    var s = (sev || "").toLowerCase();
    var known = SEVERITIES.indexOf(s) >= 0 ? s : "other";
    return el("span", { class: "sev sev-" + known }, [sev || "—"]);
  }

  function stateChip(state) {
    var s = (state || "ok").toLowerCase();
    return el("span", { class: "astate astate-" + s }, [s]);
  }

  function durationSince(iso) {
    var ms = Date.now() - new Date(iso).getTime();
    if (!isFinite(ms) || ms < 0) ms = 0;
    var secs = Math.round(ms / 1000);
    if (secs < 60) return secs + "s";
    if (secs < 3600) return Math.floor(secs / 60) + "m " + (secs % 60) + "s";
    if (secs < 86400) return Math.floor(secs / 3600) + "h " + Math.floor((secs % 3600) / 60) + "m";
    return Math.floor(secs / 86400) + "d " + Math.floor((secs % 86400) / 3600) + "h";
  }

  function tsText(iso) {
    var t = new Date(iso).getTime();
    if (!t) return "—";
    return new Date(t).toISOString().replace("T", " ").slice(0, 19);
  }

  function alertsHead(view, sub) {
    var tabs = [
      { key: "rules", label: "Rules", hash: "#/alerts" },
      { key: "active", label: "Active", hash: "#/alerts/active" },
      { key: "history", label: "History", hash: "#/alerts/history" },
      { key: "datasources", label: "Datasources", hash: "#/alerts/datasources" },
    ];
    var nav = el("div", { class: "tabs alerts-tabs" }, tabs.map(function (t) {
      var a = el("a", { href: t.hash, class: t.key === sub ? "active" : "" }, [t.label]);
      return a;
    }));
    var actions = el("div", {}, []);
    if (sub === "rules") {
      var newBtn = el("a", { class: "run", href: "#/alerts/new" }, ["+ New rule"]);
      actions.appendChild(newBtn);
    }
    view.appendChild(el("div", { class: "page-head" }, [
      el("div", {}, [el("div", { class: "eyebrow" }, ["Alerting"]), el("h1", {}, ["Alerts"])]),
      actions,
    ]));
    view.appendChild(nav);
  }

  function alertsError(view, err) {
    view.appendChild(el("div", { class: "error-banner" }, [String(err.message || err)]));
  }

  function renderAlerts(view, parts) {
    var sub = parts[1] || "rules";
    if (sub === "new") return renderRuleEditor(view, null);
    if (sub === "edit") return renderRuleEditor(view, parts[2]);
    alertsHead(view, sub === "edit" ? "rules" : sub);
    var content = el("div", {}, []);
    view.appendChild(content);
    if (sub === "active") renderActiveAlerts(content);
    else if (sub === "history") renderAlertHistory(content);
    else if (sub === "datasources") renderDatasources(content);
    else renderRulesList(content);
  }

  function ruleStateIndex(active) {
    // Map rule_id -> worst current state + max value.
    var idx = {};
    (active || []).forEach(function (in_) {
      var cur = idx[in_.rule_id] || { state: "ok", value: null };
      var rank = { ok: 0, pending: 1, firing: 2 };
      if ((rank[in_.status] || 0) >= (rank[cur.state] || 0)) {
        cur.state = in_.status;
        cur.value = in_.current_value;
      }
      idx[in_.rule_id] = cur;
    });
    return idx;
  }

  function renderRulesList(view) {
    Promise.all([api("/api/v1/alerts"), api("/api/v1/alerts/active")]).then(function (res) {
      var rules = res[0] || [], idx = ruleStateIndex(res[1] || []);
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "flex:1.4" }, ["Rule"]),
          el("span", { style: "width:90px" }, ["Severity"]),
          el("span", { style: "width:80px" }, ["State"]),
          el("span", { style: "width:90px;text-align:right" }, ["Value"]),
          el("span", { style: "width:70px;text-align:right" }, ["Every"]),
          el("span", { style: "width:200px;text-align:right;padding-right:8px" }, ["Actions"]),
        ]),
      ]);
      if (!rules.length) panel.appendChild(el("div", { class: "empty" }, ["No alert rules yet — create one."]));
      rules.forEach(function (r) {
        var st = idx[r.id] || { state: "ok", value: null };
        var nameCell = el("span", { style: "flex:1.4" }, [
          el("div", {}, [el("a", { href: "#/alerts/edit/" + r.id, class: "rule-name" }, [r.name])]),
          el("div", { class: "mono rule-q" }, [r.promql]),
        ]);
        panel.appendChild(el("div", { class: "row" + (r.enabled ? "" : " row-disabled") }, [
          nameCell,
          el("span", { style: "width:90px" }, [severityChip(r.severity)]),
          el("span", { style: "width:80px" }, [stateChip(st.state)]),
          el("span", { class: "mono", style: "width:90px;text-align:right;display:inline-block" }, [st.value == null ? "—" : fmt(st.value)]),
          el("span", { class: "mono", style: "width:70px;text-align:right;display:inline-block" }, [r.evaluation_interval_seconds + "s"]),
          ruleActions(r),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (e) { alertsError(view, e); });
  }

  function ruleActions(r) {
    var wrap = el("span", { class: "act", style: "width:200px;display:inline-flex;gap:6px;justify-content:flex-end" }, []);
    var evalBtn = el("button", { class: "mini", title: "Evaluate now" }, ["Eval"]);
    evalBtn.addEventListener("click", function () {
      apiSend("POST", "/api/v1/alerts/" + r.id + "/evaluate").then(route).catch(alertWindow);
    });
    var toggle = el("button", { class: "mini", title: r.enabled ? "Disable" : "Enable" }, [r.enabled ? "Disable" : "Enable"]);
    toggle.addEventListener("click", function () {
      apiSend("POST", "/api/v1/alerts/" + r.id + "/" + (r.enabled ? "disable" : "enable")).then(route).catch(alertWindow);
    });
    var del = el("button", { class: "mini danger", title: "Delete" }, ["Delete"]);
    del.addEventListener("click", function () {
      if (!window.confirm("Delete rule \"" + r.name + "\"? History is retained.")) return;
      apiSend("DELETE", "/api/v1/alerts/" + r.id).then(route).catch(alertWindow);
    });
    wrap.appendChild(evalBtn);
    wrap.appendChild(toggle);
    wrap.appendChild(del);
    return wrap;
  }

  function alertWindow(err) { window.alert(String(err.message || err)); }

  function renderActiveAlerts(view) {
    Promise.all([api("/api/v1/alerts/active"), api("/api/v1/alerts")]).then(function (res) {
      var active = res[0] || [], rules = res[1] || [];
      var byId = {};
      rules.forEach(function (r) { byId[r.id] = r; });
      var firing = active.filter(function (a) { return a.status === "firing"; }).length;
      view.appendChild(el("div", { class: "summary alerts-summary" }, [
        el("span", { class: "mono down" }, [firing + " firing"]),
        el("span", { class: "mono" }, [(active.length - firing) + " pending"]),
      ]));
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "width:80px" }, ["Status"]),
          el("span", { style: "width:90px" }, ["Severity"]),
          el("span", { style: "flex:1.4" }, ["Rule"]),
          el("span", { style: "width:110px;text-align:right" }, ["Value"]),
          el("span", { style: "width:150px;text-align:right" }, ["Started"]),
          el("span", { style: "width:110px;text-align:right;padding-right:8px" }, ["Duration"]),
        ]),
      ]);
      if (!active.length) panel.appendChild(el("div", { class: "empty" }, ["No active alerts."]));
      active.forEach(function (a) {
        var rule = byId[a.rule_id] || {};
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { style: "width:80px" }, [stateChip(a.status)]),
          el("span", { style: "width:90px" }, [severityChip(rule.severity)]),
          el("span", { style: "flex:1.4" }, [
            el("div", {}, [rule.name || a.rule_id]),
            el("div", { class: "mono rule-q" }, [shortLabels(a.labels || {})]),
          ]),
          el("span", { class: "mono", style: "width:110px;text-align:right;display:inline-block" }, [fmt(a.current_value)]),
          el("span", { class: "mono", style: "width:150px;text-align:right;display:inline-block" }, [tsText(a.started_at)]),
          el("span", { class: "mono", style: "width:110px;text-align:right;display:inline-block;padding-right:8px" }, [durationSince(a.started_at)]),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (e) { alertsError(view, e); });
  }

  function renderAlertHistory(view) {
    Promise.all([api("/api/v1/alerts/history?limit=200"), api("/api/v1/alerts")]).then(function (res) {
      var hist = res[0] || [], rules = res[1] || [];
      var byId = {};
      rules.forEach(function (r) { byId[r.id] = r; });
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "width:160px" }, ["Time"]),
          el("span", { style: "flex:1.2" }, ["Rule"]),
          el("span", { style: "width:170px" }, ["Transition"]),
          el("span", { style: "width:90px;text-align:right" }, ["Value"]),
          el("span", { style: "flex:1;padding-left:16px" }, ["Reason"]),
        ]),
      ]);
      if (!hist.length) panel.appendChild(el("div", { class: "empty" }, ["No transitions recorded yet."]));
      // Newest first.
      hist.slice().reverse().forEach(function (h) {
        var rule = byId[h.rule_id] || {};
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { class: "mono", style: "width:160px" }, [tsText(h.timestamp)]),
          el("span", { style: "flex:1.2" }, [rule.name || h.rule_id]),
          el("span", { style: "width:170px" }, [stateChip(h.previous_state), el("span", { class: "arrow" }, ["→"]), stateChip(h.new_state)]),
          el("span", { class: "mono", style: "width:90px;text-align:right;display:inline-block" }, [fmt(h.value)]),
          el("span", { style: "flex:1;padding-left:16px" }, [h.reason || ""]),
        ]));
      });
      view.appendChild(panel);
    }).catch(function (e) { alertsError(view, e); });
  }

  /* ---------- rule editor ---------- */
  function renderRuleEditor(view, id) {
    alertsHead(view, "rules");
    var form = el("div", { class: "panel editor" }, []);
    view.appendChild(form);

    Promise.all([
      api("/api/v1/datasources").catch(function () { return []; }),
      id ? api("/api/v1/alerts/" + id) : Promise.resolve(null),
    ]).then(function (res) {
      var datasources = res[0] || [];
      var rule = res[1] ? res[1].rule : { name: "", description: "", promql: "", evaluation_interval_seconds: 30, for_duration_seconds: 0, severity: "warning", datasource_id: "", labels: {}, annotations: {}, enabled: true };
      buildRuleForm(form, rule, datasources, id);
    }).catch(function (e) { alertsError(form, e); });
  }

  function field(labelText, control) {
    return el("label", { class: "field" }, [el("span", { class: "flabel" }, [labelText]), control]);
  }

  function buildRuleForm(form, rule, datasources, id) {
    clear(form);
    var name = el("input", { type: "text", value: rule.name || "", placeholder: "High error rate" });
    var desc = el("input", { type: "text", value: rule.description || "", placeholder: "Optional description" });
    var promql = el("textarea", { rows: "3", spellcheck: "false", placeholder: "sum(rate(http_requests_total{status=~\"5..\"}[5m])) > 5" });
    promql.value = rule.promql || "";

    var dsSel = el("select", {}, datasources.map(function (d) {
      var o = el("option", { value: d.id }, [d.name + (d.source !== "api" ? " (" + d.source + ")" : "")]);
      if (d.id === rule.datasource_id) o.setAttribute("selected", "selected");
      return o;
    }));
    if (!datasources.length) dsSel.appendChild(el("option", { value: "" }, ["(default)"]));

    var sevSel = el("select", {}, SEVERITIES.map(function (s) {
      var o = el("option", { value: s }, [s]);
      if (s === rule.severity) o.setAttribute("selected", "selected");
      return o;
    }));
    if (SEVERITIES.indexOf(rule.severity) < 0 && rule.severity) {
      var o = el("option", { value: rule.severity }, [rule.severity]);
      o.setAttribute("selected", "selected");
      sevSel.appendChild(o);
    }

    var interval = el("input", { type: "number", min: "1", value: String(rule.evaluation_interval_seconds || 30) });
    var forD = el("input", { type: "number", min: "0", value: String(rule.for_duration_seconds || 0) });
    var enabled = el("input", { type: "checkbox" });
    if (rule.enabled !== false) enabled.setAttribute("checked", "checked");

    var labelsEd = kvEditor(rule.labels || {});
    var annEd = kvEditor(rule.annotations || {});

    form.appendChild(field("Name", name));
    form.appendChild(field("Description", desc));
    form.appendChild(field("PromQL expression", promql));
    form.appendChild(el("div", { class: "field-row" }, [
      field("Datasource", dsSel),
      field("Severity", sevSel),
      field("Evaluation interval (s)", interval),
      field("For duration (s)", forD),
    ]));
    form.appendChild(field("Labels", labelsEd.node));
    form.appendChild(field("Annotations", annEd.node));
    form.appendChild(el("label", { class: "field toggle" }, [enabled, el("span", {}, ["Enabled"])]));

    var err = el("div", { class: "form-err" }, []);
    var save = el("button", { class: "run" }, [id ? "Save changes" : "Create rule"]);
    save.addEventListener("click", function () {
      clear(err);
      var body = {
        name: name.value.trim(),
        description: desc.value,
        promql: promql.value.trim(),
        datasource_id: dsSel.value,
        severity: sevSel.value,
        evaluation_interval_seconds: parseInt(interval.value, 10) || 0,
        for_duration_seconds: parseInt(forD.value, 10) || 0,
        labels: labelsEd.value(),
        annotations: annEd.value(),
        enabled: enabled.checked,
      };
      var p = id ? apiSend("PUT", "/api/v1/alerts/" + id, body) : apiSend("POST", "/api/v1/alerts", body);
      p.then(function () { location.hash = "#/alerts"; }).catch(function (e) {
        err.appendChild(el("span", {}, [String(e.message || e)]));
      });
    });
    var cancel = el("a", { class: "mini", href: "#/alerts" }, ["Cancel"]);
    form.appendChild(el("div", { class: "form-actions" }, [save, cancel, err]));
  }

  // kvEditor builds a dynamic key/value editor seeded from obj.
  function kvEditor(obj) {
    var rows = el("div", { class: "kv-rows" }, []);
    function addRow(k, v) {
      var key = el("input", { type: "text", value: k || "", placeholder: "key", class: "kv-key" });
      var val = el("input", { type: "text", value: v || "", placeholder: "value", class: "kv-val" });
      var rm = el("button", { class: "mini danger", title: "Remove" }, ["×"]);
      var row = el("div", { class: "kv-row" }, [key, val, rm]);
      rm.addEventListener("click", function () { rows.removeChild(row); });
      rows.appendChild(row);
    }
    Object.keys(obj || {}).forEach(function (k) { addRow(k, obj[k]); });
    var add = el("button", { class: "mini", type: "button" }, ["+ Add"]);
    add.addEventListener("click", function () { addRow("", ""); });
    var node = el("div", { class: "kv" }, [rows, add]);
    return {
      node: node,
      value: function () {
        var out = {};
        rows.querySelectorAll(".kv-row").forEach(function (r) {
          var k = r.querySelector(".kv-key").value.trim();
          var v = r.querySelector(".kv-val").value;
          if (k) out[k] = v;
        });
        return out;
      },
    };
  }

  /* ---------- datasources ---------- */
  function renderDatasources(view) {
    api("/api/v1/datasources").then(function (list) {
      list = list || [];
      var panel = el("div", { class: "panel" }, [
        el("div", { class: "col-head" }, [
          el("span", { style: "flex:1" }, ["Name"]),
          el("span", { style: "flex:1.4" }, ["URL"]),
          el("span", { style: "width:90px" }, ["Auth"]),
          el("span", { style: "width:90px" }, ["Source"]),
          el("span", { style: "width:160px;text-align:right;padding-right:8px" }, ["Actions"]),
        ]),
      ]);
      list.forEach(function (d) {
        var acts = el("span", { style: "width:160px;display:inline-flex;gap:6px;justify-content:flex-end" }, []);
        var test = el("button", { class: "mini" }, ["Test"]);
        test.addEventListener("click", function () {
          apiSend("POST", "/api/v1/datasources/" + d.id + "/test")
            .then(function () { window.alert("Datasource OK"); })
            .catch(alertWindow);
        });
        acts.appendChild(test);
        if (d.source === "api") {
          var del = el("button", { class: "mini danger" }, ["Delete"]);
          del.addEventListener("click", function () {
            if (!window.confirm("Delete datasource \"" + d.name + "\"?")) return;
            apiSend("DELETE", "/api/v1/datasources/" + d.id).then(route).catch(alertWindow);
          });
          acts.appendChild(del);
        } else {
          acts.appendChild(el("span", { class: "mono", "data-dim": "1", style: "align-self:center" }, ["read-only"]));
        }
        panel.appendChild(el("div", { class: "row" }, [
          el("span", { style: "flex:1" }, [d.name]),
          el("span", { class: "mono", style: "flex:1.4" }, [d.base_url]),
          el("span", { style: "width:90px" }, [d.auth_type]),
          el("span", { style: "width:90px" }, [d.source]),
          acts,
        ]));
      });
      view.appendChild(panel);
      view.appendChild(datasourceCreator());
    }).catch(function (e) { alertsError(view, e); });
  }

  function datasourceCreator() {
    var name = el("input", { type: "text", placeholder: "remote-prometheus" });
    var url = el("input", { type: "text", placeholder: "https://prom.example" });
    var authSel = el("select", {}, ["none", "bearer", "basic"].map(function (a) { return el("option", { value: a }, [a]); }));
    var cred = el("input", { type: "password", placeholder: "bearer token / password" });
    var user = el("input", { type: "text", placeholder: "basic user (basic only)" });
    var timeout = el("input", { type: "number", min: "1", value: "30000" });
    var err = el("div", { class: "form-err" }, []);
    var add = el("button", { class: "run" }, ["Add datasource"]);
    add.addEventListener("click", function () {
      clear(err);
      var body = {
        name: name.value.trim(),
        type: "prometheus",
        base_url: url.value.trim(),
        auth_type: authSel.value,
        timeout_ms: parseInt(timeout.value, 10) || 30000,
        enabled: true,
      };
      if (authSel.value === "bearer") body.credentials = cred.value;
      if (authSel.value === "basic") { body.basic_user = user.value; body.basic_pass = cred.value; }
      apiSend("POST", "/api/v1/datasources", body).then(route).catch(function (e) {
        err.appendChild(el("span", {}, [String(e.message || e)]));
      });
    });
    return el("div", { class: "panel editor" }, [
      el("div", { class: "panel-title" }, ["Add a datasource"]),
      el("div", { class: "field-row" }, [field("Name", name), field("URL", url)]),
      el("div", { class: "field-row" }, [field("Auth", authSel), field("Credential", cred), field("Basic user", user), field("Timeout (ms)", timeout)]),
      el("div", { class: "form-actions" }, [add, err]),
    ]);
  }

  /* ---------- boot ---------- */
  initTheme();
  window.addEventListener("hashchange", route);
  if (!location.hash) location.hash = "#/graph";
  route();
})();
