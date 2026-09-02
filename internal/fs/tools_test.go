package fs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jabing/shutu-agent/internal/attachment"
	"github.com/jabing/shutu-agent/internal/session"
)

func TestDecodeWebPConfigReadsVP8XCanvas(t *testing.T) {
	data := make([]byte, 30)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WEBP")
	copy(data[12:16], "VP8X")
	binary.LittleEndian.PutUint32(data[16:20], 10)
	// VP8X stores width-1 and height-1 as three-byte little-endian values.
	data[24], data[25], data[26] = 0x3f, 0x01, 0x00 // 320px
	data[27], data[28], data[29] = 0xef, 0x00, 0x00 // 240px

	width, height, err := attachment.ProbeImage(data, "image/webp")
	if err != nil {
		t.Fatalf("decode WebP config: %v", err)
	}
	if width != 320 || height != 240 {
		t.Fatalf("WebP config = %dx%d, want 320x240", width, height)
	}
}

func TestReadImageUsesSharedAdmissionLimits(t *testing.T) {
	svc, ft, _ := newToolsWithEvents(t)
	ft.SetImageLimits(attachment.Limits{MaxImageDimension: 1}, 1<<20)
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.White)
	var data bytes.Buffer
	if err := png.Encode(&data, img); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	path := filepath.Join(svc.Root(), "oversized.png")
	if err := os.WriteFile(path, data.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := ft.ReadImage().ExecuteResult(context.Background(), map[string]any{"file_path": "oversized.png"})
	if err == nil || !strings.Contains(err.Error(), "dimension") {
		t.Fatalf("read_image error = %v, want shared dimension limit", err)
	}
}

// eventRec is one event emitted through the FsTools onEvent sink.
type eventRec struct {
	typ  string
	data any
}

// newToolsWithEvents returns a FileService and an FsTools bundle wired to a
// slice that records every emitted fs/* event (the composition root wires the
// same sink to the session log in cmd/pa, D3).
func newToolsWithEvents(t *testing.T) (FileService, *FsTools, *[]eventRec) {
	t.Helper()
	svc := NewLocalFS(t.TempDir())
	t.Cleanup(func() { svc.Close() })
	recs := &[]eventRec{}
	return svc, NewFsTools(svc, func(typ string, data any) {
		*recs = append(*recs, eventRec{typ: typ, data: data})
	}), recs
}

// decodeEvent unmarshals a captured event payload into T (the session payloads
// are plain JSON-serializable data).
func decodeEvent[T any](t *testing.T, ev eventRec) T {
	t.Helper()
	raw, err := json.Marshal(ev.data)
	if err != nil {
		t.Fatalf("marshal %s event data: %v", ev.typ, err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s event data %s: %v", ev.typ, raw, err)
	}
	return out
}

// eventTypes returns the emitted event types in order.
func eventTypes(recs []eventRec) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.typ)
	}
	return out
}

// TestFsToolSchemas verifies the D7 shapes the registry compiles and sends to
// the model (dispatch-m6f-3 §4): additionalProperties false and the required
// fields for each tool.
func TestFsToolSchemas(t *testing.T) {
	_, ft, _ := newToolsWithEvents(t)
	write := ft.Write().Schema()
	if write["type"] != "object" || write["additionalProperties"] != false {
		t.Fatalf("write schema = %+v, want type object / additionalProperties false", write)
	}
	wreq, _ := write["required"].([]string)
	if len(wreq) != 2 || wreq[0] != "file_path" || wreq[1] != "content" {
		t.Fatalf("write required = %v, want [file_path content]", wreq)
	}
	wprops, _ := write["properties"].(map[string]any)
	if _, ok := wprops["content"]; !ok {
		t.Fatal("write content property missing")
	}

	list := ft.List().Schema()
	if list["type"] != "object" || list["additionalProperties"] != false {
		t.Fatalf("list schema = %+v, want type object / additionalProperties false", list)
	}
	lreq, _ := list["required"].([]string)
	if len(lreq) != 1 || lreq[0] != "dir" {
		t.Fatalf("list required = %v, want [dir]", lreq)
	}

	edit := ft.Edit().Schema()
	if edit["type"] != "object" || edit["additionalProperties"] != false {
		t.Fatalf("edit schema = %+v, want type object / additionalProperties false", edit)
	}
	ereq, _ := edit["required"].([]string)
	if len(ereq) != 3 || ereq[0] != "file_path" || ereq[1] != "old_string" || ereq[2] != "new_string" {
		t.Fatalf("edit required = %v, want [file_path old_string new_string]", ereq)
	}

	read := ft.Read().Schema()
	if read["type"] != "object" || read["additionalProperties"] != false {
		t.Fatalf("read schema = %+v, want type object / additionalProperties false", read)
	}
}

