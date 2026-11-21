package llm

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
)

// Every request carries the whole conversation, sent again on each tool
// round, so a long session uploads megabytes per turn. Compressing the body
// shortens that upload (Claude Code does the same); it changes nothing the
// model sees or what it costs. Thinking signatures and images are base64 and
// barely shrink, so a body comes out at about half its size, not a tenth.
const gzipMinBody = 32 << 10 // smaller bodies upload in no time anyway

var (
	gzipOn  atomic.Bool
	gzipOff sync.Map // host → true: refused a compressed body, sent plain since
	gzipers = sync.Pool{New: func() any {
		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return w
	}}
	// gzipHost reports whether a host is known to take compressed bodies.
	// Proxies and other providers may not, so only these hosts get them.
	gzipHost = func(host string) bool { return gzipHosts[host] }
)

// gzipHosts took a compressed body when tried (2026-10).
var gzipHosts = map[string]bool{
	"api.anthropic.com":                 true,
	"daily-cloudcode-pa.googleapis.com": true, // Antigravity
	"chatgpt.com":                       true, // Codex
}

func init() { gzipOn.Store(true) }

// SetGzipRequests turns request compression on or off ("gzip_requests").
func SetGzipRequests(on bool) { gzipOn.Store(on) }

// gzipFor is the compressed body to send to endpoint, or nil to send body
// as it is.
func gzipFor(endpoint string, body []byte) []byte {
	if !gzipOn.Load() || len(body) < gzipMinBody {
		return nil
	}
	host := hostOf(endpoint)
	if host == "" || !gzipHost(host) {
		return nil
	}
	if _, off := gzipOff.Load(host); off {
		return nil
	}
	var b bytes.Buffer
	b.Grow(len(body) / 2)
	w := gzipers.Get().(*gzip.Writer)
	defer gzipers.Put(w)
	w.Reset(&b)
	if _, err := w.Write(body); err != nil {
		return nil
	}
	if err := w.Close(); err != nil {
		return nil
	}
	return b.Bytes()
}

// gzipRefused reports whether an answer to a compressed body may mean the
// server could not read it: then the request is sent again plain. A 415
// may; a 400 only when it is about the encoding, or about a body it could
// not parse (what is sent is valid JSON, so one it can't parse is one it
// did not decompress). An ordinary bad request ("prompt is too long") is
// the request's own fault and is not sent twice.
func gzipRefused(ae *APIError) bool {
	switch ae.Status {
	case http.StatusUnsupportedMediaType:
		return true
	case http.StatusBadRequest:
		b := strings.ToLower(ae.Body)
		for _, k := range gzipRefusalHints {
			if strings.Contains(b, k) {
				return true
			}
		}
	}
	return false
}

// gzipRefusalHints are what 400s about an unreadable body say: the encoding
// itself, or the JSON parser's failure on compressed bytes (OpenAI: "could
// not parse the JSON body", Google: "Invalid JSON payload received", Go:
// "invalid character '\x1f' looking for beginning of value"). Not a bare
// "encoding": an image's "invalid base64 encoding" is the request's fault.
var gzipRefusalHints = []string{
	"gzip", "compress", "content-encoding", "content encoding",
	"invalid json", "not valid json", "malformed json", "json body", "json payload", "json decode", "decode json",
	"could not parse", "could not be parsed", "unable to parse", "failed to parse", "error parsing", "parse error",
	"unexpected token", "invalid character", "unexpected end of json",
}

// noGzip stops compressing for endpoint's host.
func noGzip(endpoint string) { gzipOff.Store(hostOf(endpoint), true) }

func hostOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
