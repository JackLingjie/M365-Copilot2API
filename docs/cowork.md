# Cowork 模型发现与文本聊天

Cowork 使用独立的 HTTP/SSE 服务，不走 Copilot Chat 的 ChatHub WebSocket。启用后，网关读取账号实际的 `GET /v1/models` 列表，并将每个上游 ID 暴露为 `cowork-<id>`。新增模型无需维护硬编码映射；可用性仍取决于账号权限、地区、上游配额及协议兼容性。

## 配置

先通过现有账号管理流程登录具有 Cowork 访问权限的 M365 账号。在浏览器中打开 Cowork，在开发者工具 Network 面板找到发往 `/v1/messages`、`/v1/subscribe` 或 `/v1/models` 的请求：

- `M365_COWORK_BASE_URL`：该请求 URL 的 HTTPS origin，不包含 `/v1/...`、查询参数或会话 ID。服务区域因账号而异，没有通用默认地址。留空时关闭 Cowork。
- `M365_COWORK_CONTAINER_CONFIG`：该请求的 `x-container-config` 值。网关会删除捕获时的 `model` 和 `reasoningEffort`，由每次 API 调用重新选择；其他配置原样保留。
- `M365_COWORK_TIMEZONE`：可选，例如 `Asia/Shanghai`，传给 `x-copilot-timezone`。

不要保存或提交浏览器的 Authorization、Cookie、完整 HAR 或账号数据。Cowork 凭据由网关通过账号已有的 refresh token 获取，使用独立 scope `6ab48b67-cd74-4ad4-81af-5932984589be/.default`。这仍要求账号已有 Cowork 访问权限和对应资源授权；ChatHub 能登录不代表 Cowork 一定可用。轮换的 refresh token 使用原有加密存储，Cowork access token 仅缓存在内存，不替换 ChatHub access token。

这些变量是服务启动配置，不是运行时模型映射。当前服务只配置一个 Cowork origin；多账号应使用同一区域的 origin，跨区域账号请使用独立实例。

## 查看模型和选择账号

```sh
curl http://127.0.0.1:4141/v1/models \
  -H 'Authorization: Bearer YOUR_API_KEY'
```

`data` 和 `models` 返回同一份列表。Cowork 条目的 `owned_by` 是 `microsoft-365-cowork`，包含上游模型 ID、显示名、provider、默认及可用思考强度。

模型列表按账号和 origin 缓存 5 分钟。未指定账号时，Cowork 目录和聊天都使用存储顺序中的第一个启用调度且具有 refresh token 的账号，不进行 ChatHub 健康探测或账号轮询。多账号可用 `GET /v1/models?account_id=ACCOUNT_ID` 查看指定账号，聊天请求用 `accountId` 选择同一账号。管理员目录 `/api/admin/models` 同样支持 `account_id`，模型连通测试 `/api/admin/models/test` 支持 Cowork。

发现失败时原有 ChatHub 目录仍可用，响应顶层 `cowork_discovery_error` 给出简短错误，Cowork 条目不会凭空添加。未知或不属于当前账号的 `cowork-*` 模型返回错误，不回退到其他 ChatHub 模型。

2026-09-17 在一个有权限的账号上实际发现并验证的示例：

| API 模型 ID | Cowork 显示名称 | 支持的思考强度 | 默认值 |
| --- | --- | --- | --- |
| `cowork-gpt-5.5` | GPT-5.5 | low, medium, high, xhigh | xhigh |
| `cowork-opus-5` | Opus 5 | low, medium, high, xhigh, max | medium |
| `cowork-sonnet-5` | Sonnet 5 | low, medium, high, xhigh, max | medium |
| `cowork-melon` | Fable 5.1 | low, medium, high, xhigh, max | medium |
| `cowork-auto` | Auto | 由上游选择，省略思考强度 | 由上游决定 |

这张表是观察样本，代码不依赖这些模型名称。`fable-5.1`（目录兼容别名）和 `cowork-fable-5.1`（请求兼容别名）只有在实际目录包含 `melon` 时才可用。`gpt-5.5` 和 `cowork-gpt-5.5` 是不同渠道；名称来自各自目录，不应据此推断它们底层实现相同。

## 调用

```sh
curl http://127.0.0.1:4141/v1/chat/completions \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "cowork-opus-5",
    "reasoning_effort": "low",
    "messages": [{"role":"user","content":"你好"}],
    "stream": true
  }'
```

`reasoning_effort` 或 `reasoning.effort` 按所选模型的实际能力校验，然后通过 `x-container-config` 的 `reasoningEffort` 传递。省略时使用上游默认值。Auto 会彻底移除 `model` 字段，避免误用捕获时的选择。

OpenAI Chat Completions 支持实时文本 SSE。现有 Responses 适配器支持文本请求及流式转换；Anthropic Messages 支持文本请求，流式格式目前在完整回复生成后输出。

## 范围与限制

- 每次 API 调用创建一个新的 Cowork 云端任务，文本历史按角色合并后发送。客户端需要提供历史；不复用 ChatHub 会话。云端任务会在 Cowork 界面出现，不自动删除。
- 当前支持文本聊天，不支持客户端 function/tool calls、tool results、图片、附件或结构化输出。这些请求会明确报错；目录不宣称工具、视觉或未知的上下文长度能力。因此还不能当作 Claude Code 等工具密集型客户端的完整后端。
- 网关给 Cowork 发送仅在聊天中回答、不要调用工具的指令；这不是上游工具权限的强制关闭。Cowork 的内置连接器和工作区能力仍由上游账号/容器配置控制。
- `max_tokens`、`max_completion_tokens`、temperature、top_p 等生成参数没有已验证的上游等价字段，暂不转发；token usage 为本地估算，不是账单数据。
- 使用现有每账号并发限制和聊天超时。Cowork 请求尊重账号绑定代理，认证与模型发现支持取消。流断开后只恢复订阅，不自动重复提交消息或跨账号重试，防止重复创建云端任务。

## 验证依据

模型 schema、Auto 行为、模型 ID 和 `reasoningEffort` 字段来自 Cowork 实际网页接口及请求。实测上述四个模型及 Auto 均返回预期文本；独立读取任务详情，确认 `activeModel`、`modelSelection` 和 `reasoningEffort`。测试不依靠模型自报身份。

单元测试覆盖发现新模型及容器后缀 ID、账号缓存隔离、有效/无效 effort、Auto 清除旧配置、未知模型不误路由、管理端测试、SSE 恢复去重、取消、鉴权/限流错误和禁止携带凭据跟随重定向。运行项目要求的 `go test ./...`、`go vet ./...` 和 `go build ./...`。
