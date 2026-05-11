# CPA-Gateway — Agent 执行规范

> 以下为强制边界，所有 agent 必须遵守。违反任一条 → 代码不可合并。

---

## 🚨 强制架构边界

### 1. CPA-Gateway Gin 全权控制 HTTP

- ✅ **必须**：CPA-Gateway 自己创建 `gin.Default()`，注册所有路由，运行 `http.Server`
- ✅ **必须**：所有 HTTP handler 由 CPA-Gateway 的 `.go` 文件实现
- ❌ **禁止**：调用 SDK 的 `Builder.Build()` / `Service.Run()`
- ❌ **禁止**：导入或使用 SDK 的 `sdk/api` 包（含 `sdk/api/handlers/`）
- ❌ **禁止**：让 SDK 注册任何 HTTP 路由

### 2. SDK 仅作为纯函数库

- ✅ **允许**：调用 SDK 的执行函数（`authManager.Execute()`, `ExecuteStream()` 等）
- ✅ **允许**：使用 SDK 的类型定义（`cliproxyexecutor.Request`, `Response` 等）
- ✅ **允许**：使用 SDK 的 translator（格式转换）
- ❌ **禁止**：SDK 控制任何 HTTP 层的生命周期

### 3. 项目结构

- ✅ **必须**：所有 Go 代码在同一 package（扁平结构，无 `internal/` 嵌套）
- ✅ **必须**：文件命名：`main.go`, `config.go`, `db.go`, `auth.go`, `ledger.go`, `middleware.go`, `handler_*.go`, `response.go`, `errors.go`
- ❌ **禁止**：创建 `internal/` 深层目录

### 4. 技术栈锁定

| 组件 | 必须使用 | 禁止使用 |
|------|---------|---------|
| HTTP 框架 | Gin | - |
| ORM | GORM | ent |
| 数据库 | PostgreSQL | SQLite |
| 缓存 | Redis (go-redis/v9) | - |
| SDK 版本 | CLIProxyAPI v7.0.2 | 其他版本 |
| 前端框架 | React 19 + Vite | - |
| UI 库 | shadcn/ui + Tailwind CSS | Ant Design, MUI |
| 状态管理 | Zustand + TanStack Query | Redux |

### 5. 功能范围

- ✅ **包含**：用户注册/登录、API Key 管理、计费预扣/结算、AI 代理转发（/v1/chat/completions）、前端仪表盘、支付、工单、退款、订阅管理、管理后台、邮件通知、OAuth、多租户

### 6. 代码规范

- 响应格式统一：`{ "code": 0, "message": "ok", "data": {...} }`
- API Key 存储：SHA-256 哈希，**绝不存明文**
- 密码存储：bcrypt 哈希
- 错误处理：返回结构化 JSON，非纯文本
- Redis key 前缀：`cpa-gateway:{domain}:{entity}:{id}`
- 日志：使用 `log/slog`，非 `fmt.Println`

### 7. 前端规范

- 前端独立部署（不 embed 到 Go 二进制）
- 开发环境 Vite proxy → `http://127.0.0.1:8888`
- API 请求前缀：`/api/panel`
- Token 存储：localStorage，Authorization header 自动注入
- 401 响应 → 自动 logout
- 扁平目录：页面组件直接放 `src/`，不嵌套 `pages/public/` 等

---

## 📂 参考项目

- **SDK 文档**：`docs/SDK/v7.0.2/STRUCTURE.md`
- **SDK 源码**：`github.com/router-for-me/CLIProxyAPI/v7@v7.0.2`
