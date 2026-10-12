package helps

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// CodexUpstreamRequestCompressionMinBytes is the smallest request body worth compressing.
// Below this size the content-coding framing and CPU outweigh the saving.
const CodexUpstreamRequestCompressionMinBytes = 1024

// codexUpstreamRequestCompressionHost is the official ChatGPT Codex backend verified
// to accept compressed request bodies. Custom codex-api-key base URLs may reject
// request content codings, so they are never compressed automatically.
const codexUpstreamRequestCompressionHost = "chatgpt.com"

// Content codings accepted by codex.upstream-request-compression.
const (
	CodexUpstreamRequestEncodingGzip = "gzip"
	CodexUpstreamRequestEncodingBr   = "br"
	// CodexUpstreamRequestEncodingZstd is what the official Codex CLI sends to the ChatGPT
	// backend (enable_request_compression, zstd level 3), so it is the default and recommended value.
	CodexUpstreamRequestEncodingZstd = "zstd"
)

// codexZstdLevel mirrors the official Codex CLI, which encodes with zstd level 3.
const codexZstdLevel = 3

var (
	codexZstdEncoderOnce sync.Once
	codexZstdEncoder     *zstd.Encoder
	codexZstdEncoderErr  error
	codexGzipWriterPool  = sync.Pool{
		New: func() any {
			zw, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
			return zw
		},
	}
	codexBrotliWriterPool = sync.Pool{
		New: func() any {
			return brotli.NewWriterLevel(io.Discard, brotli.DefaultCompression)
		},
	}
	codexUnknownEncoding sync.Map
)

func codexSharedZstdEncoder() (*zstd.Encoder, error) {
	codexZstdEncoderOnce.Do(func() {
		codexZstdEncoder, codexZstdEncoderErr = zstd.NewWriter(nil,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(codexZstdLevel)),
			zstd.WithEncoderConcurrency(1),
		)
	})
	if codexZstdEncoderErr != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", codexZstdEncoderErr)
	}
	return codexZstdEncoder, nil
}

// NormalizeCodexUpstreamRequestCompression maps the configured value of
// codex.upstream-request-compression to a content coding, or "" when compression is
// explicitly disabled or the value is not recognised. Empty or "auto" defaults to "zstd".
func NormalizeCodexUpstreamRequestCompression(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto", "true", CodexUpstreamRequestEncodingZstd, "zstandard":
		return CodexUpstreamRequestEncodingZstd
	case CodexUpstreamRequestEncodingGzip, "x-gzip":
		return CodexUpstreamRequestEncodingGzip
	case CodexUpstreamRequestEncodingBr, "brotli":
		return CodexUpstreamRequestEncodingBr
	case "off", "none", "false", "identity", "disabled":
		return ""
	default:
		if _, seen := codexUnknownEncoding.LoadOrStore(value, struct{}{}); !seen {
			log.Warnf("codex.upstream-request-compression: unsupported value %q, sending identity bodies (use zstd, gzip, br, or off)", value)
		}
		return ""
	}
}

// CodexUpstreamRequestEncoding returns the content coding the upstream request body of
// req should be sent with, or "" to send it as-is. Only the Responses endpoints of the
// official ChatGPT Codex backend are compressed: they carry the full conversation on
// every turn and accept zstd (what the official Codex CLI sends), gzip, and br.
func CodexUpstreamRequestEncoding(cfg *config.Config, req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}
	rawCfg := ""
	if cfg != nil {
		rawCfg = cfg.Codex.UpstreamRequestCompression
	}
	encoding := NormalizeCodexUpstreamRequestCompression(rawCfg)
	if encoding == "" {
		return ""
	}
	if !strings.EqualFold(req.URL.Hostname(), codexUpstreamRequestCompressionHost) {
		return ""
	}
	path := strings.TrimSuffix(req.URL.Path, "/")
	if strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/responses/compact") {
		return encoding
	}
	return ""
}

func codexCompressBody(body []byte, encoding string) ([]byte, error) {
	switch encoding {
	case CodexUpstreamRequestEncodingZstd:
		enc, err := codexSharedZstdEncoder()
		if err != nil {
			return nil, err
		}
		return enc.EncodeAll(body, make([]byte, 0, len(body)/3)), nil
	case CodexUpstreamRequestEncodingGzip:
		var buf bytes.Buffer
		buf.Grow(len(body) / 3)
		zw, ok := codexGzipWriterPool.Get().(*gzip.Writer)
		if !ok || zw == nil {
			var err error
			zw, err = gzip.NewWriterLevel(&buf, gzip.DefaultCompression)
			if err != nil {
				return nil, fmt.Errorf("create gzip writer: %w", err)
			}
		} else {
			zw.Reset(&buf)
		}
		defer func() {
			zw.Reset(io.Discard)
			codexGzipWriterPool.Put(zw)
		}()
		if _, err := zw.Write(body); err != nil {
			return nil, fmt.Errorf("gzip request body: %w", err)
		}
		if err := zw.Close(); err != nil {
			return nil, fmt.Errorf("close gzip writer: %w", err)
		}
		return bytes.Clone(buf.Bytes()), nil
	case CodexUpstreamRequestEncodingBr:
		var buf bytes.Buffer
		buf.Grow(len(body) / 3)
		bw, ok := codexBrotliWriterPool.Get().(*brotli.Writer)
		if !ok || bw == nil {
			bw = brotli.NewWriterLevel(&buf, brotli.DefaultCompression)
		} else {
			bw.Reset(&buf)
		}
		defer func() {
			bw.Reset(io.Discard)
			codexBrotliWriterPool.Put(bw)
		}()
		if _, err := bw.Write(body); err != nil {
			return nil, fmt.Errorf("brotli request body: %w", err)
		}
		if err := bw.Close(); err != nil {
			return nil, fmt.Errorf("close brotli writer: %w", err)
		}
		return bytes.Clone(buf.Bytes()), nil
	default:
		return nil, fmt.Errorf("unsupported content coding %q", encoding)
	}
}

// ApplyCodexUpstreamRequestCompression replaces the request body with its encoding
// (zstd, gzip, or br) and updates Content-Encoding, ContentLength and GetBody to match.
func ApplyCodexUpstreamRequestCompression(req *http.Request, body []byte, encoding string) (bool, error) {
	if req == nil {
		return false, nil
	}
	req.Header.Del("Content-Encoding")
	if len(body) < CodexUpstreamRequestCompressionMinBytes {
		return false, nil
	}
	compressed, err := codexCompressBody(body, encoding)
	if err != nil {
		return false, err
	}
	if len(compressed) >= len(body) {
		return false, nil
	}
	if req.Body != nil {
		if errClose := req.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("codex upstream request compression: close original request body")
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.ContentLength = int64(len(compressed))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	req.Header.Set("Content-Encoding", encoding)
	return true, nil
}
