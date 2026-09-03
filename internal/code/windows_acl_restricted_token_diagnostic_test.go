//go:build windows

package code

import (
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var isTokenRestrictedDiagnosticProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("IsTokenRestricted")

type restrictedTokenDiagnosticCase struct {
	name  string
	flags uintptr
	sids  []windows.SIDAndAttributes
}

func TestCreateRestrictedTokenParameterMatrix(t *testing.T) {
	current, err := openWindowsACLCurrentToken()
	if err != nil {
		t.Fatalf("open existing token: %v", err)
	}
	defer windows.CloseHandle(windows.Handle(current))

	const existingTokenAccess = windows.TOKEN_QUERY | windows.TOKEN_DUPLICATE |
		windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ASSIGN_PRIMARY
	t.Logf("ExistingToken type: %s", tokenTypeDiagnostic(t, current))
	t.Logf("ExistingToken access mask: requested=0x%x (OpenProcessToken)", existingTokenAccess)
	t.Logf("ExistingToken IsTokenRestricted: %s", isTokenRestrictedDiagnostic(t, current))
	t.Logf("ExistingToken TokenElevationType: %s", tokenElevationTypeDiagnostic(t, current))
	t.Logf("ExistingToken Integrity level: %s", tokenIntegrityDiagnostic(t, current))

	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("create world SID: %v", err)
	}
	currentSID, err := requireWindowsCurrentUser()
	if err != nil {
		t.Fatalf("current user SID: %v", err)
	}
	workspaceSID, err := windows.StringToSid(workspaceWriteSID(t.TempDir()))
	if err != nil {
		t.Fatalf("create workspace SID: %v", err)
	}
	tempSID, err := windows.StringToSid(tempWriteSID(t.TempDir()))
	if err != nil {
		t.Fatalf("create temp SID: %v", err)
	}

	entries := []windows.SIDAndAttributes{
		{Sid: worldSID},
		{Sid: workspaceSID},
		{Sid: tempSID},
	}
	readOnlySID, err := windows.StringToSid(workspaceReadSID(t.TempDir()))
	if err != nil {
		t.Fatalf("create readonly SID: %v", err)
	}
	logonSID, err := windowsACLLogonSID(current)
	if err != nil {
		t.Fatalf("current logon SID: %v", err)
	}
	logSIDAndAttributesDiagnostic(t, entries, []string{"#1 world", "#2 workspace capability", "#3 private temp capability"})
	logSIDAndAttributesDiagnostic(t, []windows.SIDAndAttributes{{Sid: currentSID}}, []string{"current user diagnostic SID"})
	logSIDAndAttributesLayoutDiagnostic(t, entries)

	caseSID := func(indexes ...int) []windows.SIDAndAttributes {
		result := make([]windows.SIDAndAttributes, 0, len(indexes))
		for _, index := range indexes {
			result = append(result, entries[index])
		}
		return result
	}
	cases := []restrictedTokenDiagnosticCase{
		{name: "A flags=0 count=0", flags: 0},
		{name: "B LUA_TOKEN count=0", flags: createRestrictedTokenLua},
		{name: "C flags=0 SID#1", flags: 0, sids: caseSID(0)},
		{name: "D LUA_TOKEN SID#1", flags: createRestrictedTokenLua, sids: caseSID(0)},
		{name: "E LUA_TOKEN SID#1", flags: createRestrictedTokenLua, sids: caseSID(0)},
		{name: "E LUA_TOKEN SID#2", flags: createRestrictedTokenLua, sids: caseSID(1)},
		{name: "E LUA_TOKEN SID#3", flags: createRestrictedTokenLua, sids: caseSID(2)},
		{name: "F LUA_TOKEN SID#1+SID#2", flags: createRestrictedTokenLua, sids: caseSID(0, 1)},
		{name: "F LUA_TOKEN SID#1+SID#3", flags: createRestrictedTokenLua, sids: caseSID(0, 2)},
		{name: "F LUA_TOKEN SID#2+SID#3", flags: createRestrictedTokenLua, sids: caseSID(1, 2)},
		{name: "F LUA_TOKEN SID#1+SID#2+SID#3", flags: createRestrictedTokenLua, sids: caseSID(0, 1, 2)},
		{name: "G production flags=LUA_TOKEN count=3", flags: createRestrictedTokenLua, sids: caseSID(0, 1, 2)},
		{name: "H current readonly flags=LUA_TOKEN count=3", flags: createRestrictedTokenLua, sids: []windows.SIDAndAttributes{
			{Sid: worldSID, Attributes: windows.SE_GROUP_USE_FOR_DENY_ONLY},
			{Sid: logonSID, Attributes: windows.SE_GROUP_USE_FOR_DENY_ONLY},
			{Sid: readOnlySID},
		}},
		{name: "I fixed readonly production flags=LUA_TOKEN count=3", flags: createRestrictedTokenLua, sids: windowsACLRestrictingSIDs(SandboxReadOnly, worldSID, logonSID, readOnlySID, nil, nil)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logSIDAndAttributesDiagnostic(t, tc.sids, nil)
			if len(tc.sids) == 0 {
				t.Logf("CreateRestrictedToken flags: %s (0x%x)", createRestrictedTokenFlagsName(tc.flags), tc.flags)
				t.Logf("DisableSidCount=0 SidsToDisable=NULL DeletePrivilegeCount=0 PrivilegesToDelete=NULL RestrictedSidCount=0 SidsToRestrict=NULL")
			} else {
				t.Logf("CreateRestrictedToken flags: %s (0x%x)", createRestrictedTokenFlagsName(tc.flags), tc.flags)
				t.Logf("DisableSidCount=0 SidsToDisable=NULL DeletePrivilegeCount=0 PrivilegesToDelete=NULL RestrictedSidCount=%d SidsToRestrict=%p", len(tc.sids), unsafe.Pointer(&tc.sids[0]))
			}
			created, callErr := callCreateRestrictedTokenDiagnostic(current, tc.sids, tc.flags)
			if callErr != nil {
				t.Logf("CreateRestrictedToken RESULT: FAIL GetLastError numeric=%d message=%v", errnoDiagnostic(callErr), callErr)
				return
			}
			defer windows.CloseHandle(windows.Handle(created))
			t.Logf("CreateRestrictedToken RESULT: PASS")
			t.Logf("new token IsTokenRestricted: %s", isTokenRestrictedDiagnostic(t, created))
			t.Logf("new token TokenRestrictedSids: %s", tokenRestrictedSIDsDiagnostic(t, created))
			t.Logf("new token TokenElevationType: %s", tokenElevationTypeDiagnostic(t, created))
			t.Logf("new token Integrity level: %s", tokenIntegrityDiagnostic(t, created))
		})
	}
}

