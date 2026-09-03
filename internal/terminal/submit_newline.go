package terminal

import "runtime"

func submitNewline() string {
	if runtime.GOOS == "windows" {
		return "\r\n"
	}
	return "\n"
}
