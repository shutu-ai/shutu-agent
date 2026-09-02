//go:build windows

package code

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/jabing/shutu-agent/internal/pathsecure"
	"golang.org/x/sys/windows"
)

const (
	createRestrictedTokenDisableMaxPrivilege = 0x1
	createRestrictedTokenLua                 = 0x4
	createRestrictedTokenWriteRestricted     = 0x8
	// DSH's FILE_GENERIC_WRITE minus READ_CONTROL plus DELETE and
	// FILE_DELETE_CHILD. x/sys/windows does not expose FILE_DELETE_CHILD or
	// FILE_ALL_ACCESS as named constants in this module version.
	grantMask     uint32 = 0x00110156
	fileAllAccess uint32 = 0x001F01FF
)

var createRestrictedTokenProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
var (
	procInitializeACL                = windows.NewLazySystemDLL("advapi32.dll").NewProc("InitializeAcl")
	procAddAccessAllowedAceEx        = windows.NewLazySystemDLL("advapi32.dll").NewProc("AddAccessAllowedAceEx")
	procInitializeSecurityDescriptor = windows.NewLazySystemDLL("advapi32.dll").NewProc("InitializeSecurityDescriptor")
	procSetSecurityDescriptorDacl    = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetSecurityDescriptorDacl")
	procSetSecurityDescriptorOwner   = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetSecurityDescriptorOwner")
	procSetSecurityDescriptorGroup   = windows.NewLazySystemDLL("advapi32.dll").NewProc("SetSecurityDescriptorGroup")
)

type windowsACLJournal struct {
	Version       int    `json:"version"`
	Path          string `json:"path"`
	OriginalACL   string `json:"originalAcl"`
	DACLProtected bool   `json:"daclProtected"`
	TrusteeSID    string `json:"trusteeSid"`
	CreatedAt     string `json:"createdAt"`
}

// windowsACLRun is the Windows file-effect backend used for controlled local
// shell calls. It mirrors DSH's boundary: the workspace grant is standing and
// deterministic, while the private temp grant is session-scoped and revoked
// after the child exits. It deliberately does not claim network/read/process
// isolation; those requirements remain unsupported on this backend.
type windowsACLRun struct {
	mu                   sync.Mutex
	closed               bool
	token                windows.Token
	tempBase             string
	tempDir              string
	tempSID              *windows.SID
	workspaceSID         *windows.SID
	readSID              *windows.SID
	readAuxSID           *windows.SID
	workspacePath        string
	workspaceGranted     bool
	workspaceOriginal    *windows.SECURITY_DESCRIPTOR
	workspaceOriginalACL []byte
	workspaceProtected   bool
	journalPath          string
}

type tokenDefaultDACL struct {
	dacl *windows.ACL
}

var windowsACLStatusOnce sync.Once
var windowsACLStatus bool
var capabilityAuthorityOnce sync.Once
var capabilityAuthoritySID string
var currentUserSIDOnce sync.Once
var currentUserSID *windows.SID

func windowsACLBackendAvailable() bool {
	windowsACLStatusOnce.Do(func() { windowsACLStatus = windowsACLProbe() == nil })
	return windowsACLStatus
}

