package handlers

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

// MaxRequestBodyBytesContextKey stores the configured request body size limit (in bytes) on the Gin context.
const MaxRequestBodyBytesContextKey = "maxRequestBodyBytes"

// DefaultMaxDecodedRequestBodyBytes bounds the decompressed request body when
// max-request-body-bytes is not configured, so a small compressed body cannot
// expand without limit.
const DefaultMaxDecodedRequestBodyBytes int64 = 256 << 20

// MaxZstdRequestWindowBytes caps the zstd window of request bodies. The default
// cap (512 MiB) lets a tiny frame header force a huge allocation; 8 MiB is the
// largest window used by regular (non-ultra) compression levels.
const MaxZstdRequestWindowBytes uint64 = 8 << 20

// EffectiveDecodedRequestBodyLimit returns the decoded-size limit for maxBytes.
func EffectiveDecodedRequestBodyLimit(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return DefaultMaxDecodedRequestBodyBytes
	}
	return maxBytes
}

// ReadRequestBody reads the incoming request body and decodes supported
// Content-Encoding values (gzip, br, zstd, deflate) before handlers inspect JSON fields.
func ReadRequestBody(c *gin.Context) ([]byte, error) {
	maxBytes := int64(0)
	if c != nil {
		maxBytes = c.GetInt64(MaxRequestBodyBytesContextKey)
	}
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}

	encoding := ""
	if c != nil && c.Request != nil {
		encoding = strings.TrimSpace(c.Request.Header.Get("Content-Encoding"))
	}
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		if magicEncoding := detectMagicRequestEncoding(raw); magicEncoding != "" {
			decoded, errMagic := decodeRequestBody(raw, magicEncoding, maxBytes)
			if errMagic != nil {
				var maxBytesErr *http.MaxBytesError
				if errors.As(errMagic, &maxBytesErr) {
					return nil, errMagic
				}
				return raw, nil
			}
			return decoded, nil
		}
		return raw, nil
	}

	decoded, err := decodeRequestBody(raw, encoding, maxBytes)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, err
		}
		if json.Valid(raw) {
			return raw, nil
		}
		return nil, err
	}
	return decoded, nil
}

func detectMagicRequestEncoding(raw []byte) string {
	switch {
	case len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b:
		return "gzip"
	case len(raw) >= 4 && raw[0] == 0x28 && raw[1] == 0xb5 && raw[2] == 0x2f && raw[3] == 0xfd:
		return "zstd"
	default:
		return ""
	}
}

func decodeRequestBody(raw []byte, encoding string, maxBytes int64) ([]byte, error) {
	maxBytes = EffectiveDecodedRequestBodyLimit(maxBytes)
	parts := strings.Split(encoding, ",")
	body := raw
	for i := len(parts) - 1; i >= 0; i-- {
		enc := strings.ToLower(strings.TrimSpace(parts[i]))
		switch enc {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			decoded, err := decodeGzipRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		case "br":
			decoded, err := decodeBrotliRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		case "zstd":
			decoded, err := decodeZstdRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		case "deflate":
			decoded, err := decodeDeflateRequestBody(body, maxBytes)
			if err != nil {
				return nil, err
			}
			body = decoded
		default:
			return nil, fmt.Errorf("unsupported request content encoding: %s", enc)
		}
	}
	return body, nil
}

func readBoundedDecodedRequestBody(reader io.Reader, maxBytes int64, codec string) ([]byte, error) {
	bounded := reader
	if maxBytes > 0 && maxBytes < math.MaxInt64 {
		bounded = io.LimitReader(reader, maxBytes+1)
	}
	decoded, err := io.ReadAll(bounded)
	if err != nil {
		return nil, fmt.Errorf("failed to decode %s request body: %w", codec, err)
	}
	if maxBytes > 0 && int64(len(decoded)) > maxBytes {
		return nil, &http.MaxBytesError{Limit: maxBytes}
	}
	return decoded, nil
}

func decodeGzipRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip request decoder: %w", err)
	}
	defer func() {
		_ = reader.Close()
	}()
	return readBoundedDecodedRequestBody(reader, maxBytes, "gzip")
}

func decodeBrotliRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(raw))
	return readBoundedDecodedRequestBody(reader, maxBytes, "br")
}

func decodeZstdRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	decoder, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(MaxZstdRequestWindowBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create zstd request decoder: %w", err)
	}
	defer decoder.Close()
	return readBoundedDecodedRequestBody(decoder, maxBytes, "zstd")
}

func decodeDeflateRequestBody(raw []byte, maxBytes int64) ([]byte, error) {
	buffered := bufio.NewReader(bytes.NewReader(raw))
	var reader io.ReadCloser
	if header, errPeek := buffered.Peek(2); errPeek == nil && isRequestZlibHeader(header) {
		zlibReader, errZlib := zlib.NewReader(buffered)
		if errZlib != nil {
			return nil, fmt.Errorf("failed to create deflate request decoder: %w", errZlib)
		}
		reader = zlibReader
	} else {
		reader = flate.NewReader(buffered)
	}
	defer func() {
		_ = reader.Close()
	}()
	return readBoundedDecodedRequestBody(reader, maxBytes, "deflate")
}

func isRequestZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}
