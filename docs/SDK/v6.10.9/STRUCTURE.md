# CLIProxyAPI SDK v6.10.9 — 结构分析

## 概览

- **模块**: `github.com/router-for-me/CLIProxyAPI/v6`
- **版本**: v6.10.9
- **许可证**: MIT
- **文档**: https://pkg.go.dev/github.com/router-for-me/CLIProxyAPI/v6@v6.10.9

---

## 包结构树

```
github.com/router-for-me/CLIProxyAPI/v6
├── sdk/
│   ├── cliproxy/                    # 核心服务构建与生命周期
│   ├── cliproxy/auth/               # 认证凭证生命周期管理
│   ├── cliproxy/executor/           # 请求执行与流式结果
│   ├── cliproxy/pipeline/           # 请求/响应管道与中间件钩子
│   ├── cliproxy/usage/              # 用量追踪插件接口
│   ├── config/                      # SDK 配置结构（type alias 合集）
│   ├── api/                         # HTTP 服务嵌入 helpers
│   ├── api/handlers/                # 基础 API 处理器
│   ├── api/handlers/openai/         # OpenAI + Responses 端点
│   ├── api/handlers/claude/         # Claude 端点
│   ├── api/handlers/gemini/         # Gemini + Gemini CLI 端点
│   ├── translator/                  # 跨格式请求/响应转换
│   ├── translator/builtin/          # 内置转换器注册
│   ├── access/                      # 请求级认证访问控制
│   ├── auth/                        # OAuth 登录与认证器
│   ├── logging/                     # 请求日志基础设施
│   └── proxyutil/                   # 代理配置解析与 HTTP Transport 构建
```

---

## sdk/cliproxy — 核心服务

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy`

提供 Service 生命周期管理和 Builder 模式。

### 常量 / 变量
无导出常量/变量。

### 函数

| 函数 | 签名 | 说明 |
|---|---|---|
| `NewBuilder` | `func NewBuilder() *Builder` | 创建 Builder，默认依赖未设置 |
| `GlobalModelRegistry` | `func GlobalModelRegistry() ModelRegistry` | 返回共享的全局模型注册表 |
| `NewAPIKeyClientProvider` | `func NewAPIKeyClientProvider() APIKeyClientProvider` | 返回默认 API key 客户端加载器 |
| `NewFileTokenClientProvider` | `func NewFileTokenClientProvider() TokenClientProvider` | 返回默认 token 客户端加载器 |
| `SetGlobalModelRegistryHook` | `func SetGlobalModelRegistryHook(hook ModelRegistryHook)` | 在全局注册表上注册钩子 (v6.6.79) |

### 接口

#### `Builder`
```go
type Builder struct { /* unexported fields */ }

func (b *Builder) Build() (*Service, error)
func (b *Builder) WithConfig(cfg *config.Config) *Builder
func (b *Builder) WithConfigPath(path string) *Builder
func (b *Builder) WithAPIKeyClientProvider(provider APIKeyClientProvider) *Builder
func (b *Builder) WithTokenClientProvider(provider TokenClientProvider) *Builder
func (b *Builder) WithAuthManager(mgr *sdkAuth.Manager) *Builder
func (b *Builder) WithCoreAuthManager(mgr *coreauth.Manager) *Builder
func (b *Builder) WithRequestAccessManager(mgr *sdkaccess.Manager) *Builder
func (b *Builder) WithServerOptions(opts ...api.ServerOption) *Builder
func (b *Builder) WithHooks(h Hooks) *Builder
func (b *Builder) WithWatcherFactory(factory WatcherFactory) *Builder
func (b *Builder) WithLocalManagementPassword(password string) *Builder     // v6.0.1
func (b *Builder) WithPostAuthHook(hook coreauth.PostAuthHook) *Builder      // v6.8.27
```

#### `Service`
```go
type Service struct { /* unexported fields */ }

