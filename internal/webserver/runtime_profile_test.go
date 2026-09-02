package webserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jabing/shutu-agent/internal/profile"
)

func TestNativeRuntimeProfilesFailClosed(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetProfileRegistry(profile.Local())
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/"+method, "tok",
			fmt.Sprintf(`{"type":"client-request","rpcId":"profiles","method":%q,"payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}

	list := call(t, "runtime.profiles", `{}`)
	if !list.Result.OK {
		t.Fatalf("runtime.profiles = %+v", list.Result)
	}
	var projected struct {
		Default  string               `json:"default"`
		Profiles []profile.Descriptor `json:"profiles"`
	}
	encoded, err := json.Marshal(list.Result.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatal(err)
	}
	if projected.Default != profile.IDStorageSQLite || len(projected.Profiles) != 9 {
		t.Fatalf("runtime profiles = %+v", projected)
	}
	available := map[string]bool{}
	for _, descriptor := range projected.Profiles {
		available[descriptor.ID] = descriptor.State == profile.StateAvailable
	}
	for _, id := range []string{
		profile.IDStorageSQLite, profile.IDFileLocal, profile.IDSessionReference,
	} {
		if !available[id] {
			t.Fatalf("selected profile %q is not available", id)
		}
	}

	selected := call(t, "runtime.profile", fmt.Sprintf(`{"id":%q}`, profile.IDStorageSQLite))
	if !selected.Result.OK {
		t.Fatalf("selected runtime.profile = %+v", selected.Result)
	}
	unsupported := call(t, "runtime.profile", fmt.Sprintf(`{"id":%q}`, profile.IDSandboxesE2B))
	if unsupported.Result.OK || unsupported.Result.Error == nil ||
		unsupported.Result.Error.Code != "profile-unsupported" {
		t.Fatalf("e2b runtime.profile = %+v, want profile-unsupported", unsupported.Result)
	}

	for _, method := range []string{
		"dynamicCordisRunner/syncInspectManifest", "dynamicCordisRunner/inventory",
	} {
		response := call(t, method, `{}`)
		if response.Result.OK || response.Result.Error == nil ||
			response.Result.Error.Code != "profile-unsupported" {
			t.Fatalf("%s = %+v, want stable profile-unsupported failure", method, response.Result)
		}
	}
}

func TestNativeRuntimeCapabilitiesExposeExactClassificationAndNegatives(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetProfileRegistry(profile.Local())
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/"+method, "tok",
			fmt.Sprintf(`{"type":"client-request","rpcId":"capabilities","method":%q,"payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}

	list := call(t, "runtime.capabilities", `{}`)
	if !list.Result.OK {
		t.Fatalf("runtime.capabilities = %+v", list.Result)
	}
	var value struct {
		Capabilities []profile.Capability `json:"capabilities"`
		Count        int                  `json:"count"`
	}
	encoded, err := json.Marshal(list.Result.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	if value.Count != len(profile.CapabilityIDs) || len(value.Capabilities) != value.Count {
		t.Fatalf("capability listing count=%d rows=%d", value.Count, len(value.Capabilities))
	}
	byID := map[string]profile.Capability{}
	for _, capability := range value.Capabilities {
		byID[capability.ID] = capability
	}
	available, ok := byID["ctx.tools"]
	if !ok || available.State != profile.StateAvailable || available.Implementation == "" {
		t.Fatalf("ctx.tools listing = %+v", available)
	}

	selected := call(t, "runtime.capability", `{"id":"ctx.tools"}`)
	if !selected.Result.OK {
		t.Fatalf("available runtime.capability = %+v", selected.Result)
	}
	e2b := call(t, "runtime.capability", `{"id":"ctx.e2b"}`)
	if e2b.Result.OK || e2b.Result.Error == nil || e2b.Result.Error.Code != "capability-unsupported" {
		t.Fatalf("optional e2b capability = %+v, want stable capability-unsupported", e2b)
	}
	cordis := call(t, "runtime.capability", `{"id":"ctx.dynamicCordisRunner"}`)
	if cordis.Result.OK || cordis.Result.Error == nil || cordis.Result.Error.Code != "capability-unsupported" {
		t.Fatalf("required Cordis capability = %+v, want stable capability-unsupported", cordis)
	}
	unknown := call(t, "runtime.capability", `{"id":"ctx.not-in-reference"}`)
	if unknown.Result.OK || unknown.Result.Error == nil || unknown.Result.Error.Code != "capability-unknown" {
		t.Fatalf("unknown capability = %+v, want capability-unknown", unknown)
	}
}
