package helps

import (
	"io"
	"net/http"
	"strings"
)

type compositeReadCloser struct {
	io.Reader
	closers []func() error
}

func (c *compositeReadCloser) Close() error {
	var firstErr error
	for i := range c.closers {
		if c.closers[i] == nil {
			continue
		}
		if errClose := c.closers[i](); errClose != nil && firstErr == nil {
			firstErr = errClose
		}
	}
	return firstErr
}

// DecodeResponseBody decodes declared encodings in reverse order, or sniffs gzip/zstd without a header.
// Closing the returned body also closes all decoders and the original body, including on setup errors.
// It shares DecodeResponseBodyWithStatus, which decodes zstd synchronously with a bounded window and
// sniffs magic bytes on the first Read instead of blocking until the upstream sends data.
func DecodeResponseBody(body io.ReadCloser, header http.Header) (io.ReadCloser, error) {
	decoded, _, errDecode := DecodeResponseBodyWithStatus(body, strings.Join(header.Values("Content-Encoding"), ","))
	return decoded, errDecode
}
