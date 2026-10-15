package middleware

import (
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/httpwire"
)

type flushWriteCloser interface {
	io.WriteCloser
	Flush() error
}

// Long-lived SSE streams hold one encoder each, so keep per-encoder memory small.
// The default zstd window (8 MiB) costs about 17 MiB per stream and the default
// brotli window (4 MiB) several MiB; 1 MiB and 256 KiB windows lose little ratio
// on JSON and SSE.
var brotliResponseWriterOptions = brotli.WriterOptions{Quality: 5, LGWin: 18}

func zstdResponseEncoderOptions() []zstd.EOption {
	return []zstd.EOption{
		zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(1 << 20),
		zstd.WithLowerEncoderMem(true),
	}
}

var (
	gzipResponseWriterPool = sync.Pool{
		New: func() any {
			w, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
			return w
		},
	}
	brotliResponseWriterPool = sync.Pool{
		New: func() any {
			return brotli.NewWriterOptions(io.Discard, brotliResponseWriterOptions)
		},
	}
	zstdResponseWriterPool = sync.Pool{
		New: func() any {
			w, _ := zstd.NewWriter(io.Discard, zstdResponseEncoderOptions()...)
			return w
		},
	}
	deflateResponseWriterPool = sync.Pool{
		New: func() any {
			w, _ := flate.NewWriter(io.Discard, flate.DefaultCompression)
			return w
		},
	}
)

// ResponseCompressionMiddleware negotiates downstream HTTP response compression
// (zstd, br, gzip, deflate) from Accept-Encoding and compresses both standard
// responses and streaming Server-Sent Events (text/event-stream) with immediate per-chunk flushing.
func ResponseCompressionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			c.Next()
			return
		}
		if c.Request.Method == http.MethodHead || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if isWebsocketUpgradeRequest(c.Request) {
			c.Next()
			return
		}

		encoding := negotiateResponseContentEncoding(c.Request.Header.Values("Accept-Encoding"))
		if encoding == "" {
			c.Next()
			return
		}

		cw := &compressResponseWriter{
			ResponseWriter: c.Writer,
			encoding:       encoding,
		}
		c.Writer = cw
		defer cw.Close()

		c.Next()
	}
}

func isWebsocketUpgradeRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
		return true
	}
	for _, v := range req.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") &&
				strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket") {
				return true
			}
		}
	}
	return false
}

// negotiateResponseContentEncoding parses Accept-Encoding headers and selects
// the best supported encoding. Tie-breaking order: zstd > br > gzip > deflate.
func negotiateResponseContentEncoding(headerValues []string) string {
	if len(headerValues) == 0 {
		return ""
	}
	joined := strings.TrimSpace(strings.Join(headerValues, ","))
	if joined == "" {
		return ""
	}

	supportedPriority := []string{"zstd", "br", "gzip", "deflate"}
	scores := make(map[string]float64, 5)
	explicit := make(map[string]bool, 5)
	wildcardQ := -1.0

	for _, part := range strings.Split(joined, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		q := 1.0
		if semi := strings.IndexByte(part, ';'); semi >= 0 {
			name = strings.TrimSpace(part[:semi])
			params := part[semi+1:]
			for _, param := range strings.Split(params, ";") {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(strings.ToLower(param), "q=") {
					parsedQ, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64)
					if err != nil || parsedQ < 0 || parsedQ > 1 {
						q = -1
					} else {
						q = parsedQ
					}
					break
				}
			}
		}
		if q < 0 {
			continue
		}
		name = strings.ToLower(name)
		if name == "x-gzip" {
			name = "gzip"
		}
		if name == "*" {
			if q > wildcardQ {
				wildcardQ = q
			}
			continue
		}
		explicit[name] = true
		if prev, ok := scores[name]; !ok || q > prev {
			scores[name] = q
		}
	}

	if wildcardQ >= 0 {
		for _, cand := range supportedPriority {
			if !explicit[cand] {
				scores[cand] = wildcardQ
			}
		}
		if !explicit["identity"] {
			scores["identity"] = wildcardQ
		}
	}

	bestEncoding := ""
	bestQ := 0.0
	for _, cand := range supportedPriority {
		q, ok := scores[cand]
		if !ok || q <= 0 {
			continue
		}
		if q > bestQ {
			bestQ = q
			bestEncoding = cand
		}
	}
	if bestEncoding == "" {
		return ""
	}
	if idQ, ok := scores["identity"]; ok && idQ > bestQ {
		return ""
	}
	return bestEncoding
}

