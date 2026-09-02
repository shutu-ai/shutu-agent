package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContentBlockJSONUsesReferenceVocabularyAndPreservesExtensions(t *testing.T) {
	known := []ContentBlock{
		Text("hello"),
		{Kind: BlockImage, Image: ImageRef{ID: "attachment-1", MediaType: "image/png", Bytes: 2, Width: 1, Height: 1, Name: "one.png"}},
		{Kind: BlockToolResult, CallID: "call-1", Blocks: []ContentBlock{Text("ok")}, IsError: true},
	}
	raw, err := json.Marshal(known)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	for _, forbidden := range []string{`"Kind"`, `"Image"`, `"CallID"`, `"Blocks"`} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("known blocks leaked runtime field %s: %s", forbidden, encoded)
		}
	}
	var decoded []ContentBlock
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded[1].Image.ID) != "attachment-1" || decoded[1].Image.Name != "one.png" {
		t.Fatalf("image round trip = %#v", decoded[1].Image)
	}

	extension := json.RawMessage(`{"type":"x-plugin/block","payload":{"opaque":true}}`)
	raw, err = json.Marshal([]ContentBlock{{Kind: "x-plugin/block", Raw: extension}})
	if err != nil || string(raw) != `[{"type":"x-plugin/block","payload":{"opaque":true}}]` {
		t.Fatalf("extension marshal = (%s, %v)", raw, err)
	}
	decoded = nil
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded[0].Kind != "x-plugin/block" || string(decoded[0].Raw) != string(extension) {
		t.Fatalf("extension round trip = %#v", decoded[0])
	}
}
