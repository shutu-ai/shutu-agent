# GAP-1 派发：fs-search（文件内容全文检索）

> 标准模式缺口 ADR `docs/decisions/2026-08-20-standard-gaps.md`（D-GAP-1）。本文件是 **GAP-1** 契约：`internal/fssearch` 全文检索 + `fs_search` 工具 + config cap + cmd/sta 接线 + 测试。对齐 dsh `tool-fs-search`。

## 纪律

- 零新依赖、CGO-free；只动 internal/fssearch（新建）、internal/config、cmd/sta、config.yaml；gofmt；不改 loop。
- 默认关（D10）：`fs_search.enabled` 默认 false；未启用不注册不白名单。
- 提交 1 个：`GAP-1: fs-search 文件内容全文检索（internal/fssearch + fs_search 工具 + config + 接线）`

## 已知现状（实施时通读对应区域，勿猜）

- 工具实现模式照 `internal/fs/tools.go`（Name/Description/Schema/Execute 结构 + tools.Tool 方法集）。工具名常量 `FsSearchToolName = "fs_search"`。
- `internal/config/config.go`：Config struct（Eval 后已有 Mode 字段，Mode-1 已合入）；applyDefaults 各 cap 白名单 append 模式（`if cfg.X.Enabled && !contains(cfg.Tools.Enabled, name) { append }`）；Mode-1 的 minimal 分支在 applyDefaults 末尾（`if cfg.Mode == ModeMinimal { ... }`，里面逐一关各 cap + 重置白名单）——**fs-search 需在 minimal 分支里加 `cfg.FsSearch.Enabled = false`**（minimal 不含搜索）。
- 默认 cwd 读取：看 `internal/fs` 或 cmd/sta 如何取 agent cwd（config.DataDir 之外的工作目录；`cfg.Tools.RunCommand.Workdir` 或 os.Getwd；实施时确认实际默认，工具 path 缺省用它）。
- D3 事件模式：照 jobs/eval 工具 emit 事件。

## 变更清单（精确）

### 1. internal/fssearch/search.go（新建，包 internal/fssearch）
```go
// Package fssearch searches file contents under a directory tree
// (D-GAP-1). It is a read-only, bounded capability: ignored VCS/dependency
// directories and binary files are skipped, per-file and aggregate limits
// bound the scan, and the default match is a plain substring
// (regex:true switches to a regular expression). It never writes.
package fssearch
```
- 类型：
```go
// Hit is one matching line.
type Hit struct {
	Path string // absolute path
	Line int    // 1-based line number
	Text string // matching line (trailing newline trimmed)
}

// Options bounds a Search. Zero values fall back to the defaults.
type Options struct {
	Path          string // root directory; "" → the caller-supplied default (组合根注入 cwd)
	FilePattern   string // optional glob restricting files, e.g. "*.go" (filepath.Match on base name)
	Regex         bool   // treat Query as a regular expression
	MaxResults    int    // cap total hits; <=0 → DefaultMaxResults
	MaxFileBytes  int64  // skip files larger than this; <=0 → DefaultMaxFileBytes
	MaxFiles      int    // cap files scanned; <=0 → DefaultMaxFiles
	CaseSensitive bool   // default false (case-insensitive match)
}

// Defaults (D-GAP-1 有界与安全).
const (
	DefaultMaxResults   = 50
	DefaultMaxFileBytes = 1 << 20 // 1 MiB
	DefaultMaxFiles     = 20000
)

// Search finds Query in file contents under opts.Path and returns hits in
// file-then-line order. ErrLimit is returned when MaxFiles/MaxResults caps are
// hit (the caller may still use the partial hits). Query must be non-empty.
func Search(ctx context.Context, query string, opts Options) ([]Hit, error)
```
- 行为（严格）：
  - `ctx` 取消 → 尽早返回 `ctx.Err()`。
  - 忽略目录集合：`.git`、`.hg`、`.svn`、`node_modules`、`vendor`（WalkDir 时跳过子树）。`opts.Path` 本身不存在 → error。
  - 二进制检测：文件前 8KB 含 NUL 字节 → 跳过。
  - `MaxFiles` 达到 → 停止扫描，返回 `ErrLimit` + 已收集命中（partial）。
  - `MaxResults` 达到 → 停止，返回 `ErrLimit` + 命中（partial）。
  - 匹配：默认大小写不敏感子串（`strings.Contains(strings.ToLower(line), strings.ToLower(query))`，query 空 → error）；`Regex` 时 `regexp.Compile`（编译失败 → error）逐行 `MatchString`；`CaseSensitive` 时不 lower。
  - 行遍历：bufio.Scanner 默认缓冲即可；行号 1-based；`Text` 去行尾 `\r`/`\n`。
- 哨兵：`var ErrLimit = errors.New("fssearch: search limit reached")`。

### 2. internal/fssearch/search_test.go
用 `t.TempDir()` 造临时树，用例：
1. 子串命中：多文件多行 → Hit{Path,Line,Text} 正确、顺序按文件再行。
2. 大小写不敏感默认 / `CaseSensitive` 区分。
3. `Regex` 命中 + 非法正则 → error。
4. 忽略目录：`.git`/`node_modules` 下命中被跳过。
5. 二进制跳过：含 NUL 文件不产出命中（也不报错）。
6. `MaxResults` / `MaxFiles` 上限 → ErrLimit + partial hits。
7. `FilePattern` 过滤（`*.go` 只匹配 .go）。
8. `MaxFileBytes` 跳过超大文件。
9. path 不存在 → error；query 空 → error。
10. ctx 取消 → 返回 ctx.Err()（可造大目录 + 预取消）。

