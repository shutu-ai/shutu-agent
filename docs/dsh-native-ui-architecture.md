# Shutu DSH 原生 UI 接入架构

本文件记录 P36-1 的构建边界。`deepseek-harness` 是只读参考源，Shutu 不修改其中任何文件，也不在发布运行时依赖其源码目录。

## 当前入口

`web/src/main.tsx` 现在只挂载 `web/src/dsh-native-entry.ts` 的 DSH `AppWebEntry`；默认 `npm.cmd run build` 即生成 DSH React/Cordis 原生 UI，不再存在旧 Shutu shell 的运行时分支。

```text
npm.cmd run build
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

所有 unary 请求使用 `client-request` / `server-response` 和 `rpcId` 回显；事件使用 DSH 的 `session/subscribed` 与 `session/event` frame。Host 事件推送、重连基线、投影基线和当前 DSH client plugin bundle 已在 `shutu-agent` 内落地，且 native build 已经作为默认生产入口。

Host bridge、manifest、插件 bundle、Host 事件推送、重连基线和完整 RPC 已接入；后续 P36-5 及以后只继续补齐真实交互验收、性能和生产交付，不切回旧 UI。