// prepareWindowsACLRun materializes a restricted token and its file grants.
// The caller must close the returned handle after the child has exited.
func prepareWindowsACLRun(mode SandboxMode, workspaceRoot string) (windowsACLHandle, error) {
	if mode != SandboxReadOnly && mode != SandboxWorkspaceWrite {
		return nil, nil
	}
	workspaceRoot, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("code: resolve Windows ACL workspace: %w", err)
	}
	if info, err := os.Stat(workspaceRoot); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return nil, fmt.Errorf("code: Windows ACL workspace is unavailable: %w", err)
	}
	workspaceRoot, err = pathsecure.ResolveExisting(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("code: canonicalize Windows ACL workspace: %w", err)
	}

	workspaceSID, err := windows.StringToSid(workspaceWriteSID(workspaceRoot))
	if err != nil {
		return nil, fmt.Errorf("code: Windows ACL workspace SID: %w", err)
	}
	run := &windowsACLRun{workspaceSID: workspaceSID}
	run.workspacePath = workspaceRoot
	if err := recoverWindowsACLJournalFor(windowsACLJournalName(workspaceRoot)); err != nil {
		return nil, fmt.Errorf("code: stale Windows ACL sandbox state: %w", err)
	}
	cleanup := func(original error) (windowsACLHandle, error) {
		_ = run.close()
		return nil, original
	}
	if mode == SandboxWorkspaceWrite {
		run.tempBase, err = os.MkdirTemp("", "shutu-code-")
		if err != nil {
			return cleanup(fmt.Errorf("code: create Windows ACL private temp: %w", err))
		}
		run.tempDir = filepath.Join(run.tempBase, "private")
		if pathsOverlap(workspaceRoot, run.tempDir) {
			return cleanup(fmt.Errorf("code: Windows ACL private temp %q overlaps workspace %q", run.tempDir, workspaceRoot))
		}
		run.tempSID, err = windows.StringToSid(tempWriteSID(run.tempDir))
		if err != nil {
			return cleanup(fmt.Errorf("code: Windows ACL temp SID: %w", err))
		}
		if err := createWindowsSecureDirectory(run.tempDir, run.tempSID, false); err != nil {
			return cleanup(fmt.Errorf("code: create Windows ACL private temp: %w", err))
		}

		// Snapshot the exact DACL and journal it before the first mutation.
		// The journal is the crash commit point; partial initialization calls
		// the same cleanup path and restores immediately.
		original, err := windows.GetNamedSecurityInfo(
			workspaceRoot, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION,
		)
		if err != nil {
			return cleanup(fmt.Errorf("code: snapshot Windows ACL workspace: %w", err))
		}
		hasGrant, err := windowsDACLGrants(original, run.workspaceSID, windows.ACCESS_MASK(grantMask))
		if err != nil {
			return cleanup(fmt.Errorf("code: inspect Windows ACL workspace: %w", err))
		}
		if !hasGrant {
			run.workspaceOriginal = original
			run.workspaceOriginalACL, run.workspaceProtected, err = windowsACLBytes(original)
			if err != nil {
				return cleanup(fmt.Errorf("code: encode Windows ACL workspace snapshot: %w", err))
			}
			state := windowsACLJournal{
				Version:       1,
				Path:          workspaceRoot,
				OriginalACL:   base64.StdEncoding.EncodeToString(run.workspaceOriginalACL),
				DACLProtected: run.workspaceProtected,
				TrusteeSID:    run.workspaceSID.String(),
				CreatedAt:     time.Now().UTC().Format(time.RFC3339),
			}
			run.journalPath, err = writeWindowsACLJournal(state)
			if err != nil {
				return cleanup(fmt.Errorf("code: journal Windows ACL sandbox: %w", err))
			}
			if err := grantWindowsACL(workspaceRoot, run.workspaceSID); err != nil {
				return cleanup(fmt.Errorf("code: grant Windows ACL workspace: %w", err))
			}
			run.workspaceGranted = true
		}
	}

	current, err := openWindowsACLCurrentToken()
	if err != nil {
		return cleanup(fmt.Errorf("code: open Windows ACL process token: %w", err))
	}
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		_ = windows.CloseHandle(windows.Handle(current))
		return cleanup(fmt.Errorf("code: create Windows ACL Everyone SID: %w", err))
	}
	logonSID, logonErr := windowsACLLogonSID(current)
	if logonErr != nil {
		_ = windows.CloseHandle(windows.Handle(current))
		return cleanup(logonErr)
	}
	// Read-only tokens deliberately omit the logon SID from the restriction
	// set. Management ACEs may grant the invoking logon cleanup access without
	// turning the read-only child into a writer.
	_ = worldSID
	var restricting []windows.SIDAndAttributes
	switch mode {
	case SandboxWorkspaceWrite:
		restricting = []windows.SIDAndAttributes{
			{Sid: worldSID},
			{Sid: run.workspaceSID},
			{Sid: run.tempSID},
		}
	case SandboxReadOnly:
		usersSID, sidErr := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
		if sidErr != nil {
			return cleanup(fmt.Errorf("code: Windows ACL Users SID: %w", sidErr))
		}
		run.readSID = usersSID
		restricting = []windows.SIDAndAttributes{
			// Deny-only world gives the restricted-token check a valid fourth
			// SID without granting readonly child Everyone's management Full.
			{Sid: worldSID, Attributes: windows.SE_GROUP_USE_FOR_DENY_ONLY},
			{Sid: logonSID, Attributes: windows.SE_GROUP_USE_FOR_DENY_ONLY},
			{Sid: run.readSID},
		}
	default:
		restricting = []windows.SIDAndAttributes{{Sid: worldSID}}
	}
	run.token, err = createWindowsACLRestrictedToken(current, restricting)
	_ = windows.CloseHandle(windows.Handle(current))
	if err != nil {
		return cleanup(fmt.Errorf("code: create Windows ACL restricted token: %w", err))
	}
	defaultSID := worldSID
	if mode == SandboxWorkspaceWrite {
		defaultSID = run.tempSID
	}
	if err := setWindowsACLDefaultDACL(run.token, defaultSID); err != nil {
		return cleanup(fmt.Errorf("code: set Windows ACL default DACL: %w", err))
	}
	return run, nil
}

