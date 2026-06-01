# ADR-0020 — Live alert stream over Server-Sent Events

- Status: Accepted
- Date: 2026-06-01
- Phase: D-3

## Context

The Phase C-4 operator UI shows alerts via polling — the user
hits "Refresh" or waits for the 30-second poll. That's adequate
for browsing but useless when chasing an incident, where
operators need sub-second feedback on whether a fix worked.

We need a push channel from `alert-manager` to the UI without
adding a new infra dependency (no Redis pub/sub, no NATS as a
hard requirement).

## Decision

Add Server-Sent Events at `GET /v1/alerts/stream`.

Server side:

- A `sseHub` in `services/alert-manager/cmd/sse.go` keeps a
  per-tenant map of subscriber channels with bounded buffer
  (default 64). Publishing is non-blocking; slow subscribers
  drop events for themselves only.
- The existing `eventsHandler` and `observationsHandler` call
  `hub.Publish(...)` on every firing/resolution edge.
- Heartbeat every 25 s keeps NGINX (60 s) / CloudFront (30 s)
  read timeouts from killing the connection.

Client side:

- Browser uses `fetch` + reader (not `EventSource`) so we can
  attach the OIDC bearer token via `Authorization` header.
- Tab in the operator UI prepends events to a scrolling log.

## Trade-offs

- **SSE over WebSocket** — SSE is half-duplex (server → client)
  which is all we need; it survives every HTTP proxy in the
  stack without configuration; and `text/event-stream` is the
  one persistent-stream content type CDN edges treat correctly
  out of the box.
- **Drop-on-slow rather than back-pressure** — the alternative
  is blocking the publisher, which would punish every other
  subscriber for one slow consumer. SSE permits gaps; clients
  reconnect with `Last-Event-ID`.
- **No durable replay** — if the UI tab is closed and reopened,
  events that fired during the gap are not replayed. The user
  can hit "Refresh" to pull current state from the existing
  `GET /v1/alerts`. Adding durable replay would require
  promoting NATS JS to a hard requirement; out of scope for v1.

## Package changes

- `services/alert-manager/cmd/sse.go` (new) — hub, handler,
  `writeSSE` helper.
- `services/alert-manager/cmd/sse_test.go` (new) — fan-out, slow
  subscriber drop, end-to-end via `httptest` server.
- `services/alert-manager/cmd/main.go` — accept hub, publish
  on firing/resolved edges.
- `services/ui-server/cmd/assets/app.js` — `toggleStream` reader
  loop + UI affordance.

## Verification

- `go test -race ./services/alert-manager/cmd/...`
- Browser: connect with the operator UI, fire an event via
  `curl … /v1/events`, confirm the event renders in the live
  feed within ~50 ms.
