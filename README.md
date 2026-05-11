# CPA-Gateway — CLIProxyAPI Gateway Architecture

## 概述

CPA-Gateway 是基于 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) SDK 的二次开发项目，实现了计费管理和多租户功能。

核心架构：**Gin 做 HTTP 层 + SDK cliproxy 做代理核心 + 自有 billing 做计费**

---

## 架构图

```
┌─────────────────────────────────────────────────────────────────┐
│                        Gin Engine                                │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │  middleware chain (gin.HandlerFunc[])                    │  │
│  │  ├── metricsMiddleware                                  │  │
│  │  ├── traceID                                           │  │
│  │  ├── authMiddleware        ← 认证 + 提取 BillingCtx   │  │
│  │  └── billingMiddleware     ← 计费前置检查 + 预扣费      │  │
│  └──────────────────────────────────────────────────────────┘  │
│                              │                                  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │  RouterRegistrar                                        │  │
│  │  ├── /api/panel/*     ← CPA 自有管理面板 (admin/user)  │  │
│  │  ├── /webhooks/*      ← 支付回调                       │  │
│  │  ├── /v0/management/* ← SDK 管理端点 (代理)             │  │
│  │  └── /v1/*            ← AI 代理请求 (SDK Handler)      │  │
│  └──────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│  cliproxy.Service (SDK 核心)                                    │
│  ├── cliproxy/auth    ← 凭证管理 (OAuth/Token Store)            │
│  ├── cliproxy/executor← 请求执行                               │
│  └── usage.Plugin     ← 用量回调 → billing.Plugin              │
└─────────────────────────────────────────────────────────────────┘
```

---

## 分层职责

| 层级 | 代码位置 | 职责 |
|------|----------|------|
| **计费/多租户** | `internal/app/billing/*` | Ledger、预扣费、结算、订阅 |
| **业务逻辑** | `internal/app/pricing/*` | 定价解析、规则匹配 |
| **认证** | `internal/http/middleware/auth.go` | JWT 验证、API Key 校验、BillingCtx 注入 |
| **计费中间件** | `internal/http/middleware/billing.go` | 余额/订阅检查、并发控制、速率限制 |
| **Gin 路由** | `internal/http/router.go` | 自有 panel + SDK 端点注册 |
| **SDK 桥接** | `internal/infra/sdkbridge/*` | 隔离 SDK 类型，只用接口交互 |

---

## 关键集成点

### 1. BillingCtx 跨请求传递

```go
// internal/http/middleware/auth.go 注入
ctx := domainbilling.WithBillingCtx(c.Request.Context(), bc)
c.Request = c.Request.WithContext(ctx)

// internal/app/billing/plugin.go 消费
bc := domainbilling.BillingCtxFromUsageContext(ctx)
```

### 2. Usage Plugin 结算

```go
// internal/infra/sdkbridge/quota/register.go
func RegisterUsagePlugin(svc *cliproxy.Service, sink sdkbridge.UsageSink) error {
    plugin := billing.NewPlugin(...)
    svc.RegisterUsagePlugin(plugin)  // SDK 用量回调 → 你的结算逻辑
}
```

### 3. Gin 中间件链 → SDK

```go
// internal/app/init_sdk.go
Middlewares: []gin.HandlerFunc{
    metrics.Middleware(),
    middlewares.traceID.Handle,
    protectSDKManagementRoutes(internalMgmtKey),
    middlewares.auth.Handle,      // 认证 + BillingCtx
    middlewares.billing.Handle,   // 预扣费 + 限额
},
```

---

## SDK 包映射

| SDK 包 | CPA Gateway 中的用途 | 隔离位置 |
|--------|---------------------|----------|
| `sdk/cliproxy` | `Service.Build()`，`RegisterUsagePlugin()` | `sdkbridge` |
| `sdk/cliproxy/usage` | `UsagePluginAdapter.HandleUsage()` — 转换为 `domainbilling.UsageReport` | `sdkbridge/quota` |
| `sdk/cliproxy/auth` | `RuntimeAuthAdapter` — 提取 bearer token / management key | `sdkbridge/authfile` |
| `sdk/config` | `ConfigAdapter` — 读写 `ConfigSnapshot` | `sdkbridge/configpanel` |
| `sdk/api` | `WithRouterConfigurator` — 挂载 CPA router | `sdkbridge/center` |
| `sdk/api/handlers` | `BaseAPIHandler` — 模型端点转发 | `sdkbridge/provider` |
| `sdk/translator` | `ModelAdapter` — 拉取 `/v1/models` 原始 JSON | `sdkbridge/provider` |
| `sdk/access` | `RequestAccessManager` — 请求级认证 | 尚未直接使用 |
| `sdk/auth` | OAuth 认证器（Gemini CLI / Claude / Codex 等） | `sdkbridge/authfile` |
| `sdk/proxyutil` | 代理配置解析 | 尚未直接使用 |

---

## 扩展方向

| 扩展点 | 位置 | 说明 |
|--------|------|------|
| **多租户隔离** | `middleware/auth.go` | 在 BillingCtx 里加 TenantID，按租户做资源隔离 |
| **自定义计费模型** | `app/billing/` | 支持按 Token、按请求、按包年包月混合计费 |
| **配额管理** | `middleware/billing.go` | 扩展 `ConcurrencyLimiter`/`RateLimiter` |
| **用量通知** | `app/billing/plugin.go` | webhook / 邮件通知 |
| **审计日志** | `middleware/` | 加 middleware 记录请求明细 |

---

## 技术栈

- **HTTP 框架**: Gin (与 SDK 原生一致)
- **数据库**: ent (Go ORM)
- **缓存**: Redis (计费预扣、订阅限额)
- **SDK 版本**: CLIProxyAPI v6.10.8+
