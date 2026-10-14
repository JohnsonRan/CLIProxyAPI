package helps

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// DefaultUpstreamAcceptEncoding is the standard set of content codings CPA can
// transparently decode from upstream HTTP responses.
const DefaultUpstreamAcceptEncoding = "gzip, deflate, br, zstd"

// EnsureDefaultAcceptEncoding sets Accept-Encoding to DefaultUpstreamAcceptEncoding
// when the outgoing request does not already specify an Accept-Encoding header.
func EnsureDefaultAcceptEncoding(req *http.Request) {
	if req == nil || req.Header == nil {
		return
	}
	if strings.TrimSpace(req.Header.Get("Accept-Encoding")) == "" {
		req.Header.Set("Accept-Encoding", DefaultUpstreamAcceptEncoding)
	}
}

type peekableResponseBody struct {
	*bufio.Reader
	closer io.Closer
}

func (p *peekableResponseBody) Close() error {
	if p == nil || p.closer == nil {
		return nil
	}
	return p.closer.Close()
}

type compositeResponseReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeResponseReadCloser) Close() error {
	if c == nil {
		return nil
	}
	var errs []error
	for _, fn := range c.closers {
		if fn == nil {
			continue
		}
		if err := fn(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DecodeHTTPResponseBody transparently wraps resp.Body to decompress gzip, deflate,
// br (Brotli), or zstd responses (including magic-byte gzip/zstd when Content-Encoding
// is omitted by a misbehaving upstream) and strips Content-Encoding / Content-Length
// from resp.Header once wrapped.
func DecodeHTTPResponseBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return nil
	}
	contentEncoding := ""
	if resp.Header != nil {
		contentEncoding = strings.Join(resp.Header.Values("Content-Encoding"), ",")
	}
	decodedBody, decoded, err := DecodeResponseBodyWithStatus(resp.Body, contentEncoding)
	if err != nil {
		return err
	}
	resp.Body = decodedBody
	if decoded && resp.Header != nil {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		resp.Uncompressed = true
	}
	return nil
}

// DecodeResponseBodyWithStatus wraps body according to contentEncoding (or magic bytes
// when contentEncoding is empty) and reports whether a decompressor was applied.
func DecodeResponseBodyWithStatus(body io.ReadCloser, contentEncoding string) (io.ReadCloser, bool, error) {
	if body == nil {
		return nil, false, fmt.Errorf("response body is nil")
	}
	contentEncoding = strings.TrimSpace(contentEncoding)
	if contentEncoding == "" {
		// Detect magic bytes on the first Read rather than here: a streaming upstream
		// may send headers long before its first byte, and blocking now would stall
		// the caller (and its bootstrap timers) until data arrives.
		return &magicSniffResponseBody{
			body: &peekableResponseBody{Reader: bufio.NewReader(body), closer: body},
		}, false, nil
	}

	encodings := strings.Split(contentEncoding, ",")
	reader := io.Reader(body)
	decoderClosers := make([]func() error, 0, len(encodings))
	applied := false
	cleanup := func() {
		for i := len(decoderClosers) - 1; i >= 0; i-- {
			_ = decoderClosers[i]()
		}
		_ = body.Close()
	}

	for index := len(encodings) - 1; index >= 0; index-- {
		encoding := strings.TrimSpace(strings.ToLower(encodings[index]))
		switch encoding {
		case "", "identity":
			continue
		case "gzip", "x-gzip":
			gzipReader, errGzip := gzip.NewReader(reader)
			if errGzip != nil {
				cleanup()
				return nil, false, fmt.Errorf("failed to create gzip reader: %w", errGzip)
			}
			reader = gzipReader
			decoderClosers = append(decoderClosers, gzipReader.Close)
			applied = true
		case "deflate":
			deflateReader, errDeflate := newUpstreamDeflateReader(reader)
			if errDeflate != nil {
				cleanup()
				return nil, false, errDeflate
			}
			reader = deflateReader
			decoderClosers = append(decoderClosers, deflateReader.Close)
			applied = true
		case "br":
			reader = brotli.NewReader(reader)
			applied = true
		case "zstd":
			decoder, errZstd := zstd.NewReader(reader)
			if errZstd != nil {
				cleanup()
				return nil, false, fmt.Errorf("failed to create zstd reader: %w", errZstd)
			}
			reader = decoder
			decoderClosers = append(decoderClosers, func() error {
				decoder.Close()
				return nil
			})
			applied = true
		default:
			cleanup()
			return nil, false, fmt.Errorf("unsupported content encoding %q", encoding)
		}
	}

	if !applied {
		return body, false, nil
	}
	closers := make([]func() error, 0, len(decoderClosers)+1)
	for index := len(decoderClosers) - 1; index >= 0; index-- {
		closers = append(closers, decoderClosers[index])
	}
	closers = append(closers, body.Close)
	return &compositeResponseReadCloser{Reader: reader, closers: closers}, true, nil
}

// magicSniffResponseBody decodes a body that arrived without Content-Encoding but
// starts with gzip or zstd magic bytes, as some misbehaving upstreams send.
// Detection happens on the first Read and only waits for more than the first
// buffered byte when that byte can start a gzip or zstd frame.
type magicSniffResponseBody struct {
	body    *peekableResponseBody
	once    sync.Once
	mu      sync.Mutex
	reader  io.Reader
	decoder func()
	err     error
}

func (m *magicSniffResponseBody) detect() {
	reader := io.Reader(m.body)
	var closeDecoder func()
	defer func() {
		m.mu.Lock()
		m.reader = reader
		m.decoder = closeDecoder
		m.mu.Unlock()
	}()
	first, _ := m.body.Peek(1)
	if len(first) == 0 || (first[0] != 0x1f && first[0] != 0x28) {
		return
	}
	head, _ := m.body.Peek(4)
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gzipReader, errGzip := gzip.NewReader(m.body)
		if errGzip != nil {
			m.err = fmt.Errorf("magic-byte gzip: failed to create reader: %w", errGzip)
			return
		}
		reader = gzipReader
		closeDecoder = func() { _ = gzipReader.Close() }
	case len(head) >= 4 && head[0] == 0x28 && head[1] == 0xb5 && head[2] == 0x2f && head[3] == 0xfd:
		decoder, errZstd := zstd.NewReader(m.body)
		if errZstd != nil {
			m.err = fmt.Errorf("magic-byte zstd: failed to create reader: %w", errZstd)
			return
		}
		reader = decoder
		closeDecoder = decoder.Close
	}
}

func (m *magicSniffResponseBody) Read(p []byte) (int, error) {
	m.once.Do(m.detect)
	if m.err != nil {
		return 0, m.err
	}
	return m.reader.Read(p)
}

func (m *magicSniffResponseBody) Close() error {
	m.mu.Lock()
	decoder := m.decoder
	m.mu.Unlock()
	if decoder != nil {
		decoder()
	}
	return m.body.Close()
}

func newUpstreamDeflateReader(reader io.Reader) (io.ReadCloser, error) {
	buffered := bufio.NewReader(reader)
	header, errPeek := buffered.Peek(2)
	if errPeek == nil && isUpstreamZlibHeader(header) {
		zlibReader, errZlib := zlib.NewReader(buffered)
		if errZlib != nil {
			return nil, fmt.Errorf("failed to create zlib deflate reader: %w", errZlib)
		}
		return zlibReader, nil
	}
	return flate.NewReader(buffered), nil
}

func isUpstreamZlibHeader(header []byte) bool {
	if len(header) < 2 {
		return false
	}
	cmf, flg := header[0], header[1]
	return cmf&0x0f == 8 && cmf>>4 <= 7 && (uint16(cmf)<<8|uint16(flg))%31 == 0
}
