# SDK 文档

## 版本列表

| 版本 | 文档 |
|---|---|
| v7.0.2 | [v7.0.2/STRUCTURE.md](v7.0.2/STRUCTURE.md) |
| v6.10.9 | [v6.10.9/STRUCTURE.md](v6.10.9/STRUCTURE.md) |

## CPA Gateway 与 SDK 版本对应关系

当前项目依赖: `github.com/router-for-me/CLIProxyAPI/v7@v7.0.2`

CPA-Gateway 只把 CLIProxyAPI SDK 作为纯函数/类型库使用。允许导入 `sdk/cliproxy/auth`、`sdk/cliproxy/executor`、`sdk/translator`；禁止导入 `sdk/api` 或让 SDK 控制 HTTP 生命周期。

## SDK 原始文档

- pkg.go.dev: https://pkg.go.dev/github.com/router-for-me/CLIProxyAPI/v7@v7.0.2
- GitHub: https://github.com/router-for-me/CLIProxyAPI
