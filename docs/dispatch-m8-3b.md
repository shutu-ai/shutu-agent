# M8-3b 派发：provider 图片序列化 + offload 上限最老替换 + 图片 fail-closed

> 里程碑 M8 第三段后半（ADR `docs/decisions/2026-08-20-m8-message-model.md`）。本文件是 **M8-3b** 契约：三个 provider 序列化图片（deepseek/openai parts array + anthropic image block）、请求级图片预算 offload（最老替换）、纯文本模型遇图片 fail-closed。前置：M8-1（ImageRef/BlockImage）、M8-2（provider 注册表）、M8-3a（attachment 存储 + /attach + config 多模态，只读）已验收。

## 0. 纪律

- **不改 `internal/loop/loop.go` 的 turn/step 结构**（D4）；主循环串行（D5）；**零新第三方依赖**；CGO-free；原有测试全绿。
- 默认关（D10）已由 M8-3a 保证（enabled=false 无图片可 attach）；本段在 enabled 且收到图片时工作。
- 图片字节只在请求序列化时读文件转 base64，绝不落日志/内存常驻。
- 每模块阶段提交（commit message 前缀 `M8-3`）。

## 1. 范围

**做**：
1. `internal/llm` 公共 helper：`OffloadRequestImages(msgs, maxBytes)` + `HasImage` 检查。
2. `deepseek`/`openai`：`toWireMessage` 支持图片 → content parts array（`image_url` + data URL）；无图片保持现有 string 行为。
3. `anthropic`：`textBlocks` 支持图片 → `{type:"image", source:{type:"base64", media_type, data}}`。
4. 图片 fail-closed：`model_input_modalities` 不含 image 时遇图片 → 序列化报错（不静默忽略）。
5. provider Config 增 `SupportsImages bool` / `MaxRequestImageBytes int`；组合根（cmd/sta registerLLM）从 config 传入。
6. 测试。

**不做（本段）**：图片生成/输出；宽高解码（ImageRef.Width/Height 仍 0）；`temperature` 等高级参数。

## 2. internal/llm 公共 helper 契约

```go
// offload.go（新文件）
// 占位符：被 offload 的图片替换为的文本（dsh OFFLOADED_IMAGE_TEXT 同款）。
const OffloadedImageText = "[image omitted]"

// OffloadRequestImages 对一条请求的 messages 施加图片字节预算（maxBytes 默认 20MiB，
// dispatch-m8-3b）：按消息历史顺序累计所有 image block 的 Bytes，超过预算的图片（从最
// 老开始）替换为 OffloadedImageText 文本 block（原地改 Content 的 block 列表，像
// truncateInjectorContext 一样）。不超预算则原样返回（无复制、无副作用）。
// maxBytes <= 0 视为无预算（不 offload）。
func OffloadRequestImages(msgs []llm.Message, maxBytes int) []llm.Message
```

**语义**：累计 = 历史顺序；`image block.Bytes` 计入（ImageRef.Bytes 已在 /attach 存实字节数）；替换图片 block → `{Kind: BlockText, Text: OffloadedImageText}`（保留其在 content 里的相对位置；同一消息多图逐个判断）。

## 3. 图片 fail-closed 契约

- provider 收到含图片消息，且 `SupportsImages == false` → `Stream` 返回错误（fail-closed，不静默忽略）：
  `fmt.Errorf("%s: model does not support image input (model_input_modalities=text)", p.ID())`
- 检查时机：`Stream` 内序列化前，对 `req.Messages` 先 `HasImage` 检查；通过后再 `OffloadRequestImages`，再序列化。
- 顺序：**先 fail-closed 检查，再 offload**（避免 offload 后图片被替换导致检查漏报）。

## 4. provider 序列化契约

### 4.1 deepseek / openai（OpenAI 兼容）