func TestCreateRestrictedTokenProductionReadOnlyParameters(t *testing.T) {
	current, err := openWindowsACLCurrentToken()
	if err != nil {
		t.Fatalf("open existing token: %v", err)
	}
	defer windows.CloseHandle(windows.Handle(current))
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatalf("create world SID: %v", err)
	}
	logonSID, err := windowsACLLogonSID(current)
	if err != nil {
		t.Fatalf("current logon SID: %v", err)
	}
	readOnlySID, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatalf("create readonly SID: %v", err)
	}
	restricting := windowsACLRestrictingSIDs(SandboxReadOnly, worldSID, logonSID, readOnlySID, nil, nil)
	if len(restricting) != 3 {
		t.Fatalf("production readonly restricting SID count=%d, want 3", len(restricting))
	}
	for index, entry := range restricting {
		if entry.Sid == nil || !entry.Sid.IsValid() {
			t.Fatalf("production readonly SID[%d] is invalid", index)
		}
		if entry.Attributes != 0 {
			t.Fatalf("production readonly SID[%d] attributes=0x%x, want 0", index, entry.Attributes)
		}
	}
	logSIDAndAttributesDiagnostic(t, restricting, []string{"readonly world", "readonly logon", "readonly Users"})
	created, err := callCreateRestrictedTokenDiagnostic(current, restricting, createRestrictedTokenLua)
	if err != nil {
		t.Fatalf("CreateRestrictedToken readonly production parameters: %v", err)
	}
	defer windows.CloseHandle(windows.Handle(created))
	if got := isTokenRestrictedDiagnostic(t, created); got != "true" {
		t.Fatalf("IsTokenRestricted(newToken)=%s, want true", got)
	}
	buffer, err := createRestrictedTokenInfoBytesDiagnostic(created, windows.TokenRestrictedSids)
	if err != nil {
		t.Fatalf("GetTokenInformation(TokenRestrictedSids): %v", err)
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	if groups.GroupCount != uint32(len(restricting)) {
		t.Fatalf("TokenRestrictedSids count=%d, want %d", groups.GroupCount, len(restricting))
	}
	for index, group := range groups.AllGroups() {
		if group.Sid == nil || !group.Sid.Equals(restricting[index].Sid) {
			t.Fatalf("TokenRestrictedSids[%d]=%v, want %s", index, group.Sid, restricting[index].Sid.String())
		}
	}
	t.Logf("CreateRestrictedToken RESULT: PASS; IsTokenRestricted=true; TokenRestrictedSids=%d", groups.GroupCount)
}

