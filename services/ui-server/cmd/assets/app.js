// ObserveX operator console — vanilla JS, no framework, no build.
//
// Auth model: the operator authenticates against the configured
// OIDC issuer using the in-browser OAuth2 implicit flow (or pastes
// a token they already have). The token is stored in
// sessionStorage and attached to every /api/* fetch. The ui-server
// validates the same token on the server side before proxying.

(() => {
  const $ = (sel) => document.querySelector(sel);
  const $$ = (sel) => Array.from(document.querySelectorAll(sel));

  const state = {
    config: null,
    token: sessionStorage.getItem("observex.token") || null,
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

  // ── Auth surface ──────────────────────────────────────────────────
  $("#auth-btn").addEventListener("click", async () => {
    if (state.token) {
      // Already authed; offer logout.
      sessionStorage.removeItem("observex.token");
      state.token = null;
      renderAuth();
      return;
    }
    // Minimal flow: prompt for a bearer token. Real OIDC redirect
    // flow lives in a follow-up; for ops this is enough since
    // operators usually have a token from `kubectl-oidc-login` or
    // similar. The pasted token is validated by the ui-server on
    // the next API call — if it's bad, the call returns 401.
    const tok = prompt(
      `Paste a bearer token (issuer: ${state.config?.oidc_issuer || "(not configured)"} ).\n` +
      `If unset, the ui-server is in dev mode and any token works.`
    );
    if (tok) {
      sessionStorage.setItem("observex.token", tok);
      state.token = tok;
      renderAuth();
      refreshAll();
    }
  });

  function renderAuth() {
    $("#who-text").textContent = state.token
      ? "authenticated"
      : "not authenticated";
    $("#auth-btn").textContent = state.token ? "Sign out" : "Authenticate";
  }

  // ── API plumbing ──────────────────────────────────────────────────
  async function api(path, init = {}) {
    const headers = new Headers(init.headers || {});
    if (state.token) headers.set("Authorization", `Bearer ${state.token}`);
    headers.set("Content-Type", "application/json");
    const r = await fetch(path, { ...init, headers });
    if (r.status === 401) {
      throw new Error("unauthorised — re-authenticate");
    }
    if (r.status === 403) {
      throw new Error("forbidden — your token lacks the required scope");
    }
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const ct = r.headers.get("content-type") || "";
    if (ct.includes("application/json")) return r.json();
    return r.text();
  }

  // ── Tenants panel ─────────────────────────────────────────────────
  $("#refresh-tenants").addEventListener("click", loadTenants);
  async function loadTenants() {
    const tbody = $("#tenants-table tbody");
    tbody.innerHTML = "";
    try {
      const data = await api("/api/tenant/v1/tenants");
      const rows = data.tenants || data || [];
      if (!rows.length) {
        $("#tenants-empty").style.display = "block";
        return;
      }
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
  async function loadAlerts() {
    const tbody = $("#alerts-table tbody");
    tbody.innerHTML = "";
    try {
      const data = await api("/api/alert/v1/alerts");
      const rows = data.alerts || data || [];
      if (!rows.length) {
        $("#alerts-empty").style.display = "block";
        return;
      }
      $("#alerts-empty").style.display = "none";
      for (const a of rows) {
        const tr = document.createElement("tr");
        const state = (a.state || "unknown").toLowerCase();
        tr.innerHTML = `
          <td><span class="state-${state}">${escapeHTML(state)}</span></td>
          <td><code>${escapeHTML(a.tenant_id || "")}</code></td>
          <td>${escapeHTML(a.rule_name || a.rule || "")}</td>
          <td>${escapeHTML(a.fired_at || a.first_fired || "")}</td>
          <td>${escapeHTML(a.last_notified_at || "—")}</td>`;
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

  function refreshAll() {
    loadTenants();
    loadAlerts();
  }

  fetch("/config").then((r) => r.json()).then((c) => {
    state.config = c;
    renderAuth();
    if (state.token) refreshAll();
  }).catch(() => {
    renderAuth();
  });
})();