func (s *Service) Run(ctx context.Context) error          // 启动服务，阻塞直到 context 取消
func (s *Service) Shutdown(ctx context.Context) error    // 优雅停止后台 worker 和 HTTP server
func (s *Service) RegisterUsagePlugin(plugin usage.Plugin)
```

#### `APIKeyClientProvider`
```go
type APIKeyClientProvider interface {
    Load(ctx context.Context, cfg *config.Config) (*APIKeyClientResult, error)
}
```

#### `TokenClientProvider`
```go
type TokenClientProvider interface {
    Load(ctx context.Context, cfg *config.Config) (*TokenClientResult, error)
}
```

#### `ModelRegistry`
```go
type ModelRegistry interface {
    RegisterClient(clientID, clientProvider string, models []*ModelInfo)
    UnregisterClient(clientID string)
    SetModelQuotaExceeded(clientID, modelID string)
    ClearModelQuotaExceeded(clientID, modelID string)
    ClientSupportsModel(clientID, modelID string) bool
    GetAvailableModels(handlerType string) []map[string]any
    GetAvailableModelsByProvider(provider string) []*ModelInfo
}
```

#### `WatcherFactory`
```go
type WatcherFactory func(configPath, authDir string, reload func(*config.Config)) (*WatcherWrapper, error)
```

### 结构体

#### `APIKeyClientResult`
```go
type APIKeyClientResult struct {
    GeminiKeyCount       int
    VertexCompatKeyCount int
    ClaudeKeyCount       int
    CodexKeyCount        int
    OpenAICompatCount    int
}
```

#### `TokenClientResult`
```go
type TokenClientResult struct {
    SuccessfulAuthed int
}
```

#### `Hooks`
```go
type Hooks struct {
    OnBeforeStart func(*config.Config)
    OnAfterStart  func(*Service)
}
```

#### `ModelInfo` (type alias)
```go
type ModelInfo = registry.ModelInfo
```

#### `WatcherWrapper`
```go
type WatcherWrapper struct { /* unexported */ }

func (w *WatcherWrapper) Start(ctx context.Context) error
func (w *WatcherWrapper) Stop() error
func (w *WatcherWrapper) SetConfig(cfg *config.Config)
func (w *WatcherWrapper) SetAuthUpdateQueue(queue chan<- watcher.AuthUpdate)
func (w *WatcherWrapper) SnapshotAuths() []*coreauth.Auth
func (w *WatcherWrapper) DispatchRuntimeAuthUpdate(update watcher.AuthUpdate) bool  // v6.5.28
```

---

## sdk/cliproxy/auth — 认证凭证管理

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth`

### 常量

```go
const CloseAllExecutionSessionsID = "__all_execution_sessions__"
```

### 核心结构体

#### `Auth`
```go
type Auth struct {
    ID              string
    Index           string
    Provider        string
    Prefix          string
    FileName        string
    Storage         baseauth.TokenStorage
    Label           string
    Status          Status
    StatusMessage   string
    Disabled        bool
    Unavailable     bool
    ProxyURL        string
    Attributes      map[string]string
    Metadata        map[string]any
    Quota           QuotaState
    LastError       *Error
    CreatedAt       time.Time
    UpdatedAt       time.Time
    LastRefreshedAt time.Time
    NextRefreshAfter time.Time
    NextRetryAfter  time.Time
    ModelStates     map[string]*ModelState
    Runtime         any
    Success         int64
    Failed          int64
}
```

#### `Status` 枚举
```go
type Status string
const (
    StatusUnknown    Status = "unknown"
    StatusActive     Status = "active"
    StatusPending    Status = "pending"
    StatusRefreshing Status = "refreshing"
    StatusError      Status = "error"
    StatusDisabled   Status = "disabled"
)
```

#### `ModelState`
```go
type ModelState struct {
    Status         Status
    StatusMessage  string
    Unavailable    bool
    NextRetryAfter time.Time
    LastError      *Error
    Quota          QuotaState
    UpdatedAt      time.Time
}
```

#### `QuotaState`
```go
type QuotaState struct {
    Exceeded      bool
    Reason        string
    NextRecoverAt time.Time
    BackoffLevel  int
}
```

#### `Error`
```go
type Error struct {
    Code       string
    Message    string
    Retryable  bool
    HTTPStatus int
}
func (e *Error) Error() string
func (e *Error) StatusCode() int
```

### 核心接口

#### `Selector`
```go
type Selector interface {
    Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
}
```

#### `ProviderExecutor`
```go
type ProviderExecutor interface {
    Identifier() string
    Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
    ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
    Refresh(ctx context.Context, auth *Auth) (*Auth, error)
    CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
    HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error)
}
```

#### `Store`
```go
type Store interface {
    List(ctx context.Context) ([]*Auth, error)
    Save(ctx context.Context, auth *Auth) (string, error)
    Delete(ctx context.Context, id string) error
}
```

#### `Hook`
```go
type Hook interface {
    OnAuthRegistered(ctx context.Context, auth *Auth)
    OnAuthUpdated(ctx context.Context, auth *Auth)
    OnResult(ctx context.Context, result Result)
}
```

### Manager 方法（部分）

