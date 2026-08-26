# DSH Web 对齐验收记录

本记录对应 P34。`deepseek-harness` 仅作为只读参考，验收对象为 `shutu-agent`。

## 自动化检查

| 检查 | 结果 | 说明 |
|---|---|---|
| `go test ./...` | 通过 | Go 后端全部包通过 |
| `go vet ./...` | 通过 | 无 vet 问题 |
| `go build ./...` | 通过 | 后端可构建 |
| `npm.cmd run typecheck` | 通过 | 前端 TypeScript 无错误 |
| `npm.cmd run test` | 通过 | 7 个测试文件、35 个测试 |
| `npm.cmd run build` | 通过 | Vite 生产构建成功 |
| `npm.cmd run verify` | 通过 | dist、入口和资源校验成功 |
| `npm.cmd run e2e` | 通过 | Playwright 桌面/移动端通过，控制台无错误 |

## 已覆盖交互

- Conversation / Trajectory / Running Tab 切换
- 会话加载、SSE 实时事件、重复事件去重
- 轨迹动态虚拟滚动和动态高度
- Turn、assistant tool call 折叠与状态恢复
- 时间线点击、Shift 范围选择、拖拽选择和选区清除
- 轨迹搜索、结构化详情命中、结果高亮和摘要片段
- 请求 Inspector、Token、输入/输出、错误、重试和父子关系
- 历史分页边界、加载状态、失败提示和重试入口
- 空数据、坏时间戳、未知事件、坏 SSE 帧和图片加载失败
- 桌面与移动端首屏布局、主要控件响应和控制台健康

## 剩余风险

- 尚未在真实生产数据规模下做长时间内存和帧率基线记录。
- 尚未在多个真实浏览器内核上运行完整 E2E 矩阵。
- P35 仍需完成目标环境部署包、启动、升级和回滚验证。

当前下一步：`P35`。
