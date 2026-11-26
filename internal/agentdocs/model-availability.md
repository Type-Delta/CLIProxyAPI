# Check model availability with a routing dry run

Use `POST /v8/management/routing/dry-run` to check whether CPA can route a
request for a model using its current local state. The dry run makes no upstream
request, consumes no inference tokens, and does not advance routing state or
create session bindings. It uses cached quota observations without refreshing them.
ClickHouse and analytics are not required.

This checks local routing eligibility. It cannot prove that the provider will
accept the model, that credentials still work upstream, or that quota remains.
Confirming those requires a real inference request.

## Run the check

Set `CPA_BASE` to your CPA base URL without a trailing slash,
`CPA_MANAGEMENT_KEY` to an authorized management key, and `CPA_MODEL` to the
exact model or alias the client will request, including any thinking suffix.
Management keys are separate from client inference keys. See
[management access and errors](management.md#access-and-errors).

With `curl` and `jq` installed:

```sh
jq -n --arg model "$CPA_MODEL" '{model: $model}' |
  curl --fail-with-body --silent --show-error \
    "$CPA_BASE/v8/management/routing/dry-run" \
    -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
    -H 'Content-Type: application/json' \
    --data-binary @-
```

The minimum body is `{"model":"MODEL_FROM_REQUEST"}`. The management endpoint
returns HTTP 200 for a completed simulation, including when inference would
fail. Read `result.status` for the projected inference status.

## Read the result

Start with `result`. Use `trace` when you need to explain the decision.

| Field | Meaning |
| --- | --- |
| `result.success` | `true` when CPA expects to route the request locally, `false` when CPA predicts a local error, or `null` when the outcome depends on a scheduler the dry run cannot evaluate. |
| `result.status` | Projected HTTP status, such as `200`, `400`, or `429`. |
| `result.body` | The string `"success"` on a routable request, or a string containing the projected OpenAI-compatible JSON error body. Parse the string as JSON on failure to read fields such as `error.reset_time`. |
| `result.retry_after` | Projected `Retry-After` in seconds. A cooldown uses the earliest known time an eligible credential can recover, including quota reserve releases. |
| `result.credential` | Object with `id` and `label` when CPA can predict a specific credential. |
| `result.error_class` | CPA's machine-readable error class, such as `model_cooldown`, `auth_not_found`, or `model_not_found`. |

Fields with unknown or inapplicable values are `null`. A request can be locally
routable with a null credential if live routing would refresh quota before
choosing one. An older CPA may return only `trace` and `warnings`.

### Explain the decision

All candidate references are zero-based indexes into `trace.candidates`.

| Field | Meaning |
| --- | --- |
| `trace.pick` | Selected candidate index, or `-1` when no pick was made. Resolve a nonnegative value through `trace.candidates[trace.pick]` to get its `id`, `label` and `provider`. |
| `trace.pick_predicted` | `true` means the dry run predicts that selection. False or absent means the exact pick is uncertain, even if `pick` contains a cached estimate. |
| `trace.error` | `no_eligible` means this simulated request has no eligible route. Inspect candidate gates and affinity to learn why. |
| `trace.candidates[].gate` | Missing or empty means that candidate passed local eligibility checks. A nonempty value explains exclusion. Passing gates alone does not guarantee selection. |
| `trace.candidates[].until_ms` | Expected recovery time for a cooldown, quota or reserve gate, as Unix milliseconds; absent or zero means unknown. |
| `trace.tier.pool` | Candidate indexes the strategy can pick from after transport preference. With `trace.tier.rule_pool: true`, the picker uses eligible custom-rule targets across tiers with equal configured weights. |
| `trace.rules`, `trace.affinity` | Rule and session-binding decisions that can narrow or block routing. |
| `trace.decided_by` | Layer that determined the selection. `home` means local simulation cannot determine availability. |
| `warnings` | Draft or saved-rule problems such as unknown credential targets. Review these before drawing a conclusion. |

For example, `pick: 0` with `pick_predicted: true` and candidate 0 labeled
`Primary account` means CPA currently predicts routing to that account. Report
it as locally routable, with upstream access unverified.

If `trace.error` is `no_eligible`, report that the request is locally blocked.
Common gates are `unsupported`, `disabled`, `unauthorized`, `cooldown`, `quota`,
`reserve`, `policy` and `weight`. An empty candidate list can mean no provider
was resolved for that model. Use
`GET /v8/management/routing/credentials` for credential labels and current cooldown
detail; [the management guide](management.md) describes that response.

If the pick is uncertain, inspect `trace.tier.pool` and
`trace.strategy_step.fallback`. For example,
`missing_recovery_data_refresh_pending` means live routing would refresh quota
observations that the dry run deliberately leaves cached. Report eligible
candidates and the uncertainty rather than asserting that the cached pick will win.

## Match the actual request

A model-only check does not reproduce client-key rules, headers or existing
session bindings. Supply the relevant request context when those affect routing:

```json
{
  "model": "MODEL_FROM_REQUEST",
  "api_key_label": "CONFIGURED_CLIENT_KEY_LABEL",
  "client_type": "codex",
  "headers": {"User-Agent": "ACTUAL_CLIENT_USER_AGENT"},
  "session_id": "CANONICAL_SESSION_ID_FROM_REQUEST",
  "downstream_websocket": false
}
```

`api_key_label` is the configured client-key label, not the key secret. Unknown
or ambiguous labels return HTTP 400. Use actual inbound headers for header rules
and client detection. Session IDs must retain their source prefix and any
`:agent:` suffix. Omit context fields you do not have rather than inventing them.
Set `downstream_websocket` to `true` for a downstream WebSocket request.

For native Images API requests, add `"source_format":"openai-image"` to select
the image credential track. Omit it for ordinary text-route checks. Optional
`rules` replaces saved routing rules for this simulation only; omit it when
checking current production rules.

Missing or blank `model` returns HTTP 400 with `path: "model"`; invalid
`source_format` returns HTTP 400 with `path: "source_format"`. Authentication
or access errors describe management access, not model availability. Home mode
disables local management and these docs; use the Home control plane for its
routing decisions. Do not clear cooldowns or change credentials merely to check
availability.