```go
func NewManager(store Store, selector Selector, hook Hook) *Manager
func (m *Manager) Load(ctx context.Context) error
func (m *Manager) Register(ctx context.Context, auth *Auth) (*Auth, error)
func (m *Manager) Update(ctx context.Context, auth *Auth) (*Auth, error)
func (m *Manager) List() []*Auth
func (m *Manager) GetByID(id string) (*Auth, bool)
func (m *Manager) Execute(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
func (m *Manager) ExecuteStream(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
func (m *Manager) ExecuteCount(ctx context.Context, providers []string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
func (m *Manager) RegisterExecutor(executor ProviderExecutor)
func (m *Manager) SetSelector(selector Selector)
func (m *Manager) SetStore(store Store)
func (m *Manager) SetOAuthModelAlias(aliases map[string][]internalconfig.OAuthModelAlias)
func (m *Manager) MarkResult(ctx context.Context, result Result)
func (m *Manager) StartAutoRefresh(parent context.Context, interval time.Duration)
func (m *Manager) StopAutoRefresh()
```

---

## sdk/cliproxy/executor — 请求执行

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor`

### 常量

```go
const (
    PinnedAuthMetadataKey             = "pinned_auth_id"
    SelectedAuthMetadataKey           = "selected_auth_id"
    SelectedAuthCallbackMetadataKey   = "selected_auth_callback"
    ExecutionSessionMetadataKey       = "execution_session_id"
    DisallowFreeAuthMetadataKey       = "disallow_free_auth"
    RequestPathMetadataKey            = "request_path"
    RequestedModelMetadataKey         = "requested_model"
)
```

### 核心类型

#### `Request`
```go
type Request struct {
    Model    string
    Payload  []byte
    Format   sdktranslator.Format
    Metadata map[string]any
}
```

#### `Options`
```go
type Options struct {
    Stream          bool
    Alt             string
    Headers         http.Header
    Query           url.Values
    OriginalRequest []byte
    SourceFormat    sdktranslator.Format
    Metadata        map[string]any
}
```

#### `Response`
```go
type Response struct {
    Payload  []byte
    Metadata map[string]any
    Headers  http.Header
}
```

#### `StreamChunk`
```go
type StreamChunk struct {
    Payload []byte
    Err     error
}
```

#### `StreamResult` (v6.8.22)
```go
type StreamResult struct {
    Headers http.Header
    Chunks  <-chan StreamChunk
}
```

#### `StatusError` (interface)
```go
type StatusError interface {
    error
    StatusCode() int
}
```

### 函数

```go
func DownstreamWebsocket(ctx context.Context) bool         // v6.8.19
func WithDownstreamWebsocket(ctx context.Context) context.Context  // v6.8.19
```

---

## sdk/cliproxy/pipeline — 请求/响应管道

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/pipeline`

### 类型

#### `Context`
```go
type Context struct {
    Request     cliproxyexecutor.Request
    Options     cliproxyexecutor.Options
    Auth        *cliproxyauth.Auth
    Translator  *sdktranslator.Pipeline
    HTTPClient  *http.Client
}
```

#### `Hook` (interface)
```go
type Hook interface {
    BeforeExecute(ctx context.Context, execCtx *Context)
    AfterExecute(ctx context.Context, execCtx *Context, resp cliproxyexecutor.Response, err error)
    OnStreamChunk(ctx context.Context, execCtx *Context, chunk cliproxyexecutor.StreamChunk)
}
```

#### `HookFunc`
```go
type HookFunc struct {
    Before func(context.Context, *Context)
    After  func(context.Context, *Context, cliproxyexecutor.Response, error)
    Stream func(context.Context, *Context, cliproxyexecutor.StreamChunk)
}
// 实现了 Hook 接口
```

#### `RoundTripperProvider`
```go
type RoundTripperProvider interface {
    RoundTripperFor(auth *cliproxyauth.Auth) http.RoundTripper
}
```

---

## sdk/cliproxy/usage — 用量追踪

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage`

### 类型

#### `Detail`
```go
type Detail struct {
    InputTokens     int64
    OutputTokens    int64
    ReasoningTokens int64
    CachedTokens    int64
    TotalTokens     int64
}
```

#### `Record`
```go
type Record struct {
    Provider    string
    Model       string
    Alias       string
    APIKey      string
    AuthID      string
    AuthIndex   string
    AuthType    string
    Source      string
    RequestedAt time.Time
    Latency     time.Duration
    Failed      bool
    Detail      Detail
}
```

#### `Plugin` (interface)
```go
type Plugin interface {
    HandleUsage(ctx context.Context, record Record)
}
```

#### `Manager`
```go
type Manager struct { /* unexported */ }

func (m *Manager) Publish(ctx context.Context, record Record)
func (m *Manager) Register(plugin Plugin)
func (m *Manager) Start(ctx context.Context)
func (m *Manager) Stop()
```

### 函数

```go
func PublishRecord(ctx context.Context, record Record)
func RegisterPlugin(plugin Plugin)
func StartDefault(ctx context.Context)
func StopDefault()
func RequestedModelAliasFromContext(ctx context.Context) string         // v6.10.7
func WithRequestedModelAlias(ctx context.Context, alias string) context.Context  // v6.10.7
func DefaultManager() *Manager
func NewManager(buffer int) *Manager
```

---

## sdk/config — 配置类型别名

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/config`

