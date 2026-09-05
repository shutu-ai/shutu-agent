//go:build windows

package terminal

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// shellCommand 返回配置好 shell 的 *exec.Cmd（尚未 Start）。cmd.exe 附加
// /Q（关闭回显）与 /K chcp 65001 >nul（UTF-8 代码页）；PowerShell 用其自身的
// UTF-8 输出编码初始化（cmd 专属参数会让 powershell.exe 直接报错退出）；
// Git Bash / WSL 原生输出 UTF-8，不加参数。Environ 不在此设置（session.go
// 统一设置）。
func shellCommand(opts SessionOpts) *exec.Cmd {
	shell := opts.Shell
	if shell == "" {
		shell = "cmd.exe"
	}
	args := append([]string{}, opts.Args...)
	switch strings.ToLower(filepath.Base(shell)) {
	case "cmd.exe", "cmd":
		args = append(args, "/Q", "/K", "chcp 65001 >nul")
	case "powershell.exe", "powershell", "pwsh.exe", "pwsh":
		// Keep the process in the interactive stdin loop. Supplying -Command
		// here executes the bootstrap script but does not reliably transition
		// Windows PowerShell back to reading a redirected stdin pipe.
		// Agent-owned shells start without the user's profile: profile scripts
		// are an untrusted environment channel and can also delay readiness.
		// Explicit terminal.args remain the escape hatch for deployments that
		// intentionally need a profile-aware shell.
		addPowerShellFlagOnce := func(name string) {
			if !slices.ContainsFunc(args, func(arg string) bool {
				return strings.EqualFold(strings.TrimSpace(arg), name)
			}) {
				args = append(args, name)
			}
		}
		addPowerShellFlagOnce("-NoLogo")
		addPowerShellFlagOnce("-NoExit")
		if !slices.ContainsFunc(args, func(arg string) bool {
			switch strings.ToLower(strings.TrimSpace(arg)) {
			case "-noprofile", "-profile", "/noprofile", "/profile":
				return true
			default:
				return false
			}
		}) {
			addPowerShellFlagOnce("-NoProfile")
		}
	}
	return exec.Command(shell, args...)
}

// killProcessTree 终止 cmd：Windows 无进程组，taskkill 不可用，直接
// Process.Kill() 只杀直接子进程；孙进程残留为文档化残余风险。
func killProcessTree(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// interruptInput 尽力而为的中断：向 s.stdin 写入字节 0x03（Ctrl+C）。
func interruptInput(s *Session) {
	if s != nil && s.stdin != nil {
		_, _ = s.stdin.Write([]byte{0x03})
	}
}