func createRestrictedTokenFlagsName(flags uintptr) string {
	if flags == 0 {
		return "0"
	}
	parts := make([]string, 0, 3)
	if flags&createRestrictedTokenDisableMaxPrivilege != 0 {
		parts = append(parts, "DISABLE_MAX_PRIVILEGE")
	}
	if flags&createRestrictedTokenLua != 0 {
		parts = append(parts, "LUA_TOKEN")
	}
	if flags&createRestrictedTokenWriteRestricted != 0 {
		parts = append(parts, "WRITE_RESTRICTED")
	}
	return fmt.Sprintf("%v", parts)
}

func callCreateRestrictedTokenDiagnostic(current windows.Token, restricting []windows.SIDAndAttributes, flags uintptr) (windows.Token, error) {
	var restricted windows.Token
	var restrictingPointer uintptr
	if len(restricting) > 0 {
		restrictingPointer = uintptr(unsafe.Pointer(&restricting[0]))
	}
	r1, _, callErr := createRestrictedTokenProc.Call(
		uintptr(current), flags,
		0, 0,
		0, 0,
		uintptr(len(restricting)), restrictingPointer, uintptr(unsafe.Pointer(&restricted)),
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

func logSIDAndAttributesDiagnostic(t *testing.T, entries []windows.SIDAndAttributes, purposes []string) {
	t.Helper()
	for index, entry := range entries {
		purpose := ""
		if index < len(purposes) {
			purpose = purposes[index]
		}
		if entry.Sid == nil {
			t.Logf("SID index=%d purpose=%q SID=<nil> IsValidSid=false GetLengthSid=0 Attributes=0x%x", index+1, purpose, entry.Attributes)
			continue
		}
		t.Logf("SID index=%d purpose=%q SID=%q IsValidSid=%t GetLengthSid=%d Attributes=0x%x", index+1, purpose, entry.Sid.String(), entry.Sid.IsValid(), entry.Sid.Len(), entry.Attributes)
	}
}

func logSIDAndAttributesLayoutDiagnostic(t *testing.T, entries []windows.SIDAndAttributes) {
	t.Helper()
	typeInfo := reflect.TypeOf(windows.SIDAndAttributes{})
	sidField, _ := typeInfo.FieldByName("Sid")
	attributesField, _ := typeInfo.FieldByName("Attributes")
	stride := unsafe.Sizeof(windows.SIDAndAttributes{})
	t.Logf("SID_AND_ATTRIBUTES Go layout: size=%d align=%d Sid(type=%s offset=%d) Attributes(type=%s offset=%d) pointerSize=%d", stride, unsafe.Alignof(windows.SIDAndAttributes{}), sidField.Type, sidField.Offset, attributesField.Type, attributesField.Offset, unsafe.Sizeof(uintptr(0)))
	if sidField.Type.Kind() != reflect.Pointer || sidField.Type.Elem() != reflect.TypeOf(windows.SID{}) {
		t.Errorf("SID_AND_ATTRIBUTES.Sid type=%s, want pointer to windows.SID", sidField.Type)
	}
	if attributesField.Type.Kind() != reflect.Uint32 {
		t.Errorf("SID_AND_ATTRIBUTES.Attributes type=%s, want uint32", attributesField.Type)
	}
	for index := 1; index < len(entries); index++ {
		previous := uintptr(unsafe.Pointer(&entries[index-1]))
		current := uintptr(unsafe.Pointer(&entries[index]))
		if current-previous != stride {
			t.Errorf("SID_AND_ATTRIBUTES array stride=%d at index=%d, want %d", current-previous, index, stride)
		}
	}
}

func createRestrictedTokenInfoBytesDiagnostic(token windows.Token, class uint32) ([]byte, error) {
	var needed uint32
	err := windows.GetTokenInformation(token, class, nil, 0, &needed)
	if needed == 0 && err != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, err
	}
	if needed == 0 {
		needed = 1024
	}
	buffer := make([]byte, needed)
	if err := windows.GetTokenInformation(token, class, &buffer[0], uint32(len(buffer)), &needed); err != nil {
		return nil, err
	}
	return buffer[:needed], nil
}

func tokenTypeDiagnostic(t *testing.T, token windows.Token) string {
	t.Helper()
	return scalarTokenInfoDiagnostic(t, token, windows.TokenType)
}

func tokenElevationTypeDiagnostic(t *testing.T, token windows.Token) string {
	t.Helper()
	return scalarTokenInfoDiagnostic(t, token, windows.TokenElevationType)
}

func scalarTokenInfoDiagnostic(t *testing.T, token windows.Token, class uint32) string {
	t.Helper()
	buffer, err := createRestrictedTokenInfoBytesDiagnostic(token, class)
	if err != nil || len(buffer) < 4 {
		return fmt.Sprintf("unavailable err=%v", err)
	}
	return fmt.Sprintf("%d", *(*uint32)(unsafe.Pointer(&buffer[0])))
}

func tokenIntegrityDiagnostic(t *testing.T, token windows.Token) string {
	t.Helper()
	buffer, err := createRestrictedTokenInfoBytesDiagnostic(token, windows.TokenIntegrityLevel)
	if err != nil || len(buffer) < int(unsafe.Sizeof(windows.Tokenmandatorylabel{})) {
		return fmt.Sprintf("unavailable err=%v", err)
	}
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0]))
	if label.Label.Sid == nil {
		return "SID=<nil>"
	}
	return fmt.Sprintf("SID=%s RID=%d", label.Label.Sid.String(), label.Label.Sid.SubAuthority(uint32(label.Label.Sid.SubAuthorityCount()-1)))
}