所有类型都是 `internalconfig.*` 的 type alias，不可见具体字段定义。

### 常量

```go
const DefaultPanelGitHubRepository string
```

### 函数

| 函数 | 签名 |
|---|---|
| `LoadConfig` | `func LoadConfig(configFile string) (*Config, error)` |
| `LoadConfigOptional` | `func LoadConfigOptional(configFile string, optional bool) (*Config, error)` |
| `NormalizeCommentIndentation` | `func NormalizeCommentIndentation(data []byte) []byte` |
| `SaveConfigPreserveComments` | `func SaveConfigPreserveComments(configFile string, cfg *Config) error` |
| `SaveConfigPreserveCommentsUpdateNestedScalar` | `func SaveConfigPreserveCommentsUpdateNestedScalar(configFile string, path []string, value string) error` |

### 类型别名

```go
type AmpCode               = internalconfig.AmpCode
type ClaudeKey             = internalconfig.ClaudeKey
type CodexKey              = internalconfig.CodexKey
type Config                = internalconfig.Config
type GeminiKey             = internalconfig.GeminiKey
type OAuthModelAlias       = internalconfig.OAuthModelAlias          // v6.7.0
type OpenAICompatibility   = internalconfig.OpenAICompatibility
type OpenAICompatibilityAPIKey = internalconfig.OpenAICompatibilityAPIKey
type OpenAICompatibilityModel  = internalconfig.OpenAICompatibilityModel
type PayloadConfig         = internalconfig.PayloadConfig
type PayloadFilterRule     = internalconfig.PayloadFilterRule          // v6.7.39
type PayloadModelRule      = internalconfig.PayloadModelRule
type PayloadRule           = internalconfig.PayloadRule
type RemoteManagement      = internalconfig.RemoteManagement
type SDKConfig             = internalconfig.SDKConfig
type StreamingConfig       = internalconfig.StreamingConfig           // v6.6.49
type TLS / TLSConfig       = internalconfig.TLSConfig
type VertexCompatKey       = internalconfig.VertexCompatKey
type VertexCompatModel     = internalconfig.VertexCompatModel
```

---

## sdk/api — HTTP 服务嵌入

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/api`

### 类型

#### `ServerOption` (type alias)
```go
type ServerOption = internalapi.ServerOption
```

#### `ManagementTokenRequester` (interface) — v6.6.64
```go
type ManagementTokenRequester interface {
    RequestAnthropicToken(c *gin.Context)
    RequestGeminiCLIToken(c *gin.Context)
    RequestCodexToken(c *gin.Context)
    RequestAntigravityToken(c *gin.Context)
    RequestKimiToken(c *gin.Context)
    GetAuthStatus(c *gin.Context)
    PostOAuthCallback(c *gin.Context)
}
```

### 函数

| 函数 | 签名 |
|---|---|
| `NewManagementTokenRequester` | `func NewManagementTokenRequester(cfg *config.Config, manager *coreauth.Manager) ManagementTokenRequester` |
| `WithEngineConfigurator` | `func WithEngineConfigurator(fn func(*gin.Engine)) ServerOption` |
| `WithKeepAliveEndpoint` | `func WithKeepAliveEndpoint(timeout time.Duration, onTimeout func()) ServerOption` |
| `WithLocalManagementPassword` | `func WithLocalManagementPassword(password string) ServerOption` |
| `WithMiddleware` | `func WithMiddleware(mw ...gin.HandlerFunc) ServerOption` |
| `WithRequestLoggerFactory` | `func WithRequestLoggerFactory(factory func(*config.Config, string) logging.RequestLogger) ServerOption` |
| `WithRouterConfigurator` | `func WithRouterConfigurator(fn func(*gin.Engine, *handlers.BaseAPIHandler, *config.Config)) ServerOption` |

---

## sdk/api/handlers — 基础 API 处理器

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers`

### 类型

#### `BaseAPIHandler`
```go
type BaseAPIHandler struct {
    AuthManager *coreauth.Manager
    Cfg        *config.SDKConfig
}

func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler
func (h *BaseAPIHandler) ExecuteWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage)
func (h *BaseAPIHandler) ExecuteStreamWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) (<-chan []byte, http.Header, <-chan *interfaces.ErrorMessage)
func (h *BaseAPIHandler) ExecuteCountWithAuthManager(ctx context.Context, handlerType, modelName string, rawJSON []byte, alt string) ([]byte, http.Header, *interfaces.ErrorMessage)
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc)
func (h *BaseAPIHandler) WriteErrorResponse(c *gin.Context, msg *interfaces.ErrorMessage)
func (h *BaseAPIHandler) LoggingAPIResponseError(ctx context.Context, err *interfaces.ErrorMessage)
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig)
func (h *BaseAPIHandler) ForwardStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, opts StreamForwardOptions)
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func()
```