### 3. internal/fssearch/tools.go（新建）
- `const FsSearchToolName = "fs_search"`
- `FsSearchTool`：持有 `cwd string`（path 缺省时的根）与 `searchFn`（测试注入或默认 Search）。照 `internal/fs/tools.go` 模式实现 tools.Tool：
  - `Name() string` → fs_search；`Description()` → "搜索目录下文件内容（子串或正则），返回匹配文件与行"。
  - `Schema()` → 上文 JSON（path/query/pattern/regex/max_results/case_sensitive；required [query]）。
  - `Execute(ctx, args any) (string, error)`：unmarshal → query 空拒绝 → `Search`（缺省 path → cwd；MaxResults 缺省 → DefaultMaxResults）→ 格式化：每命中 `path:line: text`（path 用相对展示：相对 cwd 更可读，实施时定）→ 末尾 `N matches`；`ErrLimit` → 结果 + ` (limit reached)` 后缀；无命中 → `no matches for "<query>" in <path>`。
- `tools_test.go`：直接构造 FsSearchTool{searchFn:...} 断言 Execute 输出格式（命中/无命中/空 query 拒绝）。

### 4. internal/config/config.go + config.yaml
- Config struct（Mode 后）：`FsSearch FsSearchConfig \`yaml:"fs_search"\``
- 类型（TerminalConfig 附近）：
```go
// FsSearchConfig is the file-content-search policy (D-GAP-1). The capability
// is default off (D10): when Enabled is false the composition root registers
// no fs_search tool. minimal 模式同样关闭 (D-MODE-2).
type FsSearchConfig struct {
	Enabled bool `yaml:"enabled"` // default false (D10)
}
```
- applyDefaults 白名单（照各 cap 模式，放 eval 白名单附近）：
```go
	if cfg.FsSearch.Enabled && !contains(cfg.Tools.Enabled, FsSearchToolName) {
		cfg.Tools.Enabled = append(cfg.Tools.Enabled, FsSearchToolName)
	}
```
  （`FsSearchToolName` 常量——config 包已这样镜像各工具名：查现有如 `terminalToolNames` 变量位置，照放 `var fsSearchToolNames = []string{"fs_search"}`，applyDefaults 用循环，与其他 cap 一致。实施时选与现有一致的风格。）
- **minimal 分支**（Mode-1 已合入的 `if cfg.Mode == ModeMinimal` 块内）加：`cfg.FsSearch.Enabled = false`。
- config.yaml：`fs_search:` 段 + 注释（enabled 默认 false D10）。

### 5. cmd/sta/fssearch.go（新建）+ main.go 接线
- `registerFsSearch() error`（照 terminal.go/eval.go 模式）：
```go
// registerFsSearch wires the file-content-search seam (D-GAP-1) when
// fs_search.enabled (默认关 D10): it registers fs_search into the registry.
// config.applyDefaults already whitelisted the name when enabled. Read-only,
// no resources → no deferred Close.
func (a *app) registerFsSearch() error {
	if !a.cfg.FsSearch.Enabled {
		return nil
	}
	// cwd: agent 工作目录 — 照现有 run_command/fs 取 cwd 的方式
	if err := a.reg.Register(fssearch.NewFsSearchTool(cwd)); err != nil {
		return fmt.Errorf("pa: register %s: %w", fssearch.FsSearchToolName, err)
	}
	return nil
}
```
  （`NewFsSearchTool(cwd string)` 构造器。）
- main.go：import `internal/fssearch`；`registerFsSearch()` 调用（放 registerFs 附近或 registerTerminal 前均可，无依赖）；无 defer。
- cmd/sta/fssearch_test.go：makeXxxApp + 白名单模式：
  - `TestRegisterFsSearchDisabledRegistersNothing`（D10 门）。
  - `TestRegisterFsSearchEnabledRegistersAndSearches`：enabled + 临时目录 → 注册 fs_search → Execute `{"path": tmp, "query": "needle"}` 命中断言。
  - `TestFsSearchWhitelist`：enabled → 白名单含 fs_search（cfg 层或 app 层）。

## 验证

`go build ./...` + `go test -count=1 ./internal/fssearch/ ./internal/config/ ./cmd/sta/ -run 'FsSearch|Fssearch|Search' -v` 全 PASS 后提交；随后 `go test -count=1 ./...` 全绿确认（含 Mode-1 minimal 分支测试不回归）。

## 环境

- Go：`C:\Program Files\Go\bin\go.exe`；env：`$env:GOTELEMETRY='off'; $env:GOFLAGS='-mod=mod'; $env:GOMODCACHE='D:\dev-projects\Agent\shutu-agent\.gomodcache'; $env:GOPATH='D:\dev-projects\Agent\shutu-agent\.gopath'; $env:GOCACHE='D:\dev-projects\Agent\shutu-agent\.gocache'`；提交身份 `-c user.name='Personal Agent' -c user.email='dev@shutu-agent.local'`；pwsh，workdir=`D:\dev-projects\Agent\shutu-agent`。

## 报告（简短）
提交 hash + go test 结果 + 偏离说明（cwd 取法、工具名常量风格）。不要贴代码。