func (run *windowsACLRun) configure(cmd *exec.Cmd) error {
	if run == nil || run.token == 0 {
		return errors.New("code: Windows ACL token is not initialized")
	}
	if cmd == nil {
		return errors.New("code: Windows ACL command is nil")
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Token = syscall.Token(run.token)
	if run.tempDir != "" {
		env := cmd.Env
		if env == nil {
			env = os.Environ()
		}
		env = setEnvironmentValue(env, "TMP", run.tempDir)
		env = setEnvironmentValue(env, "TEMP", run.tempDir)
		cmd.Env = env
	}
	return nil
}

func (run *windowsACLRun) close() error {
	if run == nil {
		return nil
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	if run.closed {
		return nil
	}
	var failures []error
	if run.workspaceGranted {
		if err := restoreWindowsACLBytes(run.workspacePath, run.workspaceOriginalACL, run.workspaceProtected); err != nil {
			failures = append(failures, fmt.Errorf("restore workspace DACL: %w", err))
		} else {
			run.workspaceGranted = false
		}
	}
	if run.journalPath != "" && !run.workspaceGranted {
		if err := os.Remove(run.journalPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, fmt.Errorf("remove ACL journal: %w", err))
		} else {
			run.journalPath = ""
		}
	}
	if run.tempBase != "" {
		if err := os.RemoveAll(run.tempBase); err != nil {
			failures = append(failures, fmt.Errorf("remove private temp: %w", err))
		}
		run.tempDir = ""
	}
	if run.token != 0 {
		if err := windows.CloseHandle(windows.Handle(run.token)); err != nil {
			failures = append(failures, fmt.Errorf("close restricted token: %w", err))
		}
		run.token = 0
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	run.closed = true
	return nil
}

func windowsACLJournalName(path string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(path)))
	return fmt.Sprintf("%x.json", digest[:8])
}

func openWindowsACLCurrentToken() (windows.Token, error) {
	var token windows.Token
	// Use a real process handle. The DSH backend avoids passing the
	// GetCurrentProcess pseudo-handle through this boundary because the token
	// duplication path is sensitive to the handle ABI on Windows.
	processHandle, err := windows.OpenProcess(windows.PROCESS_QUERY_INFORMATION, false, uint32(os.Getpid()))
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(processHandle)
	err = windows.OpenProcessToken(processHandle,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ASSIGN_PRIMARY,
		&token)
	return token, err
}

func windowsACLLogonSID(token windows.Token) (*windows.SID, error) {
	groups, err := token.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID && group.Sid != nil {
			return group.Sid.Copy()
		}
	}
	return nil, errors.New("token has no logon SID")
}

func createWindowsACLRestrictedToken(current windows.Token, restricting []windows.SIDAndAttributes) (windows.Token, error) {
	// WRITE_RESTRICTED is not accepted by every managed-host token; LUA_TOKEN
	// is the supported restricted-token form on those hosts and still drops
	// privileged token capabilities.
	return createWindowsACLRestrictedTokenWithFlags(current, restricting, createRestrictedTokenLua)
}

