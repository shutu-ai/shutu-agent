// Package attachment 提供图片附件存储（M8-3，ADR 2026-08-20-m8-message-model.md）。
// 图片文件持久在 <data_dir>/attachments/，会话日志只存 ImageRef 引用（dsh 7078918
// 范式：落库只存引用，请求时才转 data URL）。零新依赖。
//
// 依赖方向是单向的：attachment 依赖 internal/llm（ImageRef 类型），而 llm 不依赖
// attachment（provider 只拿 ImageRef.Path 自行读文件，保持 llm 纯接缝——见 M8-3b）。
// 宽高不解析记 0（M8 裁剪：解码不做，ImageRef.Width/Height 仅作元数据）。
package attachment

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/shutu-ai/shutu-agent/internal/llm"
)

// SupportedMediaTypes 是受支持的图片媒体类型（扩展名 → media type，dsh 同款）。
var SupportedMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

const (
	defaultMaxImagesPerMessage  = 20
	defaultMaxMessageImageBytes = 100 * 1024 * 1024
	defaultMaxImagePixels       = 40_000_000
	defaultMaxImageDimension    = 2000
)

// Limits is the deployment-resolved image admission policy. Keeping these
// limits on the store makes every producer share one fail-closed boundary.
type Limits struct {
	MaxImagesPerMessage  int
	MaxMessageImageBytes int
	MaxImagePixels       int
	MaxImageDimension    int
}

var defaultLimits = Limits{
	MaxImagesPerMessage:  defaultMaxImagesPerMessage,
	MaxMessageImageBytes: defaultMaxMessageImageBytes,
	MaxImagePixels:       defaultMaxImagePixels,
	MaxImageDimension:    defaultMaxImageDimension,
}

// DefaultLimits returns the built-in decoded-image and batch policy.
func DefaultLimits() Limits { return defaultLimits }

// NormalizeLimits fills unset policy fields with the built-in defaults. It is
// shared by non-store producers such as the workspace read_image tool.
func NormalizeLimits(limits Limits) Limits {
	if limits.MaxImagesPerMessage <= 0 {
		limits.MaxImagesPerMessage = defaultLimits.MaxImagesPerMessage
	}
	if limits.MaxMessageImageBytes <= 0 {
		limits.MaxMessageImageBytes = defaultLimits.MaxMessageImageBytes
	}
	if limits.MaxImagePixels <= 0 {
		limits.MaxImagePixels = defaultLimits.MaxImagePixels
	}
	if limits.MaxImageDimension <= 0 {
		limits.MaxImageDimension = defaultLimits.MaxImageDimension
	}
	return limits
}

// Fail-closed sentinel errors from SaveImage (dispatch-m8-3 §2: 校验 mediaType
// 受支持、data 非空且 ≤ maxBytes，超限返回错误 fail-closed)。
var (
	// ErrUnsupportedType 是 mediaType 不在 SupportedMediaTypes 时的错误。
	ErrUnsupportedType = errors.New("attachment: unsupported media type")
	// ErrEmptyData 是 data 为空（len 0）时的错误。
	ErrEmptyData = errors.New("attachment: empty image data")
	// ErrTooLarge 是 data 超过 maxBytes 时的错误。
	ErrTooLarge          = errors.New("attachment: image exceeds max bytes")
	ErrInvalidImage      = errors.New("attachment: invalid image data")
	ErrTypeMismatch      = errors.New("attachment: declared image type does not match its data")
	ErrTooManyPixels     = errors.New("attachment: image exceeds pixel limit")
	ErrDimensionTooLarge = errors.New("attachment: image exceeds dimension limit")
	ErrTooManyImages     = errors.New("attachment: image count exceeds limit")
	ErrBatchTooLarge     = errors.New("attachment: image batch exceeds max bytes")
	// ErrNotFound 是 id 对应的附件文件不存在时的错误（P5 web 回显/发送用）。
	ErrNotFound = errors.New("attachment: image not found")
)

// Store 持久化图片附件到一个目录。不是并发安全的：主循环严格串行（D5）。
type Store struct {
	dir    string
	limits Limits
	mu     sync.Mutex
}

// ImageInput is one image admitted as part of a single ordered batch.  The
// batch API lets protocol adapters validate the complete request first and
// roll back newly-created objects if a later write fails, so a rejected
// multi-image prompt cannot leave reachable-looking orphan files behind.
type ImageInput struct {
	MediaType string
	Data      []byte
	Name      string
}

