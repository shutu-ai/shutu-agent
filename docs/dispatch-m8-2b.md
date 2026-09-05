# M8-2b 派发：anthropic provider（Messages API 流式 + tool use + thinking reasoning 回传）

> 里程碑 M8 第二段后半（ADR `docs/decisions/2026-08-20-m8-message-model.md`）。本文件是 **M8-2b** 契约：`internal/llm/anthropic` provider（Anthropic Messages API 流式 + 工具调用 + thinking reasoning 跨 provider 回传）+ 注册接线。前置：M8-1（content parts Message）与 M8-2a（Provider/Registry/config/registerLLM，只读）已验收。参照源：M7 的 `internal/web/deepseek.go`（Anthropic 兼容 HTTP 客户端：headers/key/redirect 策略复用）。

## 0. 纪律

- **不改 `internal/loop/loop.go` 的 turn/step 结构**（D4）；主循环串行（D5）；**零新第三方依赖**；CGO-free；原有测试全绿。
- 凭证 env-only（纪律 6）：`ANTHROPIC_API_KEY`。
- 每模块阶段提交（commit message 前缀 `M8-2`）。

## 1. 范围

**做**：`internal/llm/anthropic` provider（`ID()="anthropic"`），实现 text + 工具调用 + thinking（reasoning）流式与回传；注册接线（registerLLM 加 anthropic 注册 + /llm-status 显示）；config anthropic 段（M8-2a 已留占位，校正默认值）。

**不做（本段）**：图片输入（M8-3）；`max_tokens`/`temperature`/`stop` 等高级参数（用 provider 默认，接口未暴露）。

## 2. Anthropic Messages API 契约（internal/llm/anthropic）

### 2.1 端点与请求

```
POST {baseURL}/messages        # 默认 https://api.anthropic.com/v1
Headers:
  x-api-key: <key>
  anthropic-version: 2023-06-01
  content-type: application/json
  accept: application/json
```

请求体（stream=true）：
```json
{
  "model": "<model>",
  "max_tokens": 4096,
  "system": "<system prompt 或省略>",
  "messages": [ {"role": "user"|"assistant", "content": [blocks...]} ],
  "tools": [ {"name": "...", "description": "...", "input_schema": {...}} ],
  "stream": true
}
```

**序列化规则**（`toWireMessages(req llm.ChatRequest)`）：
1. **system 提取**：遍历 `req.Messages`，`RoleSystem` 消息的 `Text()` 拼成顶层 `system` 字段；system 消息不进入 `messages` 数组。
2. **user 消息**：content 转 blocks（本段仅 text block → `{"type":"text","text":...}`；M8-3 加 image）。
3. **assistant 消息**：content 转 blocks，**顺序保留**（dsh 范式：reasoning 在 text 前）：
   - `BlockReasoning` → `{"type":"thinking","thinking":<Text>}`
   - `BlockText` → `{"type":"text","text":<Text>}`
   - `m.ToolCalls` → `{"type":"tool_use","id":<CallID>,"name":<Name>,"input":<解析后的 Arguments JSON>}`（Arguments 是 raw JSON 字符串 → `json.Unmarshal` 成 `map[string]any`，解析失败则 `{"_raw": <string>}` 兜底）
4. **tool 结果消息**（`RoleTool`）→ 归入前一条 user 消息的 content：`{"type":"tool_result","tool_use_id":<ToolCallID>,"content":<Output>}`（同一轮多个 tool result 追加到同一条 user 消息；实现上把连续 tool 结果合并到其前一个 user 消息的 content 尾部）。
5. **空 content 处理**：user 消息 content 为空数组时发 `{"type":"text","text":"(no output)"}`（Anthropic 拒绝空 content，照 dsh 同款）。

### 2.2 SSE 流解析（`sse.go`，事件流 → llm.StreamEvent）

Anthropic stream 是 `event:` + `data:` 行（区别于 OpenAI 的纯 data: JSON）。用 `bufio.Scanner`/Reader 逐行解析，按 `event:` 字段区分类型，`data:` 为 JSON。

| event | data 内容 | 映射 |
|---|---|---|
| `message_start` | `{message:{...}}` | 忽略（或记 request id） |
| `content_block_start` | `{index, content_block:{type, ...}}` | `type=="tool_use"` → 新建 tool call（`id`/`name` 在此）；其他忽略 |
| `content_block_delta` | `{index, delta:{type, ...}}` | `text_delta`→`StreamTextDelta(Text=text)`；`thinking_delta`→`StreamReasoningDelta(Text=thinking)`；`input_json_delta`→追加 `partial_json` 到该 index 的 tool call arguments（**不产出事件**，累积）；`signature_delta`→忽略 |
| `content_block_stop` | `{index}` | 忽略 |
| `message_delta` | `{delta:{stop_reason}, usage}` | 记录 stop_reason |
| `message_stop` | `{}` | 终止：返回 `StreamFinish{FinishReason: mapStopReason(...), ToolCalls: 累积, Reasoning: 累积}` |
| 错误 | `{type:"error", error:{...}}` | 返回错误（包装 `anthropic: provider error`） |

