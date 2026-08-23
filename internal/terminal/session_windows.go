//go:build windows

package terminal

import (
	"os/exec"
)

// shellCommand 返回配置好 shell 的 *exec.Cmd（尚未 Start）：cmd.exe，
// 若 opts.Shell 为空则用 "cmd.exe"，参数 = append(opts.Args, "/Q")
// （/Q 关闭命令回显）。Environ 不在此设置（session.go 统一设置）。
func shellCommand(opts SessionOpts) *exec.Cmd {
	shell := opts.Shell
	if shell == "" {
		shell = "cmd.exe"
	}
	args := append(append([]string{}, opts.Args...), "/Q", "/K", "chcp 65001 >nul")
	cmd := exec.Command(shell, args...)
	// /Q suppresses command echo; /K initializes the persistent shell's code
	// page before it accepts model commands.
	return cmd
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