// TestFsWriteToolWritesAndEmits covers the happy path: write creates the
// file (and its missing parents), returns the written path, and lands fs/write
// through the event sink.
func TestFsWriteToolWritesAndEmits(t *testing.T) {
	svc, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	out, err := ft.Write().Execute(ctx, json.RawMessage(`{"path":"a/b/deep.txt","content":"deep"}`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(out, "a/b/deep.txt") {
		t.Fatalf("write output = %q, want it to carry the written path", out)
	}
	got, err := svc.Read(ctx, "a/b/deep.txt", 0)
	if err != nil || got != "deep" {
		t.Fatalf("read back = %q, %v, want deep", got, err)
	}
	if types := eventTypes(*recs); len(types) != 1 || types[0] != session.EventFsWrite {
		t.Fatalf("emitted types = %v, want [fs/write]", types)
	}
	d := decodeEvent[struct {
		Path string `json:"path"`
	}](t, (*recs)[0])
	if d.Path != "a/b/deep.txt" {
		t.Fatalf("fs/write payload = %+v, want path a/b/deep.txt", d)
	}
}

// TestFsListToolListsAndEmits covers the happy path: list returns the
// formatted table and lands fs/list (dir + count) through the event sink.
func TestFsListToolListsAndEmits(t *testing.T) {
	svc, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if err := svc.Write(ctx, "notes.txt", "hello fs"); err != nil {
		t.Fatalf("seed notes.txt: %v", err)
	}
	if err := svc.Write(ctx, "d/inner.txt", "x"); err != nil {
		t.Fatalf("seed nested: %v", err)
	}
	out, err := ft.List().Execute(ctx, json.RawMessage(`{"dir":"."}`))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "[.] 2 entries") || !strings.Contains(out, "notes.txt") || !strings.Contains(out, "d  dir") {
		t.Fatalf("list output = %q, want the header and both entries", out)
	}
	if types := eventTypes(*recs); len(types) != 1 || types[0] != session.EventFsList {
		t.Fatalf("emitted types = %v, want [fs/list]", types)
	}
	d := decodeEvent[struct {
		Dir   string `json:"dir"`
		Count int    `json:"count"`
	}](t, (*recs)[0])
	if d.Dir != "." || d.Count != 2 {
		t.Fatalf("fs/list payload = %+v, want dir . / count 2", d)
	}
}

// TestFsEditToolReplacesAndEmits covers the dsh edit semantics: the FIRST
// occurrence is replaced (replace_all replaces every one), the file is written
// back, and fs/write is emitted. An absent old_string errors and leaves the
// file untouched.
func TestFsEditToolReplacesAndEmits(t *testing.T) {
	svc, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if err := svc.Write(ctx, "notes.txt", "alpha beta alpha"); err != nil {
		t.Fatalf("seed notes.txt: %v", err)
	}
	if _, err := ft.Read().Execute(ctx, json.RawMessage(`{"path":"notes.txt"}`)); err != nil {
		t.Fatalf("observe notes.txt: %v", err)
	}
	out, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"notes.txt","old_string":"alpha","new_string":"gamma"}`))
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if !strings.Contains(out, "notes.txt") {
		t.Fatalf("edit output = %q, want the edited path", out)
	}
	got, err := svc.Read(ctx, "notes.txt", 0)
	if err != nil || got != "gamma beta alpha" {
		t.Fatalf("after first-occurrence edit = %q, %v, want gamma beta alpha", got, err)
	}
	if types := eventTypes(*recs); len(types) != 2 || types[0] != session.EventFsRead || types[1] != session.EventFsWrite {
		t.Fatalf("emitted types = %v, want [fs/read fs/write]", types)
	}

	// replace_all replaces every occurrence.
	if _, err := ft.Read().Execute(ctx, json.RawMessage(`{"path":"notes.txt"}`)); err != nil {
		t.Fatalf("re-observe notes.txt: %v", err)
	}
	_, err = ft.Edit().Execute(ctx, json.RawMessage(`{"path":"notes.txt","old_string":"alpha","new_string":"x","replace_all":true}`))
	if err != nil {
		t.Fatalf("edit all: %v", err)
	}
	got, err = svc.Read(ctx, "notes.txt", 0)
	if err != nil || got != "gamma beta x" {
		t.Fatalf("after replace-all edit = %q, %v, want gamma beta x", got, err)
	}
}

// TestFsEditToolMissingOldStringErrors verifies an absent old_string is an
// error and the file is left untouched.
func TestFsEditToolMissingOldStringErrors(t *testing.T) {
	svc, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if err := svc.Write(ctx, "notes.txt", "hello"); err != nil {
		t.Fatalf("seed notes.txt: %v", err)
	}
	if _, err := ft.Read().Execute(ctx, json.RawMessage(`{"path":"notes.txt"}`)); err != nil {
		t.Fatalf("observe notes.txt: %v", err)
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"notes.txt","old_string":"nope","new_string":"x"}`)); err == nil {
		t.Fatal("edit with an absent old_string must error")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("edit error = %v, want a not-found message", err)
	}
	got, err := svc.Read(ctx, "notes.txt", 0)
	if err != nil || got != "hello" {
		t.Fatalf("file must stay untouched, got %q, %v", got, err)
	}
	if types := eventTypes(*recs); len(types) != 1 || types[0] != session.EventFsRead {
		t.Fatalf("failed edit must not add an event, got %v", types)
	}
}

