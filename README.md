方案 A（SDK gin）+ 你的业务路由
你的场景完全适合用 SDK 的 gin，原因如下：

SDK 给了你所有需要的扩展点：

你的需求	SDK 提供的接口
多租户 API Key 鉴权	access.Provider 接口，完全替换 SDK 默认鉴权
每次请求的计费数据	usage.Plugin，含 tokens in/out/cached/reasoning + latency
前端管理界面的路由	WithRouterConfigurator 注入你的 gin 路由组
请求前置拦截（限流/配额）	WithMiddleware 注入 gin middleware
凭证持久化到 PG	实现 auth.Store 接口（3 个方法）
启动后拿到 Manager	Hooks.OnAfterStart 回调拿 *Service，再注册 executor
SDK 帮你免费处理的：

所有 AI 路由（/v1/chat/completions、/v1/messages、Gemini 等）
流式转发、格式转换、header 透传
凭证轮询、自动刷新、quota 冷却
gin engine 生命周期
完整项目结构
myapp/
├── main.go                        # 入口，Builder 组装
├── config.go                      # 配置结构 + 加载
├── config.yaml
│
├── infra/
│   ├── db.go                      # PostgreSQL 初始化（gorm）
│   ├── redis.go                   # Redis 初始化
│   └── migrate.go                 # 表结构迁移
│
├── sdk/
│   ├── store.go                   # 实现 auth.Store → PG 持久化凭证
│   ├── access.go                  # 实现 access.Provider → 多租户 API Key 鉴权
│   └── usage.go                   # 实现 usage.Plugin → 写计费记录到 PG
│
├── executor/
│   ├── util.go                    # readHTTPResponse / streamHTTPResponse 共享工具
│   ├── openai.go                  # ProviderExecutor for OpenAI-compatible
│   ├── claude.go                  # ProviderExecutor for Claude
│   ├── gemini.go                  # ProviderExecutor for Gemini
│   └── vertex.go                  # ProviderExecutor for Vertex
│
├── api/
│   ├── routes.go                  # WithRouterConfigurator 注入的路由注册
│   ├── middleware.go              # 租户上下文注入 middleware
│   │
│   ├── auth/
│   │   └── handler.go             # POST /api/auth/login, /register
│   ├── user/
│   │   └── handler.go             # GET/PATCH /api/users/me
│   ├── billing/
│   │   └── handler.go             # GET /api/billing/usage, /invoices
│   ├── tenant/
│   │   └── handler.go             # 管理员：租户 CRUD
│   └── admin/
│       └── handler.go             # 管理员：凭证池管理、配额设置
│
└── model/
    ├── user.go                    # User, APIKey gorm 模型
    ├── tenant.go                  # Tenant gorm 模型
    └── usage_log.go               # UsageLog gorm 模型（计费记录）
main.go 的组装方式
func main() {
    cfg := loadConfig()
    db  := infra.InitDB(cfg)
    rdb := infra.InitRedis(cfg)

    // 你实现的三个 SDK 接口
    pgStore    := sdk.NewPGStore(db)           // auth.Store
    myAccess   := sdk.NewTenantAccessProvider(db, rdb)  // access.Provider
    myUsage    := sdk.NewUsagePlugin(db, rdb)  // usage.Plugin

    // access.Manager 注入你的鉴权
    accessMgr := sdkaccess.NewManager()
    accessMgr.SetProviders([]sdkaccess.Provider{myAccess})

    var authMgr *cliproxyauth.Manager

    svc, err := cliproxy.NewBuilder().
        WithConfig(cfg.SDK).
        WithServerOptions(
            sdkapi.WithMiddleware(api.TenantMiddleware(db, rdb)),
            sdkapi.WithRouterConfigurator(api.RegisterRoutes(db, rdb)),
        ).
        WithRequestAccessManager(accessMgr).
        WithHooks(cliproxy.Hooks{
            OnAfterStart: func(s *cliproxy.Service) {
                // 启动后注册 executor
                authMgr = cliproxyauth.NewManager(pgStore,
                    &cliproxyauth.RoundRobinSelector{},
                    cliproxyauth.NoopHook{})
                authMgr.Load(context.Background())
                authMgr.RegisterExecutor(executor.NewOpenAI(cfg))
                authMgr.RegisterExecutor(executor.NewClaude(cfg))
                authMgr.RegisterExecutor(executor.NewGemini(cfg))
                s.RegisterUsagePlugin(myUsage)
            },
        }).
        Build()

    svc.Run(context.Background())
}
三个关键接口的实现要点
store.go
 — 凭证持久化到 PG（3 个方法）

