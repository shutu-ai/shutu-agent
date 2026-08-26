# Shutu Agent 生产交付

## 交付包结构

```text
bin/shutu-agent(.exe)  Go 服务
web/dist/              React/Cordis 前端静态资源
config/prompts/        standard/code 模式提示词资源
config.yaml            服务配置，web_server.dist_dir 使用相对路径 web/dist
release.json           构建提交和目标平台元数据
deployment.md          本说明
```

## 生成交付包

在仓库根目录执行：

```powershell
node scripts/release-package.mjs
```

也可以指定输出目录：

```powershell
node scripts/release-package.mjs --output D:\packages\shutu-agent
```

脚本会重新构建并校验前端，然后构建当前平台的 Go 可执行文件，并复制 standard/code 模式运行所需的 `config/prompts/`。默认包输出到 `release/`，该目录不纳入 Git。

仓库内可用发布包做一次本地升级/回滚演练（使用临时端口和共享临时 `data_dir`）：

```powershell
node scripts/deployment-smoke.mjs
```

## 首次启动

1. 将交付目录复制到目标主机。
2. 设置 `DEEPSEEK_API_KEY` 环境变量；API key 不写入配置文件或交付包。
3. 生产环境建议编辑 `config.yaml` 中的 `web_server.token` 设置随机 bearer token；再按需调整 `web_server.addr`、`data_dir` 和工具开关。空 token 表示本机开放模式，配置 token 后才启用 bearer 认证。
4. 启动 Web 门户：

```powershell
bin\shutu-agent.exe --web-only --config config.yaml
```

5. 检查健康状态：

```powershell
Invoke-WebRequest http://127.0.0.1:18099/api/health
```

配置了 `web_server.token` 时，健康检查需要携带 `Authorization: Bearer <token>`；未配置 token 时为本机开放模式。

## 升级与回滚

- 先停止旧进程，再将新包复制到新的版本目录。
- 保留旧版本目录和 `data_dir`，确认 `/api/health`、会话列表、Trajectory SSE 后再切换服务入口。
- 回滚时停止当前进程，恢复旧版本目录；不要删除共享的 `data_dir`。
- 新包不依赖 `deepseek-harness` 运行时目录；DSH 目录只用于开发期工具链和参考实现。

## 本地交付包验收

以下检查已在仓库工作区完成；目标主机仍需按下一节执行一次。

- [x] `release.json` 的 commit 与生成时的 HEAD 一致。
- [x] `web/dist/index.html`、assets 和 `config/prompts/` 存在。
- [x] 发布包配置 bearer token 后，`--web-only --config config.yaml` 可启动。
- [x] `/api/health` 返回 200。
- [x] `deployment-smoke.mjs` 完成旧版本启动、新版本切换和旧版本回滚。

## 目标环境验收清单

- [ ] `release.json` 的 commit 与发布提交一致。
- [ ] `web/dist/index.html` 和 assets 存在。
- [ ] `--web-only --config config.yaml` 能启动。
- [ ] `/api/health` 返回 200。
- [ ] 会话列表、Conversation、Trajectory、SSE 和历史分页可用。
- [ ] 配置 token 后未授权请求返回 401，授权请求正常。
- [ ] 前端控制台无错误，移动端首屏可用。