func TestFsReadToolWindowAndObservationPolicy(t *testing.T) {
	svc, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if err := svc.Write(ctx, "notes.txt", "one\ntwo\nthree\nfour"); err != nil {
		t.Fatalf("seed notes.txt: %v", err)
	}
	out, err := ft.Read().Execute(ctx, json.RawMessage(`{"path":"notes.txt","offset":2,"limit":2}`))
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if out != "2\ttwo\n3\tthree" {
		t.Fatalf("read window = %q, want numbered lines 2-3", out)
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"notes.txt","old_string":"two","new_string":"TWO"}`)); err != nil {
		t.Fatalf("edit after read: %v", err)
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"notes.txt","old_string":"three","new_string":"THREE"}`)); err == nil {
		t.Fatal("second edit without a fresh read must be rejected")
	}
	if len(*recs) < 1 || (*recs)[0].typ != session.EventFsRead {
		t.Fatalf("events = %v, want fs/read first", eventTypes(*recs))
	}
}

// TestFsToolsRejectBadArgs verifies the tools' own argument checks (the
// registry enforces the same via D7): empty path/dir/content/old_string errors
// are returned, and no fs/* event may be emitted on a failed call.
func TestFsToolsRejectBadArgs(t *testing.T) {
	_, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if _, err := ft.Write().Execute(ctx, json.RawMessage(`{"path":"","content":"x"}`)); err == nil {
		t.Fatal("write with an empty path must error")
	}
	if _, err := ft.Write().Execute(ctx, json.RawMessage(`{"path":"x.txt"}`)); err == nil {
		t.Fatal("write with no content must error")
	}
	if _, err := ft.List().Execute(ctx, json.RawMessage(`{"dir":""}`)); err == nil {
		t.Fatal("list with an empty dir must error")
	}
	if _, err := ft.List().Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("list with no dir must error")
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"","old_string":"a","new_string":"b"}`)); err == nil {
		t.Fatal("edit with an empty path must error")
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"x.txt","old_string":"","new_string":"b"}`)); err == nil {
		t.Fatal("edit with an empty old_string must error")
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"x.txt","old_string":"a"}`)); err == nil {
		t.Fatal("edit with no new_string must error")
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed call, got %v", eventTypes(*recs))
	}
}

func TestStrReplaceEditorMatchesDSHCommandsAndObservation(t *testing.T) {
	svc, ft, _ := newToolsWithEvents(t)
	ctx := context.Background()
	path := filepath.Join(svc.Root(), "editor.txt")
	if err := svc.Write(ctx, path, "alpha\nbeta\ngamma"); err != nil {
		t.Fatalf("seed editor file: %v", err)
	}

	jsonPath := filepath.ToSlash(path)
	out, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"view","path":"`+jsonPath+`"}`))
	if err != nil {
		t.Fatalf("editor view: %v", err)
	}
	if !strings.Contains(out, "total of 3 lines") || !strings.Contains(out, "     1  alpha") {
		t.Fatalf("editor view = %q, want DSH header and padded line numbers", out)
	}

	// DSH observes the current version as part of an edit; an explicit view is
	// not required before the first replacement.
	path2 := filepath.Join(svc.Root(), "first-edit.txt")
	if err := svc.Write(ctx, path2, "before"); err != nil {
		t.Fatal(err)
	}
	jsonPath2 := filepath.ToSlash(path2)
	if _, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"str_replace","path":"`+jsonPath2+`","old_str":"before","new_str":"after"}`)); err != nil {
		t.Fatalf("first editor replacement: %v", err)
	}
	if err := svc.Write(ctx, path, "changed externally"); err != nil {
		t.Fatal(err)
	}
	if _, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"str_replace","path":"`+jsonPath+`","old_str":"alpha","new_str":"x"}`)); err == nil {
		t.Fatal("editor must reject a stale observed version")
	}

	if _, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"view","path":"editor.txt"}`)); err == nil {
		t.Fatal("editor must require an absolute path")
	}
	if _, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"view","path":"`+jsonPath+`","view_range":[99,100]}`)); err == nil {
		t.Fatal("editor must reject a view range outside the file")
	}
}