#### `StreamForwardOptions`
```go
type StreamForwardOptions struct {
    KeepAliveInterval      *time.Duration
    WriteChunk             func(chunk []byte)
    WriteTerminalError     func(errMsg *interfaces.ErrorMessage)
    WriteDone              func()
    WriteKeepAlive         func()
}
```

#### `APIHandlerCancelFunc`
```go
type APIHandlerCancelFunc func(params ...interface{})
```

#### `ErrorResponse` / `ErrorDetail`
```go
type ErrorResponse struct {
    Error ErrorDetail `json:"error"`
}
type ErrorDetail struct {
    Message string `json:"message"`
    Type    string `json:"type"`
    Code    string `json:"code,omitempty"`
}
```

### 函数

| 函数 | 签名 |
|---|---|
| `BuildErrorResponseBody` | `func(status int, errText string) []byte` |
| `BuildOpenAIResponsesStreamErrorChunk` | `func(status int, errText string, sequenceNumber int) []byte` |
| `FilterUpstreamHeaders` | `func(src http.Header) http.Header` |
| `PassthroughHeadersEnabled` | `func(cfg *config.SDKConfig) bool` |
| `StreamingKeepAliveInterval` | `func(cfg *config.SDKConfig) time.Duration` |
| `NonStreamingKeepAliveInterval` | `func(cfg *config.SDKConfig) time.Duration` |
| `StreamingBootstrapRetries` | `func(cfg *config.SDKConfig) int` |
| `WithExecutionSessionID` | `func(ctx context.Context, sessionID string) context.Context` |
| `WithPinnedAuthID` | `func(ctx context.Context, authID string) context.Context` |
| `WithSelectedAuthIDCallback` | `func(ctx context.Context, callback func(string)) context.Context` |
| `WithDisallowFreeAuth` | `func(ctx context.Context) context.Context` |
| `WriteUpstreamHeaders` | `func(dst http.Header, src http.Header)` |

---

## sdk/api/handlers/openai — OpenAI 端点

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/openai`

### 类型

```go
type OpenAIAPIHandler struct {
    *handlers.BaseAPIHandler
}

