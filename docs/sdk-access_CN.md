# @sdk/access 开发指引

`github.com/router-for-me/CLIProxyAPI/v6/sdk/access` 包负责代理的入站访问认证。它提供一个轻量的管理器，用于按顺序链接多种凭证校验实现，让服务器在 CLI 运行时内外都能复用相同的访问控制逻辑。

## 引用方式

```go
import (
    sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)
```

通过 `go get github.com/router-for-me/CLIProxyAPI/v6/sdk/access` 添加依赖。

## Provider Registry

访问提供者是全局注册，然后以快照形式挂到 `Manager` 上：

- `RegisterProvider(type, provider)` 注册一个已经初始化好的 provider 实例。
- 每个 `type` 第一次出现时会记录其注册顺序。
- `RegisteredProviders()` 会按该顺序返回 provider 列表。

## 管理器生命周期

```go
manager := sdkaccess.NewManager()
manager.SetProviders(sdkaccess.RegisteredProviders())
```

- `NewManager` 创建空管理器。
- `SetProviders` 替换提供者切片并做防御性拷贝。
- `Providers` 返回适合并发读取的快照。

如果管理器本身为 `nil` 或未配置任何 provider，调用会返回 `nil, nil`，可视为关闭访问控制。

## 认证请求

```go
result, authErr := manager.Authenticate(ctx, req)
switch {
case authErr == nil:
    // Authentication succeeded; result carries provider and principal.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeNoCredentials):
    // No recognizable credentials were supplied.
case sdkaccess.IsAuthErrorCode(authErr, sdkaccess.AuthErrorCodeInvalidCredential):
    // Credentials were present but rejected.
default:
    // Provider surfaced a transport-level failure.
}
```

`Manager.Authenticate` 会按顺序遍历 provider：遇到成功立即返回，`AuthErrorCodeNotHandled` 会继续尝试下一个；`AuthErrorCodeNoCredentials` / `AuthErrorCodeInvalidCredential` 会在遍历结束后汇总给调用方。

`Result` 提供认证提供者标识、解析出的主体以及可选元数据（例如凭证来源）。

## 内建 `config-api-key` Provider

代理内置一个访问提供者：

- `config-api-key`：校验 `config.yaml` 顶层的 `api-keys`。
  - 凭证来源：`Authorization: Bearer`、`X-Goog-Api-Key`、`X-Api-Key`、`?key=`、`?auth_token=`
  - 元数据：`Result.Metadata["source"]` 会写入匹配到的来源标识

在 CLI 服务端与 `sdk/cliproxy` 中，该 provider 会根据加载到的配置自动注册。

```yaml
api-keys:
  - sk-test-123
  - sk-prod-456
```

### 按 API Key 限制使用量

顶层 `api-keys` 列表也支持带有可选使用量限制的映射项。这些限制针对认证访问本代理的入站客户端 API Key，而不是上游 Provider Key。同一个列表中可以混用裸字符串和映射项；裸字符串仍然不受限制。

```yaml
api-keys:
  - "plain-key-stays-unlimited"
  - key: "team-a-key"
    limits:
      max-requests: 1000
      max-tokens-m: 20        # 2000 万 Token；允许小数（0.5 = 50 万）
      resets: "weekly"        # 省略表示永不重置的 lifetime 限制
```

所有 `limits` 字段均为可选：

| 字段 | 含义 | 说明 |
| --- | --- | --- |
| `max-requests` | 每个窗口的最大请求数 | 省略或设为 `0` 表示不限制请求数 |
| `max-tokens-m` | 以百万为单位的每个窗口最大 Token 数 | 允许小数；`0.5` 表示 500,000 Token。省略或设为 `0` 表示不限制 Token 数 |
| `resets` | 重置周期 | 可选 `hourly`、`daily`、`weekly`、`monthly`；省略表示永不重置的 lifetime 限制 |

如果既未设置 `max-requests` 也未设置 `max-tokens-m`，该 Key 完全不受限制。`resets` 为空同样表示 lifetime。

每个 Key 只有一个窗口，请求数和 Token 数共享该窗口。不能在同一个 Key 上同时设置按分钟的突发上限和按月预算，必须选择一个周期。所有窗口均使用 UTC；周窗口遵循 ISO-8601，从 UTC 周一 00:00 开始。

计数器会持久化到 `<auth-dir>/state/usage-limits.json`，并在启动时恢复，因此重启后限制仍然有效。使用 `state` 子目录是为了让快照文件不进入凭据命名空间：直接放在 `<auth-dir>` 下的所有 `*.json` 文件都会被当作认证文件处理。快照只保存每个 Key 的 SHA-256 哈希，不保存 Key 本身，并以 `0600` 权限写入（Windows 不支持该权限位）。计数器仍按实例独立计算：在负载均衡器后运行 `N` 个副本时，实际有效限制大约是配置值的 `N` 倍。

限制按 Key 全局生效，不区分模型或 Provider。每个到达受限端点的请求都会计数，包括随后在上游失败的请求。Token 使用量在响应完成后记录，因此 Token 限制最多可能被一个正在处理的请求超出。

