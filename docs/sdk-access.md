# @sdk/access SDK Reference

The `github.com/router-for-me/CLIProxyAPI/v6/sdk/access` package centralizes inbound request authentication for the proxy. It offers a lightweight manager that chains credential providers, so servers can reuse the same access control logic inside or outside the CLI runtime.

## Importing

```go
import (
    sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)
```

Add the module with `go get github.com/router-for-me/CLIProxyAPI/v6/sdk/access`.

## Provider Registry

Providers are registered globally and then attached to a `Manager` as a snapshot:

- `RegisterProvider(type, provider)` installs a pre-initialized provider instance.
- Registration order is preserved the first time each `type` is seen.
- `RegisteredProviders()` returns the providers in that order.

## Manager Lifecycle

```go
manager := sdkaccess.NewManager()
manager.SetProviders(sdkaccess.RegisteredProviders())
```

* `NewManager` constructs an empty manager.
* `SetProviders` replaces the provider slice using a defensive copy.
* `Providers` retrieves a snapshot that can be iterated safely from other goroutines.

If the manager itself is `nil` or no providers are configured, the call returns `nil, nil`, allowing callers to treat access control as disabled.

## Authenticating Requests

```go
result, authErr := manager.Authenticate(ctx, req)
switch {
case authErr == nil:
    // Authentication succeeded; result describes the provider and principal.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials):
    // No recognizable credentials were supplied.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential):
    // Supplied credentials were present but rejected.
default:
    // Internal/transport failure was returned by a provider.
}
```

`Manager.Authenticate` walks the configured providers in order. It returns on the first success, skips providers that return `AuthErrorCodeNotHandled`, and aggregates `AuthErrorCodeNoCredentials` / `AuthErrorCodeInvalidCredential` for a final result.

Each `Result` includes the provider identifier, the resolved principal, and optional metadata (for example, which header carried the credential).

## Built-in `config-api-key` Provider

The proxy includes one built-in access provider:

- `config-api-key`: Validates API keys declared under top-level `api-keys`.
  - Credential sources: `Authorization: Bearer`, `X-Goog-Api-Key`, `X-Api-Key`, `?key=`, `?auth_token=`
  - Metadata: `Result.Metadata["source"]` is set to the matched source label.

In the CLI server and `sdk/cliproxy`, this provider is registered automatically based on the loaded configuration.

```yaml
api-keys:
  - sk-test-123
  - sk-prod-456
```

### Per-API-key usage limits

The top-level `api-keys` list also accepts mapping entries with optional usage limits. These are limits for inbound client API keys that authenticate to this proxy, not for upstream provider keys. Bare strings and mapping entries can be mixed in the same list; a bare string remains unlimited.

```yaml
api-keys:
  - "plain-key-stays-unlimited"
  - key: "team-a-key"
    limits:
      max-requests: 1000
      max-tokens-m: 20        # 20 million tokens; fractional allowed (0.5 = 500k)
      resets: "weekly"        # omit for a lifetime limit that never resets
```

All `limits` fields are optional:

| Field | Meaning | Notes |
| --- | --- | --- |
| `max-requests` | Maximum requests | Omitted or `0` means unlimited requests |
| `max-tokens-m` | Maximum tokens in millions | Fractional values are allowed; `0.5` means 500,000 tokens. Omitted or `0` means unlimited tokens |
| `resets` | Reset cadence | `hourly`, `daily`, `weekly`, or `monthly`; omitted means a lifetime limit that never resets |

If neither `max-requests` nor `max-tokens-m` is set, the key is entirely unlimited. An empty `resets` value also means lifetime.

Each key has exactly one window shared by both metrics. You cannot set a per-minute burst cap and a monthly budget on the same key; choose one cadence. Window calculations use UTC. Weekly windows use ISO-8601 weeks, beginning Monday at 00:00 UTC.

Counters are persisted to `<auth-dir>/state/usage-limits.json` and restored on startup, so limits survive restarts. The `state` subdirectory keeps the snapshot out of the credential namespace, because every `*.json` placed directly in `<auth-dir>` is treated as an authentication file. The snapshot stores a SHA-256 hash of each key, never the key itself, and is written with `0600` permissions (Windows does not support these permission bits). Counters are still per instance: running `N` replicas behind a load balancer gives an effective limit of roughly `N` times the configured value because each instance counts independently.

Limits are global per key; they are not scoped per model or provider. Every request to a limited endpoint counts, including a request that later fails upstream. Token usage is recorded after the response completes, so a token limit can be exceeded by at most one in-flight request.

The following endpoints never consume quota:

- `GET /v1/models`
- `GET /v1beta/models`
- `POST /v1/messages/count_tokens`
- `GET /v1/videos/:request_id`
- `GET /openai/v1/videos/:video_id`
- `GET /openai/v1/videos/:video_id/content`

When a limit is exceeded, the proxy returns HTTP `429` with these headers:

- `X-RateLimit-Limit`
- `X-RateLimit-Remaining: 0`
- `Retry-After` and `X-RateLimit-Reset` for windowed limits only

