# M8-3a 派发：图片附件存储 + /attach 命令 + 多模态 config + inputModalities 声明

> 里程碑 M8 第三段（多模态，ADR `docs/decisions/2026-08-20-m8-message-model.md`）。本文件是 **M8-3a（前半）** 契约：`internal/attachment` 附件存储、`/attach` 命令、多模态 config（默认关 D10）、inputModalities 声明。**M8-3b（provider 序列化图片 + offload 上限 + fail-closed）** 在后续派发。前置：M8-1 已定义 `ContentBlock.Image`/`ImageRef` 与 `userMessageData.Content` 预留（只读）。

## 0. 纪律

- **不改 `internal/loop/loop.go` 的 turn/step 结构**（D4）；主循环串行（D5）；**零新第三方依赖**；CGO-free；原有测试全绿。
- **默认关（D10）**：`llm.multimodal.enabled=false` 时 `/attach` 不可用。
- 凭证/数据安全：附件文件是用户本地图片，不做任何外传（只在模型请求时转 data URL，env 不外泄）。
- 每模块阶段提交（commit message 前缀 `M8-3`）。

## 1. 范围

**做**：
1. `internal/attachment`：`Store`（`SaveImage`/`Read` + 校验：类型/大小）。
2. config：`llm.multimodal.enabled`（默认 false）+ `llm.model_input_modalities`（默认 `text`）+ 图片上限。
3. `cmd/pa`：`/attach <path>` 命令（启用时）：校验文件 → `SaveImage` → 落 `user/message` 事件（`Content` 含 image block，只存 `ImageRef`）→ 返回附件 id 提示。
4. `inputModalities` 声明 + `/llm-status` 显示 modalities（`text` / `text,image`）。
5. 测试。

**不做（本段）**：provider 序列化图片（M8-3b）；offload 20MiB 上限最老替换（M8-3b）；图片 fail-closed（M8-3b）。

## 2. internal/attachment 契约

```go
// Package attachment 提供图片附件存储（M8-3，ADR 2026-08-20-m8-message-model.md）。
// 图片文件持久在 <data_dir>/attachments/，会话日志只存 ImageRef 引用（dsh 7078918
// 范式：落库只存引用，请求时才转 data URL）。零新依赖。
package attachment

// 支持的图片媒体类型（dsh 同款）。
var SupportedMediaTypes = map[string]string{
    ".png":  "image/png",
    ".jpg":  "image/jpeg",
    ".jpeg": "image/jpeg",
    ".webp": "image/webp",
    ".gif":  "image/gif",
}

type Store struct{ dir string }

// NewStore 创建/打开附件目录（<dir> 不存在则 mkdir）。dir 空 → 默认 <data_dir>/attachments。
func NewStore(dir string) (*Store, error)

// SaveImage 把图片字节写入附件存储：校验 mediaType 受支持、data 非空且 ≤ maxBytes，
// 生成随机 id（hex），写 <dir>/<id><ext>，返回 ImageRef（ID/MediaType/Bytes/Width/Height/Path；
// 宽高不解析记 0，M8 裁剪）。超限返回错误（fail-closed）。
func (s *Store) SaveImage(mediaType string, data []byte, maxBytes int) (llm.ImageRef, error)

// Read 按 ImageRef.Path 读回原始字节。Path 缺失/不可读返回错误。
func (s *Store) Read(ref llm.ImageRef) ([]byte, error)
```

- **说明**：`attachment` 依赖 `internal/llm`（`ImageRef`）；`llm` **不**依赖 `attachment`（provider 只拿 `ImageRef.Path` 自行读文件，保持 llm 纯接缝——见 M8-3b）。若担心反向依赖，`SaveImage` 返回的 `ImageRef` 由调用方（cmd/pa）组装即可，`attachment` 可只返回 `(id string, err error)` 让调用方构造——**由你决定**，原则是 `llm ← attachment` 单向、`llm` 不反向依赖。

## 3. config 契约（internal/config）

```go
// 在 LLMConfig 内新增：
type MultimodalConfig struct {
    // Enabled 门：false 时 /attach 不可用、图片 block 不序列化（D10）。
    Enabled bool `yaml:"enabled"`
    // MaxImageBytes 单图原始字节上限（默认 3.5MiB）。
    MaxImageBytes int `yaml:"max_image_bytes"`
}
// LLMConfig 增加：
//   ModelInputModalities string `yaml:"model_input_modalities"` // "text" | "text,image"（exact-model 能力声明）
//   Multimodal MultimodalConfig `yaml:"multimodal"`
```

- 默认：`multimodal.enabled=false`；`model_input_modalities` 缺省 `text`；`multimodal.max_image_bytes` 缺省 3.5MiB；批量上限为 20 张/100MiB，解码像素上限 40M、单边上限 2000px。
- config.yaml `llm:` 段补 `model_input_modalities` 与 `multimodal:` 子段注释（含"默认关 D10 / 图片只存引用"说明）。

## 4. cmd/pa 接线契约

- `app` 增加 `attachStore *attachment.Store` 字段。
- `registerAttachments()`（或并入 registerLLM 之后）：`llm.multimodal.enabled` 时创建 `attachment.NewStore(filepath.Join(cfg.DataDir, "attachments"))` 存入 `a.attachStore`；disabled 不创建（D10）。
- `/attach <path>` 命令（`command` switch 增加，照 `/llm-status` 风格）：
  1. disabled → 错误 "multimodal disabled (llm.multimodal.enabled=false)"。
  2. `os.ReadFile(path)`（路径由用户提供，读文件属命令路径，D5 串行）。
  3. 校验：扩展名在 `SupportedMediaTypes`；字节数 ≤ `max_image_bytes`；否则 fail-closed 错误。
  4. `a.attachStore.SaveImage(mediaType, data, maxBytes)` → ImageRef。
  5. 落 `user/message` 事件：`a.log.Append(session.EventUserMessage, session.NewUserMessageWithBlocks("", []llm.ContentBlock{{Kind: llm.BlockImage, Image: ref}}))`——**M8-1 的 NewUserMessage 若无 blocks 变参，本段扩展一个 `NewUserMessageWithBlocks`（照 NewAssistantMessage 变参同款风格）**；折叠后成为带 image block 的 user 消息。
  6. 输出提示：`attached <path> as image <id> (png, N bytes)`。
- `/llm-status`：modalities 行改为 `modalities: <cfg.LLM.ModelInputModalities>`（text / text,image）；multimodal 行 `multimodal: enabled|disabled`。
- `printHelp` 增 `/attach <path>` 行。

## 5. 测试要求

- `internal/attachment`：SaveImage 校验（坏扩展名/空数据/超限 fail-closed）；Save 后 Read 往返一致；目录创建；id 唯一（两次 Save 不同 id）。
- `internal/config`：multimodal 默认（enabled=false、model_input_modalities=text、max_image_bytes 默认）；解析覆盖。
- `cmd/pa`：`/attach` disabled → 错误；enabled → 校验通过落 user/message 事件（事件 data 含 image block 且只有 ImageRef 无字节）；坏扩展名/超限/不存在文件 → fail-closed 错误；`/llm-status` 显示 modalities + multimodal 状态。

## 6. 提交与报告

- 每模块阶段提交（`M8-3: ...`）。
- 完成后 `go vet ./...` / `go test -count=1 ./...` / `go build ./...` 全绿再报告。
- 报告：改动文件清单、实现决策（对照本契约的偏离）、跑过的命令、测试结果。
