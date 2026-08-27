# P36 原生 DSH UI 部署、升级与回滚手册

## 交付物

发布包由 `web/dist`、Go 服务二进制、`config.yaml`、`config/prompts` 和 `release.json` 组成。部署时必须保留一个稳定的数据目录；SQLite 数据库、附件、spill 和会话日志都位于 `data_dir`，升级只替换程序/前端文件，不删除该目录。

## 启动前检查

1. 确认 `release.json` 中的 `platform`、`revision` 与目标机器匹配。
2. 确认 `web/dist/index.html` 存在，配置中的 `web_server.dist_dir` 指向该目录。
3. 确认 `data_dir` 可读写；Linux/WSL 的 SQLite 数据目录应放在 Linux 原生文件系统，不要放在 `/mnt/c`、`/mnt/d` 等 Windows 挂载路径。
4. 通过环境变量注入模型密钥；不要把密钥写入配置、发布包或日志。

## Windows 启动与验收

```powershell
.\bin\shutu-agent.exe --web-only --config .\config.yaml
Invoke-WebRequest http://127.0.0.1:18099/api/health
Invoke-WebRequest http://127.0.0.1:18099/api/sessions
```

浏览器验收必须确认 `/` 返回原生 DSH 页面、`/api/events.mux` 和 `/api/events.host` 均能 upgrade 到 `101`，并检查一次 `session.list`、历史加载和断线自动重连。

## Linux/WSL 启动与验收

```bash
export DEEPSEEK_API_KEY='from-secret-manager'
./bin/shutu-agent --web-only --config ./config.yaml
curl -fsS http://127.0.0.1:18099/api/health
curl -fsS http://127.0.0.1:18099/api/sessions
```

systemd 部署时使用专用用户、固定工作目录和持久 `data_dir`；服务异常退出后由 systemd 重启，但不得在启动失败时自动删除数据目录。`/api/health`、静态首页和两条 WebSocket upgrade 都通过后再接收流量。

## 升级流程

1. 记录当前 `release.json` revision、服务状态和数据目录备份位置。
2. 对数据目录做一致性备份；停止旧服务并确认旧 PID 已退出、监听端口已释放。
3. 原子替换二进制与 `web/dist`，保留 `data_dir` 和运行配置。
4. 启动新版本，依次检查 health、静态首页、session list、已有会话 history 和两条 WebSocket。
5. 用已有会话 ID 验证事件计数、标题、附件引用和配置仍可读；确认新建会话不会写入旧版本目录。

## 回滚与失败恢复

若启动、数据库打开、静态资源或 WebSocket 检查失败：

1. 立即停止新版本并保留失败日志与 revision。
2. 恢复上一版本二进制/前端文件，不删除或重建数据目录。
3. 用相同配置启动旧版本，检查 `/api/health` 和原有会话 ID；若数据库锁仍存在，确认没有残留进程后再重试。
4. 若数据目录损坏，从升级前一致性备份恢复到新的目录，先以只读副本验证，再切换服务配置。
5. 记录失败原因、恢复时间、恢复后的 revision 和数据校验结果。

## 已验证范围与限制

本手册已在 Windows 本机和 AlmaLinux-8 WSL2 执行对应 smoke；WSL2 还完成了旧 Linux binary → 新 Linux binary → 旧 Linux binary 的共享数据目录升级/回滚验证。正式目标 Windows/Linux 主机、systemd 实例和故障注入恢复仍需在目标环境执行。