`toWireMessage` 的 `wireMessage.Content` 字段类型从 `string` 改为 `any`（`json:"content,omitempty"`），序列化：
- 无图片：`Content: m.Text()`（string，保持现有 wire 与测试不变）。
- 有图片：`Content: []any{ {"type":"text","text":<m.Text()>}, {"type":"image_url","image_url":{"url":"data:<mime>;base64,<data>"}}, ... }`（每个 image block 一个 image_url part；text part 在前）。

`data` 生成 helper（两 provider 共享思路，可放 internal/llm 或各包内）：
```go
// imageDataURL 读 ImageRef.Path 的字节并编码为 data URL。
// 读失败 → 错误（fail-closed，不静默丢图）。
func imageDataURL(ref llm.ImageRef) (string, error)
//   data:<mediaType>;base64,<base64(bytes)>
```
（`llm` 不依赖 attachment——provider 直接 `os.ReadFile(ref.Path)`，保持 M8-3a 的单向依赖纪律。）

**Config 扩展**（deepseek.Config 与 openai.Config 都加）：
```go
// SupportsImages 是模型输入模态（来自 config llm.model_input_modalities，组合根传入）。
// false（默认）时收到图片 fail-closed。
SupportsImages bool
// MaxRequestImageBytes 是请求图片字节预算（默认 20MiB）；offload 超限图片。
MaxRequestImageBytes int
```
- `New` 应用默认：`MaxRequestImageBytes <= 0 → 20MiB`。

### 4.2 anthropic

`textBlocks`（user 消息 content 转换）扩展：遍历 blocks 时
- `BlockText` → `{"type":"text","text":...}`
- `BlockImage` → `{"type":"image","source":{"type":"base64","media_type":<MediaType>,"data":<base64>}}`（读 Path）
- 空结果占位 `(no output)` 规则不变。

`anthropic.Config` 同样加 `SupportsImages` / `MaxRequestImageBytes`（默认 20MiB）。

## 5. 组合根接线（cmd/sta/llm.go 修改）

`registerLLM` 三处 `New(...)` 补传：
```go
SupportsImages: strings.Contains(a.cfg.LLM.ModelInputModalities, "image"),
MaxRequestImageBytes: a.cfg.LLM.Multimodal.MaxRequestImageBytes, // 默认 20MiB 由 New 兜底
```
- deepseek（恒注册）、openai/anthropic（按 key 注册）各传。

## 6. config 契约（internal/config）

`MultimodalConfig` 增加 `MaxRequestImageBytes int`（`yaml:"max_request_image_bytes"`，默认 20MiB）；config.yaml `llm.multimodal:` 子段补该字段注释（"请求级图片总预算，超限最老图片替换为占位符"）。

## 7. 测试要求

- `internal/llm`：OffloadRequestImages（不超限原样；超限最老替换；多图同消息；占位符位置正确；maxBytes<=0 不 offload）。
- `internal/llm/deepseek`：带图片请求 → 请求体 content 为 parts array（httptest 断言 image_url + data URL 前缀 `data:image/png;base64,`）；无图片 → 仍 string（回归）；SupportsImages=false 遇图片 → 错误；读图失败 → 错误。
- `internal/llm/openai`：同上（委托 deepseek 后由 deepseek 测试覆盖，openai 补一个带图走通）。
- `internal/llm/anthropic`：带图 user 消息 → `{type:"image",source:{base64}}` 断言；SupportsImages=false → 错误。
- `internal/config`：`max_request_image_bytes` 默认 20MiB + 解析。
- `cmd/sta`：registerLLM 传 SupportsImages/MaxRequestImageBytes（默认 deepseek 回归：modalities=text → SupportsImages=false）。
- 全项目门禁绿；loop.go 无改动（D4）。

## 8. 提交与报告

- 每模块阶段提交（`M8-3: ...`）：helper → deepseek/openai → anthropic → config/wiring → 测试。
- 完成后 `go vet ./...` / `go test -count=1 ./...` / `go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
