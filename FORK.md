# CLIProxyAPI Fork

This fork keeps per-API-key request and token usage limits and an optional web-console Docker deployment that are not supplied by the shared base.

## Divergence Log

This is a current-state record only. Each entry describes a surviving difference between `HEAD` and the latest shared base. The sole fork commit is `2037ab99973e15f50685ba84d75785659c55a83b`; its parent, `a14dfc779f43aed588e68b31fb34ab5ced700851`, is the current shared base. This checkout has no `upstream` remote or `base` tag, so update the recorded base explicitly when upstream is integrated.

Keep stable IDs when updating this section; gaps are intentional. When upstream absorbs a difference, remove or rewrite the entry rather than preserving chronology here. Update its behavior, implementation evidence, and validation when the surviving difference changes.

### DL001 - Per-API-key request and token usage limits

Inbound client API keys may be configured as structured entries with optional request and token caps while bare-string keys remain compatible and unlimited. A key can use a lifetime window or a UTC hourly, daily, weekly, or monthly reset cadence; request and token limits share that window.

The proxy enforces limits across the OpenAI-, Anthropic-, and Gemini-compatible API surfaces, returning protocol-appropriate HTTP `429` bodies and rate-limit headers. Model discovery, token counting, and asynchronous video status or content routes do not consume quota. Requests count when admitted, while tokens are recorded from normalized response usage after completion.

Usage counters persist under the authentication directory at `state/usage-limits.json`. Persisted entries identify keys only by SHA-256 hash, survive restarts, and remain local to each proxy instance. Configuration hot reload applies limit changes immediately, preserves counters unless the reset cadence changes, and removes counters for deleted or unlimited keys.

The management API exposes current limited-key consumption through `GET /v0/management/api-key-limits` and resets one key's counters through `POST /v0/management/api-key-limits/reset`. A reset for a configured key that has no recorded usage succeeds as a no-op, and only an unconfigured key returns `404`.

`PATCH /v0/management/api-keys` creates, rotates, and re-limits one key in a single call. It resolves the target from an in-range `index`, then from `old` or `match`, and otherwise appends a new entry, so a body that carries only `new` creates a key instead of failing. Its `limits` field distinguishes three states: omitted preserves the existing limits, an explicit `null` clears them, and an object replaces them as a whole. An object that resolves to no caps is stored as no limits, which keeps the key a bare string in `config.yaml`; a reset cadence without a cap counts as no caps, because a cadence alone enforces nothing and the usage tracker ignores it. An out-of-range index, a blank key on create, a `limits` value that is neither an object nor `null`, and an invalid cap or cadence return `400` with a descriptive message and leave the configuration unchanged. Key selection through `old` or `match` compares trimmed key strings.

The TUI API Keys tab accepts mixed bare and structured key representations and displays each limited key's request and token usage, reset cadence, and reset time, including keys that have configured limits but no recorded usage. Adding and editing a key use a four-field form covering the key, the request cap, the token cap in millions, and a reset-cadence selector; limits stay optional, and blank caps with a cadence of never clear the limits, while a cadence without a cap is rejected in the form. Rows carry their original position in the configured list, so editing or deleting a row targets that entry even when the list contains blank entries. The tab keeps keys masked and keeps the confirmed per-key usage reset action, which is now available for every key with configured limits.

**Implementation evidence:** `internal/config/api_key_entry.go`, `internal/usagelimit/`, `internal/api/middleware/usage_limit.go`, `internal/api/usage_limit.go`, `internal/api/server.go`, `internal/api/handlers/management/{api_key_limits.go,config_lists.go,handler.go}`, `internal/access/config_access/provider.go`, `internal/tui/{client.go,keys_tab.go,i18n.go}`, `cmd/server/main.go`, `config.example.yaml`, and `docs/sdk-access.md`.

**Recorded validation:** focused config, usage-limit tracker, middleware, management API, server API, TUI, and server-entrypoint Go test suites; required `cmd/server` compile check; live management-API run against a server started from a temporary config, covering create, rotate, limit edit, limit clear, rejected limit values, out-of-range index, usage reset, and the resulting `config.yaml`.

**Last updated:** 2026-08-17

### DL002 - Two-service web-console Docker deployment

`docker-compose-web.yml` runs CLIProxyAPI behind an Nginx service on port `8317`. Nginx serves a current Cli-Proxy-API-Management-Center build at `/` and proxies every other path to CLIProxyAPI, including streaming and WebSocket traffic. The Management Center ref and public web port can be selected through `MANAGEMENT_CENTER_REF` and `CLI_PROXY_WEB_PORT`.

`Dockerfile.web` builds the Management Center from its GitHub repository with the Bun version declared by that project, then copies its single-file production output into an Nginx image. The deployment retains the existing CLIProxyAPI configuration, auth, log, plugin, and OAuth callback mounts and ports.

**Implementation evidence:** `docker-compose-web.yml`, `Dockerfile.web`, and `nginx-web.conf`.

**Recorded validation:** Docker Compose configuration rendering, both image builds, and a live two-container request check covering the Management Center at `/`, the proxied health endpoint at `/healthz`, and the authenticated Management API.

**Last updated:** 2026-08-27

## Merge History

This is an append-only historical decision record. It provides context for integrations but never, by itself, establishes an ongoing fork divergence; use the current Divergence Log for that determination.

No upstream integration has been recorded for this fork.