// NewStore 创建/打开附件目录（<dir> 不存在则 mkdir -p）。dir 空 → 默认
// <data_dir>/attachments。
func NewStore(dir string) (*Store, error) {
	return NewStoreWithLimits(dir, Limits{})
}

// NewStoreWithLimits creates a store with explicit batch and decoded-image
// limits. Non-positive values use the reference-compatible defaults.
func NewStoreWithLimits(dir string, limits Limits) (*Store, error) {
	if dir == "" {
		dir = filepath.Join("data", "attachments")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("attachment: create dir %s: %w", dir, err)
	}
	return &Store{dir: dir, limits: NormalizeLimits(limits)}, nil
}

// SaveImage 把图片字节写入附件存储：校验 mediaType 受支持、data 非空且 ≤ maxBytes，
// 生成随机 id（hex），写 <dir>/<id><ext>，返回 ImageRef（ID/MediaType/Bytes/Width/
// Height/Path；宽高不解析记 0，M8 裁剪）。超限返回错误（fail-closed）。附件字节只
// 落附件文件，绝不进入会话日志——日志只存返回的 ImageRef（dsh 7078918）。
func (s *Store) SaveImage(mediaType string, data []byte, maxBytes int) (llm.ImageRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, _, err := s.saveImage(mediaType, data, maxBytes)
	return ref, err
}

// saveImage returns whether this invocation created the backing file.  The
// distinction matters for batch rollback because content-addressed images can
// legitimately be reused by this batch or by an earlier request.
func (s *Store) saveImage(mediaType string, data []byte, maxBytes int) (llm.ImageRef, bool, error) {
	ext := extensionForMediaType(mediaType)
	if ext == "" {
		return llm.ImageRef{}, false, ErrUnsupportedType
	}
	if len(data) == 0 {
		return llm.ImageRef{}, false, ErrEmptyData
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return llm.ImageRef{}, false, ErrTooLarge
	}
	width, height, err := validateImage(mediaType, data, s.limits)
	if err != nil {
		return llm.ImageRef{}, false, err
	}
	id := imageID(data)
	path := filepath.Join(s.dir, id+ext)
	created := false
	if existing, statErr := os.Stat(path); statErr == nil {
		if existing.Size() != int64(len(data)) {
			return llm.ImageRef{}, false, fmt.Errorf("attachment: content-address collision for %s", id)
		}
		existingData, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existingData, data) {
			return llm.ImageRef{}, false, fmt.Errorf("attachment: content-address verification failed for %s", id)
		}
	} else if errors.Is(statErr, os.ErrNotExist) {
		var publishErr error
		created, publishErr = publishAtomically(s.dir, path, data)
		if publishErr != nil {
			return llm.ImageRef{}, false, publishErr
		}
	} else {
		return llm.ImageRef{}, false, fmt.Errorf("attachment: inspect %s: %w", path, statErr)
	}
	return llm.ImageRef{
		ID:        id,
		MediaType: mediaType,
		Bytes:     int64(len(data)),
		Width:     width,
		Height:    height,
		Path:      path,
	}, created, nil
}

// publishAtomically writes and syncs a private temporary file, then links it
// into place without replacing a concurrent writer's object. A content
// addressed object is reported as created only when this call published it;
// callers can therefore roll back safely without deleting another request's
// object.
func publishAtomically(dir, path string, data []byte) (bool, error) {
	temporary, err := os.CreateTemp(dir, ".attachment-*")
	if err != nil {
		return false, fmt.Errorf("attachment: create temporary object: %w", err)
	}
	temporaryName := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}
	if _, err := temporary.Write(data); err != nil {
		cleanup()
		return false, fmt.Errorf("attachment: write temporary object: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return false, fmt.Errorf("attachment: sync temporary object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return false, fmt.Errorf("attachment: close temporary object: %w", err)
	}
	if err := os.Link(temporaryName, path); err == nil {
		if err := syncAttachmentDirectory(dir); err != nil {
			_ = os.Remove(path)
			_ = os.Remove(temporaryName)
			return false, fmt.Errorf("attachment: sync published object: %w", err)
		}
		_ = os.Remove(temporaryName)
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		_ = os.Remove(temporaryName)
		return false, fmt.Errorf("attachment: publish %s: %w", path, err)
	}
	_ = os.Remove(temporaryName)
	existing, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(existing, data) {
		if readErr != nil {
			return false, fmt.Errorf("attachment: inspect published object: %w", readErr)
		}
		return false, fmt.Errorf("attachment: content-address verification failed for %s", filepath.Base(path))
	}
	return false, nil
}

