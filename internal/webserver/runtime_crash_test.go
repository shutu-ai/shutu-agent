package webserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jabing/shutu-agent/internal/crashboundary"
)

func TestNativeCrashBoundariesAreMachineReadable(t *testing.T) {
	srv, _ := newTestServer(t, "tok")
	srv.SetCrashBoundaryRegistry(crashboundary.Required())
	call := func(t *testing.T, method, payload string) nativeRPCResponse {
		t.Helper()
		rec := doReqBody(t, srv.Handler(), http.MethodPost, "/api/"+method, "tok",
			fmt.Sprintf(`{"type":"client-request","rpcId":"crash","method":%q,"payload":%s}`, method, payload))
		return nativeResponse(t, rec.Body.Bytes())
	}

	list := call(t, "runtime.crash-contracts", `{}`)
	if !list.Result.OK {
		t.Fatalf("runtime.crash-contracts = %+v", list.Result)
	}
	encoded, err := json.Marshal(list.Result.Value)
	if err != nil {
		t.Fatal(err)
	}
	var projected struct {
		Contracts []crashboundary.Contract `json:"contracts"`
	}
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatal(err)
	}
	if len(projected.Contracts) != 8 {
		t.Fatalf("crash contracts = %d, want 8", len(projected.Contracts))
	}

	one := call(t, "runtime.crash-contract", fmt.Sprintf(`{"id":%q}`, "mcp.call"))
	if !one.Result.OK {
		t.Fatalf("mcp crash contract = %+v", one.Result)
	}
	var contract crashboundary.Contract
	if err := json.Unmarshal(mustValue(t, one.Result.Value), &contract); err != nil {
		t.Fatal(err)
	}
	if contract.CrashPolicy != crashboundary.AtMostOnce ||
		contract.TransportFailurePolicy != crashboundary.NoAutomaticReplay ||
		!contract.ProcessDeathLosesEffect || contract.AutomaticReplay {
		t.Fatalf("MCP crash contract = %+v", contract)
	}
	missing := call(t, "runtime.crash-contract", `{"id":"not-a-boundary"}`)
	if missing.Result.OK || missing.Result.Error == nil || missing.Result.Error.Code != "crash-boundary-unknown" {
		t.Fatalf("unknown crash contract = %+v", missing.Result)
	}
}

func mustValue(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
