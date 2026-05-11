# SDK Gin 全栈 + Hold/Settle 闭环 + Provider 级 Token 抽取

## TL;DR

> **Quick Summary**: 将 CPA-Gateway 从手动 Gin 方案 B 迁到 SDK Builder 方案 D，实现完整的 Hold（预扣）→ Settle（精扣）→ Release（失败退款）计费闭环，包括按 token 类型定价（input/output/cached/reasoning）、provider 级流式 token 解析、订阅日/周/月配额管控与周期 reset、以及项目结构拆包重组。
>
> **Deliverables**:
> - 项目结构拆包：`infra/`, `model/`, `ledger/`, `pricing/`, `sdk/`, `executor/`, `api/`, `authutil/`, `config/`（可选）共 8-9 个包
> - 3 个 SDK 接口实现：`access.Provider`（CPA tenant 鉴权）、`usage.Plugin`（结算），以及 `HoldMiddleware`（gin middleware）
> - `pricing.Calculator`：`Estimate()` 预估算 + `Compute()` 精算，按 ModelPrice 四列定价 + `ModelPriceCache`
> - 每个 executor 增加 `usage_parser.go` 解析 provider 专属 token usage + 在 Execute/ExecuteStream 中调 `usage.PublishRecord(ctx, rec)`
> - `SettleCtx` 通过 ctx value 在 HoldMiddleware → executor → UsagePlugin 链路传递（替代 request_id keyed collector）
> - 订阅日/周/月配额 + period reset 字段（`DailyResetAt/WeeklyResetAt/MonthlyResetAt`）
> - Go 测试基础设施搭建（miniredis 作为 Redis mock）+ 核心模块 TDD 测试
> - ModelPrice seed（12+ 真实常用模型预设价格，标注占位值）
> - `main.go` 切到 Builder 模式（~100 行，含 `svc.RegisterUsagePlugin` 调用）
> - 删除旧 `ProxyChatHandler` / `BillingMiddleware` / `calculateProxyUsage`（~500 行）
>
> **Estimated Effort**: XLarge（23 个实现任务 + 4 个验证任务，每步独立可编译）
> **Parallel Execution**: YES — 5 waves
> **Critical Path**: Wave 1 (拆包) → Wave 2 (sdk + pricing + usage_parser) → Wave 3 (executor 集成) → Wave 4 (main.go 切换 + 清理) → Wave 5 (审查)

---

## Context