func createWindowsACLRestrictedTokenWithFlags(current windows.Token, restricting []windows.SIDAndAttributes, flags uintptr) (windows.Token, error) {
	if len(restricting) == 0 {
		return 0, errors.New("restricted token requires at least one SID")
	}
	var restricted windows.Token
	r1, _, callErr := createRestrictedTokenProc.Call(
		uintptr(current), flags,
		0, 0,
		0, 0,
		uintptr(len(restricting)), uintptr(unsafe.Pointer(&restricting[0])), uintptr(unsafe.Pointer(&restricted)),
	)
	runtime.KeepAlive(restricting)
	if r1 == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.GetLastError()
		}
		return 0, fmt.Errorf("%w (flags=0x%x restrictingSIDs=%d)", callErr, flags, len(restricting))
	}
	if restricted == 0 {
		return 0, errors.New("CreateRestrictedToken returned a null token")
	}
	return restricted, nil
}

func setWindowsACLDefaultDACL(token windows.Token, sid *windows.SID) error {
	var needed uint32
	_ = windows.GetTokenInformation(token, windows.TokenDefaultDacl, nil, 0, &needed)
	if needed < uint32(unsafe.Sizeof(tokenDefaultDACL{})) {
		return fmt.Errorf("invalid TokenDefaultDacl size %d", needed)
	}
	buffer := make([]byte, needed)
	if err := windows.GetTokenInformation(token, windows.TokenDefaultDacl, &buffer[0], uint32(len(buffer)), &needed); err != nil {
		return err
	}
	old := *(*tokenDefaultDACL)(unsafe.Pointer(&buffer[0]))
	entry := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(fileAllAccess),
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, old.dacl)
	if err != nil {
		return err
	}
	updated := tokenDefaultDACL{dacl: merged}
	err = windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&updated)), uint32(unsafe.Sizeof(updated)))
	runtime.KeepAlive(sid)
	runtime.KeepAlive(merged)
	return err
}

func grantWindowsACL(path string, sid *windows.SID) error {
	return editWindowsACL(path, sid, windows.GRANT_ACCESS, grantMask)
}

func revokeWindowsACL(path string, sid *windows.SID) error {
	return editWindowsACL(path, sid, windows.REVOKE_ACCESS, 0)
}

func editWindowsACL(path string, sid *windows.SID, mode windows.ACCESS_MODE, permissions uint32) error {
	return withWindowsACLPathLock(path, func() error {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			return err
		}
		oldDACL, _, daclErr := sd.DACL()
		if daclErr != nil && !errors.Is(daclErr, windows.ERROR_OBJECT_NOT_FOUND) {
			return daclErr
		}
		entry := windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.ACCESS_MASK(permissions),
			AccessMode:        mode,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		}
		merged, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{entry}, oldDACL)
		if err != nil {
			return err
		}
		err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, merged, nil)
		runtime.KeepAlive(sid)
		runtime.KeepAlive(merged)
		return err
	})
}

// withWindowsACLPathLock serializes the read/merge/write DACL transaction
// across processes. Without this lock, two simultaneous sessions could each
// read the same old DACL and one grant would erase the other, which is exactly
// the race DSH's per-path LockFileEx seam prevents.
func withWindowsACLPathLock(path string, action func() error) error {
	digest := sha256.Sum256([]byte(strings.ToLower(path)))
	lockDir := filepath.Join(os.TempDir(), "shutu-acl-locks")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return err
	}
	lockPath := filepath.Join(lockDir, fmt.Sprintf("%x.lock", digest[:8]))
	lockName, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(lockName, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS, 0, 0)
	if err != nil {
		return err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped); err != nil {
		_ = windows.CloseHandle(handle)
		return err
	}
	actionErr := action()
	unlockErr := windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	closeErr := windows.CloseHandle(handle)
	if actionErr != nil {
		return actionErr
	}
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func windowsACLJournalDirectory() string {
	return filepath.Join(os.TempDir(), "shutu-acl-recovery")
}

