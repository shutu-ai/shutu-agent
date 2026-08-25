// policy.go — the pure, network-free half of the local HTTP(S) fetch provider
// (dispatch-m7-2 §2, mirroring dsh web-fetch-http/src/policy.ts): content-type
// classification, charset parsing, UTF-8-tolerant decoding, and same-origin
// checks. httpfetch.go composes these with the transport (redirect following,
// byte caps). Each function is standalone-testable.
package web

import (
	"mime"
	"net/url"
	"strings"
)

// classifyContentType 按 Content-Type 返回 "html" / "text" / "unsupported"。
// html: text/html, application/xhtml+xml；
// text: 其他 text/*、application/json、application/xml、application/javascript、
//
//	+json/+xml 结构化类型等；
//
// unsupported: 其余（二进制 image/*、application/pdf、application/octet-stream 等）。
func classifyContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		// 非法 media type：取原始串按前缀/等值兜底（尽力分类，不失败）。
		mt = ct
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	switch {
	case mt == "text/html" || mt == "application/xhtml+xml":
		return "html"
	case strings.HasPrefix(mt, "text/"),
		mt == "application/json",
		mt == "application/xml",
		mt == "application/javascript",
		strings.HasSuffix(mt, "+json"),
		strings.HasSuffix(mt, "+xml"):
		return "text"
	default:
		return "unsupported"
	}
}

// parseCharset 提取 Content-Type 的 charset 参数（小写化、去引号/空白），
// 无声明返回 "utf-8"（provider 的默认解码假设）。
func parseCharset(ct string) string {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "utf-8"
	}
	cs := strings.ToLower(strings.TrimSpace(params["charset"]))
	if cs == "" {
		return "utf-8"
	}
	return cs
}

// decodeBody 按 charset 解码字节→字符串：仅支持 utf-8 / us-ascii（原样按 UTF-8
// 容错读）；其他声明编码不转码（按原始字节转 string，乱码风险记为已知裁剪，零依赖
// 取舍）。返回解码字符串（可能已含替换符，不失败）。
func decodeBody(b []byte, charset string) string {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		// string(b) 在 Go 中不校验 UTF-8：非法字节序列原样保留（呈现为 U+FFFD），
		// 正是"UTF-8 容错读"。
		return string(b)
	default:
		// 其他声明编码（latin-1、utf-16、gbk 等）不转码：按原始字节转 string。
		return string(b)
	}
}

// isSameOrigin 判断两 URL scheme+host+port 相同（镜像 dsh isSameOrigin）。
// scheme/host 小写比较；缺省端口按 scheme 归一化（http=80、https=443），
// 使 https://a 与 https://a:443 判为同源（跨源重定向因此被正确阻断/放行）。
func isSameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil {
		return false
	}
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	return strings.EqualFold(a.Hostname(), b.Hostname()) && effectivePort(a) == effectivePort(b)
}

// effectivePort 返回 URL 的有效端口：显式端口原样返回；缺省时按 scheme 返回
// 默认端口（http=80、https=443），其他 scheme 返回空串。
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}