func syncAttachmentDirectory(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

// SaveImages validates and persists an image batch as one admission unit.
// Validation is completed before the first write. If any write fails, every
// file created by this call is removed; pre-existing attachments are never
// touched. The returned error is the original admission/write error.
func (s *Store) SaveImages(inputs []ImageInput, maxBytes int) ([]llm.ImageRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(inputs) == 0 {
		return nil, nil
	}
	if len(inputs) > s.limits.MaxImagesPerMessage {
		return nil, ErrTooManyImages
	}
	totalBytes := 0
	for _, input := range inputs {
		if extensionForMediaType(input.MediaType) == "" {
			return nil, ErrUnsupportedType
		}
		if len(input.Data) == 0 {
			return nil, ErrEmptyData
		}
		if maxBytes > 0 && len(input.Data) > maxBytes {
			return nil, ErrTooLarge
		}
		totalBytes += len(input.Data)
		if totalBytes > s.limits.MaxMessageImageBytes {
			return nil, ErrBatchTooLarge
		}
		if _, _, err := validateImage(input.MediaType, input.Data, s.limits); err != nil {
			return nil, err
		}
	}
	refs := make([]llm.ImageRef, 0, len(inputs))
	createdPaths := make([]string, 0, len(inputs))
	for _, input := range inputs {
		ref, created, err := s.saveImage(input.MediaType, input.Data, maxBytes)
		if err != nil {
			for _, path := range createdPaths {
				_ = os.Remove(path)
			}
			return nil, err
		}
		if created {
			createdPaths = append(createdPaths, ref.Path)
		}
		if input.Name != "" {
			ref.Name = input.Name
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// validateImage checks the declared media type against a real raster header,
// and applies decompression-bomb boundaries before any model-visible use.
// The standard library has no WebP decoder, so WebP admission verifies its
// RIFF/WEBP container signature while the other supported formats use the
// standard image header parser.
func validateImage(mediaType string, data []byte, limits Limits) (int, int, error) {
	width, height, err := ProbeImage(data, mediaType)
	if err != nil {
		return 0, 0, err
	}
	return checkImageBounds(width, height, limits)
}

// ProbeImage verifies the image container and returns its declared raster
// dimensions without applying deployment limits. Callers that admit model
// input should use ValidateImage instead.
func ProbeImage(data []byte, mediaType string) (int, int, error) {
	if mediaType == "image/webp" {
		return webPDimensions(data)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrInvalidImage, err)
	}
	expected := map[string]string{"image/png": "png", "image/jpeg": "jpeg", "image/gif": "gif"}[mediaType]
	if expected == "" || config.Width <= 0 || config.Height <= 0 {
		return 0, 0, ErrInvalidImage
	}
	if format != expected {
		return 0, 0, ErrTypeMismatch
	}
	return config.Width, config.Height, nil
}

// ValidateImage verifies a supported image and applies decompression-bomb
// bounds before the bytes become model-visible.
func ValidateImage(data []byte, mediaType string, limits Limits) (int, int, error) {
	if len(data) == 0 {
		return 0, 0, ErrEmptyData
	}
	width, height, err := ProbeImage(data, mediaType)
	if err != nil {
		return 0, 0, err
	}
	return checkImageBounds(width, height, NormalizeLimits(limits))
}

func checkImageBounds(width, height int, limits Limits) (int, int, error) {
	if limits.MaxImageDimension <= 0 {
		limits.MaxImageDimension = defaultLimits.MaxImageDimension
	}
	if limits.MaxImagePixels <= 0 {
		limits.MaxImagePixels = defaultLimits.MaxImagePixels
	}
	if width > limits.MaxImageDimension || height > limits.MaxImageDimension {
		return 0, 0, ErrDimensionTooLarge
	}
	if width > limits.MaxImagePixels/height {
		return 0, 0, ErrTooManyPixels
	}
	return width, height, nil
}

// webPDimensions validates the RIFF framing and extracts dimensions from one
// of the standardized VP8/VP8L/VP8X chunks. Go's standard image package does
// not decode WebP, but accepting an arbitrary RIFF/WEBP prefix would make the
// attachment boundary trivially bypassable.
func webPDimensions(data []byte) (int, int, error) {
	if len(data) < 20 || !bytes.Equal(data[:4], []byte("RIFF")) || !bytes.Equal(data[8:12], []byte("WEBP")) {
		return 0, 0, ErrInvalidImage
	}
	riffSize := int(binary.LittleEndian.Uint32(data[4:8]))
	if riffSize < 4 || riffSize > len(data)-8 {
		return 0, 0, ErrInvalidImage
	}
	end := 8 + riffSize
	for offset := 12; offset+8 <= end; {
		kind := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		if size < 0 || offset+8+size > end {
			return 0, 0, ErrInvalidImage
		}
		payload := data[offset+8 : offset+8+size]
		switch kind {
		case "VP8X":
			if len(payload) < 10 {
				return 0, 0, ErrInvalidImage
			}
			width := 1 + (int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16)
			height := 1 + (int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16)
			return width, height, nil
		case "VP8 ":
			if len(payload) < 10 || payload[3] != 0x9d || payload[4] != 0x01 || payload[5] != 0x2a {
				return 0, 0, ErrInvalidImage
			}
			return int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3fff), int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3fff), nil
		case "VP8L":
			if len(payload) < 5 || payload[0] != 0x2f {
				return 0, 0, ErrInvalidImage
			}
			width := 1 + (int(payload[1]) | int(payload[2]&0x3f)<<8)
			height := 1 + (int(payload[2]>>6) | int(payload[3])<<2 | int(payload[4]&0x0f)<<10)
			return width, height, nil
		}
		offset += 8 + size
		if size&1 != 0 {
			offset++
		}
	}
	return 0, 0, ErrInvalidImage
}

func imageID(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// Read 按 ImageRef.Path 读回原始字节。Path 缺失/不可读返回错误。M8-3b 的 provider
// 序列化用它把图片字节转 data URL（请求时才读文件，内存与日志都不常驻字节）。
func (s *Store) Read(ref llm.ImageRef) ([]byte, error) {
	root, rootErr := filepath.Abs(s.dir)
	path, pathErr := filepath.Abs(ref.Path)
	if rootErr != nil || pathErr != nil {
		return nil, errors.New("attachment: invalid storage path")
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Dir(rel) != "." {
		return nil, errors.New("attachment: image path escapes storage root")
	}
	data, err := os.ReadFile(ref.Path)
	if err != nil {
		return nil, fmt.Errorf("attachment: read %s: %w", ref.Path, err)
	}
	if len(ref.ID) == 64 && imageID(data) != ref.ID {
		return nil, fmt.Errorf("attachment: digest mismatch for %s", ref.ID)
	}
	if _, _, err := validateImage(ref.MediaType, data, s.limits); err != nil {
		return nil, err
	}
	return data, nil
}

// GetByID 按附件 id 找回 ImageRef（扫描 <dir>/<id>.<ext>；宽高不解析记 0）。
// P5 web 回显（GET .../attachments/{id}）与发送带图（id → ImageRef）用它把
// 前端持有的 id 解析回持久引用。id 不存在返回 ErrNotFound。
func (s *Store) GetByID(id string) (llm.ImageRef, error) {
	if id == "" || strings.ContainsAny(id, `/\`) {
		return llm.ImageRef{}, ErrNotFound
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return llm.ImageRef{}, fmt.Errorf("attachment: list %s: %w", s.dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		dot := strings.LastIndexByte(name, '.')
		if dot <= 0 || name[:dot] != id {
			continue
		}
		mediaType := SupportedMediaTypes[name[dot:]]
		if mediaType == "" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(s.dir, name))
		if readErr != nil || (len(id) == 64 && imageID(data) != id) {
			continue
		}
		width, height, validateErr := validateImage(mediaType, data, s.limits)
		if validateErr != nil {
			continue
		}
		return llm.ImageRef{
			ID:        id,
			MediaType: mediaType,
			Bytes:     info.Size(),
			Width:     width,
			Height:    height,
			Path:      filepath.Join(s.dir, name),
		}, nil
	}
	return llm.ImageRef{}, fmt.Errorf("%w: %q", ErrNotFound, id)
}

// MediaTypeForExtension 按（点前缀）扩展名返回受支持的 media type；不受支持返回空
// 串。/attach 用它校验文件扩展名（dispatch-m8-3 §4 步骤 3）。
func MediaTypeForExtension(ext string) string {
	return SupportedMediaTypes[strings.ToLower(ext)]
}

// extensionForMediaType 返回受支持 media type 对应的文件扩展名（SupportedMediaTypes
// 的逆映射）。写成确定性 switch：.jpg/.jpeg 都映射 image/jpeg，固定落 .jpg。
func extensionForMediaType(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ""
}

// randomID 返回一个随机 hex id（16 字节 → 32 个 hex 字符）。