func writeWindowsACLJournal(state windowsACLJournal) (string, error) {
	if err := os.MkdirAll(windowsACLJournalDirectory(), 0o700); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(strings.ToLower(state.Path)))
	path := filepath.Join(windowsACLJournalDirectory(), fmt.Sprintf("%x.json", digest[:8]))
	file, err := os.CreateTemp(windowsACLJournalDirectory(), ".journal-*")
	if err != nil {
		return "", err
	}
	tempPath := file.Name()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(state)
	if encodeErr == nil {
		encodeErr = file.Sync()
	}
	closeErr := file.Close()
	if encodeErr == nil {
		encodeErr = closeErr
	}
	if encodeErr != nil {
		_ = os.Remove(tempPath)
		return "", encodeErr
	}
	// Rename is the commit point. A crash before this point cannot have made a
	// new grant; a crash after it leaves the exact snapshot recoverable.
	_ = os.Remove(path)
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return path, nil
}

func recoverWindowsACLJournalFor(name string) error {
	path := filepath.Join(windowsACLJournalDirectory(), name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state windowsACLJournal
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("journal %s is unreadable: %w", path, err)
	}
	originalACL, err := base64.StdEncoding.DecodeString(state.OriginalACL)
	if err != nil {
		return fmt.Errorf("journal %s has an invalid DACL snapshot: %w", path, err)
	}
	if err := restoreWindowsACLBytes(state.Path, originalACL, state.DACLProtected); err != nil {
		return fmt.Errorf("journal %s could not be recovered: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RecoverWindowsACLSandboxes restores any DACL snapshot whose journal survived
// a process crash. Startup calls this before admitting controlled sandbox runs.
func RecoverWindowsACLSandboxes() error {
	entries, err := os.ReadDir(windowsACLJournalDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if err := recoverWindowsACLJournalFor(entry.Name()); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func windowsACLBytes(sd *windows.SECURITY_DESCRIPTOR) ([]byte, bool, error) {
	control, _, err := sd.Control()
	if err != nil {
		return nil, false, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, false, err
	}
	if dacl == nil {
		return nil, control&windows.SE_DACL_PROTECTED != 0, nil
	}
	// x/sys does not expose ACL size or ACE_HEADER fields. GetAce returns each
	// native ACE in place, so its own little-endian AceSize gives the exact
	// on-disk DACL length without a second security lookup.
	daclBase := uintptr(unsafe.Pointer(dacl))
	var daclEnd uintptr
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return nil, false, err
		}
		if ace == nil {
			return nil, false, errors.New("ACL returned a nil ACE")
		}
		header := (*struct {
			aceType  byte
			aceFlags byte
			aceSize  uint16
		})(unsafe.Pointer(ace))
		if header.aceSize == 0 {
			return nil, false, errors.New("ACL returned a zero-sized ACE")
		}
		daclEnd = uintptr(unsafe.Pointer(ace)) - daclBase + uintptr(header.aceSize)
	}
	if daclEnd == 0 {
		daclEnd = unsafe.Sizeof(windows.ACL{})
	}
	return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(dacl)), daclEnd)...),
		control&windows.SE_DACL_PROTECTED != 0, nil
}

func restoreWindowsACLBytes(path string, rawACL []byte, protected bool) error {
	if len(rawACL) == 0 {
		return errors.New("original DACL snapshot is missing")
	}
	dacl := (*windows.ACL)(unsafe.Pointer(&rawACL[0]))
	sd, err := newWindowsSecurityDescriptorWithDACL(dacl, protected)
	if err != nil {
		return err
	}
	defer func() { _, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sd))) }()
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION)
	} else {
		info |= windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil)
}

func newWindowsSecurityDescriptorWithDACL(dacl *windows.ACL, protected bool) (*windows.SECURITY_DESCRIPTOR, error) {
	sd := new(windows.SECURITY_DESCRIPTOR)
	r1, _, callErr := procInitializeSecurityDescriptor.Call(uintptr(unsafe.Pointer(sd)), 1)
	if r1 == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.GetLastError()
		}
		return nil, callErr
	}
	r1, _, callErr = procSetSecurityDescriptorDacl.Call(
		uintptr(unsafe.Pointer(sd)), 1, uintptr(unsafe.Pointer(dacl)), 0,
	)
	if r1 == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.GetLastError()
		}
		return nil, callErr
	}
	if protected {
		if err := sd.SetControl(windows.SE_DACL_PROTECTED, windows.SE_DACL_PROTECTED); err != nil {
			return nil, err
		}
	}
	if user := windowsCurrentUser(); user != nil {
		r1, _, callErr := procSetSecurityDescriptorOwner.Call(
			uintptr(unsafe.Pointer(sd)), uintptr(unsafe.Pointer(user)), 1,
		)
		if r1 == 0 {
			if callErr == nil || callErr == syscall.Errno(0) {
				callErr = windows.GetLastError()
			}
			return nil, callErr
		}
		r1, _, callErr = procSetSecurityDescriptorGroup.Call(
			uintptr(unsafe.Pointer(sd)), uintptr(unsafe.Pointer(user)), 1,
		)
		if r1 == 0 {
			if callErr == nil || callErr == syscall.Errno(0) {
				callErr = windows.GetLastError()
			}
			return nil, callErr
		}
	}
	return sd, nil
}