Lifetime limits send neither `Retry-After` nor `X-RateLimit-Reset`, because there is no reset time to report. The error body follows the protocol family of the route. OpenAI routes use `{"error":{"message":"...","type":"rate_limit_exceeded","code":"usage_limit_exceeded"}}`; Anthropic routes use `{"type":"error","error":{"type":"rate_limit_error","message":"..."}}`; Gemini routes use `{"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}`. For a windowed limit, the message is `usage limit exceeded: <metric> limit of <limit> reached; resets at <RFC3339 UTC>`; for a lifetime limit, it is `usage limit exceeded: <metric> limit of <limit> reached`.

The `/v1`, `/openai/v1`, and `/backend-api/codex` route groups use the OpenAI error shape, including Claude requests at `/v1/messages`; `/v1beta` uses the Gemini error shape.

Read current consumption with `GET /v0/management/api-key-limits`:

```json
{
  "api-key-limits": [
    {
      "key": "...",
      "limits": {
        "max_requests": 1000,
        "requests_used": 25,
        "max_tokens": 20000000,
        "tokens_used": 125000,
        "resets": "weekly",
        "reset_at": "<RFC3339>"
      }
    }
  ]
}
```

The response is sorted by key and omits keys without limits. `max_tokens` is the absolute token limit, while `max_requests`, `requests_used`, and `tokens_used` are counts. For a lifetime limit, `resets` is `"lifetime"` and `reset_at` is omitted.

Reset one limited key's counters with `POST /v0/management/api-key-limits/reset`:

```json
{"key": "..."}
```

Success returns `{"status":"ok"}`. The endpoint returns HTTP `400` for a malformed or blank key, `404` when the key has no configured limits, and `503` when the usage tracker is unavailable.

Config hot-reload applies limit changes immediately and does not reset existing counters, except that changing a key's `resets` cadence resets that key's counters. Lowering a limit below current usage blocks the key until the window rolls over.

## Loading Providers from External Go Modules

To consume a provider shipped in another Go module, import it for its registration side effect:

```go
import (
    _ "github.com/acme/xplatform/sdk/access/providers/partner" // registers partner-token
    sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)
```

The blank identifier import ensures `init` runs so `sdkaccess.RegisterProvider` executes before you call `RegisteredProviders()` (or before `cliproxy.NewBuilder().Build()`).

### Metadata and auditing

`Result.Metadata` carries provider-specific context. The built-in `config-api-key` provider, for example, stores the credential source (`authorization`, `x-goog-api-key`, `x-api-key`, `query-key`, `query-auth-token`). Populate this map in custom providers to enrich logs and downstream auditing.

## Writing Custom Providers

```go
type customProvider struct{}

func (p *customProvider) Identifier() string { return "my-provider" }

func (p *customProvider) Authenticate(ctx context.Context, r *http.Request) (*sdkaccess.Result, *sdkaccess.AuthError) {
    token := r.Header.Get("X-Custom")
    if token == "" {
        return nil, sdkaccess.NewNotHandledError()
    }
    if token != "expected" {
        return nil, sdkaccess.NewInvalidCredentialError()
    }
    return &sdkaccess.Result{
        Provider:  p.Identifier(),
        Principal: "service-user",
        Metadata:  map[string]string{"source": "x-custom"},
    }, nil
}

func init() {
    sdkaccess.RegisterProvider("custom", &customProvider{})
}
```

A provider must implement `Identifier()` and `Authenticate()`. To make it available to the access manager, call `RegisterProvider` inside `init` with an initialized provider instance.

## Error Semantics

- `NewNoCredentialsError()` (`AuthErrorCodeNoCredentials`): no credentials were present or recognized. (HTTP 401)
- `NewInvalidCredentialError()` (`AuthErrorCodeInvalidCredential`): credentials were present but rejected. (HTTP 401)
- `NewNotHandledError()` (`AuthErrorCodeNotHandled`): fall through to the next provider.
- `NewInternalAuthError(message, cause)` (`AuthErrorCodeInternal`): transport/system failure. (HTTP 500)

Errors propagate immediately to the caller unless they are classified as `not_handled` / `no_credentials` / `invalid_credential` and can be aggregated by the manager.

## Integration with cliproxy Service

`sdk/cliproxy` wires `@sdk/access` automatically when you build a CLI service via `cliproxy.NewBuilder`. Supplying a manager lets you reuse the same instance in your host process:

```go
coreCfg, _ := config.LoadConfig("config.yaml")
accessManager := sdkaccess.NewManager()

svc, _ := cliproxy.NewBuilder().
  WithConfig(coreCfg).
  WithConfigPath("config.yaml").
  WithRequestAccessManager(accessManager).
  Build()
```

Register any custom providers (typically via blank imports) before calling `Build()` so they are present in the global registry snapshot.

### Hot reloading

When configuration changes, refresh any config-backed providers and then reset the manager's provider chain:

```go
// configaccess is github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access
configaccess.Register(&newCfg.SDKConfig)
accessManager.SetProviders(sdkaccess.RegisteredProviders())
```

This mirrors the behaviour in `internal/access.ApplyAccessProviders`, enabling runtime updates without restarting the process.