### Original Request
用户提供了完整的方案 D 技术规格：SDK gin engine 接管所有 /v1/* AI 路由，实现 Hold/Settle/Release 计费闭环，provider 级 token 解析（按 input/output/cached/reasoning 定价），订阅配额管控，以及项目结构重组。

### Interview Summary
**Key Discussions**:
- **测试策略**: TDD — 先搭建 Go 测试基础设施，每个模块先写测试再实现（RED→GREEN→REFACTOR）
- **ModelPrice Seed**: 预设 12+ 常用真实模型价格（占位值，生产需按官方定价对齐），admin 可后期修改
- **迁移策略**: Hard cut — 所有任务全部完成后一次性切换，不双写
- **流式 usage 传递**: executor 在 `Execute` / `ExecuteStream` 结束前调 `usage.PublishRecord(ctx, rec)`，`UsagePlugin.HandleUsage` 从 ctx 取 `SettleCtx`（替代 request_id keyed collector，简化并发管理）
- **订阅配额 reset**: 给 `Subscription` 模型增加 `DailyResetAt/WeeklyResetAt/MonthlyResetAt`；HoldMiddleware 判断 `now >= ResetAt` 就归零 + 推进到下一个周期点（UTC）
- **测试依赖**: 允许引入 `github.com/alicebob/miniredis/v2` 作为 in-process Redis mock（test-only，不进入 production binary）

### Metis Review
**Identified CRITICAL Gaps** (addressed, v2 更新):
1. **`cliproxy.Builder` 无 `WithUsagePlugin` 方法**: Builder 方法表已核查（v7.0.2），只有 `WithAuthManager/WithRequestAccessManager/WithServerOptions/WithHooks/WithConfig/WithConfigPath/WithAPIKeyClientProvider/WithTokenClientProvider/WithWatcherFactory/WithPostAuthHook/WithCoreAuthManager/WithLocalManagementPassword`。UsagePlugin 必须通过 `svc.RegisterUsagePlugin(plugin)` 在 `Build()` 之后、`Run()` 之前注册。**Task 20 组装流程依此修订**。
2. **`usage.Record` 无 request_id 或 Metadata 字段**: 字段清单已核查（`Provider/Model/Alias/APIKey/AuthID/AuthIndex/AuthType/Source/RequestedAt/Latency/Failed/Fail/Detail`）。**放弃 request_id keyed UsageCollector**。改用方案：HoldMiddleware 把 `SettleCtx{RequestID, UserID, RateMult, SubscriptionID}` 注入 ctx → SDK 调 `manager.Execute(ctx, ...)` → executor 读 ctx 填 `usage.Record.Detail` 并调 `usage.PublishRecord(ctx, rec)` → `UsagePlugin.HandleUsage(ctx, rec)` 从同一 ctx 读 SettleCtx。
3. **`access.Result` 结构确认**: 仅有 `Provider`/`Principal`/`Metadata map[string]string` — Metadata 只能放 string。subscription 数值（daily_used/limit 等）需序列化为 string 或 JSON 字符串。
4. **`WithRouterConfigurator` 三元组签名**: `func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)` 已核查。api/ 包的 `RegisterPanelRoutes` 返回此签名的 closure，后两个参数用 `_` 忽略（业务路由不需要 SDK 的 BaseAPIHandler 和 SDK 自己的 Config）。
5. **`usage.Plugin` 接口仅一个方法**: `HandleUsage(ctx context.Context, record Record)` — 注册后 SDK 在每次 Record publish 时同步（default manager 为非阻塞 channel，plugin 写 DB 时应控制耗时）。

---

## Work Objectives

### Core Objective
将 CPA-Gateway 从手动 Gin HTTP 服务器迁移到 SDK Builder 模式，同时实现精确的 Hold/Settle 计费闭环和 provider 级 token 解析，并补全订阅配额周期 reset 逻辑。

### Concrete Deliverables
- 8-9 个新 Go package（`infra/`, `model/`, `ledger/`, `pricing/`, `sdk/`, `executor/`, `api/`, `authutil/`，可选 `config/`）
- 3 个 SDK 接口实现文件（`sdk/access.go`, `sdk/usage.go`, `sdk/holdmw.go`）+ `sdk/settlectx.go` + `sdk/interfaces.go`
- 5 个 executor 的 `usage_parser.go`（OpenAI/Claude/Gemini/Codex/Vertex）+ 每个 executor 在 Execute/ExecuteStream 中调 `usage.PublishRecord`
- 1 个 `pricing.Calculator`（`Estimate + Compute + TokensFromMetadata`）+ `ModelPriceCache`
- `Subscription` 模型增加 `DailyResetAt/WeeklyResetAt/MonthlyResetAt` 字段 + HoldMiddleware 内的 reset helper
- Go 测试文件覆盖核心模块（ledger, pricing, sdk/holdmw, sdk/access, sdk/usage, executor/usage_parser），使用 `miniredis`
- `SeedModelPrices` 脚本（12+ 真实模型预设价格，注释标注为占位值）
- 重写的 `main.go`（~100 行 Builder 组装，含 `svc.RegisterUsagePlugin`）
- 删除的代码：旧 `ProxyChatHandler`、`ProxyModelsHandler`、`BillingMiddleware`、`calculateProxyUsage`、`openAIUsageTokens`、`approximateTokensFromBytes`、`executeProxyNonStream`、`executeProxyStream`（~700 行）

### Definition of Done
- [ ] `go build ./...` 编译通过
- [ ] `go test ./...` 全部通过（≥15 个测试用例）
- [ ] 所有 `/v1/*` 路由由 SDK 内置 handler 处理（不再经过 `ProxyChatHandler`）
- [ ] 所有 `/api/panel/**` 路由正常工作
- [ ] Hold/Release 正常工作（预扣成功 2xx 续留，失败 4xx 释放）
- [ ] Settle 精确使用 ModelPrice 四列定价而非 flat price
- [ ] 流式请求的 usage 来自 executor 解析并通过 `usage.PublishRecord` 上报
- [ ] 订阅日/周/月配额预检、累扣、周期 reset 正常工作
- [ ] `ledger_test.go`、`pricing/calculator_test.go`、`sdk/holdmw_test.go`、`sdk/usage_test.go`、`executor/usage_parser_test.go` 全部通过

### Must Have
- SDK gin engine 接管 `/v1/*` 路由
- Hold → Settle/Release 闭环（借助 Ledger 现有 API，签名不变）
- 按 ModelPrice 表的四列定价（input/output/cached/reasoning）
- Executor 级 token 解析 + 主动调 `usage.PublishRecord`
- 订阅配额管控（日/周/月预检 + 累扣 + 周期 reset）
- Go 测试覆盖核心模块（使用 miniredis 作为 Redis mock）

### Must NOT Have (Guardrails)
- 不修改 Ledger 的 Hold/Settle/Release API 签名
- 不修改 `User/ApiKey/Group/UsageLog/ModelPrice` 的 GORM 模型定义（**`Subscription` 允许追加 reset 字段**，因业务需要）
- 不修改 executor 的 `Identifier()` 和执行语义（只增加 usage 解析 + `usage.PublishRecord` 调用）
- 不修改 `/api/panel/**` 的业务 handler 逻辑
- 不修改前端代码
- 不引入 production 新外部依赖（test-only 依赖 `miniredis` 允许）
- 不创建 `sdk/` 之外的包的 SDK 耦合（`model/`, `infra/`, `api/` 不 import cliproxy SDK 相关包，executor/ 只 import `cliproxy/auth` + `cliproxy/executor` + `cliproxy/usage`）
- 过渡期间每一步必须可编译（`go build ./...` 通过）
- 不做双写（hard cut）
- 不使用 request_id keyed 全局 UsageCollector（v2 已放弃，改用 ctx value）

---

## Verification Strategy

> **ZERO HUMAN INTERVENTION** — ALL verification is agent-executed.

### Test Decision
- **Infrastructure exists**: NO (Go backend zero test files)
- **Automated tests**: TDD (RED → GREEN → REFACTOR)
- **Framework**: `go test` + 标准库 `testing` + `github.com/alicebob/miniredis/v2`（test-only）
- **Test setup task**: Task 2 创建 `Makefile test` target，引入 miniredis，验证 `go test ./...` 运行通过

### QA Policy
Every task MUST include agent-executed QA scenarios. Evidence saved to `.sisyphus/evidence/task-{N}-{scenario-slug}.{ext}`.

- **API endpoints**: Use bash (curl) — Send requests, assert status + response fields
- **Go tests**: Use bash (`go test`) — Run specific test, assert PASS
- **Build verification**: Use bash (`go build`) — Assert exit 0
- **Database verification**: Use bash (psql) — Query tables, assert data

---

## Execution Strategy

### Parallel Execution Waves

```
Wave 1 (Start Immediately — 拆包 + 测试基础设施):
├── Task 1:  创建项目骨架 (目录 + doc.go)                [sequential first]
├── Task 2:  Go 测试基础设施搭建 (miniredis + Makefile) [parallel after T1]
├── Task 3:  model/ 包 + Subscription 加 reset 字段      [parallel after T1]
├── Task 4:  infra/ 包 (db + redis + cache)              [parallel after T1,T3]
├── Task 5:  ledger/ 包 (删除旧 ledger.go)               [parallel after T1,T3]
├── Task 7:  ModelPrice seed (12+ 真实 SKU)              [parallel after T1,T3]
├── Task 6a: authutil/ 包 (+ 可选 config/ 包)            [parallel after T1,T3]
├── Task 6b: executor/ 包迁移 (5 个 executor + util)     [parallel after T1,T3]
└── Task 6c: api/ 包 + PanelRouter + 8 handler 迁移     [sequential after T3,T4,T5,T6a]

Wave 2 (After Wave 1 — SDK 接口 + pricing + usage_parser):
├── Task 8:  pricing/calculator.go (Estimate + Compute) [TDD]
├── Task 9:  pricing/cache.go (ModelPriceCache)
├── Task 10: sdk/store.go (auth.Store wrapper)
├── Task 11: sdk/access.go + settlectx.go              [TDD, SettleCtx helpers]
├── Task 12: sdk/holdmw.go                             [TDD, 注入 SettleCtx + 配额 reset]
├── Task 13: executor/usage_parser.go (5 provider)     [TDD]
└── Task 14: sdk/usage.go + sdk/interfaces.go          [TDD, 从 ctx 读 SettleCtx]

Wave 3 (After Wave 2 — executor usage.PublishRecord 集成):
├── Task 15: executor/openai.go  调 usage.PublishRecord
├── Task 16: executor/claude.go  调 usage.PublishRecord
├── Task 17: executor/gemini.go  调 usage.PublishRecord
├── Task 18: executor/codex.go   调 usage.PublishRecord
└── Task 19: executor/vertex.go  调 usage.PublishRecord

Wave 4 (After Wave 3 — main.go 切 Builder + 清理):
├── Task 20: main.go Builder 模式 (svc.RegisterUsagePlugin)
└── Task 21: 删除旧代码 + 路由验证

Wave 5 (FINAL — 审查):
├── F1: Plan Compliance Audit     [oracle]
├── F2: Code Quality Review       [unspecified-high]
├── F3: Real Manual QA            [unspecified-high]
└── F4: Scope Fidelity Check      [deep]

Critical Path: T1 → T3 → T4,T5,T6a → T6b,T6c → T8,T11,T12,T14 → T13 → T15-T19 → T20 → T21 → F1-F4
```

### Agent Dispatch Summary

- **Wave 1**: **9** — T1/T2/T3/T4/T5/T6a/T6b/T7 → `quick` × 8, T6c → `unspecified-high`
- **Wave 2**: **7** — T9/T10 → `quick`, T8/T11/T12/T14 → `unspecified-high`, T13 → `deep`
- **Wave 3**: **5** — T15-T19 → `quick` × 5
- **Wave 4**: **2** — T20 → `deep`, T21 → `quick`
- **Wave 5 (FINAL)**: **4** — F1 → `oracle`, F2 → `unspecified-high`, F3 → `unspecified-high`, F4 → `deep`

---

## TODOs

- [x] 1. 创建项目骨架 (目录 + package 声明)

  **What to do**:
  - 创建以下目录结构：
    ```
    infra/     — package infra    (db.go, redis.go, cache.go 后续移入)
    model/     — package model    (所有 gorm 模型后续移入)
    ledger/    — package ledger   (ledger.go 后续移入)
    pricing/   — package pricing  (calculator.go, cache.go 新建)
    sdk/       — package sdk      (store/access/usage/holdmw/settlectx/interfaces 新建)
    executor/  — package executor (5 个 executor + util + usage_parser)
    api/       — package api      (routes + 8 handler + middleware)
    authutil/  — package authutil (GenerateJWT/ValidateJWT/HashAPIKey)
    ```
  - 每个目录创建 `doc.go` 文件，声明 package 名 + 一句话注释
  - 不移动任何现有文件
  - `go build ./...` 必须编译通过（新包为空，不影响 main）

  **Must NOT do**:
  - 不要移动或修改现有 `.go` 文件
  - 不要修改 `go.mod`

  **Recommended Agent Profile**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO（必须先于所有拆包任务）
  - **Parallel Group**: Wave 1 — sequential first
  - **Blocks**: Tasks 2-7, 6a, 6b, 6c
  - **Blocked By**: None

  **References**:
  - `go.mod:1` — module: `github.com/xxww0098/cpa-gateway`
  - `main.go:36-82` — 现 run() 函数

  **Acceptance Criteria**:
  - [x] `ls infra/ model/ ledger/ pricing/ sdk/ executor/ api/ authutil/` 均存在
  - [x] 每个目录有 `doc.go` 声明正确 package 名
  - [x] `go build ./...` 编译通过（exit 0）

  **QA Scenarios**:
  ```
  Scenario: 目录结构 + 编译验证
    Tool: Bash
    Steps:
      1. for d in infra model ledger pricing sdk executor api authutil; do head -1 $d/doc.go; done
      2. go build ./...
    Expected: 全部 "package X" 打印 + exit 0
    Evidence: .sisyphus/evidence/task-1-skeleton.txt
  ```

  **Commit**: YES — `chore: create project package skeleton (infra/model/ledger/pricing/sdk/executor/api/authutil)`

- [x] 2. Go 测试基础设施搭建（miniredis + Makefile）

  **What to do**:
  - `go get github.com/alicebob/miniredis/v2@latest`（test-only，由 `go mod tidy` 放入 go.sum）
  - 在 `Makefile` 增加：
    - `test: go test ./... -count=1 -timeout 30s`
    - `test-verbose: go test -v ./...`
    - `test-race: go test -race ./... -count=1`
  - 创建 `ledger/ledger_test.go` 示例测试（此时 ledger 包仍为空 doc.go，测试文件只有一个占位 `TestPlaceholder`，证明测试框架能跑；Task 5 会填充真实测试）
  - 为 `miniredis` 使用创建一个共享辅助：`testutil/redis.go`（或在 ledger_test.go 内就地写），提供：
    ```go
    func MustMiniRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis)
    ```
  - `go test ./...` 必须 exit 0

  **Must NOT do**:
  - 不引入 testify/ginkgo/gomega（标准 `testing` 足够）
  - `miniredis` 只在 `_test.go` 文件 import，production binary 不包含

  **Recommended Agent Profile**: `quick`

  **Parallelization**: YES / Wave 1 / Blocks: T5, T8, T11, T12, T13, T14 (TDD 任务) / Blocked By: T1

  **References**:
  - `Makefile` — 现有 targets
  - `go.mod` — 准备追加 miniredis（test-only）

  **Acceptance Criteria**:
  - [ ] `make test` 执行 `go test ./... -count=1 -timeout 30s`
  - [ ] `go.mod` 包含 `github.com/alicebob/miniredis/v2`
  - [ ] `go test ./ledger/` 有 ≥1 个测试通过
  - [ ] `go build ./...` 不带 miniredis 符号（`go build . && go tool nm cpa-gateway | grep miniredis` 应返回空）

  **QA Scenarios**:
  ```
  Scenario: 测试基础设施可运行
    Tool: Bash
    Steps:
      1. make test 2>&1
      2. go mod tidy && grep miniredis go.mod
    Expected: exit 0, miniredis 在 go.mod require 块
    Evidence: .sisyphus/evidence/task-2-test-infra.txt
  ```
  ```
  Scenario: production binary 不包含 miniredis
    Tool: Bash
    Steps:
      1. go build -o /tmp/cpa-gw .
      2. go tool nm /tmp/cpa-gw | grep -c miniredis || echo "0"
    Expected: 0
    Evidence: .sisyphus/evidence/task-2-prod-binary.txt
  ```

  **Commit**: YES — `test: add Go test infrastructure (miniredis + Makefile targets + ledger_test.go skeleton)`

- [x] 3. 拆包 model/ + Subscription 加 reset 字段

  **What to do**:
  - 将 `db.go` 中所有 GORM 模型移入 `model/`：
    - `model/user.go`: `User`, `ApiKey`, `Group`
    - `model/billing.go`: `BalanceLog`, `UsageLog`
    - `model/subscription.go`: `SubscriptionPackage`, `Subscription` **+ 新增字段**
    - `model/ticket.go`: `Ticket`, `TicketReply`
    - `model/catalog.go`: `ModelPrice`, `ModelCatalogEntry`
    - `model/sdk.go`: `AuthRecord`, `ProviderConfig`, `OAuthSession`, `AmpcodeConfig`
  - **Subscription 新字段**（与现有 `DailyUsageUSD/WeeklyUsageUSD/MonthlyUsageUSD` 配对）：
    ```go
    DailyResetAt   time.Time `gorm:"index"`  // 下次日配额重置时间（UTC）
    WeeklyResetAt  time.Time `gorm:"index"`  // 下次周配额重置时间（UTC 周一 0:00）
    MonthlyResetAt time.Time `gorm:"index"`  // 下次月配额重置时间（UTC 1 号 0:00）
    ```
  - `model/` 包不 import 外部库（仅标准库 + `time`）
  - 更新所有引用旧类型的文件：加 `model.` 前缀（`store.go`, `handler_*.go`, `ledger.go`, `middleware.go`, `auth.go`, `seed.go`, `db.go`）
  - `AutoMigrate` 调用会自动给 Subscription 加新列（Postgres 加列零停机）

  **Must NOT do**:
  - 不修改 `User/ApiKey/Group/UsageLog/ModelPrice` 的字段定义或 tag
  - 不修改 `TableName` 方法
  - 不修改 `AutoMigrate` 注册顺序（但需增加新字段生效，`AutoMigrate(&model.Subscription{})` 已有）

  **Recommended Agent Profile**: `quick`

  **Parallelization**: YES / Wave 1 / Blocks: Tasks 4-21 / Blocked By: T1

  **References**:
  - `db.go:67-220` — 所有模型定义
  - `store.go:25-45` — AuthRecord
  - `seed.go:1-115` — Subscription seed 用来初始化 ResetAt（把 package 创建时间作为首个 reset 基准）

  **Acceptance Criteria**:
  - [ ] `model/` 各文件按归类正确
  - [ ] `model/subscription.go` 含 `DailyResetAt/WeeklyResetAt/MonthlyResetAt` 三个 time.Time 字段
  - [ ] 所有引用旧类型的地方已更新为 `model.TypeName`
  - [ ] `go build ./...` 编译通过
  - [ ] `grep -r "gorm.io\|github.com" model/` 输出为空（除 `model/sdk.go` 可能因 `json` 标准库除外）

  **QA Scenarios**:
  ```
  Scenario: model 包拆分完整 + 新字段存在
    Tool: Bash
    Steps:
      1. go build ./...
      2. grep -l "DailyResetAt\|WeeklyResetAt\|MonthlyResetAt" model/subscription.go
      3. grep -r "gorm.io\|github.com" model/ | grep -v "_test.go" || echo "CLEAN"
    Expected: exit 0, subscription.go 匹配, "CLEAN"
    Evidence: .sisyphus/evidence/task-3-model.txt
  ```

  **Commit**: YES — `refactor: extract GORM models to model/ package + add Subscription reset fields`

- [x] 4. 拆包 infra/ (db + redis + cache)

  **What to do**:
  - `infra/db.go`: 移动 `InitDB` + `AutoMigrate`（源：`db.go`），接收 `*config.Config`（或现有 `*Config`），依赖 `model` 包
  - `infra/redis.go`: 移动 `initRedis`（源：`main.go`），改为导出 `InitRedis`
  - `infra/cache.go`: 提取 API key 缓存，改为：
    ```go
    type APIKeyCache struct { ... }
    func NewAPIKeyCache() *APIKeyCache
    func (c *APIKeyCache) Get(hash string) (*CachedKey, bool)
    func (c *APIKeyCache) Set(hash string, v *CachedKey)
    func (c *APIKeyCache) Start(ctx context.Context)  // 后台 cleanup
    ```
  - `infra` 包依赖 `model` 包 + 外部库（gorm, redis）
  - 更新 `main.go` + `auth.go`（过渡期 auth.go 仍调用 infra.APIKeyCache 的方法，完整迁移在 Task 6c 完成）

  **Must NOT do**:
  - 不修改 `InitDB/AutoMigrate/initRedis` 的逻辑
  - 不修改 `AutoMigrate` 类型列表

  **Recommended Agent Profile**: `quick`

  **Parallelization**: YES / Wave 1 / Blocks: T20 / Blocked By: T1, T3

  **References**:
  - `db.go:16-61`, `main.go:84-109`, `auth.go:160-252`

  **Acceptance Criteria**:
  - [ ] `infra/db.go` 有 `InitDB`, `AutoMigrate`
  - [ ] `infra/redis.go` 有 `InitRedis`
  - [ ] `infra/cache.go` 有 `APIKeyCache` 结构体 + 方法
  - [ ] `go build ./...` exit 0

  **QA Scenarios**:
  ```
  Scenario: infra 拆包编译
    Tool: Bash
    Steps: go build ./...
    Expected: exit 0
    Evidence: .sisyphus/evidence/task-4-build.txt
  ```

  **Commit**: YES — `refactor: extract infra/ package (db, redis, cache) from main/db/auth`

- [x] 5. 拆包 ledger/ (删除旧 ledger.go)

  **What to do**:
  - 把现有 `ledger.go` **整体移动**到 `ledger/ledger.go`，改为 `package ledger`
  - 方法签名保持不变：`Hold/Settle/Release/GetBalance/Credit/Debit`
  - 导出错误：`ErrInsufficientBalance`, `ErrUserNotFound`（从 `errors.go` 迁移或在 ledger 包内重新定义）
  - **删除 root 的 `ledger.go` 文件**（不是留空或写废弃注释，直接 `rm`）
  - 更新 `main.go` / `middleware.go` / `handler_proxy.go`：`GlobalLedger *Ledger` → `GlobalLedger *ledger.Ledger`，`NewLedger` → `ledger.New`
  - **补充 TDD 测试**（使用 miniredis）：`ledger/ledger_test.go` 至少 5 个测试：
    - `TestHold_Success` — miniredis SET NX 成功
    - `TestHold_InsufficientBalance` — GetBalance 返回不足
    - `TestHold_DuplicateRequestID` — SET NX 冲突
    - `TestSettle_SuccessDebits` — SET NX Del + Debit 通过
    - `TestRelease_DelHoldKey` — Del 成功，balance 不变

  **Must NOT do**:
  - 不修改 Ledger 方法签名或 Redis key pattern
  - 不留僵尸文件

  **Recommended Agent Profile**: `quick`

  **Parallelization**: YES / Wave 1 / Blocks: T8, T11, T12, T14 / Blocked By: T1, T2, T3

  **References**:
  - `ledger.go:1-216` — 完整源
  - `errors.go:1-14` — ErrInsufficientBalance, ErrUserNotFound
  - `middleware.go:151`, `handler_proxy.go:434,461,465,483` — 使用点

  **Acceptance Criteria**:
  - [ ] `ledger/ledger.go` 完整实现 + `ledger/ledger_test.go` 5+ 测试
  - [ ] Root `ledger.go` 已 `rm`
  - [ ] `go test ./ledger/ -v -count=1` PASS
  - [ ] `go build ./...` exit 0

  **QA Scenarios**:
  ```
  Scenario: ledger 包迁移 + 测试通过
    Tool: Bash
    Steps:
      1. ls ledger.go 2>&1 | grep -q "No such" && echo "CLEAN"
      2. go test ./ledger/ -v -count=1
    Expected: "CLEAN" + all tests PASS
    Evidence: .sisyphus/evidence/task-5-ledger.txt
  ```

  **Commit**: YES — `refactor: extract ledger package + add miniredis-based tests (delete old ledger.go)`

- [x] 6a. 拆包 authutil/ (JWT + hash helpers)

  **What to do**:
  - 创建 `authutil/authutil.go`，导出：
    - `GenerateJWT(userID uint, email string, secret string) (string, error)`
    - `ValidateJWT(tokenString string, secret string) (*Claims, error)`
    - `HashAPIKey(key string) string`
    - `NewAPIKeyPrefix() string`
    - `Claims` 结构体（导出字段）
  - 纯函数，仅依赖标准库 + `github.com/golang-jwt/jwt/v5`
  - 更新 `auth.go` 中对应函数改为调用 `authutil.XXX`（或直接删除 auth.go 中的实现，改为 thin wrapper）

  **Must NOT do**: 不修改 JWT 签名逻辑

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 1 / Blocks: T6c / Blocked By: T1, T3
  **Acceptance Criteria**:
  - [ ] `authutil/authutil.go` 包含 4 个导出函数 + Claims
  - [ ] `go build ./...` exit 0

  **Commit**: YES — `refactor: extract authutil/ package (JWT + hash helpers)`

- [x] 6b. 拆包 executor/ (5 个 executor + util)

  **What to do**:
  - 将 `executor_openai.go`, `executor_claude.go`, `executor_gemini.go`, `executor_codex.go`, `executor_vertex.go` 移入 `executor/` 包
  - 改为 `package executor`，导出类型名（如 `OpenAICompatibleExecutor`）
  - executor 不依赖全局变量（已接收 cfg 参数），直接搬
  - 保留 `executor/util.go` 放共享 helper（如 `sanitizedProxyHeaders`, `copyOutboundHeaders`）

  **Must NOT do**: 不修改 executor 执行逻辑

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 1 / Blocks: T13, T15-T19 / Blocked By: T1, T3
  **Acceptance Criteria**:
  - [ ] `executor/` 下 5 个 executor 文件 + util.go
  - [ ] `go build ./executor/` exit 0
  - [ ] Root 下旧 `executor_*.go` 已删除

  **Commit**: YES — `refactor: move executor files to executor/ package`

- [ ] 6c. 拆包 api/ + PanelRouter 依赖注入

  **What to do**:
  - 创建 `api/router.go`：
    ```go
    type PanelRouter struct {
        DB     *gorm.DB
        Redis  *redis.Client
        Ledger *ledger.Ledger
        Calc   *pricing.Calculator
        Config *Config
        Auth   *authutil.Claims // 或直接存 secret string
    }
    func NewPanelRouter(...) *PanelRouter
    func (pr *PanelRouter) RegisterPanelRoutes(r gin.IRouter)
    ```
  - 创建 `api/middleware.go`：移动 TraceID, Metrics, RateLimit middleware
  - 迁移 8 个 handler 文件到 `api/`，改为 PanelRouter 方法
  - 所有 `GlobalDB` → `pr.DB`，`GlobalConfig` → `pr.Config`，`GlobalLedger` → `pr.Ledger`
  - `GenerateJWT(...)` → `authutil.GenerateJWT(..., pr.Config.Auth.JWT.Secret)`
  - `isAdminEmail(email)` → 内联 `slices.Contains(pr.Config.Auth.AdminEmails, email)`
  - 导出 `RegisterPanelRoutes` 返回 `func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)` 签名的 closure（后两参数 `_` 忽略）

  **Must NOT do**:
  - 不修改 handler 业务逻辑
  - api/ 不 import cliproxy SDK（除了 `WithRouterConfigurator` 签名中的类型，通过 type alias 或直接在 main.go 包装）
  - 不保留 `GlobalDB/GlobalConfig/GlobalLedger` 引用

  **Recommended Agent Profile**: `unspecified-high`
  **Parallelization**: NO / Wave 1 sequential after T3,T4,T5,T6a / Blocks: T10-T14, T20 / Blocked By: T3,T4,T5,T6a
  **Acceptance Criteria**:
  - [ ] `api/` 下无 `GlobalDB/GlobalConfig/GlobalLedger` 引用
  - [ ] `go build ./...` exit 0
  - [ ] `go vet ./...` exit 0（无循环 import）

  **QA Scenarios**:
  ```
  Scenario: 全量编译 + vet
    Tool: Bash
    Steps:
      1. go build ./...
      2. go vet ./...
      3. grep -rn "GlobalDB\|GlobalConfig\|GlobalLedger" api/ || echo "CLEAN"
    Expected: exit 0 + "CLEAN"
    Evidence: .sisyphus/evidence/task-6c-build.txt
  ```

  **Commit**: YES — `refactor: extract api/ package with PanelRouter dependency injection`

- [x] 7. ModelPrice seed (12+ 真实 SKU)

  **What to do**:
  - 在 `seed.go` 添加 `SeedModelPrices(db *gorm.DB) error`
  - 使用 `clause.OnConflict{DoNothing: true}` 幂等
  - 预设价格（单位：USD/1M tokens，**占位值，生产需按官方定价对齐**）：
    ```
    gpt-4o              → input: 2.50, output: 10.00, cached: 1.25, reasoning: 0
    gpt-4o-mini         → input: 0.15, output: 0.60,  cached: 0.075, reasoning: 0
    o3                  → input: 10.00, output: 40.00, cached: 2.50, reasoning: 60.00
    o3-mini             → input: 1.10, output: 4.40,  cached: 0.55, reasoning: 4.40
    o4-mini             → input: 1.10, output: 4.40,  cached: 0.55, reasoning: 4.40
    claude-sonnet-4-20250514 → input: 3.00, output: 15.00, cached: 0.30, reasoning: 0
    claude-opus-4-20250514   → input: 15.00, output: 75.00, cached: 1.50, reasoning: 0
    claude-haiku-3-5-20241022 → input: 0.80, output: 4.00, cached: 0.08, reasoning: 0
    gemini-2.5-pro     → input: 1.25, output: 10.00, cached: 0.3125, reasoning: 0
    gemini-2.5-flash   → input: 0.15, output: 0.60,  cached: 0.0375, reasoning: 0.35
    codex-mini         → input: 1.50, output: 6.00,  cached: 0.375, reasoning: 0
    vertex-gemini-2.5-pro → input: 1.25, output: 10.00, cached: 0.3125, reasoning: 0
    ```
  - 在 `main.go` 初始化流程中 `AutoMigrate` 之后调用

  **Must NOT do**: 不覆盖已存在记录（OnConflict DoNothing）

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 1 / Blocks: T8 / Blocked By: T1, T3
  **Acceptance Criteria**:
  - [ ] `seed.go` 含 `SeedModelPrices`，≥12 条记录
  - [ ] `go build ./...` exit 0

  **Commit**: YES — `feat: add ModelPrice seed (12+ models, placeholder pricing)`

- [ ] 8. pricing/calculator.go (Estimate + Compute) — TDD

  **What to do**:
  **RED phase** — `pricing/calculator_test.go`：
  - `TestEstimate_NonStream`: 已知 model → 返回 estimatedCost > 0
  - `TestEstimate_Stream`: stream=true → 估算值更大
  - `TestEstimate_UnknownModel`: 未知 model → 回退默认价格
  - `TestCompute_AllTokenTypes`: 四列分别计算
  - `TestCompute_RateMult`: rateMult=2.0 → cost 翻倍
  - `TestCompute_ZeroTokens`: 全零 → cost=0

  **GREEN phase** — `pricing/calculator.go`：
  ```go
  type Calculator struct { cache *ModelPriceCache; defaultPrice float64 }
  type UsageTokens struct { Input, Output, Cached, Reasoning int64 }
  func NewCalculator(cache *ModelPriceCache, defaultPricePer1K float64) *Calculator
  func (c *Calculator) Estimate(model string, stream bool, rateMult float64) float64
  func (c *Calculator) Compute(model string, tokens UsageTokens, rateMult float64) float64
  ```
  - `Estimate`: 从 cache 查 ModelPrice → 按 input+output 估算（stream 时 2x）
  - `Compute`: `(inputPrice*input + outputPrice*output + cachedPrice*cached + reasoningPrice*reasoning) / 1_000_000 * rateMult`
  - 未知 model 回退 `defaultPrice`

  **Must NOT do**: 不 import SDK

  **Recommended Agent Profile**: `unspecified-high`
  **Parallelization**: YES / Wave 2 / Blocks: T11, T12, T14 / Blocked By: T3, T7, T9
  **Acceptance Criteria**:
  - [ ] `pricing/calculator_test.go` ≥6 测试
  - [ ] `go test ./pricing/ -v -count=1` PASS

  **Commit**: YES — `feat: add pricing/calculator (Estimate + Compute, per-token-type pricing)`

- [ ] 9. pricing/cache.go (ModelPriceCache)

  **What to do**:
  ```go
  type ModelPriceCache struct { mu sync.RWMutex; items map[string]*model.ModelPrice }
  func NewModelPriceCache(db *gorm.DB) (*ModelPriceCache, error)  // 启动全量加载
  func (c *ModelPriceCache) Get(modelID string) (*model.ModelPrice, bool)
  func (c *ModelPriceCache) Invalidate(db *gorm.DB) error
  func (c *ModelPriceCache) List() []*model.ModelPrice
  ```
  - `sync.RWMutex` 并发安全
  - 不做定时刷新（只在 admin 变更后调 Invalidate）

  **Must NOT do**: 不 import SDK

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 2 / Blocks: T8 / Blocked By: T1, T3
  **Acceptance Criteria**:
  - [ ] `go build ./pricing/` exit 0
  - [ ] Get 用 RLock，Invalidate 用 Lock

  **Commit**: YES — `feat: add pricing/cache (in-memory ModelPrice cache with RWMutex)`

- [ ] 10. sdk/store.go (auth.Store wrapper)

  **What to do**:
  - 创建 `sdk/store.go`：复用现有 `store.go` 的 `PostgresAuthStore` 逻辑
  - 构造：`func NewStore(db *gorm.DB) cliproxyauth.Store`
  - 实现 `cliproxyauth.Store` 接口三个方法（List/Save/Delete）

  **Must NOT do**: 不修改 AuthRecord 模型

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 2 / Blocks: T20 / Blocked By: T1, T3
  **Acceptance Criteria**:
  - [ ] `sdk/store.go` 实现 `cliproxyauth.Store`
  - [ ] `go build ./sdk/` exit 0

  **Commit**: YES — `feat: add sdk/store.go (auth.Store wrapper)`

- [ ] 11. sdk/access.go + sdk/settlectx.go — TDD

  **What to do**:
  **Step 1 — `sdk/settlectx.go`**（SettleCtx 定义 + ctx helpers）：
  ```go
  type SettleCtx struct {
      RequestID      string
      UserID         uint
      RateMult       float64
      SubscriptionID *uint
      Model          string
      Stream         bool
  }
  type settleCtxKey struct{}
  func WithSettleCtx(ctx context.Context, sc *SettleCtx) context.Context
  func SettleCtxFromContext(ctx context.Context) (*SettleCtx, bool)
  ```

  **Step 2 — TDD `sdk/access_test.go`**：
  - `TestAuthenticate_APIKey`: Bearer cpa-xxx → Result{Principal=userID}
  - `TestAuthenticate_JWT`: Bearer jwt → Result
  - `TestAuthenticate_MissingToken`: → AuthError NoCredentials
  - `TestAuthenticate_InvalidKey`: → AuthError InvalidCredential
  - `TestAuthenticate_SubscriptionLoaded`: 有活跃订阅 → Metadata 含 subscription_id

  **Step 3 — `sdk/access.go`**：
  ```go
  type AccessProvider struct {
      db          *gorm.DB
      redis       *redis.Client
      apiKeyCache *infra.APIKeyCache
      jwtSecret   string
  }
  func NewAccessProvider(db, redis, cache, jwtSecret) *AccessProvider
  func (p *AccessProvider) Identifier() string { return "cpa-tenant" }
  func (p *AccessProvider) Authenticate(ctx context.Context, r *http.Request) (*access.Result, *access.AuthError)
  ```
  - 解析 Bearer → cpa- 前缀走 API key 验证 → 否则走 JWT
  - 查 Subscription（status=active, expires_at > now）
  - 组装 `access.Result{Provider:"cpa-tenant", Principal:strconv(userID), Metadata:map[string]string{...}}`
  - Metadata 含：`user_id`, `api_key_id`, `group_id`, `rate_mult`, `subscription_id`, `daily_limit`, `daily_used`, `weekly_limit`, `weekly_used`, `monthly_limit`, `monthly_used`

  **Must NOT do**: 不在 access.go 中做 Hold 逻辑

  **Recommended Agent Profile**: `unspecified-high`
  **Parallelization**: YES / Wave 2 / Blocks: T12, T14, T20 / Blocked By: T3, T4, T6a
  **Acceptance Criteria**:
  - [ ] `sdk/settlectx.go` 有 WithSettleCtx + SettleCtxFromContext
  - [ ] `sdk/access_test.go` ≥4 测试 PASS
  - [ ] `go test ./sdk/ -run TestAuthenticate -v` PASS

  **Commit**: YES — `feat: add sdk/access.go (access.Provider) + sdk/settlectx.go`

- [ ] 12. sdk/holdmw.go — TDD

  **What to do**:
  **RED phase** — `sdk/holdmw_test.go`（使用 mock 接口）：
  - `TestHold_SufficientBalance`: → c.Next() 被调用, ctx 含 SettleCtx
  - `TestHold_InsufficientBalance`: → 402
  - `TestHold_SubscriptionDailyQuotaExceeded`: → 402
  - `TestHold_SubscriptionQuotaReset`: 过期 → reset 后通过
  - `TestHold_ReleaseOnUpstreamError`: 非 2xx → Release 调用
  - `TestHold_NonV1Path`: → 直接 c.Next()

  **GREEN phase** — `sdk/holdmw.go`：
  ```go
  type HoldMiddleware struct {
      ledger BillingLedger
      calc   PricingCalculator
      db     *gorm.DB
      ttl    time.Duration
  }
  func NewHoldMiddleware(ledger, calc, db, ttl) *HoldMiddleware
  func (m *HoldMiddleware) Handle(c *gin.Context)
  ```
  - 路径判断：非 `/v1/` 前缀 → 跳过
  - 从 `access.Result` 的 Metadata 解析 user_id/rate_mult/subscription_id
  - **订阅配额 reset 逻辑**：
    ```go
    if sub != nil && now.After(sub.DailyResetAt) {
        sub.DailyUsageUSD = 0
        sub.DailyResetAt = nextMidnightUTC(now)
        db.Save(sub)
    }
    // 同理 weekly/monthly
    ```
  - 配额预检：`sub.DailyUsageUSD + estimatedCost > sub.DailyLimitUSD` → 402
  - `estimatedCost = calc.Estimate(model, isStream, rateMult)`
  - `ledger.Hold(ctx, userID, estimatedCost, requestID, ttl)` → 失败 402
  - 注入 `SettleCtx` 到 ctx（`c.Request = c.Request.WithContext(sdk.WithSettleCtx(ctx, sc))`）
  - `c.Next()`
  - `defer`: `c.Writer.Status() >= 400` → `ledger.Release`

  **Must NOT do**: 不修改 Ledger API 签名

  **Recommended Agent Profile**: `unspecified-high`
  **Parallelization**: YES / Wave 2 / Blocks: T20 / Blocked By: T5, T8, T11
  **Acceptance Criteria**:
  - [ ] `sdk/holdmw_test.go` ≥5 测试 PASS
  - [ ] `go test ./sdk/ -run TestHold -v` PASS
  - [ ] 配额 reset 逻辑覆盖

  **Commit**: YES — `feat: add sdk/holdmw.go (HoldMiddleware + subscription quota reset)`

- [ ] 13. executor/usage_parser.go (5 provider) — TDD

  **What to do**:
  **RED phase** — `executor/usage_parser_test.go`（每 provider ≥2 测试）：
  - `TestParseOpenAIUsage_Full`: prompt_tokens + completion_tokens + cached + reasoning
  - `TestParseOpenAIUsage_Empty`: 空 payload → false
  - `TestParseClaudeUsage_MessageDelta`: output_tokens + input_tokens
  - `TestParseClaudeUsage_CacheCreation`: cache_creation_input_tokens → cached
  - `TestParseGeminiUsage_WithThoughts`: promptTokenCount + candidatesTokenCount + thoughtsTokenCount
  - `TestParseGeminiUsage_Empty`: → false
  - `TestParseCodexUsage_Body`: body 中 usage 字段
  - `TestParseVertexUsage`: usageMetadata 同 Gemini 格式
  - `TestParseOpenAIUsage_Reasoning`: completion_tokens_details.reasoning_tokens

  **GREEN phase** — `executor/usage_parser.go`：
  ```go
  func ParseOpenAIUsage(payload []byte) (UsageTokens, bool)
  func ParseClaudeUsage(payload []byte) (UsageTokens, bool)
  func ParseGeminiUsage(payload []byte) (UsageTokens, bool)
  func ParseCodexUsage(payload []byte) (UsageTokens, bool)
  func ParseVertexUsage(payload []byte) (UsageTokens, bool)
  ```
  - `UsageTokens` 复用 `pricing.UsageTokens`（或在 executor 包内定义等价结构，避免循环 import）
  - OpenAI: `usage.prompt_tokens` + `completion_tokens` + `prompt_tokens_details.cached_tokens` + `completion_tokens_details.reasoning_tokens`
  - Claude: `message_start.usage.input_tokens` + `message_delta.usage.output_tokens` + `cache_creation_input_tokens`
  - Gemini/Vertex: `usageMetadata{promptTokenCount, candidatesTokenCount, thoughtsTokenCount}`
  - Codex: body `usage` 字段或 response headers `x-usage-*`
  - 空/无效 payload → return zero, false（不 panic）

  **Must NOT do**: 不 import SDK（仅标准库 JSON 解析）

  **Recommended Agent Profile**: `deep`
  **Parallelization**: YES / Wave 2 / Blocks: T15-T19 / Blocked By: T1, T6b
  **Acceptance Criteria**:
  - [ ] `executor/usage_parser_test.go` ≥9 测试
  - [ ] `go test ./executor/ -run TestParse -v` PASS
  - [ ] 空 payload 不 panic

  **Commit**: YES — `feat: add executor/usage_parser.go (5-provider token parsing)`

- [ ] 14. sdk/usage.go + sdk/interfaces.go — TDD

  **What to do**:
  **Step 1 — `sdk/interfaces.go`**（mock 友好接口）：
  ```go
  type BillingLedger interface {
      Hold(ctx context.Context, userID uint, amount float64, requestID string, ttl time.Duration) error
      Settle(ctx context.Context, userID uint, requestID string, actualAmount float64) error
      Release(ctx context.Context, userID uint, requestID string) error
  }
  type PricingCalculator interface {
      Estimate(model string, stream bool, rateMult float64) float64
      Compute(model string, tokens pricing.UsageTokens, rateMult float64) float64
  }
  ```

  **Step 2 — TDD `sdk/usage_test.go`**（mock ledger + mock calc）：
  - `TestHandleUsage_SettleSuccess`: rec.Detail 有 tokens → Settle 调用 + UsageLog 写入
  - `TestHandleUsage_Failed`: rec.Failed=true → Release 调用
  - `TestHandleUsage_WithSubscription`: 有 subscription → 累扣 DailyUsageUSD
  - `TestHandleUsage_NoSettleCtx`: ctx 无 SettleCtx → no-op
  - `TestHandleUsage_ZeroTokens`: Detail 全零 → Settle(0) → 不 Debit

  **Step 3 — `sdk/usage.go`**：
  ```go
  type UsagePlugin struct {
      db     *gorm.DB
      ledger BillingLedger
      calc   PricingCalculator
  }
  func NewUsagePlugin(db, ledger, calc) *UsagePlugin
  func (p *UsagePlugin) HandleUsage(ctx context.Context, rec usage.Record)
  ```
  - 从 ctx 取 `SettleCtx`（`sdk.SettleCtxFromContext(ctx)`）→ 无则 return
  - tokens 从 `rec.Detail` 读取（`InputTokens/OutputTokens/CachedTokens/ReasoningTokens`）
  - `actualCost = calc.Compute(rec.Model, tokens, sc.RateMult)`
  - `rec.Failed` → `ledger.Release(ctx, sc.UserID, sc.RequestID)`
  - 非 Failed → `ledger.Settle(ctx, sc.UserID, sc.RequestID, actualCost)`
  - 写 `UsageLog` 到 DB（填充四类 token + cost + model + provider）
  - 有 subscription → `sub.DailyUsageUSD += actualCost; db.Save(sub)`
  - 错误处理：Settle 失败 log error 但不中断

  **Must NOT do**: 不做预扣（holdmw 负责）

  **Recommended Agent Profile**: `unspecified-high`
  **Parallelization**: YES / Wave 2 / Blocks: T15-T19, T20 / Blocked By: T5, T8, T11
  **Acceptance Criteria**:
  - [ ] `sdk/interfaces.go` 定义 BillingLedger + PricingCalculator
  - [ ] `sdk/usage_test.go` ≥5 测试 PASS
  - [ ] `go test ./sdk/ -run TestHandleUsage -v` PASS

  **Commit**: YES — `feat: add sdk/usage.go (usage.Plugin from ctx SettleCtx) + sdk/interfaces.go`

- [ ] 15. executor/openai.go 调 usage.PublishRecord

  **What to do**:
  - 在 `Execute` 方法中：读取 response payload 后调 `ParseOpenAIUsage(payload)` → 构造 `usage.Record{Detail: usage.Detail{...}}` → `usage.PublishRecord(ctx, rec)`
  - 在 `ExecuteStream` 方法中：在 goroutine 流结束后（close(chunks) 前），累积最后一个 SSE chunk 的 usage → `ParseOpenAIUsage(lastChunk)` → `usage.PublishRecord(ctx, rec)`
  - 新增 import: `cliproxyusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"`
  - `Response.Metadata["usage"]` 也填充（供非 SDK 路径使用，向后兼容）

  **Must NOT do**: 不修改 HTTP 请求/响应处理逻辑，不阻塞 stream

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 3 / Blocks: None / Blocked By: T13, T14
  **Acceptance Criteria**:
  - [ ] `go build ./executor/` exit 0
  - [ ] Execute 方法中有 `usage.PublishRecord` 调用

  **Commit**: YES — `feat: integrate OpenAI usage parsing + PublishRecord`

- [ ] 16. executor/claude.go 调 usage.PublishRecord

  **What to do**: 同 Task 15 模式。Claude SSE 流中累积 `message_start.usage.input_tokens` + `message_delta.usage.output_tokens`，流结束后 PublishRecord。

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 3 / Blocked By: T13, T14
  **Acceptance Criteria**: `go build ./executor/` exit 0

  **Commit**: YES — `feat: integrate Claude usage parsing + PublishRecord`

- [ ] 17. executor/gemini.go 调 usage.PublishRecord

  **What to do**: 同 Task 15 模式。Gemini `usageMetadata` 含 `thoughtsTokenCount` → reasoning。

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 3 / Blocked By: T13, T14
  **Acceptance Criteria**: `go build ./executor/` exit 0

  **Commit**: YES — `feat: integrate Gemini usage parsing + PublishRecord`

- [ ] 18. executor/codex.go 调 usage.PublishRecord

  **What to do**: 同 Task 15 模式。Codex body usage 或 headers。

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 3 / Blocked By: T13, T14
  **Acceptance Criteria**: `go build ./executor/` exit 0

  **Commit**: YES — `feat: integrate Codex usage parsing + PublishRecord`

- [ ] 19. executor/vertex.go 调 usage.PublishRecord

  **What to do**: 同 Task 15 模式。Vertex usageMetadata 同 Gemini 格式。

  **Recommended Agent Profile**: `quick`
  **Parallelization**: YES / Wave 3 / Blocked By: T13, T14
  **Acceptance Criteria**: `go build ./executor/` exit 0

  **Commit**: YES — `feat: integrate Vertex usage parsing + PublishRecord`

- [ ] 20. main.go 重写为 Builder 模式

  **What to do**:
  重写 `main.go` 的 `run()` 函数，使用 SDK Builder：
  ```go
  func run(configPath string) error {
      cfg := config.MustLoad(configPath)  // 或保持现有 LoadConfig
      db := infra.MustInitDB(cfg)
      infra.AutoMigrate(db)
      rdb := infra.InitRedis(cfg)

      SeedModelPrices(db)
      EnsureSubscriptionSeeds(db)
      EnsureSDKManagementSeeds(db, cfg)

      // 核心依赖
      ldgr := ledger.New(db, rdb)
      priceCache, _ := pricing.NewModelPriceCache(db)
      calc := pricing.NewCalculator(priceCache, cfg.Billing.DefaultPricePer1KTokens)
      apiKeyCache := infra.NewAPIKeyCache()
      go apiKeyCache.Start(context.Background())

      // SDK 接口实现
      store := sdk.NewStore(db)
      accessProvider := sdk.NewAccessProvider(db, rdb, apiKeyCache, cfg.Auth.JWT.Secret)
      usagePlugin := sdk.NewUsagePlugin(db, ldgr, calc)
      holdMW := sdk.NewHoldMiddleware(ldgr, calc, db, 5*time.Minute)

      // auth.Manager (executor 注册)
      authMgr := cliproxyauth.NewManager(store, &cliproxyauth.RoundRobinSelector{}, cliproxyauth.NoopHook{})
      authMgr.Load(context.Background())
      registerRuntimeAuths(authMgr, cfg)  // 从 config 注册 runtime_only 凭证

      // access.Manager
      accessMgr := sdkaccess.NewManager()
      accessMgr.SetProviders([]sdkaccess.Provider{accessProvider})

      // PanelRouter (业务路由)
      panelRouter := api.NewPanelRouter(db, rdb, ldgr, calc, cfg)

      // Build SDK Service
      svc, err := cliproxy.NewBuilder().
          WithConfig(cfg.SDK.ToSDKConfig()).  // 或 WithConfigPath
          WithAuthManager(authMgr).
          WithRequestAccessManager(accessMgr).
          WithServerOptions(
              sdkapi.WithMiddleware(holdMW.Handle),
              sdkapi.WithRouterConfigurator(func(e *gin.Engine, _ *handlers.BaseAPIHandler, _ *sdkconfig.Config) {
                  panelRouter.RegisterPanelRoutes(e)
              }),
          ).
          Build()
      if err != nil { return err }

      // 注册 UsagePlugin（Builder 无此方法，必须在 Build 后调）
      svc.RegisterUsagePlugin(usagePlugin)

      return svc.Run(context.Background())
  }
  ```
  - 保留 `registerRuntimeAuths` 逻辑（从 config 注册 5 个 provider 的 runtime_only 凭证）
  - 删除旧的 `gin.Default()` + `registerRoutes()` + `http.Server` 手动管理
  - 删除 `GlobalDB/GlobalConfig/GlobalLedger/GlobalStore` 全局变量（或标记 deprecated，Task 21 清理）

  **Must NOT do**:
  - 不保留旧 `gin.Default()` + `registerRoutes()` 路径
  - 不在 Builder 链上调 `WithUsagePlugin`（不存在此方法）

  **Recommended Agent Profile**: `deep`
  **Parallelization**: NO / Wave 4 / Blocks: T21 / Blocked By: T4,T5,T6c,T10,T11,T12,T13,T14,T15-T19
  **Acceptance Criteria**:
  - [ ] `go build .` exit 0
  - [ ] main.go 无 `gin.Default()` / `registerRoutes()` / `InitSDK`
  - [ ] main.go 有 `cliproxy.NewBuilder()` + `svc.RegisterUsagePlugin` + `svc.Run`
  - [ ] `grep -n "GlobalDB\|GlobalConfig\|GlobalLedger\|GlobalStore" main.go` 返回空或仅 deprecated 注释

  **QA Scenarios**:
  ```
  Scenario: main.go 编译 + 无旧代码
    Tool: Bash
    Steps:
      1. go build .
      2. grep -n "gin.Default\|registerRoutes\|InitSDK" main.go || echo "CLEAN"
      3. grep -n "RegisterUsagePlugin" main.go
    Expected: exit 0, "CLEAN", RegisterUsagePlugin 存在
    Evidence: .sisyphus/evidence/task-20-build.txt
  ```

  **Commit**: YES — `feat: rewrite main.go with SDK Builder (svc.RegisterUsagePlugin after Build)`

- [ ] 21. 删除旧代码 + 路由验证

  **What to do**:
  - 删除以下旧代码（SDK Builder 已接管）：
    - `handler_proxy.go` 中：`ProxyChatHandler`, `ProxyModelsHandler`, `executeProxyNonStream`, `executeProxyStream`, `calculateProxyUsage`, `openAIUsageTokens`, `approximateTokensFromBytes`, `settleAndLogProxyUsage`, `releaseProxyHold`
    - `middleware.go` 中：`BillingMiddleware`, `estimateRequestCost`, `holdTTL`
    - `main.go` 中：`registerRoutes`, `serverAddr`, `MetricsHandler`（如已移到 api/）
    - 全局变量：`GlobalDB`, `GlobalConfig`, `GlobalLedger`, `GlobalStore`, `authManager`
  - 保留：`sanitizedProxyHeaders`, `copyOutboundHeaders`, `writeUpstreamHeaders`（executor 可能间接使用）
  - 保留：`handler_proxy.go` 中的 `InitSDK` 改名为 `registerRuntimeAuths` 并移到 main.go 或 sdk/ 包
  - 确保 `go build .` + `go vet ./...` 通过
  - 确保 `/api/panel/**` 路由仍可注册

  **Must NOT do**: 不删除 executor 文件（已在 executor/ 包）

  **Recommended Agent Profile**: `quick`
  **Parallelization**: NO / Wave 4 sequential after T20 / Blocks: None / Blocked By: T20
  **Acceptance Criteria**:
  - [ ] `go build .` exit 0
  - [ ] `grep -rn "ProxyChatHandler\|calculateProxyUsage\|BillingMiddleware\|approximateTokensFromBytes" --include="*.go" | grep -v _test | grep -v .sisyphus` → 空
  - [ ] 预估删除 ~700 行

  **QA Scenarios**:
  ```
  Scenario: 旧代码已清除 + 编译通过
    Tool: Bash
    Steps:
      1. go build .
      2. go vet ./...
      3. grep -rn "ProxyChatHandler\|calculateProxyUsage\|BillingMiddleware" --include="*.go" | grep -v _test | grep -v .sisyphus || echo "CLEAN"
    Expected: exit 0 + "CLEAN"
    Evidence: .sisyphus/evidence/task-21-cleanup.txt
  ```

  **Commit**: YES — `refactor: remove old ProxyChatHandler/BillingMiddleware/calculateProxyUsage (~700 lines)`

---

## Final Verification Wave (MANDATORY — after ALL implementation tasks)

> 4 review agents run in PARALLEL. ALL must APPROVE.

- [ ] F1. **Plan Compliance Audit** — `oracle`
  Read plan end-to-end. For each "Must Have": verify implementation exists. For each "Must NOT Have": search codebase for forbidden patterns — reject with file:line if found. Check evidence files. Compare deliverables.
  Output: `Must Have [N/N] | Must NOT Have [N/N] | Tasks [N/N] | VERDICT: APPROVE/REJECT`

- [ ] F2. **Code Quality Review** — `unspecified-high`
  Run `go build ./...` + `go vet ./...` + `go test ./...`. Review for: unused imports, commented-out code, AI slop (over-abstraction, generic names). Check no SDK imports in `model/` or `infra/`.
  Output: `Build [PASS/FAIL] | Vet [PASS/FAIL] | Tests [N pass/N fail] | VERDICT`

- [ ] F3. **Real Manual QA** — `unspecified-high`
  Execute EVERY QA scenario from EVERY task via curl/go test. Test cross-task integration: `/v1/chat/completions` through full pipeline (Hold → Execute → Settle). Test edge cases: empty body, invalid model, streaming, insufficient balance. Save to `.sisyphus/evidence/final-qa/`.
  Output: `Scenarios [N/N pass] | Integration [N/N] | Edge Cases [N tested] | VERDICT`

- [ ] F4. **Scope Fidelity Check** — `deep`
  For each task: read "What to do", read actual diff. Verify 1:1 — everything in spec was built, nothing beyond spec was built. Check "Must NOT do" compliance. Detect cross-task contamination.
  Output: `Tasks [N/N compliant] | Contamination [CLEAN/N issues] | Unaccounted [CLEAN/N files] | VERDICT`

---

## Commit Strategy

- **Wave 1**: 9 commits — structural, no logic changes
- **Wave 2**: 7 commits — new functionality (TDD)
- **Wave 3**: 5 commits — executor enhancement
- **Wave 4**: 2 commits — switchover + cleanup
- **Wave 5**: 1 commit (combined review fixes if any)

---

## Success Criteria

### Verification Commands
```bash
go build ./...                           # Expected: exit 0
go vet ./...                             # Expected: no warnings
go test ./... -count=1 -timeout 30s      # Expected: all PASS (>=15 tests)
go test -race ./... -count=1             # Expected: no race conditions
```

### Final Checklist
- [ ] All "Must Have" features present
- [ ] All "Must NOT Have" absent (no SDK imports in model/, infra/)
- [ ] All Go tests pass (>=15 test cases)
- [ ] Old ProxyChatHandler/BillingMiddleware code deleted
- [ ] /v1/chat/completions works through SDK Builder pipeline
- [ ] /api/panel/** routes work
- [ ] HoldMiddleware 402 on insufficient balance
- [ ] HoldMiddleware 402 on subscription quota exceeded
- [ ] Subscription quota resets correctly (daily/weekly/monthly)
- [ ] Streaming requests produce correct token counts via usage.PublishRecord
- [ ] UsageLog has correct InputTokens/OutputTokens/CachedTokens/ReasoningTokens
- [ ] `svc.RegisterUsagePlugin(usagePlugin)` called after Build(), before Run()
- [ ] No request_id keyed global UsageCollector (ctx-based approach only)
