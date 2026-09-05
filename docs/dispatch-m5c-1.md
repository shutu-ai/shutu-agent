# M5c-1 实施派发消息（控制会话 → 实施会话）——compaction 折叠规则 + 接缝 + 基础 Provider + 剪枝

> 状态：已派发 2026-08-19（M5 拆四段：M5a ✅ → M5b ✅ → M5c 上下文压缩 → M5d 技能；本文件为 M5c 第一半）· 用法：把下文整段粘贴给新开的实施会话。

---

你的任务是实现 Go 项目 `D:\dev-projects\Agent\shutu-agent` 的 **M5c-1：compaction 折叠规则改造 + `internal/compaction` 接缝 + 基础 Provider + tool-result 剪枝 + 单元测试**。这是 M5c 的第一半（第二半由另一个会话做：/compact 命令 + 事件 + config + PreStep 接线）。你是实施会话。

**必读（先读这些）**：
1. `D:\dev-projects\Agent\shutu-agent\docs\dispatch-m5c.md` —— 背景契约，重点读「M5c 范围」第 1（接缝 + Provider + 剪枝）条和第 3（事件）条的**设计部分**。**本任务做第 1 条 + 折叠规则改造 + 测试**；第 2（PreStep 接线）、3（事件类型）、4（config）、`/compact` 命令是 M5c-2 的事。
2. `D:\dev-projects\Agent\shutu-agent\docs\decisions\2026-08-18-m5-agent-core.md` 的「决策 ③」（压缩语义、事件三连锁、遮蔽、配对边界）。
3. 现有代码：
   - `internal/session/session.go` —— **`derive()` 纯函数（第 158–201 行）是折叠规则改造点**：`compaction/summary` 标记的 `user/message` 带 `surfaceOp: {op: "replace", start, end}`，derive 遇到时跳过被遮蔽 seq 的旧事件、以摘要消息替代。日志仍追加式（D1，旧事件物理保留）。
   - `internal/llm/`（复用摘要模型）+ `internal/loop/loop.go`（PreStep 注入器容器，M5b-1 已升级——本任务不接线，只保证接缝可被 M5c-2 接）。
   - `internal/jobs/local.go`（truncateUTF8 模式，M5a-1）+ `internal/subagent/spawn.go`（结构参考）。
   - `internal/session/session_test.go`（derive 相关测试模式）。
4. `Agent.md` 第 10 节 D1–D10 纪律。

**实现内容**（严格按 dispatch-m5c.md）：

1. **折叠规则改造**（`internal/session`）：
   - 新增载荷支持：`user/message` 的 `userMessageData` 增加可选 `SurfaceOp *SurfaceReplace` 字段，`SurfaceReplace struct{ Op string; Start, End int64 }`（`Op == "replace"`）。`NewUserMessage` 现有签名不变（新签名或新构造 `NewUserMessageReplace(text string, start, end int64)` 均可，保持向后兼容——**原 `NewUserMessage` 调用方（loop）不改**）。
   - **`derive()` 改造**：遇到带 `surfaceOp.replace` 的 `user/message` 时，先记录被遮蔽范围（start/end seq），把摘要消息（该 user/message 的 Text）追加到结果，并跳过后续 seq ∈ [start, end] 的事件（直到 seq > end 恢复）。无 replace 标记的行为完全不变（原测试全绿）。
   - 边界：被遮蔽范围的 seq 是在**追加时**记录的（事件 Seq 单调递增）；derive 只按 seq 比较跳过，不解析被遮蔽事件内容。
   - 测试：`internal/session/session_test.go` 新增——带 replace 的 user/message 折叠后 = 摘要 + 未遮蔽尾部（被遮蔽 seq 的事件不出现）；无 replace 行为不变；遮蔽范围跨越 user/assistant/tool 混合事件；空摘要消息也被保留（作为折叠标记）；JSON 往返。

2. **`internal/compaction` 包——接缝（Service 定义）**（`service.go`）：
   ```go
   type Trigger string // pressure | context-overflow
   type Result struct {
       CompactionID  string
       Summary       string
       ShadowedRange [2]int64   // 被遮蔽 surface 位置跨度（首/尾 seq）
       ShadowedSeqs  []int64    // 被遮蔽的 surface 节点 seq（权威集合）
       ShadowedTokens int
   }
   type SessionLike interface {
       Events() []session.Event            // 读日志（只读，压缩不改日志物理内容——D1 追加式）
       Append(typ string, data any) (session.Event, error)
       DeriveHistory() []llm.Message       // 压缩前当前派生（用于摘要上下文）
   }
   type Engine interface {
       CompactIfNeeded(ctx context.Context, sess SessionLike, trigger Trigger) (*Result, error)
       CompactNow(ctx context.Context, sess SessionLike) (*Result, error)
       CompactRegion(ctx context.Context, sess SessionLike, start, end int64) (*Result, error)
   }
   ```
   - `SessionLike` 是压缩接缝对 session 的最小只读接口（D2：消费方只依赖接口）；`session.Log` 已满足。
   - 工具函数：`ToolPairingBalancedBefore(sess, seq) bool` / `ToolPairingBalancedAfter(sess, seq) bool`——检查某个 surface 位置两侧 assistant tool_calls 与其 tool/result 配对是否完整（不切断配对中间）。