type PGStore struct{ db *gorm.DB }
func (s *PGStore) List(ctx context.Context) ([]*auth.Auth, error)   // SELECT all
func (s *PGStore) Save(ctx context.Context, a *auth.Auth) (string, error) // UPSERT
func (s *PGStore) Delete(ctx context.Context, id string) error       // DELETE
access.go
 — 多租户鉴权，替换 SDK 默认 API Key 验证

// Authenticate 从 Authorization header 取 key，
// 查 PG/Redis 缓存，返回 tenant_id 作为 Principal
func (p *TenantAccessProvider) Authenticate(ctx context.Context, r *http.Request) (*access.Result, *access.AuthError) {
    key := extractBearerToken(r)
    tenant, err := p.validateKey(ctx, key)  // 查 Redis L1 → PG L2
    if err != nil {
        return nil, access.NewInvalidCredentialError()
    }
    return &access.Result{
        Provider:  "tenant-apikey",
        Principal: tenant.ID,
        Metadata:  map[string]string{"tenant_id": tenant.ID, "plan": tenant.Plan},
    }, nil
}
usage.go
 — 每次请求完成后写计费记录

// HandleUsage 在每次 AI 请求完成后被 SDK 调用
// record 里有 Provider/Model/Latency/InputTokens/OutputTokens/Failed
func (p *UsagePlugin) HandleUsage(ctx context.Context, record usage.Record) {
    tenantID := tenantFromContext(ctx)   // 从 middleware 注入的 context 取
    p.db.Create(&model.UsageLog{
        TenantID:     tenantID,
        Provider:     record.Provider,
        Model:        record.Model,
        InputTokens:  record.Detail.InputTokens,
        OutputTokens: record.Detail.OutputTokens,
        Latency:      record.Latency.Milliseconds(),
        Failed:       record.Failed,
        Cost:         calcCost(record.Detail, tenantPlan),
    })
    // 同时更新 Redis 里的实时配额计数
    p.rdb.IncrBy(ctx, quotaKey(tenantID), record.Detail.TotalTokens)
}
与方案 B（当前 CPA-Gateway）的对比
方案 A（SDK gin）	方案 B（自建 gin，当前做法）
AI 路由代码	0 行，SDK 全包	~700 行（handler_proxy.go）
流式转发	0 行	~200 行重复
计费接入点	usage.Plugin（1 个接口）	手动在每个 handler 里写
多租户鉴权	access.Provider（1 个接口）	手动 middleware + ValidateAPIKey
凭证持久化	auth.Store（3 个方法）	手动 PostgresAuthStore
你的业务路由	WithRouterConfigurator 注入	完全自己写（一样）
总代码量	大幅减少	当前 ~10000 行
结论：你的场景用方案 A，SDK 的 gin 直接用，三个接口（Store + AccessProvider + UsagePlugin）是你唯一需要实现的 SDK 对接代码，其余全是你自己的业务逻辑。



推荐：方案 C — 混合架构（SDK 接管 AI 路由 + 自建业务路由共存）
既然你决定忽视原有边界、重新构建，我不建议直接照搬 README 里的方案 A。方案 A 有一个设计问题：它把所有路由（包括你的 panel/admin/subscription/ticket 等业务路由）都塞进 SDK 的 WithRouterConfigurator，这意味着你的业务路由的生命周期完全受 SDK 控制。

更好的做法是：