type OpenAIResponsesAPIHandler struct {
    *handlers.BaseAPIHandler
}
```

### OpenAIAPIHandler 方法

| 方法 | 签名 |
|---|---|
| `NewOpenAIAPIHandler` | `func(apiHandlers *handlers.BaseAPIHandler) *OpenAIAPIHandler` |
| `ChatCompletions` | `func(h *OpenAIAPIHandler) ChatCompletions(c *gin.Context)` |
| `Completions` | `func(h *OpenAIAPIHandler) Completions(c *gin.Context)` |
| `OpenAIModels` | `func(h *OpenAIAPIHandler) OpenAIModels(c *gin.Context)` |
| `Models` | `func(h *OpenAIAPIHandler) Models() []map[string]any` |
| `HandlerType` | `func(h *OpenAIAPIHandler) HandlerType() string` |
| `ImagesEdits` | `func(h *OpenAIAPIHandler) ImagesEdits(c *gin.Context)` (v6.9.32) |
| `ImagesGenerations` | `func(h *OpenAIAPIHandler) ImagesGenerations(c *gin.Context)` (v6.9.32) |

### OpenAIResponsesAPIHandler 方法

| 方法 | 签名 |
|---|---|
| `NewOpenAIResponsesAPIHandler` | `func(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler` |
| `Responses` | `func(h *OpenAIResponsesAPIHandler) Responses(c *gin.Context)` |
| `ResponsesWebsocket` | `func(h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context)` (v6.8.19) |
| `OpenAIResponsesModels` | `func(h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context)` |
| `Models` | `func(h *OpenAIResponsesAPIHandler) Models() []map[string]any` |
| `HandlerType` | `func(h *OpenAIResponsesAPIHandler) HandlerType() string` |
| `Compact` | `func(h *OpenAIResponsesAPIHandler) Compact(c *gin.Context)` (v6.7.36) |

---

## sdk/api/handlers/claude — Claude 端点

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/claude`

### 类型

```go
type ClaudeCodeAPIHandler struct {
    *handlers.BaseAPIHandler
}
```

### 方法

| 方法 | 签名 |
|---|---|
| `NewClaudeCodeAPIHandler` | `func(apiHandlers *handlers.BaseAPIHandler) *ClaudeCodeAPIHandler` |
| `ClaudeMessages` | `func(h *ClaudeCodeAPIHandler) ClaudeMessages(c *gin.Context)` |
| `ClaudeCountTokens` | `func(h *ClaudeCodeAPIHandler) ClaudeCountTokens(c *gin.Context)` |
| `ClaudeModels` | `func(h *ClaudeCodeAPIHandler) ClaudeModels(c *gin.Context)` |
| `Models` | `func(h *ClaudeCodeAPIHandler) Models() []map[string]any` |
| `HandlerType` | `func(h *ClaudeCodeAPIHandler) HandlerType() string` |

---

## sdk/api/handlers/gemini — Gemini 端点

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers/gemini`

### 类型

```go
type GeminiAPIHandler struct {
    *handlers.BaseAPIHandler
}

type GeminiCLIAPIHandler struct {
    *handlers.BaseAPIHandler
}
```

### GeminiAPIHandler 方法

| 方法 | 签名 |
|---|---|
| `NewGeminiAPIHandler` | `func(apiHandlers *handlers.BaseAPIHandler) *GeminiAPIHandler` |
| `GeminiHandler` | `func(h *GeminiAPIHandler) GeminiHandler(c *gin.Context)` |
| `GeminiGetHandler` | `func(h *GeminiAPIHandler) GeminiGetHandler(c *gin.Context)` |
| `GeminiModels` | `func(h *GeminiAPIHandler) GeminiModels(c *gin.Context)` |
| `Models` | `func(h *GeminiAPIHandler) Models() []map[string]any` |
| `HandlerType` | `func(h *GeminiAPIHandler) HandlerType() string` |

### GeminiCLIAPIHandler 方法

| 方法 | 签名 |
|---|---|
| `NewGeminiCLIAPIHandler` | `func(apiHandlers *handlers.BaseAPIHandler) *GeminiCLIAPIHandler` |
| `CLIHandler` | `func(h *GeminiCLIAPIHandler) CLIHandler(c *gin.Context)` |
| `Models` | `func(h *GeminiCLIAPIHandler) Models() []map[string]any` |
| `HandlerType` | `func(h *GeminiCLIAPIHandler) HandlerType() string` |

---

## sdk/translator — 格式转换

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/translator`

### 常量

```go
const (
    FormatOpenAI          Format = "openai"
    FormatOpenAIResponse  Format = "openai-response"
    FormatClaude          Format = "claude"
    FormatGemini          Format = "gemini"
    FormatGeminiCLI       Format = "gemini-cli"
    FormatCodex           Format = "codex"
    FormatAntigravity    Format = "antigravity"
)

type Format string
func FromString(v string) Format
func (f Format) String() string
```

### 核心类型

#### `RequestEnvelope` / `ResponseEnvelope`
```go
type RequestEnvelope struct {
    Format Format
    Model  string
    Stream bool
    Body   []byte
}

type ResponseEnvelope struct {
    Format Format
    Model  string
    Stream bool
    Body   []byte
    Chunks [][]byte
}
```

#### `RequestTransform` / `ResponseTransform`
```go
type RequestTransform func(model string, rawJSON []byte, stream bool) []byte

type ResponseStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte
type ResponseNonStreamTransform func(ctx context.Context, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte
type ResponseTokenCountTransform func(ctx context.Context, count int64) []byte

type ResponseTransform struct {
    Stream     ResponseStreamTransform
    NonStream  ResponseNonStreamTransform
    TokenCount ResponseTokenCountTransform
}
```

#### `Pipeline`
```go
type Pipeline struct { /* unexported */ }
func NewPipeline(registry *Registry) *Pipeline
func (p *Pipeline) TranslateRequest(ctx context.Context, from, to Format, req RequestEnvelope) (RequestEnvelope, error)
func (p *Pipeline) TranslateResponse(ctx context.Context, from, to Format, resp ResponseEnvelope, originalReq, translatedReq []byte, param *any) (ResponseEnvelope, error)
func (p *Pipeline) UseRequest(mw RequestMiddleware)
func (p *Pipeline) UseResponse(mw ResponseMiddleware)
```

#### `Registry`
```go
type Registry struct { /* unexported */ }
func Default() *Registry
func NewRegistry() *Registry
func (r *Registry) Register(from, to Format, request RequestTransform, response ResponseTransform)
func (r *Registry) HasResponseTransformer(from, to Format) bool
func (r *Registry) TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte
func (r *Registry) TranslateStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte
func (r *Registry) TranslateNonStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte
func (r *Registry) TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte
```

### 函数

```go
func Register(from, to Format, request RequestTransform, response ResponseTransform)
func HasResponseTransformer(from, to Format) bool
func HasResponseTransformerByFormatName(from, to Format) bool       // v6.3.1
func TranslateRequest(from, to Format, model string, rawJSON []byte, stream bool) []byte
func TranslateRequestByFormatName(from, to Format, model string, rawJSON []byte, stream bool) []byte  // v6.3.1
func TranslateStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte
func TranslateStreamByFormatName(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte  // v6.3.1
func TranslateNonStream(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte
func TranslateNonStreamByFormatName(ctx context.Context, from, to Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte  // v6.3.1
func TranslateTokenCount(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte
func TranslateTokenCountByFormatName(ctx context.Context, from, to Format, count int64, rawJSON []byte) []byte  // v6.3.1
```

### Handler / Middleware 类型

```go
type RequestHandler func(ctx context.Context, req RequestEnvelope) (RequestEnvelope, error)
type ResponseHandler func(ctx context.Context, resp ResponseEnvelope) (ResponseEnvelope, error)
type RequestMiddleware func(ctx context.Context, req RequestEnvelope, next RequestHandler) (RequestEnvelope, error)
type ResponseMiddleware func(ctx context.Context, resp ResponseEnvelope, next ResponseHandler) (ResponseEnvelope, error)
```

---

## sdk/translator/builtin — 内置转换器

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/translator/builtin`

```go
func Registry() *sdktranslator.Registry
func Pipeline() *sdktranslator.Pipeline
```

---

## sdk/access — 请求认证

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/access`

### 常量

```go
const (
    AccessProviderTypeConfigAPIKey = "config-api-key"
    DefaultAccessProviderName     = "config-inline"
)
```

### 类型

#### `AccessConfig`
```go
type AccessConfig struct {
    Providers []AccessProvider `yaml:"providers,omitempty" json:"providers,omitempty"`
}
```

#### `AccessProvider`
```go
type AccessProvider struct {
    Name    string         `yaml:"name" json:"name"`
    Type    string         `yaml:"type" json:"type"`
    SDK     string         `yaml:"sdk,omitempty" json:"sdk,omitempty"`
    APIKeys []string       `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
    Config  map[string]any `yaml:"config,omitempty" json:"config,omitempty"`
}
```

#### `Manager`
```go
type Manager struct { /* unexported */ }

