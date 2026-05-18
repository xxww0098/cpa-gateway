# 使用中文和我沟通

# CPA-Gateway 项目规范


## 项目结构

```
cpa-gateway/
├── main.go                 # 入口，SDK Builder 组装（~140 行）
├── config.yaml             # 运行时配置
├── config.example.yaml     # 配置模板
├── Makefile                # build/test/lint targets
│
├── config/                 # 配置类型定义 + YAML 加载 + 环境变量覆盖
├── infra/                  # 基础设施：DB 初始化、Redis、APIKeyCache、数据库种子
├── model/                  # GORM 模型（User/ApiKey/Group/Subscription/UsageLog/ModelPrice/...）
├── ledger/                 # Hold/Settle/Release 余额账本（Redis 锁 + PG 持久化）
├── pricing/                # ModelPriceCache + Calculator（Estimate/Compute 四列定价）
├── sdk/                    # CLIProxyAPI SDK 接口实现
│   ├── store.go            #   auth.Store（凭证持久化）
│   ├── access.go           #   access.Provider（多租户鉴权）
│   ├── usage.go            #   usage.Plugin（Settle/Release + UsageLog 写入）
│   ├── holdmw.go           #   HoldMiddleware（预扣 + 配额管控）
│   ├── settlectx.go        #   SettleCtx ctx value 传递
│   ├── interfaces.go       #   BillingLedger / PricingCalculator 接口
│   └── runtime_auths.go    #   注册 5 个 runtime-only 上游凭证
├── executor/               # 5 个 Provider Executor + usage_parser
├── api/                    # PanelRouter + 8 handler + middleware（/api/panel/**）
├── authutil/               # JWT 签发/验证 + API Key hash
├── testutil/               # miniredis 测试辅助
└── frontend/               # React 前端（独立构建）
```

## 编码规范

- **Go 版本**: 1.22+
- **包依赖方向**: `main` → `sdk/api/infra` → `model/config/ledger/pricing/executor/authutil`。禁止反向依赖。
- **model/ 包**: 仅标准库 + `time`，不 import 外部库（gorm tag 是声明式的，不需要 import gorm）
- **sdk/ 包**: 允许 import cliproxy SDK 包。其他包（model/infra/api/executor）不应 import cliproxy SDK（executor 除外，它 import `cliproxy/auth` + `cliproxy/executor` + `cliproxy/usage`）
- **测试**: 标准 `testing` 包 + `miniredis`（test-only）+ `gorm.io/driver/sqlite`（test-only）。不使用 testify/ginkgo。
- **错误处理**: 返回 error，不 panic。SDK 插件（UsagePlugin）内部 log + swallow。
- **命名**: 导出类型 PascalCase，文件名 snake_case，包名全小写单词。
- **配置**: 所有配置通过 `config.Config` 传递，不使用全局变量。

## 计费流程

```
Request → AccessProvider.Authenticate → HoldMiddleware.Handle
  → ledger.Hold (预扣)
  → SDK executor.Execute/ExecuteStream
  → executor 内部 ParseXxxUsage + usage.PublishRecord
  → UsagePlugin.HandleUsage
    → pricing.Calculator.Compute (精算)
    → ledger.Settle (精扣) 或 ledger.Release (失败退款)
    → INSERT UsageLog
    → 累加 Subscription 配额计数器
```

关键路径补充（billing-security-hardening）：

- Preflight：HoldMiddleware 在 `ledger.Hold` 前先查 `HasUnresolvedShortfall`（有未清偿欠款 → 402 `outstanding_debt`），再按 `max(holdAmount, EstimateWithMaxTokens, Estimate)` 作 upper-bound 与余额比对（不足 → 402 `insufficient_balance`，不创建 Redis hold）
- Fallback settle：上游 usage 缺失且非 strict 时，`UsagePlugin` 用 `max(ActiveHoldAmount, Estimate(stream=true))` 兜底 Settle，并在 `UsageLog.RawMetadata.billing_fallback.reason=missing_upstream_usage` 标注
- Strict mode：`billing.strict_usage_metadata_mode=true` 时缺失 usage 不 Settle/Release，写 `UsageLog{Failed=true, reason=missing_upstream_usage_strict}`，Hold 随 TTL 自然过期
- Shortfall 记录：`Settle` 按 `min(balance, actual)` 做 partial-debit，欠款写入 `BalanceLog.Metadata.shortfall_usd`，并同步进 `UsageLog.RawMetadata.shortfall_usd`；通过 reference = `shortfall_resolve:<requestID>:<debitLogID>` 的 Credit 配对解除
- Compensating credit：订阅购买 `Debit` 成功但 `Create(&Subscription)` 失败时，立即以 `subscription_purchase:<pkgID>:compensate:<debitRef>` 为 reference 的 Credit 回滚金额，补偿失败额外告警

## 测试

```bash
make test          # go test ./... -count=1 -timeout 30s
make test-verbose  # go test -v ./...
make test-race     # go test -race ./... -count=1
```

测试覆盖的核心模块：
- `ledger/` — Hold/Settle/Release（miniredis mock）
- `pricing/` — Calculator Estimate/Compute
- `sdk/` — AccessProvider + HoldMiddleware + UsagePlugin（SQLite in-memory + mock ledger）
- `executor/` — 5 provider usage parser

## 构建 & 运行

```bash
make build         # 编译 → ./cpa-gateway
make run           # 编译 + 运行
./cpa-gateway --config config.yaml
./cpa-gateway --version
```

## 关键约束

- Ledger 的 Hold/Settle/Release API 签名不可修改
- User/ApiKey/Group/UsageLog/ModelPrice 的 GORM 模型字段不可修改
- 不引入 production 新外部依赖（test-only 依赖允许）
- 每次提交必须 `go build ./...` 通过