type windowsACE struct {
	sid  *windows.SID
	mask windows.ACCESS_MASK
}

func windowsCurrentUser() *windows.SID {
	currentUserSIDOnce.Do(func() {
		token, err := openWindowsACLCurrentToken()
		if err != nil {
			return
		}
		defer windows.CloseHandle(windows.Handle(token))
		user, err := token.GetTokenUser()
		if err != nil || user.User.Sid == nil {
			return
		}
		currentUserSID, _ = user.User.Sid.Copy()
	})
	return currentUserSID
}

func requireWindowsCurrentUser() (*windows.SID, error) {
	user := windowsCurrentUser()
	if user == nil {
		return nil, errors.New("current Windows user SID is unavailable")
	}
	return user, nil
}

// windowsManagementSIDs returns the complete set of SIDs that Windows access
// checks use for the invoking token. A process may run with restricting SIDs
// (for example under a managed agent); a filesystem ACE for only the account
// SID is then insufficient for management/cleanup operations.
func windowsManagementSIDs() ([]*windows.SID, error) {
	token, err := openWindowsACLCurrentToken()
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(windows.Handle(token))
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user.User.Sid == nil {
		return nil, errors.New("token has no user SID")
	}
	management := []*windows.SID{}
	if copied, err := user.User.Sid.Copy(); err == nil {
		management = append(management, copied)
	}
	restrictedBuffer := make([]byte, 64*1024)
	var returned uint32
	if err := windows.GetTokenInformation(
		token,
		windows.TokenRestrictedSids,
		&restrictedBuffer[0],
		uint32(len(restrictedBuffer)),
		&returned,
	); err != nil {
		return nil, fmt.Errorf("query restricted SIDs: %w", err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&restrictedBuffer[0]))
	for _, group := range groups.AllGroups() {
		if group.Sid == nil {
			continue
		}
		copied, err := group.Sid.Copy()
		if err != nil {
			return nil, err
		}
		duplicate := false
		for _, existing := range management {
			if existing.Equals(copied) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			management = append(management, copied)
		}
	}
	if len(management) == 0 {
		return nil, errors.New("token management SIDs are unavailable")
	}
	return management, nil
}

func createWindowsSecureDirectory(path string, capabilitySID *windows.SID, readOnly bool) error {
	managementSIDs, err := windowsManagementSIDs()
	if err != nil {
		return err
	}
	entries := make([]windowsACE, 0, len(managementSIDs)+2)
	// These are the management identity's *restricted* SIDs plus its user SID.
	// The sandbox child does not receive them as restricting SIDs.
	for _, sid := range managementSIDs {
		entries = append(entries, windowsACE{sid: sid, mask: windows.ACCESS_MASK(fileAllAccess)})
	}
	// The child's unrestricted token can reach this through Everyone/RX; its
	// restricted token must also pass through the unique capability SID. For
	// workspace mode the capability is Full; for read-only mode it is RX only.
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return err
	}
	entries = append(entries, windowsACE{
		sid: everyone, mask: windows.ACCESS_MASK(0x001200A9),
	})
	if capabilitySID != nil {
		mask := uint32(windows.GENERIC_READ | windows.GENERIC_EXECUTE)
		if !readOnly {
			mask = fileAllAccess
		}
		entries = append(entries, windowsACE{
			sid: capabilitySID, mask: windows.ACCESS_MASK(mask),
		})
	}
	if readOnly {
		usersSID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
		if err != nil {
			return err
		}
		entries = append(entries, windowsACE{
			sid: usersSID, mask: windows.ACCESS_MASK(0x001200A9),
		})
	}
	acl, err := newWindowsACL(entries)
	if err != nil {
		return err
	}
	sd, err := newWindowsSecurityDescriptorWithDACL(acl, true)
	if err != nil {
		return err
	}
	if !sd.IsValid() {
		return errors.New("security descriptor is invalid")
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	ptr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(ptr, &attributes); err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return errors.New("secure sandbox directory already exists")
		}
		return err
	}
	return nil
}

