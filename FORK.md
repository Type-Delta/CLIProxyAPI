# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits that are not supplied by the shared base.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference between `HEAD` and the latest shared base. The sole fork commit is `2037ab99973e15f50685ba84d75785659c55a83b`; its parent, `a14dfc779f43aed588e68b31fb34ab5ced700851`, is the current shared base. This checkout has no `upstream` remote or `base` tag, so update the recorded base explicitly when upstream is integrated.

Keep stable IDs when updating this section; gaps are intentional. When upstream absorbs a difference, remove or rewrite the entry rather than preserving chronology here. Update its behavior, implementation evidence, and validation when the surviving difference changes.

### DL001 - Per-API-key request and token usage limits

Inbound client API keys may be configured as structured entries with optional request and token caps while bare-string keys remain compatible and unlimited. A key can use a lifetime window or a UTC hourly, daily, weekly, or monthly reset cadence; request and token limits share that window.

The proxy enforces limits across the OpenAI-, Anthropic-, and Gemini-compatible API surfaces, returning protocol-appropriate HTTP `429` bodies and rate-limit headers. Model discovery, token counting, and asynchronous video status or content routes do not consume quota. Requests count when admitted, while tokens are recorded from normalized response usage after completion.

Usage counters persist under the authentication directory at `state/usage-limits.json`. Persisted entries identify keys only by SHA-256 hash, survive restarts, and remain local to each proxy instance. Configuration hot reload applies limit changes immediately, preserves counters unless the reset cadence changes, and removes counters for deleted or unlimited keys.

The management API exposes current limited-key consumption through `GET /v0/management/api-key-limits` and resets one limited key through `POST /v0/management/api-key-limits/reset`. Existing API-key management operations preserve structured limits. The TUI API Keys tab accepts mixed bare and structured key representations, displays each limited key's request and token usage, reset cadence, and reset time, and provides a confirmed per-key usage reset action while keeping keys masked.

**Implementation evidence:** `internal/config/api_key_entry.go`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/server.go`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`.

**Recorded validation:** focused config, usage-limit tracker, middleware, management API, server API, TUI, and server-entrypoint Go test suites; required `cmd/server` compile check.

**Last updated:** 2026-07-29

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

No upstream integration has been recorded for this fork.