核心思路
SDK gin engine（SDK 控制）
├── /v1/chat/completions     ← SDK 内置
├── /v1/messages             ← SDK 内置
├── /v1/models               ← SDK 内置
├── /healthz                 ← 你注入
├── /metrics                 ← 你注入
└── /api/panel/**            ← 你注入（全部业务路由）
你只实现 3 个 SDK 接口 + 把现有业务路由通过 WithRouterConfigurator 挂上去。

与方案 A 的区别
方案 A 原文建议把 auth.Manager 的注册放在 OnAfterStart 回调里，这是不必要的复杂度。实际上你可以在 Build() 之前就准备好所有 executor，通过 WithAuthManager 直接注入。

你需要实现的 3 个接口
接口	作用	对应你现有代码
auth.Store	凭证持久化	当前 PostgresAuthStore（db.go 里的 AuthRecord）
access.Provider	请求鉴权	当前 ValidateAPIKey + AuthMiddleware
usage.Plugin	计费记录	当前 BillingMiddleware + UsageLog 写入
项目结构
cpa-gateway/
├── main.go                    # Builder 组装，~60 行
├── config.go                  # 保持不变
├── config.yaml
│
├── infra/
│   ├── db.go                  # InitDB + AutoMigrate（从现有 db.go 拆出）
│   └── redis.go               # initRedis（从现有 main.go 拆出）
│
├── model/
│   ├── user.go                # User, ApiKey, Group
│   ├── billing.go             # BalanceLog, UsageLog
│   ├── subscription.go        # SubscriptionPackage, Subscription
│   ├── ticket.go              # Ticket, TicketReply
│   └── sdk.go                 # AuthRecord, ProviderConfig, OAuthSession, etc.
│
├── sdk/
│   ├── store.go               # auth.Store → 包装现有 PostgresAuthStore
│   ├── access.go              # access.Provider → 包装现有 ValidateAPIKey
│   └── usage.go               # usage.Plugin → 包装现有计费逻辑
│
├── executor/
│   ├── util.go                # readHTTPResponse / streamHTTPResponse
│   ├── openai.go              # 现有 executor_openai.go
│   ├── claude.go              # 现有 executor_claude.go
│   ├── gemini.go              # 现有 executor_gemini.go
│   ├── vertex.go              # 现有 executor_vertex.go
│   └── codex.go               # 现有 executor_codex.go
│
├── api/
│   ├── routes.go              # WithRouterConfigurator 注入的路由注册
│   ├── middleware.go          # 通用中间件（metrics、CORS 等）
│   ├── auth_handler.go        # /api/panel/auth/*
│   ├── user_handler.go        # /api/panel/users/*
│   ├── billing_handler.go     # /api/panel/billing/*
│   ├── subscription_handler.go
│   ├── admin_handler.go
│   ├── ops_handler.go
│   └── sdk_mgmt_handler.go
│
└── frontend/                  # 保持不变
main.go 骨架
func main() {
    cfg := config.Load("config.yaml")
    db := infra.InitDB(cfg)
    rdb := infra.InitRedis(cfg)

    // 1. 准备 SDK 接口实现
    store := sdk.NewPGStore(db)
    accessProvider := sdk.NewAccessProvider(db, rdb)
    usagePlugin := sdk.NewUsagePlugin(db, rdb)

    // 2. 准备 executor + auth manager
    authMgr := cliproxyauth.NewManager(store, &cliproxyauth.RoundRobinSelector{}, cliproxyauth.NoopHook{})
    authMgr.RegisterExecutor(executor.NewOpenAI(cfg))
    authMgr.RegisterExecutor(executor.NewClaude(cfg))
    authMgr.RegisterExecutor(executor.NewGemini(cfg))
    authMgr.RegisterExecutor(executor.NewVertex(cfg))
    authMgr.RegisterExecutor(executor.NewCodex(cfg))
    authMgr.Load(context.Background())

    // 3. 构建 SDK 服务
    svc, _ := cliproxy.NewBuilder().
        WithConfig(cfg.SDK.ToSDKConfig()).
        WithAuthManager(authMgr).
        WithRequestAccessManager(sdkaccess.NewManager().SetProviders([]sdkaccess.Provider{accessProvider})).
        WithServerOptions(
            sdkapi.WithMiddleware(api.MetricsMiddleware()),
            sdkapi.WithRouterConfigurator(api.RegisterRoutes(db, rdb, cfg)),
        ).
        WithUsagePlugin(usagePlugin).
        Build()

    svc.Run(context.Background())
}
你能删掉的代码
现有文件	原因
handler_proxy.go（AI 路由部分）	SDK 内置
流式转发逻辑	SDK 内置
/v1/chat/completions handler	SDK 内置
/v1/models handler	SDK 内置
凭证轮询/冷却逻辑	SDK auth.Manager 内置
预计删除 ~800-1000 行，新增 SDK 接口适配层 ~150 行（store + access + usage 三个文件）。

为什么不是纯方案 A
executor 保留 — 你已经有 5 个成熟的 executor 实现，它们是纯函数，不依赖 HTTP 框架，直接复用。
model 层保留 — 你的 gorm 模型（User、Subscription、Ticket 等）是业务核心，不需要重写。
渐进式迁移 — 你可以先让 SDK 接管 AI 路由，业务路由通过 WithRouterConfigurator 注入，逐步验证。不需要一次性重写所有代码。
迁移步骤建议
拆包（model/infra/api/executor/sdk），不改逻辑
实现 
store.go
（包装现有 PostgresAuthStore）
实现 
access.go
（包装现有 ValidateAPIKey）
实现 
usage.go
（包装现有计费写入）
改写 main.go 为 Builder 模式
删除旧的 AI proxy handler 和流式转发代码
验证所有路由正常工作
要我开始执行这个重构吗？还是你想先调整方向？