以下端点不会消耗配额：

- `GET /v1/models`
- `GET /v1beta/models`
- `POST /v1/messages/count_tokens`
- `GET /v1/videos/:request_id`
- `GET /openai/v1/videos/:video_id`
- `GET /openai/v1/videos/:video_id/content`

超出限制时，代理返回 HTTP `429`，并带有以下响应头：

- `X-RateLimit-Limit`
- `X-RateLimit-Remaining: 0`
- 窗口限制才会返回 `Retry-After` 和 `X-RateLimit-Reset`

Lifetime 限制不会返回 `Retry-After` 或 `X-RateLimit-Reset`，因为没有可报告的重置时间。错误响应体会根据路由所属的协议类型返回。OpenAI 路由使用 `{"error":{"message":"...","type":"rate_limit_exceeded","code":"usage_limit_exceeded"}}`；Anthropic 路由使用 `{"type":"error","error":{"type":"rate_limit_error","message":"..."}}`；Gemini 路由使用 `{"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}`。窗口限制的消息格式为 `usage limit exceeded: <metric> limit of <limit> reached; resets at <RFC3339 UTC>`；lifetime 限制的消息格式为 `usage limit exceeded: <metric> limit of <limit> reached`。

`/v1`、`/openai/v1` 和 `/backend-api/codex` 路由组使用 OpenAI 错误格式，包括 `/v1/messages` 的 Claude 请求；`/v1beta` 使用 Gemini 错误格式。

通过 `GET /v0/management/api-key-limits` 查看当前消耗：

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

响应按 Key 排序，并省略未配置限制的 Key。`max_tokens` 是绝对 Token 上限，`max_requests`、`requests_used` 和 `tokens_used` 是计数值。对于 lifetime 限制，`resets` 为 `"lifetime"`，并省略 `reset_at`。

配置热更新会立即应用限制变化，且不会重置已有计数器；但修改某个 Key 的 `resets` 周期会重置该 Key 的计数器。如果将限制调低到当前用量以下，该 Key 会一直被阻止，直到窗口滚动结束。

## 引入外部 Go 模块提供者

若要消费其它 Go 模块输出的访问提供者，直接用空白标识符导入以触发其 `init` 注册即可：

```go
import (
    _ "github.com/acme/xplatform/sdk/access/providers/partner" // registers partner-token
    sdkaccess "github.com/router-for-me/CLIProxyAPI/v6/sdk/access"
)
```

空白导入可确保 `init` 先执行，从而在你调用 `RegisteredProviders()`（或 `cliproxy.NewBuilder().Build()`）之前完成 `sdkaccess.RegisterProvider`。

### 元数据与审计

`Result.Metadata` 用于携带提供者特定的上下文信息。内建的 `config-api-key` 会记录凭证来源（`authorization`、`x-goog-api-key`、`x-api-key`、`query-key`、`query-auth-token`）。自定义提供者同样可以填充该 Map，以便丰富日志与审计场景。

## 编写自定义提供者

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

自定义提供者需要实现 `Identifier()` 与 `Authenticate()`。在 `init` 中用已初始化实例调用 `RegisterProvider` 注册到全局 registry。

## 错误语义

- `NewNoCredentialsError()`（`AuthErrorCodeNoCredentials`）：未提供或未识别到凭证。（HTTP 401）
- `NewInvalidCredentialError()`（`AuthErrorCodeInvalidCredential`）：凭证存在但校验失败。（HTTP 401）
- `NewNotHandledError()`（`AuthErrorCodeNotHandled`）：告诉管理器跳到下一个 provider。
- `NewInternalAuthError(message, cause)`（`AuthErrorCodeInternal`）：网络/系统错误。（HTTP 500）

除可汇总的 `not_handled` / `no_credentials` / `invalid_credential` 外，其它错误会立即冒泡返回。

## 与 cliproxy 集成

使用 `sdk/cliproxy` 构建服务时会自动接入 `@sdk/access`。如果希望在宿主进程里复用同一个 `Manager` 实例，可传入自定义管理器：

```go
coreCfg, _ := config.LoadConfig("config.yaml")
accessManager := sdkaccess.NewManager()

svc, _ := cliproxy.NewBuilder().
  WithConfig(coreCfg).
  WithConfigPath("config.yaml").
  WithRequestAccessManager(accessManager).
  Build()
```

请在调用 `Build()` 之前完成自定义 provider 的注册（通常通过空白导入触发 `init`），以确保它们被包含在全局 registry 的快照中。

### 动态热更新提供者

当配置发生变化时，刷新依赖配置的 provider，然后重置 manager 的 provider 链：

```go
// configaccess is github.com/router-for-me/CLIProxyAPI/v6/internal/access/config_access
configaccess.Register(&newCfg.SDKConfig)
accessManager.SetProviders(sdkaccess.RegisteredProviders())
```

这一流程与 `internal/access.ApplyAccessProviders` 保持一致，避免为更新访问策略而重启进程。