func newWindowsACL(entries []windowsACE) (*windows.ACL, error) {
	if len(entries) == 0 {
		return nil, errors.New("ACL requires at least one ACE")
	}
	size := unsafe.Sizeof(windows.ACL{})
	for _, entry := range entries {
		sidLen := uintptr(entry.sid.Len())
		aceSize := unsafe.Sizeof(windows.ACCESS_ALLOWED_ACE{}) + sidLen - unsafe.Sizeof(entry.mask)
		size += (aceSize + 3) &^ 3
	}
	raw := make([]byte, size)
	acl := (*windows.ACL)(unsafe.Pointer(&raw[0]))
	r1, _, callErr := procInitializeACL.Call(uintptr(unsafe.Pointer(acl)), uintptr(len(raw)), 2)
	if r1 == 0 {
		if callErr == nil || callErr == syscall.Errno(0) {
			callErr = windows.GetLastError()
		}
		return nil, callErr
	}
	for _, entry := range entries {
		r1, _, callErr := procAddAccessAllowedAceEx.Call(
			uintptr(unsafe.Pointer(acl)), 2, 3,
			uintptr(entry.mask), uintptr(unsafe.Pointer(entry.sid)),
		)
		if r1 == 0 {
			if callErr == nil || callErr == syscall.Errno(0) {
				callErr = windows.GetLastError()
			}
			return nil, callErr
		}
	}
	return acl, nil
}

func windowsDACLGrants(sd *windows.SECURITY_DESCRIPTOR, sid *windows.SID, required windows.ACCESS_MASK) (bool, error) {
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	if dacl == nil || dacl.AceCount == 0 {
		return false, nil
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var allowed *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &allowed); err != nil {
			return false, err
		}
		if allowed == nil || allowed.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if allowed.Mask&required != required {
			continue
		}
		// SidStart aliases the first DWORD of the SID; it is not a prefix
		// field followed by the SID.
		aceSID := (*windows.SID)(unsafe.Pointer(&allowed.SidStart))
		if aceSID.Equals(sid) {
			return true, nil
		}
	}
	return false, nil
}

func workspaceWriteSID(path string) string {
	digest := sha256.Sum256([]byte(path))
	first := (binary.LittleEndian.Uint32(digest[0:4]) % (1<<30 - 1)) + 1
	second := (binary.LittleEndian.Uint32(digest[4:8]) % (1<<30 - 1)) + 1
	return fmt.Sprintf("%s-%d-%d", windowsCurrentUser().String(), first, second)
}

func workspaceReadSID(path string) string {
	digest := sha256.Sum256([]byte("read\x00" + path))
	first := (binary.LittleEndian.Uint32(digest[0:4]) % (1<<30 - 1)) + 1
	second := (binary.LittleEndian.Uint32(digest[4:8]) % (1<<30 - 1)) + 1
	return fmt.Sprintf("%s-%d-%d-2", windowsCurrentUser().String(), first, second)
}

func workspaceReadAuxSID(path string) string {
	digest := sha256.Sum256([]byte("read-aux\x00" + path))
	first := (binary.LittleEndian.Uint32(digest[0:4]) % (1<<30 - 1)) + 1
	second := (binary.LittleEndian.Uint32(digest[4:8]) % (1<<30 - 1)) + 1
	return fmt.Sprintf("%s-%d-%d-3", windowsCurrentUser().String(), first, second)
}

func tempWriteSID(path string) string {
	_ = path
	digest := sha256.Sum256([]byte("temp\x00" + path))
	first := (binary.LittleEndian.Uint32(digest[0:4]) % (1<<30 - 1)) + 1
	second := (binary.LittleEndian.Uint32(digest[4:8]) % (1<<30 - 1)) + 1
	return fmt.Sprintf("%s-%d-%d-1", windowsCurrentUser().String(), first, second)
}