type compressResponseWriter struct {
	gin.ResponseWriter
	encoding    string
	encoder     flushWriteCloser
	releaseFunc func()
	statusCode  int
	decided     bool
	passthrough bool
	closed      bool
}

// IsDownstreamCompressed reports to outer/inner logging wrappers that the
// Content-Encoding header on this writer was added by downstream response compression.
func (w *compressResponseWriter) IsDownstreamCompressed() bool {
	return w != nil && w.decided && !w.passthrough
}

func (w *compressResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	if isNonCompressibleStatus(code) && w.encoder == nil {
		if w.decided && !w.passthrough {
			w.Header().Del("Content-Encoding")
		}
		w.decided = true
		w.passthrough = true
	} else if !w.decided {
		w.prepareCompressionHeaders(code)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *compressResponseWriter) WriteHeaderNow() {
	if !w.decided {
		w.prepareCompressionHeaders(w.effectiveStatusCode())
	}
	w.ResponseWriter.WriteHeaderNow()
}

func (w *compressResponseWriter) effectiveStatusCode() int {
	if w.statusCode != 0 {
		return w.statusCode
	}
	if status := w.ResponseWriter.Status(); status != 0 {
		return status
	}
	return http.StatusOK
}

func isNonCompressibleStatus(code int) bool {
	return (code >= 100 && code < 200) || code == http.StatusNoContent || code == http.StatusNotModified ||
		code == http.StatusPartialContent
}

// isIncompressibleContentType reports media that is already compressed, where
// recompressing only costs CPU and memory.
func isIncompressibleContentType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	for _, prefix := range []string{"image/", "video/", "audio/", "application/zip", "application/gzip", "application/zstd", "application/octet-stream"} {
		if strings.HasPrefix(contentType, prefix) {
			return true
		}
	}
	return false
}

func (w *compressResponseWriter) prepareCompressionHeaders(code int) {
	if w.decided {
		return
	}
	w.decided = true
	if isNonCompressibleStatus(code) {
		w.passthrough = true
		return
	}
	existing := strings.TrimSpace(w.Header().Get("Content-Encoding"))
	if existing != "" && !strings.EqualFold(existing, "identity") {
		w.passthrough = true
		return
	}
	w.Header().Set("Content-Encoding", w.encoding)
	w.Header().Del("Content-Length")
	appendVaryAcceptEncoding(w.Header())
}