**工具调用累积**：`content_block_start`（tool_use）建调用（id/name）；`input_json_delta` 的 `partial_json` 字符串按序拼接成 arguments（照现有 deepseek 的 accumulateToolCall 同款逻辑，用 index 关联）。

**stop_reason 映射**：`end_turn`→`stop`；`tool_use`→`tool_calls`；`max_tokens`→`max-tokens`；其他→原样。

**流终止**：SSE `message_stop` 事件后返回 `StreamFinish`；EOF 前无 `message_stop` → 报错（`anthropic: stream ended without message_stop`）。

### 2.3 Provider 方法

```go
type Config struct {
    BaseURL string // 默认 https://api.anthropic.com/v1
    APIKey  string // ANTHROPIC_API_KEY（env-only，组合根传入）
    Model   string // 默认 claude-sonnet-4-5（与 config 默认一致）
    MaxTokens int  // 默认 4096
    HTTPClient *http.Client // 可选
}
func New(cfg Config) *anthropicProvider
// ID() = "anthropic"
// Available() = APIKey 非空 且 BaseURL 可解析
// Stream(ctx, req) — 序列化 + POST(stream) + 返回 reader（httptest 可测）
```

**HTTP 细节**（照 M7 `internal/web/deepseek.go` 复用）：
- 重定向：不跟随（自定义 CheckRedirect 返回错误 → `anthropic: redirect blocked`），3xx 不当成功。
- ctx 取消贯穿请求与读取。
- 非 2xx：读 body（1MiB 有界）解析错误 JSON（`{"error":{"message":...}}` 或 `{"message":...}`）→ 返回错误。

## 3. 注册接线（cmd/sta/llm.go 修改）

- `registerLLM`：`if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" { reg.Register(anthropic.New(anthropic.Config{APIKey: key, BaseURL: a.cfg.LLM.Anthropic.BaseURL, Model: a.cfg.LLM.Anthropic.Model})) }`（照 openai 同款：key 非空才注册）。
- `llmCredentialEnv`：`"anthropic"` → `"ANTHROPIC_API_KEY"`。
- `llmProviderModel`：`"anthropic"` → `cfg.LLM.Anthropic.Model`。
- `/llm-status` 自动涵盖（遍历 registry）。
- **config anthropic 默认值校正**：确认 `DefaultAnthropicBaseURL="https://api.anthropic.com/v1"`、`DefaultAnthropicModel="claude-sonnet-4-5"` 与实际实现一致（M8-2a 占位，本段定稿）。

## 4. 测试要求（httptest 假 Anthropic SSE 服务，不联网）

`internal/llm/anthropic`：
1. **请求体断言**：POST `/v1/messages`、headers（x-api-key/anthropic-version/content-type）、body 含 model/max_tokens/system（从 RoleSystem 提取）/messages（user→text block、assistant→thinking+text+tool_use、tool 结果→tool_result 合并）。
2. **流解析**：构造 SSE 事件序列（content_block_start tool_use → input_json_delta 多段 → thinking_delta → text_delta → tool_use stop → message_delta stop_reason=tool_use → message_stop）→ reader 产出 `StreamReasoningDelta`/`StreamTextDelta`/`StreamFinish{ToolCalls: 参数拼接完整, Reasoning: 累积}`。
3. **终止**：无 message_stop 提前 EOF → 报错。
4. **错误**：非 2xx + error JSON → 错误含服务端 message。
5. **Available**：key 空 / base_url 非法。
6. **回传往返**：assistant 消息含 reasoning（BlockReasoning）+ tool_use 序列化正确（thinking/text/tool_use 顺序）。

`cmd/sta`：ANTHROPIC_API_KEY 非空时注册 anthropic；选中 anthropic 可用；/llm-status 显示。
`internal/config`：anthropic 默认值断言（已有，补 model/base_url 与实现一致）。

## 5. 提交与报告

- 每模块阶段提交（`M8-2: ...`）：序列化 → SSE 解析 → provider → 注册接线 → 测试。
- 完成后 `go vet ./...` / `go test -count=1 ./...` / `go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
