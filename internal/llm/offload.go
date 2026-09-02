// Request-level image offload (M8-3b, dispatch-m8-3b §2 / ADR
// 2026-08-20-m8-message-model.md 决策 M8-3). Images are only ever referenced by
// ImageRef in the log and memory (dsh 7078918 范式); the byte budget is
// enforced on a whole request at serialize time: when the cumulative image
// bytes exceed the budget, the images that pushed it over (oldest first) are
// replaced in place by an OffloadedImageText placeholder so the model still
// sees the conversation shape. The providers call OffloadRequestImages in
// Stream — after the HasImage fail-closed check, before serialization.
package llm

// OffloadedImageText is the text block that replaces an image whose bytes push
// the request image budget over its limit (dispatch-m8-3b §2, dsh
// OFFLOADED_IMAGE_TEXT 同款). It becomes a plain text block so the model sees
// a readable placeholder instead of a dropped image.
const OffloadedImageText = "[image omitted to keep the request within its image limit; older images are omitted first. If this image is still needed, read its file again when a path is available; otherwise ask the user to attach it again.]"

// OffloadRequestImages enforces the request image-byte budget (maxBytes; the
// providers' New applies the 20MiB default, dispatch-m8-3b §4) on a chat
// request's messages. The image bytes accumulate in message-history order; an
// image whose addition exceeds the budget is replaced in place by a text block
// carrying OffloadedImageText (oldest first, its position among the message's
// content blocks preserved — like truncateInjectorContext). It recurses into
// nested tool-result blocks, so a nested in-budget image is never dropped while
// an over-budget one is offloaded (ADR M8-3: tool-result 嵌套图片一并 offload).
// When the budget is not exceeded the messages are returned untouched (no copy,
// no side effect). maxBytes <= 0 means no budget (nothing is offloaded).
func OffloadRequestImages(msgs []Message, maxBytes int) []Message {
	if maxBytes <= 0 {
		return msgs
	}
	lengths := make([]int64, 0)
	for _, message := range msgs {
		collectImageBase64Lengths(message.Content, &lengths)
	}
	var total int64
	for _, length := range lengths {
		if length > maxInt64-total {
			total = maxInt64
			break
		}
		total += length
	}
	remove := 0
	for remove < len(lengths) && total > int64(maxBytes) {
		total -= lengths[remove]
		remove++
	}
	if remove == 0 {
		return msgs
	}
	remaining := remove
	out := make([]Message, len(msgs))
	for i, message := range msgs {
		out[i] = message
		content, changed := replaceOldestImages(message.Content, &remaining)
		if changed {
			out[i].Content = content
		}
	}
	return out
}

// offloadBlocks walks one message's content block list, accumulating every
// image block's Bytes into acc (shared across the whole request) and replacing
// any image whose addition pushes the total over maxBytes with an
// OffloadedImageText text block. Nested blocks (tool results) are recursed
// into. The replacement preserves the block's position in the list.
const maxInt64 = int64(^uint64(0) >> 1)

func imageBase64Length(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	if bytes > maxInt64-2 {
		return maxInt64
	}
	groups := (bytes + 2) / 3
	if groups > maxInt64/4 {
		return maxInt64
	}
	return groups * 4
}

func collectImageBase64Lengths(blocks []ContentBlock, lengths *[]int64) {
	for _, block := range blocks {
		if block.Kind == BlockImage {
			*lengths = append(*lengths, imageBase64Length(block.Image.Bytes))
			continue
		}
		if len(block.Blocks) > 0 {
			collectImageBase64Lengths(block.Blocks, lengths)
		}
	}
}

func replaceOldestImages(blocks []ContentBlock, remaining *int) ([]ContentBlock, bool) {
	var out []ContentBlock
	for i, block := range blocks {
		if block.Kind == BlockImage && *remaining > 0 {
			*remaining = *remaining - 1
			if out == nil {
				out = append([]ContentBlock(nil), blocks[:i]...)
			}
			out = append(out, Text(OffloadedImageText))
			continue
		}
		updated := block
		changed := false
		if len(block.Blocks) > 0 {
			var nested []ContentBlock
			nested, changed = replaceOldestImages(block.Blocks, remaining)
			if changed {
				updated.Blocks = nested
			}
		}
		if changed && out == nil {
			out = append([]ContentBlock(nil), blocks[:i]...)
		}
		if out != nil {
			out = append(out, updated)
		}
	}
	if out == nil {
		return blocks, false
	}
	return out, true
}