func TestStrReplaceEditorDirectoryView(t *testing.T) {
	svc, ft, _ := newToolsWithEvents(t)
	ctx := context.Background()
	root := svc.Root()
	if err := svc.Write(ctx, filepath.Join(root, "visible.txt"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Write(ctx, filepath.Join(root, "nested", "child.txt"), "x"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Write(ctx, filepath.Join(root, ".hidden.txt"), "x"); err != nil {
		t.Fatal(err)
	}
	out, err := ft.StrReplaceEditor().Execute(ctx, json.RawMessage(`{"command":"view","path":"`+filepath.ToSlash(root)+`"}`))
	if err != nil {
		t.Fatalf("directory view: %v", err)
	}
	if !strings.Contains(out, "visible.txt") || !strings.Contains(out, "child.txt") || strings.Contains(out, ".hidden.txt") {
		t.Fatalf("directory view = %q, want visible two-level listing without hidden files", out)
	}
}

// TestFsToolsReturnErrorNotPanicOnBoundaryAndMissing verifies failures are
// error messages to the model, never panics (dispatch-m6f-3 §4): a path
// escaping the root and a missing file both error and emit no event.
func TestFsToolsReturnErrorNotPanicOnBoundaryAndMissing(t *testing.T) {
	_, ft, recs := newToolsWithEvents(t)
	ctx := context.Background()
	if _, err := ft.Write().Execute(ctx, json.RawMessage(`{"path":"../../x","content":"x"}`)); err == nil {
		t.Fatal("write of an escaping path must error")
	} else if strings.Contains(err.Error(), "panic") {
		t.Fatalf("write error = %v, must not be a panic", err)
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"../../x","old_string":"a","new_string":"b"}`)); err == nil {
		t.Fatal("edit of an escaping path must error")
	}
	if _, err := ft.List().Execute(ctx, json.RawMessage(`{"dir":"../.."}`)); err == nil {
		t.Fatal("list of an escaping dir must error")
	}
	if _, err := ft.List().Execute(ctx, json.RawMessage(`{"dir":"missing"}`)); err == nil {
		t.Fatal("list of a missing dir must error")
	}
	if _, err := ft.Edit().Execute(ctx, json.RawMessage(`{"path":"nope.txt","old_string":"a","new_string":"b"}`)); err == nil {
		t.Fatal("edit of a missing file must error")
	}
	if len(*recs) != 0 {
		t.Fatalf("no event may be emitted on a failed call, got %v", eventTypes(*recs))
	}
}