func NewManager() *Manager
func (m *Manager) Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError)
func (m *Manager) SetProviders(providers []Provider)
func (m *Manager) Providers() []Provider
```

#### `Provider` (interface)
```go
type Provider interface {
    Identifier() string
    Authenticate(ctx context.Context, r *http.Request) (*Result, *AuthError)
}
```

#### `Result`
```go
type Result struct {
    Provider  string
    Principal string
    Metadata  map[string]string
}
```

#### `AuthError`
```go
type AuthError struct {
    Code       AuthErrorCode
    Message    string
    StatusCode int
    Cause      error
}
func (e *AuthError) Error() string
func (e *AuthError) HTTPStatusCode() int
func (e *AuthError) Unwrap() error
```

#### `AuthErrorCode`
```go
type AuthErrorCode string
const (
    AuthErrorCodeNoCredentials     AuthErrorCode = "no_credentials"
    AuthErrorCodeInvalidCredential AuthErrorCode = "invalid_credential"
    AuthErrorCodeNotHandled        AuthErrorCode = "not_handled"
    AuthErrorCodeInternal          AuthErrorCode = "internal_error"
)
```

### 函数

```go
func RegisterProvider(typ string, provider Provider)
func UnregisterProvider(typ string)                                    // v6.8.9
func MakeInlineAPIKeyProvider(keys []string) *AccessProvider           // v6.8.9
func NewNoCredentialsError() *AuthError
func NewInvalidCredentialError() *AuthError
func NewNotHandledError() *AuthError
func NewInternalAuthError(message string, cause error) *AuthError       // v6.8.9
func IsAuthErrorCode(authErr *AuthError, code AuthErrorCode) bool       // v6.8.9
func RegisteredProviders() []Provider                                   // v6.8.9
```

---

## sdk/auth — OAuth 认证

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/auth`

### 变量

```go
var ErrRefreshNotSupported = errors.New("cliproxy auth: refresh not supported")
```

### 认证器类型

```go
type AntigravityAuthenticator struct{}
type ClaudeAuthenticator struct{ CallbackPort int }
type CodexAuthenticator struct{ CallbackPort int }
type GeminiAuthenticator struct{}
type KimiAuthenticator struct{}       // v6.8.0
```

### Authenticator 接口

```go
type Authenticator interface {
    Provider() string
    Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error)
    RefreshLead() *time.Duration
}
```

### 构造函数

