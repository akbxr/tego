package llm

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// bodyServer records each request's Content-Encoding and decoded body; with
// refuse set it answers compressed bodies 400, as a proxy that can't read
// them would.
type bodyServer struct {
	mu        sync.Mutex
	encodings []string
	bodies    []string
	refuse    bool
}

func (s *bodyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	enc := r.Header.Get("Content-Encoding")
	var rd io.Reader = r.Body
	if enc == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		rd = zr
	}
	b, _ := io.ReadAll(rd)
	s.mu.Lock()
	s.encodings = append(s.encodings, enc)
	s.bodies = append(s.bodies, string(b))
	s.mu.Unlock()
	if enc == "gzip" && s.refuse {
		http.Error(w, `{"error":{"message":"invalid JSON body"}}`, http.StatusBadRequest)
		return
	}
	io.WriteString(w, "ok")
}

func (s *bodyServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.encodings...)
}

// trustTestHosts makes the httptest host one that takes compressed bodies.
func trustTestHosts(t *testing.T) {
	old := gzipHost
	gzipHost = func(h string) bool { return h == "127.0.0.1" }
	gzipOff.Clear()
	t.Cleanup(func() { gzipHost = old; gzipOff.Clear(); SetGzipRequests(true) })
}

func send(t *testing.T, url string, body string) error {
	t.Helper()
	resp, err := postJSON(context.Background(), "test", url, []byte(body), http.Header{"Content-Type": {"application/json"}})
	if err == nil {
		resp.Body.Close()
	}
	return err
}

func TestLargeBodiesGoCompressedToKnownHosts(t *testing.T) {
	trustTestHosts(t)
	s := &bodyServer{}
	srv := httptest.NewServer(s)
	defer srv.Close()

	big := `{"messages":"` + strings.Repeat("some conversation ", 4000) + `"}`
	small := `{"messages":"hi"}`
	for _, b := range []string{big, small} {
		if err := send(t, srv.URL, b); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.seen(); got[0] != "gzip" || got[1] != "" {
		t.Fatalf("encodings %q, want the large body compressed and the small one plain", got)
	}
	if s.bodies[0] != big {
		t.Fatal("the compressed body did not decode to what was sent")
	}

	SetGzipRequests(false) // "gzip_requests": false
	if err := send(t, srv.URL, big); err != nil {
		t.Fatal(err)
	}
	if got := s.seen(); got[2] != "" {
		t.Fatalf("compressed with gzip_requests off: %q", got)
	}
}

func TestOtherHostsGetPlainBodies(t *testing.T) {
	gzipOff.Clear()
	s := &bodyServer{}
	srv := httptest.NewServer(s)
	defer srv.Close()
	if err := send(t, srv.URL, strings.Repeat("x", 64<<10)); err != nil {
		t.Fatal(err)
	}
	if got := s.seen(); got[0] != "" {
		t.Fatalf("an unknown host (a proxy, OpenRouter, a base_url) got %q", got)
	}
	if !gzipHost("api.anthropic.com") || !gzipHost("daily-cloudcode-pa.googleapis.com") || !gzipHost("chatgpt.com") ||
		gzipHost("openrouter.ai") || gzipHost("api.openai.com") || gzipHost("my-proxy.example") || gzipHost("api.anthropic.com.example") {
		t.Fatal("known hosts list is off")
	}
}

// A host that refuses the compressed body gets it again plain, and plain
// from then on; the caller sees only the success.
func TestRefusedCompressionFallsBackToPlain(t *testing.T) {
	trustTestHosts(t)
	s := &bodyServer{refuse: true}
	srv := httptest.NewServer(s)
	defer srv.Close()
	big := strings.Repeat("y", 64<<10)
	if err := send(t, srv.URL, big); err != nil {
		t.Fatalf("fallback did not hide the refusal: %v", err)
	}
	if err := send(t, srv.URL, big); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.seen(), ","); got != "gzip,," {
		t.Fatalf("encodings %q, want gzip, then plain twice", got)
	}
}

// A 400 that plain gets too is the request's own fault: the caller gets it,
// and compression stays on.
func TestRealBadRequestKeepsCompression(t *testing.T) {
	trustTestHosts(t)
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		io.Copy(io.Discard, r.Body)
		http.Error(w, `{"error":{"message":"prompt is too long"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	err := send(t, srv.URL, strings.Repeat("z", 64<<10))
	if ae, ok := err.(*APIError); !ok || ae.Status != 400 || !strings.Contains(ae.Body, "too long") {
		t.Fatalf("err = %v", err)
	}
	if n != 2 {
		t.Fatalf("%d requests, want the compressed one and one plain", n)
	}
	if gzipFor(srv.URL, []byte(strings.Repeat("z", 64<<10))) == nil {
		t.Fatal("compression turned off after an error that was not its fault")
	}
}
