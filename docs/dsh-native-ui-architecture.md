# Shutu DSH 原生 UI 接入架构

本文件记录 P36-1 的构建边界。`deepseek-harness` 是只读参考源，Shutu 不修改其中任何文件，也不在发布运行时依赖其源码目录。

## 当前入口

`web/src/main.tsx` 保留当前 Shutu UI 作为默认模式；当构建环境设置 `SHUTU_DSH_NATIVE=1` 时，入口转到 `web/src/dsh-native-entry.ts`，调用 DSH 的 `AppWebEntry`。

```text
npm.cmd run build:native
  └─ web/src/main.tsx
      └─ mountDshNativeApp()
          └─ @deepseek-ai/dsh-client-web
              └─ AppWebEntry
                  └─ Cordis Loader → uiRenderer → root slot
```

## 只读源码映射

Vite 只读取以下 DSH 源码入口并将其编入 Shutu dist：

- `packages/client/web/src/index.ts`
- `packages/client/modules/src/client/index.ts`
- `packages/client/ui-renderer/src/client/index.ts`
- `packages/client/ui-slots/src/index.ts`
- `packages/client/ui-primitives/src/index.ts`
- `packages/client/runtime/src/client/index.ts`
- `vendor/loader/src/index.ts`
- `vendor/cordis/src/index.ts`

Node-only `node:module` 通过 DSH 自带的 browser stub 解析。构建完成后，`web/scripts/verify-dist.mjs` 检查 index 不含源码路径或 `deepseek-harness` 运行时引用。

## 当前边界

DSH `AppWebEntry` 启动前必须由 Host 注入：

- `window.__ModuleLoader__`
- `window.__DSH_BOOT__`

## P36-2 已落地的 Host transport

Go web server 已增加 DSH Connection 的核心 wire adapter：

- `POST /api/host.describe`
- `POST /api/session.list`
- `POST /api/session.search`
- `POST /api/session.create`
- `POST /api/session.history`
- `POST /api/session.rename`
- `POST /api/session.prompt`
- `POST /api/session.cancel`
- `POST /api/workspace.list`
- `GET /api/events.mux` 和 `GET /api/events.host`（downlink-only WebSocket）

所有 unary 请求使用 `client-request` / `server-response` 和 `rpcId` 回显；事件使用 DSH 的 `session/subscribed` 与 `session/event` frame。当前仍是增量适配：Host 事件推送、重连/续传、其余 RPC 方法和 DSH client plugin bundle 由后续 P36-2 及 P36-3/P36-4 完成，因此 native build 仍不作为默认生产入口。

Host bridge 的核心 RPC/downlink 已接入，但 manifest、插件 bundle、Host 事件推送、重连/续传和完整 RPC 仍在后续 P36-2/P36-3/P36-4 中补齐，因此 native dist 仍不作为默认生产入口。