```go
func NewAntigravityAuthenticator() Authenticator       // v6.5.0
func NewClaudeAuthenticator() *ClaudeAuthenticator
func NewCodexAuthenticator() *CodexAuthenticator
func NewGeminiAuthenticator() *GeminiAuthenticator
func NewKimiAuthenticator() Authenticator              // v6.8.0
func NewFileTokenStore() *FileTokenStore
func NewManager(store coreauth.Store, authenticators ...Authenticator) *Manager
```

### 认证器方法

| 类型 | 方法 |
|---|---|
| AntigravityAuthenticator | `Provider()`, `Login()`, `RefreshLead()` |
| ClaudeAuthenticator | `Provider()`, `Login()`, `RefreshLead()` |
| CodexAuthenticator | `Provider()`, `Login()`, `RefreshLead()` |
| GeminiAuthenticator | `Provider()`, `Login()`, `RefreshLead()` |
| KimiAuthenticator | `Provider()`, `Login()`, `RefreshLead()` |
| FileTokenStore | `Save()`, `Delete()`, `List()`, `SetBaseDir()` |
| Manager | `Login()`, `Register()`, `SetStore()` |
| EmailRequiredError | `Error()` |
| ProjectSelectionError | `Error()`, `ProjectsDisplay()` |

### 函数

```go
func FetchAntigravityProjectID(ctx context.Context, accessToken string, httpClient *http.Client) (string, error)  // v6.5.48
func GetTokenStore() coreauth.Store
func RegisterTokenStore(store coreauth.Store)
```

### 类型

```go
type LoginOptions struct {
    NoBrowser    bool
    ProjectID    string
    CallbackPort int
    Metadata     map[string]string
    Prompt       func(prompt string) (string, error)
}

type EmailRequiredError struct{ Prompt string }
type ProjectSelectionError struct{ Email string; Projects []interfaces.GCPProjectProjects }
```

---

## sdk/logging — 请求日志

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/logging`

```go
type RequestLogger     = internallogging.RequestLogger
type FileRequestLogger = internallogging.FileRequestLogger
type StreamingLogWriter = internallogging.StreamingLogWriter

func NewFileRequestLogger(enabled bool, logsDir string, configDir string) *FileRequestLogger
func NewFileRequestLoggerWithOptions(enabled bool, logsDir string, configDir string, errorLogsMaxFiles int) *FileRequestLogger  // v6.7.40
```

---

## sdk/proxyutil — 代理配置

**导入路径**: `github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil`

### 常量

```go
const (
    ModeInherit  Mode = iota  // 无显式代理配置
    ModeDirect                // 出站请求必须绕过代理
    ModeProxy                 // 配置了具体代理 URL
    ModeInvalid               // 代理设置格式错误或不支持
)
type Mode int
```

### 类型

```go
type Setting struct {
    Raw  string
    Mode Mode
    URL  *url.URL
}
```

### 函数

```go
func Parse(raw string) (Setting, error)
func NewDirectTransport() *http.Transport
func BuildHTTPTransport(raw string) (*http.Transport, Mode, error)
func BuildDialer(raw string) (proxy.Dialer, Mode, error)
```

---

## CPA Gateway 对 SDK 的使用映射

| SDK 包 | CPA Gateway 中的用途 | 隔离位置 |
|---|---|---|
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

## 版本差异（v6.10.8 → v6.10.9）

**v6.10.9 与 v6.10.8 相比，无新增 API。**

以下为历史版本累积的特性回顾：

| 特性 | 版本 | 说明 |
|---|---|---|
| `RequestedModelAliasFromContext` / `WithRequestedModelAlias` | v6.10.7 | 用量 sink 可获取客户端请求的模型名 |
| `DownstreamWebsocket` / `WithDownstreamWebsocket` | v6.8.19 | 支持下游 WebSocket 连接 |
| `StreamResult` | v6.8.22 | 流式结果包含上游 HTTP headers |
| `ResponsesWebsocket` | v6.8.19 | OpenAI Responses WebSocket 端点 |
| `Compact` (Responses) | v6.7.36 | OpenAI Responses 压缩端点 |
| `ImagesEdits` / `ImagesGenerations` | v6.9.32 | OpenAI 图像端点 |
| `AntigravityAuthenticator` | v6.5.0 | Antigravity OAuth |
| `KimiAuthenticator` | v6.8.0 | Kimi OAuth |
| `PayloadFilterRule` | v6.7.39 | 请求体过滤规则 |
| `OAuthModelAlias` | v6.7.0 | OAuth 模型别名配置 |
| `StreamingConfig` | v6.6.49 | 流式配置 |
| `NewFileRequestLoggerWithOptions` | v6.7.40 | 可配置错误日志保留数 |
| `NewManager` (ManagementTokenRequester) | v6.6.64 | 限制性管理 token 请求接口 |
| 各种 `ByFormatName` 函数 | v6.3.1 | 带格式名称的转换接口 |
