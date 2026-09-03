package terminal

import (
	"strings"
	"sync"
	"unicode/utf8"
)

// BoundedTextBuffer 有界文本缓冲（M9 ADR D-M9-3，dsh BoundedTextBuffer 移植）：
// 字节上限 + 可选行数上限，UTF-8 安全截断（保尾丢最早），dropped 标记。
//
// 语义：
//   - Append 追加到尾部，先按行数上限裁剪（保尾），再按字节上限裁剪（保尾，
//     不劈开 UTF-8 多字节字符）。发生任何裁剪时置 truncated=true。
//   - truncated 一旦为 true 保持，直到 Consume 消费后重置为 false。
//   - Snapshot 返回当前全量文本（含此前的 dropped 标记）；
//     Consume 返回自上次 Consume 以来的 delta 并清空缓冲、重置 truncated。
//   - maxBytes<=0 视为无限；maxLines<=0 视为不限行。
type BoundedTextBuffer struct {
	mu        sync.Mutex
	buf       string
	maxBytes  int
	maxLines  int
	truncated bool
}

// NewBoundedTextBuffer 创建一个有界文本缓冲。maxLines<=0 不限行，maxBytes<=0 不限字节。
func NewBoundedTextBuffer(maxBytes int, maxLines int) *BoundedTextBuffer {
	return &BoundedTextBuffer{maxBytes: maxBytes, maxLines: maxLines}
}

// Append 将 text 追加到缓冲尾部，并按行数/字节上限做 UTF-8 安全的保尾裁剪。
func (b *BoundedTextBuffer) Append(text string) {
	if text == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf += text

	// 1) 先按行数上限裁剪：只保留最后 maxLines 行。
	if b.maxLines > 0 {
		trimmed := trimToMaxLines(b.buf, b.maxLines)
		if trimmed != b.buf {
			b.buf = trimmed
			b.truncated = true
		}
	}

	// 2) 再按字节上限裁剪：保尾，且截断点落在字符边界。
	if b.maxBytes > 0 && len(b.buf) > b.maxBytes {
		b.buf = trimToMaxBytes(b.buf, b.maxBytes)
		b.truncated = true
	}
}

// Snapshot 返回当前全量文本及是否发生过截断（dropped 标记）。
func (b *BoundedTextBuffer) Snapshot() (text string, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf, b.truncated
}

// Consume 返回自上次 Consume 以来的增量文本，并把缓冲清空、truncated 重置为 false。
func (b *BoundedTextBuffer) Consume() (text string, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	text, truncated = b.buf, b.truncated
	b.buf = ""
	b.truncated = false
	return text, truncated
}

// Empty 返回缓冲是否为空。
func (b *BoundedTextBuffer) Empty() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf == ""
}

// trimToMaxLines 只保留字符串的最后 maxLines 行。末尾换行视为最后一个完整行的
// 终止符（不单独计为一行）。maxLines<=0 或字符串为空时原样返回。
func trimToMaxLines(s string, maxLines int) string {
	if maxLines <= 0 || s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	trailingNL := lines[len(lines)-1] == ""
	if trailingNL {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= maxLines {
		return s
	}
	kept := strings.Join(lines[len(lines)-maxLines:], "\n")
	if trailingNL {
		kept += "\n"
	}
	return kept
}

// trimToMaxBytes 保尾地截断字符串，使其字节数不超过 maxBytes，且绝不劈开
// UTF-8 多字节字符：从尾部向前逐个累加完整 rune，直到再加入一个 rune 会超出
// 上限为止。返回最长且不超限的尾部后缀。
func trimToMaxBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := len(s)
	size := 0
	for end > 0 {
		_, sz := utf8.DecodeLastRuneInString(s[:end])
		if size+sz > maxBytes {
			break
		}
		size += sz
		end -= sz
	}
	return s[end:]
}