func appendVaryAcceptEncoding(h http.Header) {
	if h == nil {
		return
	}
	for _, v := range h.Values("Vary") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Accept-Encoding") || strings.TrimSpace(token) == "*" {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

func (w *compressResponseWriter) ensureEncoder() error {
	if !w.decided {
		w.prepareCompressionHeaders(w.effectiveStatusCode())
	}
	if w.passthrough || w.encoder != nil {
		return nil
	}
	// gin sets Content-Type after WriteHeader, so check it on the first write,
	// while headers can still change.
	if isIncompressibleContentType(w.Header().Get("Content-Type")) {
		w.passthrough = true
		w.Header().Del("Content-Encoding")
		return nil
	}
	// The compressed bytes are a different representation, so a strong validator
	// no longer matches them.
	if etag := w.Header().Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		w.Header().Set("ETag", "W/"+etag)
	}

	switch w.encoding {
	case "zstd":
		enc, ok := zstdResponseWriterPool.Get().(*zstd.Encoder)
		if !ok || enc == nil {
			var err error
			enc, err = zstd.NewWriter(w.ResponseWriter, zstdResponseEncoderOptions()...)
			if err != nil {
				w.passthrough = true
				w.Header().Del("Content-Encoding")
				return nil
			}
		} else {
			enc.Reset(w.ResponseWriter)
		}
		w.encoder = enc
		w.releaseFunc = func() {
			enc.Reset(io.Discard)
			zstdResponseWriterPool.Put(enc)
		}
	case "br":
		bw, ok := brotliResponseWriterPool.Get().(*brotli.Writer)
		if !ok || bw == nil {
			bw = brotli.NewWriterOptions(w.ResponseWriter, brotliResponseWriterOptions)
		} else {
			bw.Reset(w.ResponseWriter)
		}
		w.encoder = bw
		w.releaseFunc = func() {
			bw.Reset(io.Discard)
			brotliResponseWriterPool.Put(bw)
		}
	case "gzip":
		gw, ok := gzipResponseWriterPool.Get().(*gzip.Writer)
		if !ok || gw == nil {
			gw = gzip.NewWriter(w.ResponseWriter)
		} else {
			gw.Reset(w.ResponseWriter)
		}
		w.encoder = gw
		w.releaseFunc = func() {
			gw.Reset(io.Discard)
			gzipResponseWriterPool.Put(gw)
		}
	case "deflate":
		fw, ok := deflateResponseWriterPool.Get().(*flate.Writer)
		if !ok || fw == nil {
			var err error
			fw, err = flate.NewWriter(w.ResponseWriter, flate.DefaultCompression)
			if err != nil {
				w.passthrough = true
				w.Header().Del("Content-Encoding")
				return nil
			}
		} else {
			fw.Reset(w.ResponseWriter)
		}
		w.encoder = fw
		w.releaseFunc = func() {
			fw.Reset(io.Discard)
			deflateResponseWriterPool.Put(fw)
		}
	default:
		w.passthrough = true
		w.Header().Del("Content-Encoding")
	}
	return nil
}

func (w *compressResponseWriter) Write(data []byte) (int, error) {
	if err := w.ensureEncoder(); err != nil {
		return 0, err
	}
	if w.passthrough || w.encoder == nil {
		return w.ResponseWriter.Write(data)
	}
	if len(data) == 0 {
		return 0, nil
	}
	return w.encoder.Write(data)
}

func (w *compressResponseWriter) WriteString(s string) (int, error) {
	if err := w.ensureEncoder(); err != nil {
		return 0, err
	}
	if w.passthrough || w.encoder == nil {
		return w.ResponseWriter.WriteString(s)
	}
	if len(s) == 0 {
		return 0, nil
	}
	return io.WriteString(w.encoder, s)
}

func (w *compressResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *compressResponseWriter) FlushError() error {
	if err := w.ensureEncoder(); err != nil {
		return err
	}
	if !w.passthrough && w.encoder != nil {
		if errFlush := w.encoder.Flush(); errFlush != nil {
			return errFlush
		}
	}
	return httpwire.FlushResponse(w.ResponseWriter)
}

func (w *compressResponseWriter) Close() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	if w.encoder != nil {
		_ = w.encoder.Close()
		w.encoder = nil
		if w.releaseFunc != nil {
			w.releaseFunc()
			w.releaseFunc = nil
		}
		return
	}
	if w.decided && !w.passthrough {
		if w.ResponseWriter.Written() {
			if err := w.ensureEncoder(); err == nil && w.encoder != nil {
				_ = w.encoder.Close()
				w.encoder = nil
				if w.releaseFunc != nil {
					w.releaseFunc()
					w.releaseFunc = nil
				}
			}
		} else {
			w.Header().Del("Content-Encoding")
		}
	}
}
