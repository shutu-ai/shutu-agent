package tools

import "encoding/json"

// DecodeArgs decodes the already-materialized tool arguments into a concrete
// input struct. It exists at the tool boundary so implementations never
// receive or depend on raw JSON bytes.
func DecodeArgs(args any, dst any) error {
	if args == nil {
		return json.Unmarshal([]byte("null"), dst)
	}
	// Direct unit tests may invoke a tool body without the registry. The runtime
	// itself always passes the parsed value from ParseArguments; accepting these
	// two concrete forms here does not reintroduce a raw-argument tool API.
	switch raw := args.(type) {
	case json.RawMessage:
		return json.Unmarshal(raw, dst)
	case []byte:
		return json.Unmarshal(raw, dst)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}
