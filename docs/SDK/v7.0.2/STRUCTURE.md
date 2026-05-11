# CLIProxyAPI SDK v7.0.2 — CPA-Gateway 使用边界

## 模块

- Module: `github.com/router-for-me/CLIProxyAPI/v7`
- Version: `v7.0.2`
- Docs: https://pkg.go.dev/github.com/router-for-me/CLIProxyAPI/v7@v7.0.2

## 允许使用

CPA-Gateway 只把 SDK 作为库使用，允许以下包：

- `github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth`
  - `NewManager`
  - `Manager.RegisterExecutor`
  - `Manager.Register`
  - `Manager.Execute`
  - `Manager.ExecuteStream`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor`
  - `Request`
  - `Response`
  - `Options`
  - `StreamChunk`
  - `StreamResult`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/translator`
  - 仅用于请求/响应格式转换

## 禁止使用

以下包和生命周期 API 属于 SDK HTTP 层，CPA-Gateway 中禁止使用：

- `github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy`
  - `NewBuilder`
  - `Builder.Build()`
  - `Service.Run()`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/api`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude`
- `github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/gemini`

## v6.10.9 到 v7.0.2 迁移结论

CPA-Gateway 当前使用到的 `auth.Manager`、`executor` 类型和 `translator` 包签名保持兼容；迁移主要是 import path 从 `/v6/...` 切换到 `/v7/...`。
