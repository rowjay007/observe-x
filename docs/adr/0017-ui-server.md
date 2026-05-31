# ADR-0017 — Operator UI (`ui-server`)

- Status: Accepted
- Date: 2026-05-31
- Phase: C-4

## Context

ObserveX has shipped four production services and a tenant control
plane, but every operator interaction has gone through:

- `curl` against `tenant-api` for tenant + key management;
- `psql` against the alert-manager Postgres for silence inspection;
- `clickhouse-client` for ad-hoc queries;
- The Prometheus endpoint of each service for health.

That's hostile to anyone but the SRE on call. The product needs an
operator console — a single web UI that:

1. Lists tenants and the API keys issued under each;
2. Runs ad-hoc ObserveQL queries against `query-engine` and renders
   the result;
3. Surfaces firing / pending / resolved alerts in real time;
4. Inherits the same OIDC auth used elsewhere (no new credential
   surface);
5. Ships as part of `go build ./...` — no separate npm pipeline.

## Decision

Build `services/ui-server` as a Go HTTP server that:

- Embeds a **vanilla-JS SPA** at `cmd/assets/{index.html,app.css,app.js}`
  via `go:embed`. The whole UI is ~300 LOC of hand-rolled HTML + CSS
  + JS. No React, no Vue, no Tailwind, no build step.
- Serves `/` with the SPA, falling back to `index.html` for any
  client-side route (deep links work).
- Reverse-proxies three prefixes to the upstream services:
  - `/api/tenant/*` → `tenant-api`
  - `/api/query/*` → `query-engine`
  - `/api/alert/*` → `alert-manager`
- Validates the operator's OIDC bearer token at the proxy boundary
  (reusing `pkg/oidc` from ADR-0014) and passes it through to the
  upstream — every upstream re-validates so the audit trail names
  the real principal at every hop.
- Sets the obvious security headers on the SPA: `X-Frame-Options:
  DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  strict-origin-when-cross-origin`, and a tight `Content-Security-
  Policy` that forbids external scripts.
- Exposes `/healthz`, `/readyz`, `/metrics`, and `/config` (an
  unauthenticated endpoint the SPA reads at boot to learn the OIDC
  issuer URL).

## Trade-offs

- **Vanilla JS, no framework.** The single biggest argument from
  the React/Vue/Svelte camp is "but you'll want components." We
  don't, at this stage. The operator console has three tabs and
  five interactions. A 200-line vanilla-JS SPA is faster to load
  (zero KB framework), faster to debug (no virtual DOM), and won't
  bit-rot every six months as the framework's churn ploughs through
  it. If the UI grows to 50 interactive surfaces in Phase D, we
  rewrite at that boundary; until then, the right choice is "as
  little JS as humanly possible."

- **No npm in the build.** This means the UI repo lives in the
  same Go module as the server. The cost is no Storybook, no
  Playwright, no fancy tooling. The benefit is enormous:
  `go build` produces a single static binary the operator can run.
  No node_modules. No dependabot churn over 1200 transitive deps.
  No "the UI works on my machine."

- **Reverse proxy, not a thin BFF.** We chose a transparent proxy
  over a Backend-For-Frontend pattern because the upstream APIs
  are already well-shaped for direct consumption (REST + JSON). A
  BFF would let us reshape responses for UI convenience, but the
  cost is a translation layer that must move in lockstep with two
  APIs. Worth it when the UI's needs diverge from the API's; not
  yet.

- **In-browser OIDC redirect flow deferred.** The first cut prompts
  the operator to paste a token (they typically have one from
  `kubectl-oidc-login` or similar). A full browser PKCE flow is
  straightforward to add and lives in Phase D when we have a
  customer asking for it. Pasting a token covers the immediate
  use-case and avoids us having to register an OAuth2 client for
  every IdP variant.

- **Token in sessionStorage, not localStorage.** Tabs are isolated;
  the token vanishes on tab close; XSS is harder to weaponise. The
  CSP further disallows `eval` and external scripts so the XSS
  vector is already narrow.

- **No SSR / no progressive enhancement.** The SPA needs JS. The
  operator console is for engineers; an engineer with JS disabled
  is well outside the target user. We're explicit about that here
  rather than pretending otherwise.

## Package changes

- `services/ui-server/cmd/main.go` (new): HTTP server with embed.FS
  + reverse proxy + OIDC validator + security headers.
- `services/ui-server/cmd/assets/index.html` (new): three-tab SPA.
- `services/ui-server/cmd/assets/app.css` (new): dark-mode styling.
- `services/ui-server/cmd/assets/app.js` (new): API plumbing,
  fetch+render, sessionStorage token handling.
- `services/ui-server/cmd/main_test.go` (new): embed.FS smoke test
  (verifies `/`, `/index.html`, `/app.js`, `/app.css`, SPA fallback);
  security-header assertion; path-traversal sanity; reverse-proxy
  prefix stripping.

No new top-level Go dependencies. The UI is self-contained.

## Environment

```
OBSERVE_X_UI_ADDR              defaults :8080
OBSERVE_X_TENANT_API_URL       http://tenant-api:8081
OBSERVE_X_QUERY_ENGINE_URL     http://query-engine:8082
OBSERVE_X_ALERT_MANAGER_URL    http://alert-manager:8083
OBSERVE_X_OIDC_ISSUER          inherits from ADR-0014; unset ⇒ dev mode
OBSERVE_X_OIDC_AUDIENCE
OBSERVE_X_OIDC_ADMIN_GROUPS
```

## Verification

- `go test -race ./services/ui-server/...` — embed.FS load,
  security headers, path traversal sanity, proxy prefix stripping.
- `go build ./services/ui-server/...` — single binary, no extra
  artefacts required at runtime.
- Manual smoke (run locally): `go run ./services/ui-server/...`
  then visit http://localhost:8080. Tabs render, auth button
  toggles, /api/* proxies report "upstream unavailable" until
  Compose / Helm wire the upstreams.

## Follow-ups (Phase D candidates)

- **In-browser PKCE flow** against the configured OIDC issuer to
  remove the "paste a token" UX.
- **Live alert stream over SSE** instead of poll-on-click.
- **Per-tenant retention editor** in the Tenants panel (today
  retention is column-only).
- **ObserveQL syntax highlighter / autocomplete** — useful but
  meaningful work; deferred until the query language stabilises in
  Phase D.
