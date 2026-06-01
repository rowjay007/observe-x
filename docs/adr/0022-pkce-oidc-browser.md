# ADR-0022 — Browser PKCE OIDC flow

- Status: Accepted
- Date: 2026-06-01
- Phase: D-5

## Context

ADR-0014 wired `tenant-api` to validate OIDC bearer tokens. ADR-0017
shipped the operator UI, but the UI's auth path was a token-paste:
operators acquired a JWT outside the product (e.g. via the IdP's
CLI) and pasted it into a prompt. That's:

- A footgun (tokens get into clipboard history, screen-share, etc.);
- A bad first-run experience (the prompt is opaque about *which*
  IdP, *which* audience);
- Not interoperable with operators who don't have IdP CLI access.

We need a real browser sign-in flow. Standard answer: OAuth2
Authorization Code with PKCE (RFC 7636).

## Decision

Implement PKCE end-to-end:

- **Browser** generates a 48-byte random `code_verifier`, derives
  `S256(code_verifier)` for `code_challenge`, stores `verifier`
  + state nonce in `sessionStorage`, redirects to the IdP's
  authorize endpoint.
- **IdP** authenticates the user and redirects back to
  `/oidc/callback?code=…&state=…` on `ui-server`.
- **`ui-server`** serves the SPA on `/oidc/callback`; the SPA
  reads `code` from the URL, POSTs `{code, code_verifier, redirect_uri}`
  to `/oidc/exchange`.
- **`ui-server`** exchanges with the IdP's token endpoint (using
  the optional client secret server-side), returns the response
  JSON to the SPA. The SPA stores the access token in
  `sessionStorage` and uses it for every API call.

`/config` is extended to surface `oidc_client_id`, `authorize_endpoint`,
and `token_endpoint`. The latter two are discovered from
`/.well-known/openid-configuration` on first read and cached.

## Trade-offs

- **PKCE over implicit flow** — implicit flow is deprecated
  (no refresh tokens, no proof of possession). PKCE is the OAuth
  Working Group's current SPA recommendation.
- **Token exchange goes through `ui-server`, not the browser** —
  many IdPs require a client secret on the token endpoint; we
  don't want it in the bundle. Server-side proxy is the standard
  pattern for confidential SPA clients.
- **Token in `sessionStorage`, not a cookie** — the proxy passes
  `Authorization: Bearer` upstream, which matches `tenant-api`'s
  OIDC middleware. `sessionStorage` is per-tab and cleared on
  close, narrowing the XSS impact window vs. `localStorage`.
- **No refresh token loop** — Phase D-5 takes whatever lifetime
  the IdP issues (typically 1 hour). When it expires the user
  signs in again. Silent renew via hidden iframe is a future
  add; the operator UI is interactive enough that an explicit
  re-auth is acceptable.
- **Break-glass paste flow preserved** — when OIDC is unconfigured
  server-side, the UI falls back to the old token prompt so dev
  / single-operator deployments don't grow a hard dependency.

## Package changes

- `services/ui-server/cmd/main.go` — `/oidc/callback`,
  `/oidc/exchange`, discovery, expanded `/config`.
- `services/ui-server/cmd/assets/app.js` — `pkceLogin`,
  `completePKCEIfPresent`, helpers.

## Configuration

- `OBSERVE_X_OIDC_CLIENT_ID=observex` — required.
- `OBSERVE_X_OIDC_CLIENT_SECRET=…` — optional; only set when the
  IdP requires it on the token endpoint.
- `OBSERVE_X_OIDC_AUTHORIZE_URL=…` — optional override.
- `OBSERVE_X_OIDC_TOKEN_URL=…` — optional override.

## Verification

- Test against Auth0, Okta, Keycloak (the three IdPs we test
  ADR-0014 against). All three discover endpoints correctly.
- CI continues to exercise the no-OIDC path (paste prompt) so we
  don't regress dev usability.