3. **基础 Provider（默认，`basic.go`）**：
   - `BasicEngine`（`NewBasic(BasicOpts{Tokenizer, LLM, Model, TokenThreshold, RetainTurns})`）：
     - token 压力检测：估算当前 surface token 数（字符/词估算，零依赖——如 `len(bytes)/4` 近似或 rune 计数，选简单的并注释）。
     - `CompactIfNeeded`：估算超 `TokenThreshold` 触发；`context-overflow` 比 `pressure` 更激进（强制做一次有效平衡缩减）。
     - 保留尾部策略：保留最近 `RetainTurns` 个回合（`user/message` 数）；被遮蔽范围 = 除尾部外的 surface 前缀（用 `ToolPairingBalancedBefore/After` 校正到配对边界）。
     - 摘要生成：复用 `internal/llm`（`LLM.Stream` 或非流式一次调用）把被遮蔽范围的历史折叠成摘要文本；失败则返回错误（fail-open 由 M5c-2 接线处理，本层只返回错误）。摘要上下文 = `DeriveHistory()` 中被遮蔽部分。
     - `CompactNow`：低于压力也执行一次有效压缩（选最近 `RetainTurns` 之前的最大平衡前缀）。`CompactRegion`：对给定范围做配对校正后压缩。
   - **剪枝（`pruner.go`）**：`PruneToolResults(sess SessionLike, maxBytes int) (PruneResult, error)`——纯确定性（无模型），对超预算的 `tool/result` 做 head/middle/tail 截断替换（Unicode code point 边界，不切 rune），返回 `PruneResult{Replaced []seq, SavedBytes int}`。**本任务只实现函数 + 测试**；`compaction/prune` 事件由 M5c-2 落。
   - 测试：`internal/compaction/basic_test.go` + `pruner_test.go`——token 压力触发/不触发、retain_turns 尾部保留、被遮蔽范围配对校正（不平衡范围校正到平衡）、摘要生成（fake LLM）、CompactNow/CompactRegion、剪枝（head/middle/tail、Unicode 边界）。

**纪律**：不改 loop 的 turn/step 结构（D4）；**日志仍追加式（D1）**——压缩绝不物理删除旧事件，被遮蔽事件保留在日志，只是派生时跳过（fold 规则改造在 `session.derive`，属 M2 预留的"派生规则只改折叠"位）；主循环串行（D5）；零新依赖（标准库即可）；CGO-free；原有测试全绿（尤其 session 的 derive 测试）。**不要动**：loop 源码、cmd/sta、config、jobs、subagent、tools 包（只读参考）。**本任务不做**：/compact 命令、`compaction/*` 事件类型、config、PreStep 接线（M5c-2）。

**环境（重要）**：Go 在 `C:\Program Files\Go\bin\go.exe`（不在 PATH）；每次 Go 命令设 `$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`。用 pwsh 执行命令。git 提交用 `git -C D:\dev-projects\Agent\shutu-agent -c user.name='Personal Agent' -c user.email='dev@shutu-agent.local' commit -m "..."`。不要提交 `pa.exe`、`data/`、缓存目录。

**上下文管理**：不要通读参考源码，按需精读片段；**分阶段提交**（先折叠规则改造 + session 测试 commit 一次，再 compaction 接缝 + basic Provider + 剪枝 + 测试 commit 一次，信息含 "M5c-1"）；报告只列文件名 + 一句话。

**自测**（全部通过再报告）：`go vet ./...`、`go test ./...`、`go build ./...`。新增测试至少覆盖：折叠规则（带 replace 的摘要 + 遮蔽 seq 跳过 + 无 replace 不变 + 混合事件 + 空摘要）、配对边界校正、token 压力触发/不触发、retain_turns、CompactNow/CompactRegion、剪枝 head/middle/tail + Unicode 边界。

**完成报告**：改动文件清单、实现决策（token 估算方式、折叠规则实现、剪枝策略）、测试结果、提交 hash 列表、对 M5 主 ADR 的更新说明（如有）。提交后报告，不要等待控制会话确认——报告即交接。
