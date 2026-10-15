// Package web implements web search backends and page fetching for the
// web_search and read tools.
package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
)

const (
	fetchTimeout   = 30 * time.Second
	fetchMaxBytes  = 5 << 20
	fetchRedirects = 5
	userAgent      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

var client = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: fetchTimeout,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   2,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > fetchRedirects {
			return fmt.Errorf("stopped after %d redirects", fetchRedirects)
		}
		return nil
	},
}

// Page is a fetched URL. Text is readable text (HTML converted to
// markdown-ish text); Image is set instead for images.
type Page struct {
	URL, Title, ContentType, Text string
	Image                         *llm.Block
	Truncated                     bool // body exceeded the 5 MB cap
}

// Fetch downloads rawURL (30 s timeout, at most 5 redirects, 5 MB) and
// converts it for reading: HTML becomes markdown-ish text, text/JSON/XML
// pass through, images become an image block. URL is the final URL.
func Fetch(ctx context.Context, rawURL string) (*Page, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme %q", req.URL.Scheme)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,text/plain;q=0.8,image/*;q=0.7,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rawURL, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: HTTP %s", rawURL, resp.Status)
	}
	page := &Page{URL: resp.Request.URL.String()}
	if len(body) > fetchMaxBytes {
		body, page.Truncated = body[:fetchMaxBytes], true
	}
	ct := resp.Header.Get("Content-Type")
	mt, params, _ := mime.ParseMediaType(ct)
	if mt == "" || mt == "application/octet-stream" {
		mt, params, _ = mime.ParseMediaType(http.DetectContentType(body))
	}
	page.ContentType = mt
	return page, convert(page, body, mt, params["charset"], resp.Request.URL)
}

func convert(page *Page, body []byte, mt, charset string, base *url.URL) error {
	if images.MediaType(body) != "" || strings.HasPrefix(mt, "image/") && mt != "image/svg+xml" {
		if page.Truncated {
			return fmt.Errorf("%s: image larger than %d MB", page.URL, fetchMaxBytes>>20)
		}
		blk, info, err := images.FromBytes(body, page.URL)
		if err != nil {
			return err
		}
		page.ContentType = info.MediaType
		page.Image = &blk
		page.Text = fmt.Sprintf("Image %s (%dx%d, %d KB)", page.URL, info.Width, info.Height, (info.Bytes+1023)/1024)
		return nil
	}
	isHTML := mt == "text/html" || mt == "application/xhtml+xml"
	if !isHTML && !isTextual(mt) {
		if bytes.IndexByte(body[:min(len(body), 8192)], 0) >= 0 || mt == "application/pdf" {
			return fmt.Errorf("%s: unsupported content type %s", page.URL, mt)
		}
	}
	if charset == "" && isHTML {
		charset = sniffCharset(body)
	}
	text := decodeCharset(body, charset)
	if !isHTML {
		page.Text = strings.TrimSpace(text)
		return nil
	}
	doc := extract(text, base)
	page.Title, page.Text = doc.Title, doc.Text
	return nil
}

func isTextual(mt string) bool {
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	for _, s := range []string{"json", "xml", "javascript", "ecmascript", "yaml", "toml", "x-sh", "sql", "csv"} {
		if strings.Contains(mt, s) {
			return true
		}
	}
	return false
}

// sniffCharset finds <meta charset> or a http-equiv content charset in the
// first 2 KB.
func sniffCharset(body []byte) string {
	head := strings.ToLower(string(body[:min(len(body), 2048)]))
	i := strings.Index(head, "charset=")
	if i < 0 {
		return ""
	}
	s := strings.TrimLeft(head[i+len("charset="):], `"' `)
	end := strings.IndexAny(s, `"'; />`)
	if end < 0 {
		return ""
	}
	return s[:end]
}

// decodeCharset converts body to UTF-8. UTF-8 (and unknown charsets) pass
// through with invalid bytes replaced; Latin-1 is decoded as Windows-1252,
// as browsers do, unless the body is valid UTF-8 anyway.
func decodeCharset(body []byte, charset string) string {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1", "l1", "windows-1252", "cp1252", "us-ascii", "ascii":
		if utf8.Valid(body) { // ASCII, or UTF-8 despite the label
			return string(body)
		}
		var b strings.Builder
		b.Grow(len(body) + len(body)/8)
		for _, c := range body {
			switch {
			case c < 0x80:
				b.WriteByte(c)
			case c < 0xa0:
				b.WriteRune(cp1252[c-0x80])
			default:
				b.WriteRune(rune(c))
			}
		}
		return b.String()
	}
	return strings.ToValidUTF8(string(body), "\uFFFD")
}

// cp1252 maps bytes 0x80-0x9F of Windows-1252 (also used for numeric
// references in that range, per the HTML spec).
var cp1252 = [32]rune{
	'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008d', 'Ž', '\u008f',
	'\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009d', 'ž', 'Ÿ',
}
