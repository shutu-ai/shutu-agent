package llm

import "testing"

// imageBlock is a test shorthand for a BlockImage carrying only the byte size
// that the budget accounting uses (the Path is irrelevant to offload).
func imageBlock(bytes int64) ContentBlock {
	return ContentBlock{Kind: BlockImage, Image: ImageRef{Bytes: bytes}}
}

// TestOffloadRequestImagesUnderBudgetUnchanged verifies the no-copy, no-side-
// effect contract: when the cumulative image bytes never exceed the budget the
// input slice and its blocks are returned untouched (dispatch-m8-3b §2).
func TestOffloadRequestImagesUnderBudgetUnchanged(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{Text("hello"), imageBlock(5), imageBlock(3)}}}
	got := OffloadRequestImages(msgs, 20)
	if len(got) != 1 || len(got[0].Content) != 3 {
		t.Fatalf("messages changed: %+v", got)
	}
	for i, want := range []ContentBlockKind{BlockText, BlockImage, BlockImage} {
		if got[0].Content[i].Kind != want {
			t.Errorf("block %d kind = %s, want %s", i, got[0].Content[i].Kind, want)
		}
	}
	if &got[0] != &msgs[0] {
		t.Fatal("under-budget must return the input slice unchanged (no copy)")
	}
}

// TestOffloadRequestImagesExactBudgetKept verifies the boundary: an image whose
// addition makes the total exactly equal to the budget is kept (only a total
// strictly over the budget offloads).
func TestOffloadRequestImagesExactBudgetKept(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{imageBlock(6)}}}
	got := OffloadRequestImages(msgs, 8) // base64(6) = 8
	if got[0].Content[0].Kind != BlockImage {
		t.Fatal("an image exactly at the budget must be kept")
	}
}

// TestOffloadRequestImagesOverBudgetReplacesOldest verifies the oldest-first
// replacement: the image that pushes the total over the budget is replaced by
// the placeholder, while every earlier in-budget image stays.
func TestOffloadRequestImagesOverBudgetReplacesOldest(t *testing.T) {
	msgs := []Message{
		{Role: RoleUser, Content: []ContentBlock{Text("a"), imageBlock(3)}}, // base64 4
		{Role: RoleUser, Content: []ContentBlock{imageBlock(6)}},            // base64 8; total 12 > 8
	}
	got := OffloadRequestImages(msgs, 8)
	if got[0].Content[1].Kind != BlockText {
		t.Fatal("the oldest image must be replaced first")
	}
	b := got[1].Content[0]
	if b.Kind != BlockImage {
		t.Fatalf("newest image = %+v, want it retained", b)
	}
}

// TestOffloadRequestImagesMultipleInOneMessage verifies per-message ordering:
// in one message with several images, each is judged in turn — the first fits,
// the later ones are replaced (dispatch-m8-3b §2: 同一消息多图逐个判断).
func TestOffloadRequestImagesMultipleInOneMessage(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{imageBlock(3), imageBlock(3), imageBlock(3)}}}
	got := OffloadRequestImages(msgs, 8)
	c := got[0].Content
	wantKinds := []ContentBlockKind{BlockText, BlockImage, BlockImage}
	for i, want := range wantKinds {
		if c[i].Kind != want {
			t.Errorf("block %d kind = %s, want %s", i, c[i].Kind, want)
		}
	}
	if c[0].Text != OffloadedImageText {
		t.Fatalf("placeholder = %+v, want OffloadedImageText", c[0])
	}
}

// TestOffloadRequestImagesPlaceholderPosition verifies the replacement keeps
// its position among the message's content blocks: the surrounding text blocks
// are not displaced and the over-budget image is replaced in place.
func TestOffloadRequestImagesPlaceholderPosition(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{
		Text("t0"), imageBlock(3), Text("t1"), imageBlock(3), Text("t2"),
	}}}
	got := OffloadRequestImages(msgs, 6) // the oldest image (index 1) is removed
	c := got[0].Content
	if c[0].Text != "t0" || c[2].Text != "t1" || c[4].Text != "t2" {
		t.Fatalf("text blocks displaced: %+v", c)
	}
	if c[1].Kind != BlockText || c[1].Text != OffloadedImageText {
		t.Fatal("oldest image must be replaced in place")
	}
	if c[3].Kind != BlockImage {
		t.Fatalf("newest image must be retained: %+v", c[3])
	}
}

// TestOffloadRequestImagesNoBudget verifies maxBytes <= 0 means no budget: no
// image is offloaded regardless of size (dispatch-m8-3b §2).
func TestOffloadRequestImagesNoBudget(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{imageBlock(1 << 30)}}}
	for _, mb := range []int{0, -5} {
		got := OffloadRequestImages(msgs, mb)
		if got[0].Content[0].Kind != BlockImage {
			t.Fatalf("maxBytes=%d must not offload", mb)
		}
	}
}

// TestOffloadRequestImagesNestedToolResult verifies the recursion into nested
// tool-result blocks (ADR M8-3 验收 9: tool-result 嵌套图片 offload 正确): an
// in-budget nested image stays, an over-budget one is replaced in place inside
// the nested list.
func TestOffloadRequestImagesNestedToolResult(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{
		Text("a"),
		{Kind: BlockToolResult, Blocks: []ContentBlock{imageBlock(3), imageBlock(3)}},
	}}}
	got := OffloadRequestImages(msgs, 6)
	nested := got[0].Content[1].Blocks
	if nested[0].Kind != BlockText || nested[0].Text != OffloadedImageText {
		t.Fatal("the oldest nested image must be replaced")
	}
	if nested[1].Kind != BlockImage {
		t.Fatalf("the newest nested image must be retained: %+v", nested[1])
	}
}

func TestOffloadRequestImagesDoesNotMutateDurableMessages(t *testing.T) {
	msgs := []Message{{Role: RoleUser, Content: []ContentBlock{imageBlock(3), imageBlock(3)}}}
	got := OffloadRequestImages(msgs, 6)
	if got[0].Content[0].Kind != BlockText || msgs[0].Content[0].Kind != BlockImage {
		t.Fatalf("offload mutated input: got=%+v input=%+v", got, msgs)
	}
	if &got[0] == &msgs[0] || &got[0].Content[0] == &msgs[0].Content[0] {
		t.Fatal("over-budget offload must detach changed message/content")
	}
}
