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

  // ── Boot ──────────────────────────────────────────────────────────
  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
    }[c]));
  }
  function refreshAll() { loadTenants(); loadAlerts(); loadAudit(); }

  (async () => {
    try {
      const r = await fetch("/config");
      state.config = await r.json();
    } catch { state.config = {}; }
    if (await completePKCEIfPresent()) {
      renderAuth();
      refreshAll();
      return;
    }
    renderAuth();
    if (state.token) refreshAll();
  })();
})();
