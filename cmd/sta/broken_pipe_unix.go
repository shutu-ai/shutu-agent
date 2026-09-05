//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// ACP and SDK adapters deliberately own stdout as a protocol transport. A peer
// closing that pipe is a normal disconnect, not a fatal SIGPIPE: ignore the
// signal so Write returns EPIPE and the server can drain and exit cleanly.
func ignoreTransportBrokenPipe() {
	signal.Ignore(syscall.SIGPIPE, os.Interrupt)
}