func windowsCapabilityAuthority() string {
	capabilityAuthorityOnce.Do(func() {
		token, err := openWindowsACLCurrentToken()
		if err != nil {
			return
		}
		defer windows.CloseHandle(windows.Handle(token))
		logon, err := windowsACLLogonSID(token)
		if err != nil {
			return
		}
		capabilityAuthoritySID = logon.String()
	})
	if capabilityAuthoritySID == "" {
		return "S-1-4"
	}
	return capabilityAuthoritySID
}

func pathsOverlap(a, b string) bool {
	a, _ = filepath.Abs(filepath.Clean(a))
	b, _ = filepath.Abs(filepath.Clean(b))
	relA, errA := filepath.Rel(a, b)
	relB, errB := filepath.Rel(b, a)
	if filepath.VolumeName(a) != filepath.VolumeName(b) {
		return false
	}
	if errA != nil || errB != nil {
		return true
	}
	inside := func(rel string) bool {
		return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
	}
	return inside(relA) || inside(relB)
}

func setEnvironmentValue(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, item := range env {
		if strings.HasPrefix(strings.ToUpper(item), strings.ToUpper(prefix)) {
			if !replaced {
				out = append(out, prefix+value)
				replaced = true
			}
			continue
		}
		out = append(out, item)
	}
	if !replaced {
		out = append(out, prefix+value)
	}
	return out
}

func windowsACLProbe() error {
	root, err := os.MkdirTemp("", "shutu-acl-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	root, err = pathsecure.ResolveExisting(root)
	if err != nil {
		return err
	}
	workspace := filepath.Join(root, "workspace")
	workspaceSID, err := windows.StringToSid(workspaceWriteSID(workspace))
	if err != nil {
		return err
	}
	// The probe creates its own capability-granted workspace so the probe can
	// validate the token/file-effect seam on managed hosts where an inherited
	// temp DACL lacks WRITE_DAC. Production workspaces still fail closed when
	// mutation is required but denied.
	if err := createWindowsSecureDirectory(workspace, workspaceSID, false); err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(workspace, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	hasGrant, err := windowsDACLGrants(sd, workspaceSID, windows.ACCESS_MASK(grantMask))
	if err != nil {
		return err
	}
	if !hasGrant {
		return fmt.Errorf("probe workspace capability grant missing: sid=%s sddl=%s", workspaceSID.String(), sd.String())
	}
	if !workspaceSID.IsValid() {
		return fmt.Errorf("probe workspace capability SID invalid: %s", workspaceSID.String())
	}
	run, err := prepareWindowsACLRun(SandboxWorkspaceWrite, workspace)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = run.close()
		}
	}()
	smoke := exec.Command("cmd.exe", "/d", "/s", "/c", "exit /b 42")
	smoke.Dir = workspace
	var diagnostic bytes.Buffer
	smoke.Stdout = &diagnostic
	smoke.Stderr = &diagnostic
	if err := run.configure(smoke); err != nil {
		return err
	}
	if err := smoke.Run(); err == nil {
		return errors.New("ACL probe smoke command unexpectedly succeeded")
	} else if exitErr := (*exec.ExitError)(nil); !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		return fmt.Errorf("ACL probe smoke command failed: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	diagnostic.Reset()
	allowed := filepath.Join(workspace, "allowed.txt")
	cmd := exec.Command("cmd.exe", "/d", "/s", "/c", "echo probe > allowed.txt")
	cmd.Dir = workspace
	cmd.Stdout = &diagnostic
	cmd.Stderr = &diagnostic
	if err := run.configure(cmd); err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("write probe: %w: %q", err, strings.TrimSpace(diagnostic.String()))
	}
	if err := run.close(); err != nil {
		return fmt.Errorf("cleanup probe sandbox: %w", err)
	}
	closed = true
	if _, err := os.Stat(allowed); err != nil {
		return fmt.Errorf("workspace write probe did not create file: %w", err)
	}
	return nil
}

func windowsCommandQuote(path string) string {
	return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
}