func tokenRestrictedSIDsDiagnostic(t *testing.T, token windows.Token) string {
	t.Helper()
	buffer, err := createRestrictedTokenInfoBytesDiagnostic(token, windows.TokenRestrictedSids)
	if err != nil {
		return fmt.Sprintf("unavailable err=%v", err)
	}
	if len(buffer) < int(unsafe.Sizeof(windows.Tokengroups{})) {
		return fmt.Sprintf("invalid buffer length=%d", len(buffer))
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0]))
	values := make([]string, 0, groups.GroupCount)
	for _, group := range groups.AllGroups() {
		if group.Sid == nil {
			values = append(values, "<nil>")
			continue
		}
		values = append(values, fmt.Sprintf("%s(attributes=0x%x)", group.Sid.String(), group.Attributes))
	}
	return fmt.Sprintf("count=%d sids=%v", groups.GroupCount, values)
}

func isTokenRestrictedDiagnostic(t *testing.T, token windows.Token) string {
	t.Helper()
	r1, _, callErr := isTokenRestrictedDiagnosticProc.Call(uintptr(token))
	if r1 != 0 {
		return "true"
	}
	if callErr == nil || callErr == syscall.Errno(0) {
		callErr = windows.GetLastError()
	}
	return fmt.Sprintf("false err=%v", callErr)
}

func errnoDiagnostic(err error) uint32 {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return uint32(errno)
	}
	return 0
}
