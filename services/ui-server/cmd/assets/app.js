// ObserveX operator console — vanilla JS, no framework, no build.
//
// Auth model:
//   - Production: OIDC PKCE in the browser against the configured
//     issuer. After redirect-back, the access_token lives in
//     sessionStorage and is attached to every /api/* fetch.
//   - Dev / break-glass: paste a bearer token. The ui-server in
//     no-OIDC mode treats any token as valid.
//
// Live alerts:
//   - SSE over /api/alert/v1/alerts/stream. Toggleable.
//
// Audit:
//   - Reads /api/tenant/v1/audit?tenant_id=...&limit=N

(() => {
  const $ = (sel) => document.querySelector(sel);
  const $$ = (sel) => Array.from(document.querySelectorAll(sel));

  const state = {
    config: null,
    token: sessionStorage.getItem("observex.token") || null,
    stream: null,
  };

  // ── Tab routing ───────────────────────────────────────────────────
  $$(".tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      $$(".tab").forEach((t) => t.classList.remove("active"));
      tab.classList.add("active");
      $$(".panel").forEach((p) => p.classList.remove("active"));
      $(`#panel-${tab.dataset.tab}`).classList.add("active");
    });
  });

  // ── PKCE OIDC flow (Phase D-5) ────────────────────────────────────
  //
  // Implements RFC 7636 PKCE. We avoid any external library — the
  // crypto primitives we need (random bytes, SHA-256, base64url) all
  // live in the Web Crypto API since 2017.
  const PKCE_KEY = "observex.pkce";

  function base64URL(bytes) {
    let s = "";
    bytes.forEach((b) => (s += String.fromCharCode(b)));
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }
  async function sha256(text) {
    const enc = new TextEncoder().encode(text);
    const digest = await crypto.subtle.digest("SHA-256", enc);
    return new Uint8Array(digest);
  }
  function randomString(bytes = 32) {
    const arr = new Uint8Array(bytes);
    crypto.getRandomValues(arr);
    return base64URL(arr);
  }

  async function pkceLogin() {
    if (!state.config?.oidc_issuer) {
      // Dev path — paste a token. Validation happens on the server.
      const tok = prompt(
        "OIDC not configured server-side. Paste a bearer token (dev mode)."
      );
      if (tok) {
        sessionStorage.setItem("observex.token", tok);
        state.token = tok;
        renderAuth();
        refreshAll();
      }
      return;
    }
    const verifier = randomString(48);
    const challenge = base64URL(await sha256(verifier));
    const stateNonce = randomString(16);
    sessionStorage.setItem(PKCE_KEY, JSON.stringify({
      verifier, state: stateNonce, returnTo: location.pathname + location.hash,
    }));
    // Discover authorization endpoint. We assume issuer + "/authorize"
    // works for the common IdPs (Auth0, Okta, Keycloak); we ALSO
    // accept an operator override via /config.
    const authzEndpoint = state.config.authorize_endpoint
      || (state.config.oidc_issuer.replace(/\/$/, "") + "/authorize");
    const redirectUri = location.origin + "/oidc/callback";
    const params = new URLSearchParams({
      response_type: "code",
      client_id: state.config.oidc_client_id || "observex",
      redirect_uri: redirectUri,
      scope: "openid email profile",
      state: stateNonce,
      code_challenge: challenge,
      code_challenge_method: "S256",
      audience: state.config.oidc_audience || "observex",
    });
    location.assign(`${authzEndpoint}?${params.toString()}`);
  }

  async function completePKCEIfPresent() {
    if (location.pathname !== "/oidc/callback") return false;
    const q = new URLSearchParams(location.search);
    const code = q.get("code");
    const stateParam = q.get("state");
    const raw = sessionStorage.getItem(PKCE_KEY);
    if (!code || !raw) return false;
    const { verifier, state: stateNonce, returnTo } = JSON.parse(raw);
    if (stateNonce !== stateParam) {
      alert("OIDC state mismatch — restart sign-in");
      return false;
    }
    // Delegate the token exchange to the ui-server so the
    // client_secret (when the IdP requires one) never touches the
    // browser. The server has the issuer's token endpoint config.
    try {
      const r = await fetch("/oidc/exchange", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ code, code_verifier: verifier,
          redirect_uri: location.origin + "/oidc/callback" }),
      });
      if (!r.ok) throw new Error("exchange HTTP " + r.status);
      const j = await r.json();
      sessionStorage.setItem("observex.token", j.access_token);
      state.token = j.access_token;
      sessionStorage.removeItem(PKCE_KEY);
      history.replaceState({}, "", returnTo || "/");
      return true;
    } catch (err) {
      alert("OIDC exchange failed: " + err.message);
      return false;
    }
  }

  // ── Auth surface ──────────────────────────────────────────────────
  $("#auth-btn").addEventListener("click", () => {
    if (state.token) {
      sessionStorage.removeItem("observex.token");
      state.token = null;
      renderAuth();
      return;
    }
    pkceLogin();
  });

  function renderAuth() {
    $("#who-text").textContent = state.token ? "authenticated" : "not authenticated";
    $("#auth-btn").textContent = state.token ? "Sign out" : "Authenticate";
  }

  // ── API plumbing ──────────────────────────────────────────────────
  async function api(path, init = {}) {
    const headers = new Headers(init.headers || {});
    if (state.token) headers.set("Authorization", `Bearer ${state.token}`);
    if (!headers.has("Content-Type") && init.body) headers.set("Content-Type", "application/json");
    const r = await fetch(path, { ...init, headers });
    if (r.status === 401) throw new Error("unauthorised — re-authenticate");
    if (r.status === 403) throw new Error("forbidden — your token lacks the required scope");
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const ct = r.headers.get("content-type") || "";
    return ct.includes("application/json") ? r.json() : r.text();
  }

  // ── Tenants panel ─────────────────────────────────────────────────
  $("#refresh-tenants").addEventListener("click", loadTenants);
  async function loadTenants() {
    const tbody = $("#tenants-table tbody");
    tbody.innerHTML = "";
    try {
      const data = await api("/api/tenant/v1/tenants");
      const rows = data.tenants || data || [];
      if (!rows.length) { $("#tenants-empty").style.display = "block"; return; }
      $("#tenants-empty").style.display = "none";
      for (const t of rows) {
        const tr = document.createElement("tr");
        tr.innerHTML = `
          <td><code>${escapeHTML(t.id || "")}</code></td>
          <td>${escapeHTML(t.display_name || "")}</td>
          <td>${escapeHTML(String(t.retention_days || ""))} d</td>
          <td>${escapeHTML(t.created_at || "")}</td>`;
        tbody.appendChild(tr);
      }
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="4" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }

  // ── Query panel ───────────────────────────────────────────────────
  $("#run-query").addEventListener("click", async () => {
    const tenant = $("#query-tenant").value.trim();
    const query = $("#query-text").value.trim();
    const out = $("#query-result");
    if (!query) { out.textContent = "(enter a query)"; return; }
    out.textContent = "running…";
    try {
      const r = await api("/api/query/v1/query", {
        method: "POST",
        body: JSON.stringify({ tenant_id: tenant, query }),
      });
      out.textContent = typeof r === "string" ? r : JSON.stringify(r, null, 2);
    } catch (err) {
      out.textContent = "ERROR: " + err.message;
    }
  });

  // ── Alerts panel ──────────────────────────────────────────────────
  $("#refresh-alerts").addEventListener("click", loadAlerts);
  $("#toggle-stream").addEventListener("click", toggleStream);
  async function loadAlerts() {
    const tbody = $("#alerts-table tbody");
    tbody.innerHTML = "";
    try {
      const data = await api("/api/alert/v1/alerts");
      const rows = data.alerts || data || [];
      if (!rows.length) { $("#alerts-empty").style.display = "block"; return; }
      $("#alerts-empty").style.display = "none";
      for (const a of rows) renderAlertRow(tbody, a);
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="5" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }
  function renderAlertRow(tbody, a) {
    const tr = document.createElement("tr");
    const s = (a.state || "unknown").toLowerCase();
    tr.innerHTML = `
      <td><span class="state-${s}">${escapeHTML(s)}</span></td>
      <td><code>${escapeHTML(a.tenant_id || "")}</code></td>
      <td>${escapeHTML(a.rule_name || a.rule_id || a.rule || "")}</td>
      <td>${escapeHTML(a.fired_at || a.started_at || a.first_fired || "")}</td>
      <td>${escapeHTML(a.last_notified_at || "—")}</td>`;
    tbody.appendChild(tr);
  }

  // Live SSE stream (Phase D-3). Native EventSource doesn't pass
  // custom headers — we can't attach the bearer that way — so we
  // implement our own reader on top of fetch + streaming response.
  async function toggleStream() {
    if (state.stream) {
      state.stream.abort();
      state.stream = null;
      $("#stream-status").textContent = "offline";
      $("#stream-status").classList.remove("live");
      $("#toggle-stream").textContent = "Live stream";
      return;
    }
    const out = $("#alert-stream");
    out.textContent = "(connecting…)\n";
    const ctrl = new AbortController();
    state.stream = ctrl;
    $("#stream-status").textContent = "live";
    $("#stream-status").classList.add("live");
    $("#toggle-stream").textContent = "Stop stream";
    try {
      const r = await fetch("/api/alert/v1/alerts/stream", {
        signal: ctrl.signal,
        headers: state.token ? { "Authorization": "Bearer " + state.token } : {},
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
          const evt = parseSSE(frame);
          if (!evt) continue;
          if (evt.event === "heartbeat") continue;
          out.textContent = `[${new Date().toISOString()}] ${evt.event}\n${JSON.stringify(evt.data, null, 2)}\n\n` + out.textContent;
        }
      }
    } catch (err) {
      if (err.name !== "AbortError") {
        out.textContent = "stream error: " + err.message + "\n" + out.textContent;
      }
    } finally {
      state.stream = null;
      $("#stream-status").textContent = "offline";
      $("#stream-status").classList.remove("live");
      $("#toggle-stream").textContent = "Live stream";
    }
  }
  function parseSSE(frame) {
    let event = "message", data = "";
    for (const line of frame.split("\n")) {
      if (line.startsWith(":")) continue;
      const colon = line.indexOf(":");
      if (colon < 0) continue;
      const k = line.slice(0, colon), v = line.slice(colon + 1).trimStart();
      if (k === "event") event = v;
      else if (k === "data") data += v;
    }
    if (!data) return null;
    let parsed;
    try { parsed = JSON.parse(data); } catch { parsed = data; }
    return { event, data: parsed };
  }

  // ── Audit panel (Phase D-4) ───────────────────────────────────────
  $("#refresh-audit").addEventListener("click", loadAudit);
  async function loadAudit() {
    const tbody = $("#audit-table tbody");
    tbody.innerHTML = "";
    const tenant = $("#audit-tenant").value.trim();
    try {
      const q = tenant ? `?tenant_id=${encodeURIComponent(tenant)}&limit=200` : "?limit=200";
      const data = await api("/api/tenant/v1/audit" + q);
      const rows = data.records || [];
      if (!rows.length) { $("#audit-empty").style.display = "block"; return; }
      $("#audit-empty").style.display = "none";
      for (const r of rows) {
        const tr = document.createElement("tr");
        tr.innerHTML = `
          <td><code>${escapeHTML(r.created_at || "")}</code></td>
          <td>${escapeHTML(r.actor || "")}</td>
          <td><code>${escapeHTML(r.action || "")}</code></td>
          <td>${escapeHTML(r.tenant_id || "—")}</td>
          <td><code class="audit-details">${escapeHTML(JSON.stringify(r.details || {}))}</code></td>`;
        tbody.appendChild(tr);
      }
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="5" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }

  // ─────────────────────────────────────────────────────────────────
  // Phase E-1 — Native Metrics workbench
  // ─────────────────────────────────────────────────────────────────
  //
  // Model:
  //   state.metrics.panels = [{ id, title, query, color, chart }]
  // Each panel runs a server-side ObserveQL query and renders the
  // returned (t, v) rows into an ObservexChart. Queries must select
  // at least two columns; the first numeric→time column is X, the
  // first numeric column is Y, the remainder become extra series via
  // a `series` column when present.
  state.metrics = {
    panels: [],
    nextId: 1,
    refreshTimer: null,
  };
  const PALETTE = ["#58a6ff", "#3fb950", "#f0883e", "#f85149",
                   "#d2a8ff", "#79c0ff", "#a5d6ff", "#ffa657"];

  $("#metrics-add").addEventListener("click", () => addMetricPanel());
  $("#metrics-range").addEventListener("change", refreshAllMetricPanels);
  $("#metrics-refresh").addEventListener("change", setupMetricsAutoRefresh);

  function addMetricPanel(seed) {
    const id = state.metrics.nextId++;
    const def = Object.assign(
      {
        title: "Untitled panel",
        query: 'select toStartOfMinute(timestamp) as t, avg(value) as v from metrics where metric_name = "rps" group by t order by t',
      },
      seed || {},
    );
    const wrap = document.createElement("div");
    wrap.className = "metric-panel";
    wrap.dataset.id = id;
    wrap.innerHTML = `
      <div class="panel-head">
        <input class="title" value="${escapeHTML(def.title)}" />
        <button data-act="remove" title="Remove">✕</button>
      </div>
      <textarea class="query" rows="2">${escapeHTML(def.query)}</textarea>
      <canvas></canvas>
      <div class="panel-err" hidden></div>
    `;
    $("#metrics-panels").appendChild(wrap);
    const canvas = wrap.querySelector("canvas");
    const chart = new ObservexChart(canvas, { fill: true });
    const panel = { id, title: def.title, query: def.query, chart, el: wrap };
    state.metrics.panels.push(panel);
    $("#metrics-empty").style.display = "none";
    wrap.querySelector(".title").addEventListener("change", (e) => { panel.title = e.target.value; });
    wrap.querySelector(".query").addEventListener("change", (e) => {
      panel.query = e.target.value;
      runMetricPanel(panel);
    });
    wrap.querySelector('[data-act="remove"]').addEventListener("click", () => removeMetricPanel(panel));
    runMetricPanel(panel);
    return panel;
  }

  function removeMetricPanel(panel) {
    panel.chart.destroy();
    panel.el.remove();
    state.metrics.panels = state.metrics.panels.filter((p) => p !== panel);
    if (!state.metrics.panels.length) $("#metrics-empty").style.display = "block";
  }

  async function runMetricPanel(panel) {
    const tenant = $("#metrics-tenant").value.trim();
    const rangeMs = parseInt($("#metrics-range").value, 10) || 3_600_000;
    const errEl = panel.el.querySelector(".panel-err");
    errEl.hidden = true;
    try {
      // NDJSON streaming endpoint we already shipped in Phase B-3.
      // First line is the header object; subsequent lines are rows.
      const headers = {
        "Content-Type": "application/json",
        "X-Tenant-ID": tenant,
      };
      if (state.token) headers["Authorization"] = "Bearer " + state.token;
      const r = await fetch("/api/query/v1/query", {
        method: "POST",
        headers,
        body: JSON.stringify({
          query: panel.query,
          // The planner clamps LIMIT for us; max_rows is advisory.
          max_rows: 5000,
          // Time range is enforced by the user's WHERE clause; we
          // pass these via headers so server-side log lines show
          // the operator-requested window.
          timeout_secs: 30,
        }),
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const rows = await readNDJSON(r);
      const series = ndjsonToSeries(rows, rangeMs);
      panel.chart.setSeries(series);
      panel.chart.render();
    } catch (err) {
      errEl.textContent = err.message;
      errEl.hidden = false;
      panel.chart.setSeries([]);
      panel.chart.render();
    }
  }

  async function readNDJSON(r) {
    const text = await r.text();
    const out = [];
    for (const line of text.split("\n")) {
      const l = line.trim();
      if (!l) continue;
      try {
        const obj = JSON.parse(l);
        // Skip the executor header frame, keep data rows.
        if (obj && obj._kind === "header") continue;
        out.push(obj);
      } catch {
        // ignore non-JSON lines (trailing trailer, comments, etc.)
      }
    }
    return out;
  }

  // Convert a {col→value} row set into chart series. The first
  // datetime-shaped column becomes X; the first non-X numeric column
  // becomes the primary Y; if a `series` (or `metric_name`,
  // `service_name`, …) string column is present, rows are bucketed.
  function ndjsonToSeries(rows, rangeMs) {
    if (!rows.length) return [];
    const sample = rows[0];
    const cols = Object.keys(sample);
    const tCol = cols.find((c) => looksLikeTime(sample[c]));
    if (!tCol) return [];
    const yCols = cols.filter((c) => c !== tCol && typeof sample[c] === "number");
    if (!yCols.length) return [];
    const labelCol = cols.find((c) =>
      c !== tCol && !yCols.includes(c) && typeof sample[c] === "string",
    );

    if (!labelCol && yCols.length === 1) {
      const series = [{
        name: yCols[0],
        color: PALETTE[0],
        points: rows.map((r) => ({ t: toMs(r[tCol]), v: r[yCols[0]] })),
      }];
      return series;
    }
    if (!labelCol && yCols.length > 1) {
      return yCols.map((c, i) => ({
        name: c,
        color: PALETTE[i % PALETTE.length],
        points: rows.map((r) => ({ t: toMs(r[tCol]), v: r[c] })),
      }));
    }
    // labelCol present: bucket rows by label, plot the first y.
    const yCol = yCols[0];
    const byLabel = new Map();
    for (const r of rows) {
      const lab = String(r[labelCol] ?? "");
      if (!byLabel.has(lab)) byLabel.set(lab, []);
      byLabel.get(lab).push({ t: toMs(r[tCol]), v: r[yCol] });
    }
    let i = 0;
    return Array.from(byLabel.entries()).map(([lab, pts]) => ({
      name: lab,
      color: PALETTE[i++ % PALETTE.length],
      points: pts.sort((a, b) => a.t - b.t),
    }));
  }
  function looksLikeTime(v) {
    if (v instanceof Date) return true;
    if (typeof v === "number") return v > 1e11; // unix ms-ish
    if (typeof v === "string") return !isNaN(Date.parse(v));
    return false;
  }
  function toMs(v) {
    if (v instanceof Date) return +v;
    if (typeof v === "number") return v < 1e11 ? v * 1000 : v;
    if (typeof v === "string") return Date.parse(v);
    return 0;
  }

  function refreshAllMetricPanels() {
    for (const p of state.metrics.panels) runMetricPanel(p);
  }
  function setupMetricsAutoRefresh() {
    if (state.metrics.refreshTimer) {
      clearInterval(state.metrics.refreshTimer);
      state.metrics.refreshTimer = null;
    }
    const ms = parseInt($("#metrics-refresh").value, 10) || 0;
    if (ms > 0) {
      state.metrics.refreshTimer = setInterval(refreshAllMetricPanels, ms);
    }
  }

  // ─────────────────────────────────────────────────────────────────
  // Phase E-2 — Logs explorer (search + live tail)
  // ─────────────────────────────────────────────────────────────────
  state.logs = { tail: null, rows: [] };

  $("#logs-run").addEventListener("click", runLogsSearch);
  $("#logs-tail").addEventListener("click", toggleLogsTail);

  async function runLogsSearch() {
    if (state.logs.tail) toggleLogsTail(); // stop tail first
    const tbody = $("#logs-table tbody");
    tbody.innerHTML = "";
    const tenant = $("#logs-tenant").value.trim();
    const service = $("#logs-service").value.trim();
    const sev = $("#logs-severity").value;
    const search = $("#logs-search").value.trim();
    const where = ["1=1"];
    if (service) where.push(`service_name = '${sqlEscape(service)}'`);
    if (sev) where.push(`severity = '${sev}'`);
    if (search) where.push(`positionCaseInsensitive(body, '${sqlEscape(search)}') > 0`);
    const query = `select timestamp, severity, service_name, body, trace_id, span_id, attributes
      from logs where ${where.join(" and ")} and timestamp >= now() - INTERVAL 1 HOUR
      order by timestamp desc limit 500`;
    try {
      const headers = { "Content-Type": "application/json", "X-Tenant-ID": tenant };
      if (state.token) headers["Authorization"] = "Bearer " + state.token;
      const r = await fetch("/api/query/v1/query", {
        method: "POST", headers, body: JSON.stringify({ query }),
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const rows = await readNDJSON(r);
      renderLogRows(rows);
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="4" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }

  function renderLogRows(rows) {
    const tbody = $("#logs-table tbody");
    tbody.innerHTML = "";
    if (!rows.length) {
      $("#logs-empty").style.display = "block";
      return;
    }
    $("#logs-empty").style.display = "none";
    state.logs.rows = rows;
    // Virtualization: render in 200-row chunks via requestAnimationFrame
    // so a 500-row search doesn't block the UI thread.
    let i = 0;
    const chunk = 200;
    function pump() {
      const slice = rows.slice(i, i + chunk);
      const frag = document.createDocumentFragment();
      for (const row of slice) frag.appendChild(makeLogRow(row));
      tbody.appendChild(frag);
      i += chunk;
      if (i < rows.length) requestAnimationFrame(pump);
    }
    pump();
  }

  function makeLogRow(row) {
    const tr = document.createElement("tr");
    tr.className = "row-clickable";
    const sev = String(row.severity || "").toUpperCase();
    tr.innerHTML = `
      <td class="time">${escapeHTML(formatTimestamp(row.timestamp))}</td>
      <td class="sev ${sev}">${escapeHTML(sev)}</td>
      <td>${escapeHTML(row.service_name || "")}</td>
      <td class="body">${escapeHTML(row.body || "")}</td>`;
    tr.addEventListener("click", () => {
      const expanded = tr.classList.toggle("expand");
      const bodyTd = tr.querySelector(".body");
      if (expanded) {
        const detail = {
          trace_id: row.trace_id || "",
          span_id: row.span_id || "",
          attributes: row.attributes || {},
        };
        bodyTd.innerHTML = `${escapeHTML(row.body || "")}<pre>${escapeHTML(JSON.stringify(detail, null, 2))}</pre>`;
      } else {
        bodyTd.textContent = row.body || "";
      }
    });
    return tr;
  }

  async function toggleLogsTail() {
    if (state.logs.tail) {
      state.logs.tail.abort();
      state.logs.tail = null;
      $("#logs-tail-status").textContent = "offline";
      $("#logs-tail-status").classList.remove("live");
      $("#logs-tail").textContent = "Live tail";
      return;
    }
    const tenant = $("#logs-tenant").value.trim();
    const service = $("#logs-service").value.trim();
    const sev = $("#logs-severity").value;
    const q = new URLSearchParams({ tenant_id: tenant });
    if (service) q.set("service", service);
    if (sev) q.set("severity", sev);
    const ctrl = new AbortController();
    state.logs.tail = ctrl;
    $("#logs-tail-status").textContent = "live";
    $("#logs-tail-status").classList.add("live");
    $("#logs-tail").textContent = "Stop tail";
    const tbody = $("#logs-table tbody");
    try {
      const r = await fetch("/api/query/v1/logs/stream?" + q.toString(), {
        signal: ctrl.signal,
        headers: state.token ? { "Authorization": "Bearer " + state.token } : {},
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let idx;
        while ((idx = buf.indexOf("\n\n")) >= 0) {
          const frame = buf.slice(0, idx); buf = buf.slice(idx + 2);
          const evt = parseSSE(frame);
          if (!evt || evt.event === "heartbeat") continue;
          // Prepend so newest is at the top, cap to 500.
          const tr = makeLogRow(evt.data);
          tbody.insertBefore(tr, tbody.firstChild);
          while (tbody.children.length > 500) tbody.removeChild(tbody.lastChild);
          $("#logs-empty").style.display = "none";
        }
      }
    } catch (err) {
      if (err.name !== "AbortError") {
        tbody.insertAdjacentHTML("afterbegin",
          `<tr><td colspan="4" style="color:var(--err)">tail error: ${escapeHTML(err.message)}</td></tr>`);
      }
    } finally {
      state.logs.tail = null;
      $("#logs-tail-status").textContent = "offline";
      $("#logs-tail-status").classList.remove("live");
      $("#logs-tail").textContent = "Live tail";
    }
  }

  function sqlEscape(s) { return String(s).replace(/'/g, "''"); }
  function formatTimestamp(s) {
    if (!s) return "";
    try { return new Date(s).toISOString().replace("T", " ").replace("Z", ""); }
    catch { return String(s); }
  }

  // ─────────────────────────────────────────────────────────────────
  // Phase E-3 — Traces
  // ─────────────────────────────────────────────────────────────────
  state.traces = { selected: null, spans: [] };

  $("#traces-search").addEventListener("click", runTracesSearch);

  async function runTracesSearch() {
    const tbody = $("#traces-table tbody");
    tbody.innerHTML = "";
    const tenant = $("#traces-tenant").value.trim();
    const service = $("#traces-service").value.trim();
    const minMs = parseInt($("#traces-min-ms").value, 10) || 0;
    const rangeMs = parseInt($("#traces-range").value, 10) || 3_600_000;
    const since = `now() - INTERVAL ${Math.ceil(rangeMs / 1000)} SECOND`;
    const where = [`start_time >= ${since}`];
    if (service) where.push(`service_name = '${sqlEscape(service)}'`);
    if (minMs > 0) where.push(`duration_ns >= ${minMs * 1_000_000}`);
    const query = `select trace_id, service_name, operation_name, start_time, duration_ns, status_code
      from traces where ${where.join(" and ")}
      order by start_time desc limit 200`;
    try {
      const headers = { "Content-Type": "application/json", "X-Tenant-ID": tenant };
      if (state.token) headers["Authorization"] = "Bearer " + state.token;
      const r = await fetch("/api/query/v1/query", {
        method: "POST", headers, body: JSON.stringify({ query }),
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const rows = await readNDJSON(r);
      if (!rows.length) { $("#traces-empty").style.display = "block"; return; }
      $("#traces-empty").style.display = "none";
      for (const t of rows) {
        const tr = document.createElement("tr");
        tr.className = "row-clickable";
        const sc = String(t.status_code || "OK");
        tr.innerHTML = `
          <td class="time">${escapeHTML(formatTimestamp(t.start_time))}</td>
          <td>${escapeHTML(t.service_name || "")}</td>
          <td>${escapeHTML(t.operation_name || "")}</td>
          <td>${(+(t.duration_ns || 0) / 1e6).toFixed(2)} ms</td>
          <td><span class="state-${sc === "OK" ? "resolved" : "firing"}">${escapeHTML(sc)}</span></td>`;
        tr.addEventListener("click", () => {
          $$("#traces-table tr").forEach((row) => row.classList.remove("selected"));
          tr.classList.add("selected");
          loadTraceDetail(tenant, t.trace_id);
        });
        tbody.appendChild(tr);
      }
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="5" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }

  async function loadTraceDetail(tenant, traceID) {
    state.traces.selected = traceID;
    $("#trace-detail-title").textContent = "Waterfall · " + traceID;
    const query = `select trace_id, span_id, parent_span_id, service_name, operation_name,
      start_time, end_time, duration_ns, status_code, attributes
      from traces where trace_id = '${sqlEscape(traceID)}'
      order by start_time asc`;
    try {
      const headers = { "Content-Type": "application/json", "X-Tenant-ID": tenant };
      if (state.token) headers["Authorization"] = "Bearer " + state.token;
      const r = await fetch("/api/query/v1/query", {
        method: "POST", headers, body: JSON.stringify({ query }),
      });
      if (!r.ok) throw new Error("HTTP " + r.status);
      const spans = await readNDJSON(r);
      state.traces.spans = spans;
      renderWaterfall(document.getElementById("trace-waterfall"), spans);
      renderServiceMap(document.getElementById("trace-servicemap"), spans);
    } catch (err) {
      document.getElementById("trace-waterfall").innerHTML =
        `<div style="color:var(--err)">${escapeHTML(err.message)}</div>`;
    }
  }

  // ─────────────────────────────────────────────────────────────────
  // Phase E-4 — Dashboards (save / load / share)
  // ─────────────────────────────────────────────────────────────────
  state.dash = { current: null };

  $("#dash-new").addEventListener("click", () => {
    const name = prompt("Dashboard name?");
    if (!name) return;
    state.dash.current = { id: null, name, panels: [] };
    saveCurrentDashboard();
  });
  $("#dash-refresh").addEventListener("click", loadDashboards);
  $("#dash-export").addEventListener("click", exportCurrentDashboard);
  $("#dash-import").addEventListener("click", importDashboardFromPrompt);
  $("#dash-share").addEventListener("click", shareCurrentDashboard);

  async function loadDashboards() {
    const tbody = $("#dashboards-table tbody");
    tbody.innerHTML = "";
    const tenant = $("#dash-tenant").value.trim();
    if (!tenant) { $("#dashboards-empty").style.display = "block"; return; }
    try {
      const data = await api(`/api/tenant/v1/dashboards?tenant_id=${encodeURIComponent(tenant)}`);
      const rows = data.dashboards || [];
      if (!rows.length) { $("#dashboards-empty").style.display = "block"; return; }
      $("#dashboards-empty").style.display = "none";
      for (const d of rows) {
        const tr = document.createElement("tr");
        const panelCount = (d.layout && d.layout.panels || []).length;
        tr.innerHTML = `
          <td><a href="#dash=${d.id}">${escapeHTML(d.name)}</a></td>
          <td>${panelCount}</td>
          <td>${escapeHTML(d.updated_at || "")}</td>
          <td class="dash-actions">
            <button data-act="open">Open</button>
            <button data-act="delete">Delete</button>
          </td>`;
        tr.querySelector('[data-act="open"]').addEventListener("click", () => openDashboard(d));
        tr.querySelector('[data-act="delete"]').addEventListener("click", () => deleteDashboard(d));
        tbody.appendChild(tr);
      }
    } catch (err) {
      tbody.innerHTML = `<tr><td colspan="4" style="color:var(--err)">${escapeHTML(err.message)}</td></tr>`;
    }
  }

  function openDashboard(d) {
    state.dash.current = d;
    // Switch to metrics tab and load panels.
    $('.tab[data-tab="metrics"]').click();
    while (state.metrics.panels.length) removeMetricPanel(state.metrics.panels[0]);
    for (const p of (d.layout && d.layout.panels) || []) addMetricPanel(p);
  }

  async function deleteDashboard(d) {
    if (!confirm("Delete dashboard " + d.name + "?")) return;
    try {
      await api(`/api/tenant/v1/dashboards/${encodeURIComponent(d.id)}`, { method: "DELETE" });
      loadDashboards();
    } catch (err) { alert("delete failed: " + err.message); }
  }

  async function saveCurrentDashboard() {
    if (!state.dash.current) return;
    const tenant = $("#dash-tenant").value.trim() || $("#metrics-tenant").value.trim();
    if (!tenant) { alert("Tenant required to save"); return; }
    const payload = {
      tenant_id: tenant,
      name: state.dash.current.name,
      layout: {
        panels: state.metrics.panels.map((p) => ({
          title: p.title, query: p.query,
        })),
      },
    };
    try {
      let r;
      if (state.dash.current.id) {
        r = await api(`/api/tenant/v1/dashboards/${encodeURIComponent(state.dash.current.id)}`,
                      { method: "PUT", body: JSON.stringify(payload) });
      } else {
        r = await api("/api/tenant/v1/dashboards",
                      { method: "POST", body: JSON.stringify(payload) });
        state.dash.current.id = r.id;
      }
      loadDashboards();
    } catch (err) { alert("save failed: " + err.message); }
  }

  function exportCurrentDashboard() {
    const payload = {
      name: state.dash.current?.name || "untitled",
      layout: {
        panels: state.metrics.panels.map((p) => ({ title: p.title, query: p.query })),
      },
    };
    const blob = new Blob([JSON.stringify(payload, null, 2)], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url; a.download = (payload.name || "dashboard") + ".json";
    document.body.appendChild(a); a.click(); a.remove();
    URL.revokeObjectURL(url);
  }

  function importDashboardFromPrompt() {
    const json = prompt("Paste dashboard JSON:");
    if (!json) return;
    try {
      const d = JSON.parse(json);
      state.dash.current = { id: null, name: d.name || "imported", panels: d.layout?.panels || [] };
      while (state.metrics.panels.length) removeMetricPanel(state.metrics.panels[0]);
      for (const p of d.layout?.panels || []) addMetricPanel(p);
      $('.tab[data-tab="metrics"]').click();
    } catch (err) { alert("import failed: " + err.message); }
  }

  function shareCurrentDashboard() {
    const id = state.dash.current?.id;
    if (!id) { alert("Save the dashboard first to get a shareable link."); return; }
    const url = location.origin + "/#dash=" + encodeURIComponent(id);
    navigator.clipboard?.writeText(url);
    alert("Share link copied:\n" + url);
  }

  function tryOpenFromHash() {
    const m = location.hash.match(/dash=([^&]+)/);
    if (!m) return;
    const id = decodeURIComponent(m[1]);
    api(`/api/tenant/v1/dashboards/${encodeURIComponent(id)}`)
      .then(openDashboard)
      .catch((err) => console.warn("hash open failed", err));
  }

  // ── Boot ──────────────────────────────────────────────────────────
  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    }[c]));
  }
  function refreshAll() { loadTenants(); loadAlerts(); loadAudit(); loadDashboards(); }

  (async () => {
    try {
      const r = await fetch("/config");
      state.config = await r.json();
    } catch { state.config = {}; }
    if (await completePKCEIfPresent()) {
      renderAuth();
      refreshAll();
      setupMetricsAutoRefresh();
      tryOpenFromHash();
      return;
    }
    renderAuth();
    if (state.token) {
      refreshAll();
      tryOpenFromHash();
    }
    setupMetricsAutoRefresh();
  })();
})();
