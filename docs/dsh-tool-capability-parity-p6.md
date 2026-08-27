# DSH 工具能力对齐：P6 Workflow

P6 已完成，`workflow` 的模型面向协议和运行结果已切换为 DSH 原生 JavaScript 编排契约：

- 模型参数固定为必填 `meta`、`script`，以及可选 `args`；不再向模型暴露旧的 `tasks` DAG 参数。
- `meta` 支持 `name`、`description`、`whenToUse` 和 `phases`；脚本通过 `agent`、`pipeline`、`parallel`、`phase`、`log` 完成编排。
- 脚本执行要求存在调用方 agent，运行在前台，Node runner 返回真实 `runId`。
- 成功结果保留结构化 `{runId, agentsStarted, result}`，并提供 DSH 风格的完成文本和 JSON 结果渲染。
- `cancelled`、`error` 和其他非 completed 状态不会作为成功结果返回，而是转换为对应的 workflow 错误。
- 结果渲染上限为 50000 字符，超出时追加 DSH 风格截断标记；结构化值本身不被截断。
- 旧 Go DAG 引擎仅保留为包内非模型调用路径，未出现在模型 schema 中。

验证：

- `go test ./internal/workflow ./internal/workflow/node ./cmd/pa`
- `go test ./...`

