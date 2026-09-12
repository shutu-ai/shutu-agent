package extensionhost

// ownedProcess is the transport's ownership boundary. Each stdio connection
// gets one instance for the exact process it started; Close must terminate the
// process and any descendants without inspecting global process names.
type ownedProcess interface {
	Close() error
}
