# DeepSeek Harness 等价性主契约

状态：T0 冻结草案（实现目标）  
适用范围：`shutu-agent` Go runtime、native host、Web adapter 及外部协议  
参考基线：同 workspace 的 `deepseek-harness/docs/architecture.md` 和 `docs/subsystems/`

本文件定义“能力等价”的验收边界。工具名称、页面截图或单次 happy-path 成功，不足以证明等价；实现必须满足这里的生命周期、事件、持久化、安全和扩展语义。

## 1. 等价性原则

1. **Model-visible means logged**：进入模型请求的内容必须能够从该 Agent 的 durable session log 重建。
2. **Capability is a seam**：每项能力必须有 Service、Provider、Consumer 三层；Consumer 不得依赖具体 provider 或全局应用变量。
3. **Registration is owned**：注册产生 disposer；Agent、session、plugin 销毁时必须释放其所有监听器、工具、进程和子 Agent。
4. **Fail closed**：sandbox、approval、未知事件、未知配置版本、非法输出和不可用 provider 不得静默降级为不受保护的执行。
5. **Agent isolation**：Agent、session、workspace、tool registry、approval、sandbox 和 child lineage 不得通过进程全局可变状态共享。
6. **Durability is explicit**：`flush` 返回后，调用方可以依赖约定范围内的事件已经 durable；恢复逻辑必须区分 live session 与 cold session。

## 2. Agent contract

每个 Agent 必须提供以下状态和操作：

```text
create / resume / close
run / cancel / whenIdle
send        -> next-step 或当前可处理的 inbox message
followup    -> next-turn message
steer       -> 中止当前 step 后进入下一 step
inject      -> 不唤醒 idle Agent 的 queued context
```

Agent 必须拥有：

- 稳定 opaque id
- session handle
- per-agent context/scope
- next-step 与 next-turn 两个 inbox 队列
- live status
- parent/child lineage
- provider/model selection
- agent-scoped tools、approval、sandbox 和 extensions

关闭 Agent 时必须停止 loop、等待子资源退出、移除 registry publication，并释放 scoped registrations。

## 3. Turn / step contract

标准执行结构为：

```text
turn/start
  claim inbox input
  pre-step: reject | empty | enter(messages)
  step/start
    request/header
    agent/request
    llm/stream
      assistant/chunk*
      assistant/message
      tool/call*
        tools/pre-execute
        tools/execute
        tools/post-execute
        tool/result*
    step/end
  next step or turn-stopping
turn/end
```

`agent/request`、`llm/stream`、工具执行前/执行中/执行后属于可组合 waterfall；listener 必须显式调用 `next()`。模型可见输入必须走日志化 channel，不得只存在于内存 callback。

## 4. Durable event contract

核心 durable event vocabulary：

```text
turn/start       turn/end
step/start       step/end
user/message
assistant/chunk  assistant/message
tool/call        tool/result
steering/message todo/write
request/header
```

要求：

- `seq` 连续递增且不能重排。
- `assistant/chunk` 不得从 canonical log 中过滤。
- event payload 必须可 JSON 序列化。
- 扩展事件必须声明 version、owner 和 projection 规则。
- unknown required event 必须拒绝恢复；只有显式 `ignorable` event 可以忽略。
- `deriveMessages()` 是模型 history 的唯一来源。

## 5. Persistence contract

`Session`（内存 append-only log）与 `SessionPersistence`（durability backend）必须分离。

Persistence 必须支持：

- `locate/create/append/prepare/load/inspect/readFrom/list/listSnapshots/flush`
- JSONL backend 与 SQLite backend
- 批量 append 和显式 flush checkpoint
- live/cold session 区分
- crash orphan turn 自动补 `turn/end { reason: interrupted }`
- 完整 SessionHeader：id、version、createdAt、cwd、parent、seedLength、origin、delegationDepth、agentPreset
- fork/seed boundary
- opaque revision 和 revision snapshot
- bounded history read
- 格式版本拒绝、corruption 分类和无损恢复

## 6. Security contract

### Sandbox

每次执行都必须携带 per-call policy：

```text
mode / workspaceRoot / sessionId / cwd / network / process limits / output limits
```

支持 `read-only`、`workspace-write`、`danger-full-access`。无法获得 enforcing backend 时必须返回 `SANDBOX_UNAVAILABLE`，禁止 silent unconfined passthrough。

### Approval

Approval outcome 是封闭集合：

```text
allowed-once | rejected | cancelled | unavailable
```

每次请求必须有新 request id，并记录 `approval/asked` 与 `approval/decided`。`ask/never` policy 在 service 层强制；answerer 缺失、异常或超时必须变成 `unavailable`。

## 7. Extension contract

Plugin 必须支持：

- manifest、version、dependency
- service/provider/consumer registration
- scope ownership
- disposer
- load/unload/reload
- profile/bundle/patch
- host/client inventory
- manifest snapshot
- HMR 或明确的 production reload boundary

`dynamicCordisRunner` 等 host API 不得返回静态空结果伪装成功。

## 8. Subagent contract

Child Agent 必须通过同一 Agent runtime 创建，并拥有独立 scope。Provider 必须诚实声明并执行：

- output schema
- depth limit
- tool filter
- persona
- context inheritance
- continuable inbox

完整等价还要求 durable team roster、mailbox、task board、revision/CAS、owner authorization、dependency DAG 和 write-scope 冲突检测。

## 9. External protocol contract

- ACP 必须支持约定的 prompt content、session update、permission、cancel、MCP 和 resume 语义。
- MCP 必须处理支持的 transport、task support、rich content 和动态 tool lifecycle。
- Web host 必须使用真实 capability inventory、durable event cursor 和 reconnect repair。
- SDK/API 的 wire schema、版本协商和错误码必须可由外部 client contract test 验证。

## 10. 等价性门禁

以下全部通过，才允许使用“DeepSeek Harness capability-equivalent”：

- event replay contract
- model-history contract
- multi-Agent isolation test
- inbox ordering/steer/followup test
- crash/recovery test
- persistence backend contract test
- sandbox escape and fail-closed test
- approval failure-mode test
- plugin load/unload test
- subagent/team CAS test
- ACP/MCP/SDK wire test
- 100k-event、continuous-stream、reconnect 和 multi-Agent performance test

## 11. 明确非等价项

如果以下任一项继续保持现状，只能称为“工具/UI 对齐”或“非 Cordis 部分能力迁移”，不能称为完整 Harness 等价：

- 全局 `currentID`、全局 turn lock 继续控制根 Agent。
- `dynamicCordisRunner` 继续是空实现。
- HMR 继续只是 idle channel。
- code/command 执行继续没有 enforcing sandbox。
- persistence 继续只有本地 SQLite sink，没有 flush/recovery/header/revision contract。
- approval 继续使用非封闭 bool/status 语义。
- Agent Teams task board 继续被排除。
- ACP 继续拒绝 image/audio/embedded context/permission/MCP。
