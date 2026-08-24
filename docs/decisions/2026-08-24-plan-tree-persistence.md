# 2026-08-24 Plan tree 持久化

状态：已实现并复核

## 背景

第 6 项要求 Goal/Plan/Todo 在进程重启、session 切换后仍可查询和继续执行。现有 `internal/plan` 的 Provider 是内存实现，而 `cmd/pa` 已有 session event log → SQLite 的唯一持久化路径。

## 决策

1. 不新增 plan 专用 SQLite 表。session event log 继续是唯一事实源，内存 Provider 只作为可丢弃的查询投影。
2. `plan/create` 事件增加可选 `detail` 快照：Goal 保存 objective/status/createdAt，Plan 保存 goalId/status/createdAt/steps，Todo 保存 planId/status/createdAt。旧事件不带 detail 仍可读取。
3. `plan/status` 与 `plan/delete` 作为增量事实重放；Goal 删除按 Engine 语义级联其 Plan/Todo，Plan 删除解除 Goal 关联，Todo 删除从所属 Plan 移除。
4. session 新建、恢复和切换时重建 projection；重放结束后按现有最大 ID 重新播种 `goal-N`/`plan-N`/`todo-N` 序号，避免继续执行产生碰撞。
5. 重放从空 projection 开始，因此同一事件序列重复恢复是幂等的；plan 工具继续通过现有 D3 sink 写入 session log。

## 与 dsh 的边界

- 对齐点：状态和父子树从 append-only session facts 重建，重启后继续使用同一 session 的规划状态。
- Go 实现保留现有 `Provider + Engine` 接缝，不复制 dsh 的独立 plan 数据库或后台 Goal scheduler；Goal scheduler、KB 直接功能和 `kb_import` 仍按 `AGENT.md` 后置。
- 对旧版不完整 `plan/*` 事件采取确定性兼容回退：按事件顺序关联最近 Goal/Plan；新事件写入完整 detail，不再产生该缺口。

## 验收

- `internal/plan`：恢复 Goal/Plan/Todo、状态、acceptance、完成时间、删除和 ID 接续；重复 Restore 结果一致。
- `cmd/pa`：事件写入 SQLite 后，新 app 恢复同一 session，`plan_list` 可查询完整树，后续新 Goal ID 不冲突。
- 已通过：`go test ./...`、`go vet ./...`、`go build ./...`。
