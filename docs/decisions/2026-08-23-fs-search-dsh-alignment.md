# 2026-08-23 grep/glob 工具对齐 dsh（tool-fs-search 移植）

状态：实施完成
关联：D-GAP-1（文件内容搜索，2026-08-20-standard-gaps.md）、dsh 对齐（工具改名 bash/pwsh/grep/glob/...）、2026-08-23-pwsh-dsh-alignment.md（同款对齐）

## 背景

- D-GAP-1 落地时 `grep`/`glob` 是 shutu 自定义契约：grep 参数 `path/pattern/glob/regex(布尔)/max_results/case_sensitive`，pattern 默认**子串**匹配、大小写不敏感，输出 `path:line: text` + `N matches`；glob 的 `*` 只匹配单段（`*.md` 只命中根层）。
- dsh 的 `grep`/`glob`（`packages/fs/tool-fs-search`，ripgrep 后端）契约完全不同：grep 参数 `pattern/path/include`，pattern **恒为正则**（ripgrep 语义、大小写敏感），输出 "Found N matches" + 按文件分组 `path\nLine N: text` 段；glob 参数 `pattern/path`，无 "/" 的模式按**任意深度 basename** 匹配（`*.md` 命中整棵树），按修改时间排序，输出 "No files found"。
- 模型按 dsh 习惯调用时失败：`{"pattern": "...", "include": "*.go"}` 被 shutu 的 `additionalProperties: false` 直接 D7 拒绝（用户报 "tool grep error"，已复现：`additionalProperties 'include' not allowed`）；即使不带 include，dsh 习惯的正则 pattern 被当子串查 → 静默无结果。
- 用户拍板（2026-08-23）：照抄 dsh tool-fs-search 的模型契约，同 pwsh 对齐。

## 决策

### D-FS-1 grep 契约照抄 dsh

- 参数面：`pattern`（必填，正则）、`path`（可选）、`include`（可选，**一个**正向 glob 过滤器）。删除 shutu 独有的 `glob/regex/max_results/case_sensitive` 参数。
- 语义：pattern 恒为正则（Go RE2；ripgrep 语法的高频子集一致，回看/环视等不支持的构造 fail-closed 报错）；**大小写敏感**（rg 默认）；include 无 "/" 按任意深度 basename 匹配、有 "/" 相对搜索根锚定，支持 `**`、`*`、`?`、`{a,b}` 交替（共享 `pathGlobRE`）。
- 校验消息照抄 dsh：`pattern must be a non-empty string` / `path must be a non-empty string when given` / include 拒绝空、`!` 否定、顶层逗号列表（`use {a,b} alternation instead`）；空白 pattern 是合法正则（dsh 同款）。
- 输出照抄 dsh：`Found N matches`（单数 `Found 1 match`）+ 按文件分组 `path\nLine N: text`（路径相对 agent cwd，斜杠归一）；无结果 `No matches found`；截断时头部 `(limit reached)` + 尾部 `(The complete result could not be saved; narrow pattern, path, or include to see more.)`。
- 上限：内联 250 条（dsh GREP_MAX_MATCHES）；注册表 64KB 输出上限 + spill 兜底照旧。

### D-FS-2 glob 契约照抄 dsh

- 参数面：`pattern`（必填）、`path`（可选）。删除 `max_results`。
- 语义：无 "/" 的模式按任意深度 basename 匹配（`*.md` 命中整棵树；dsh 描述原文："A pattern with no \"/\" matches the basename at any depth"）；`**` 跨段、`**/` 可匹配零段；**按修改时间排序（新在前，dsh --sort=modified）**；只列文件不列目录；隐藏文件包含（shutu walk 本来就含），VCS 目录排除。
- 输出：无结果 `No files found`；超上限（内联 100 条，dsh GLOB_MAX_RESULTS）→ 前 100 条 + `(Showing 100 of N paths. The complete result could not be saved; narrow pattern or path to see more.)`。
- 显示路径相对 agent cwd（dsh 输出 workdir 相对路径，read 可直接跟随）。

### D-FS-3 诚实标注的偏差（纯 Go 移植，零新依赖）

- 无 ripgrep 二进制：Go `regexp`(RE2) 替代 regex crate；rg 的 `--no-ignore`/`.gitignore` 语义不移植——shutu walk 固定跳过 VCS/依赖目录（.git/.hg/.svn/node_modules/vendor，D-GAP-1 安全约束；dsh glob 的 --no-ignore 会搜 node_modules，本实现不跟随）。
- 无效 UTF-8 行的 "(line is not valid UTF-8)" 占位、二进制文件跳过语义（NUL 检测，rg 的 binary-skip 同效）、glob 的顶级采样展示（"(Showing X of Y paths, sampled across ...)" 简化为平面分页）不移植。
- Web UI 的搜索卡解析器（searchCardHtml）同步改为解析 dsh 分组格式（`path\nLine N: text`）。

## 影响

- 模型可见变化：grep 的 pattern 现在是正则且大小写敏感；include 代替原 glob/regex/max_results 参数；输出格式换成 dsh 分组样式；glob 的 `*.md` 语义从"根层"变为"任意深度"。
- 旧会话回放：UI 只对新输出用新解析器；旧 "path:line: text" 行在新解析器下不产生搜索卡（回落 IO 卡）——可接受。
- 测试面：`internal/fssearch` 两套测试重写（正则语义、大小写敏感、include 校验与匹配、glob 任意深度/排序/分页、dsh 文案）；`cmd/pa/fssearch_test.go` 断言更新。

## 验收标准（全部通过）

1. `go build ./...`、`go vet ./...`、`go test ./...` 全绿。
2. dsh 风格调用 `{"pattern": <正则>, "include": "*.go"}` 不再报错且结果正确；不带 include 的正则 pattern 正常命中。
3. grep 输出为 "Found N matches" + 分组行；无结果 "No matches found"；超限带 "(limit reached)" + could-not-save 尾部。
4. glob `*.md` 命中任意深度；输出按修改时间新在前；超 100 条带 "(Showing 100 of N paths. ...)"。
5. include 校验拒绝空/否定/逗号列表，接受 `{a,b}`；空白 pattern 合法。
