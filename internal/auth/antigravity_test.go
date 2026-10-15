package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGoogle serves the token, userinfo and Cloud Code Assist endpoints the
// google-antigravity login uses, recording every request.
type fakeGoogle struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	forms    []url.Values
	bodies   []map[string]any
	loads    int
	onboard  bool // the account starts without a tier
	noRefTok bool
}

func (f *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/token":
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			f.t.Errorf("token content type %q", ct)
		}
		r.ParseForm()
		f.forms = append(f.forms, r.PostForm)
		resp := map[string]any{"access_token": "at-new", "expires_in": 3600}
		if r.PostForm.Get("grant_type") == "authorization_code" && !f.noRefTok {
			resp["refresh_token"] = "rt-1"
		}
		json.NewEncoder(w).Encode(resp)
	case r.URL.Path == "/userinfo":
		if r.Header.Get("Authorization") != "Bearer at-new" {
			f.t.Errorf("userinfo auth %q", r.Header.Get("Authorization"))
		}
		io.WriteString(w, `{"email":"me@example.com"}`)
	case r.URL.Path == "/v1internal:loadCodeAssist":
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "antigravity/hub/7.7.7 ") || r.Header.Get("Authorization") != "Bearer at-new" {
			f.t.Errorf("loadCodeAssist headers %v", r.Header)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		f.loads++
		if f.onboard && f.loads == 1 {
			io.WriteString(w, `{"allowedTiers":[{"id":"free-tier"}]}`)
			return
		}
		if body["cloudaicompanionProject"] == nil {
			io.WriteString(w, `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"proj-x"}`)
		} else {
			io.WriteString(w, `{"currentTier":{"id":"free-tier"},"cloudaicompanionProject":"proj-42"}`)
		}
	case r.URL.Path == "/v1internal:onboardUser":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.bodies = append(f.bodies, body)
		io.WriteString(w, `{"name":"operations/op-1","done":false}`)
	case r.URL.Path == "/v1internal/operations/op-1":
		io.WriteString(w, `{"name":"operations/op-1","done":true,"response":{"@type":"x","cloudaicompanionProject":"proj-42"}}`)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

func withFakeGoogle(t *testing.T, f *fakeGoogle) *oauthConfig {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("TEGO_ANTIGRAVITY_VERSION", "7.7.7")
	oldBase, oldInfo, oldPoll := codeAssistBase, googleUserinfoURL, antigravityOnboardPoll
	codeAssistBase, googleUserinfoURL, antigravityOnboardPoll = srv.URL, srv.URL+"/userinfo", time.Millisecond
	t.Cleanup(func() { codeAssistBase, googleUserinfoURL, antigravityOnboardPoll = oldBase, oldInfo, oldPoll })
	cfg := *oauthConfigs["google-antigravity"]
	cfg.tokenURL = srv.URL + "/token"
	return &cfg
}

func TestAntigravityLoginExchange(t *testing.T) {
	f := &fakeGoogle{onboard: true}
	cfg := withFakeGoogle(t, f)
	var out strings.Builder
	cred, err := cfg.exchange(context.Background(), "the-code", "st", "http://localhost:51121/oauth-callback", "verifier", &out)
	if err != nil {
		t.Fatal(err)
	}
	if cred.Type != "oauth" || cred.Access != "at-new" || cred.Refresh != "rt-1" || cred.Email != "me@example.com" || cred.ProjectID != "proj-42" {
		t.Fatalf("cred = %+v", cred)
	}
	if left := time.Until(time.UnixMilli(cred.Expires)); left < 54*time.Minute || left > 56*time.Minute {
		t.Errorf("expiry %v (want 1h minus 5m skew)", left)
	}
	form := f.forms[0]
	want := map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "code_verifier": "verifier",
		"redirect_uri": "http://localhost:51121/oauth-callback", "client_id": cfg.clientID, "client_secret": cfg.clientSecret,
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, form.Get(k), v)
		}
	}
	if !strings.HasSuffix(cfg.clientID, ".apps.googleusercontent.com") || !strings.HasPrefix(cfg.clientSecret, "GOCSPX-") {
		t.Errorf("client credentials not decoded")
	}
	calls := strings.Join(f.calls, ",")
	if calls != "POST /token,GET /userinfo,POST /v1internal:loadCodeAssist,POST /v1internal:onboardUser,GET /v1internal/operations/op-1,POST /v1internal:loadCodeAssist,POST /v1internal:loadCodeAssist" {
		t.Errorf("calls = %s", calls)
	}
	if b, _ := json.Marshal(f.bodies[0]); string(b) != `{"metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Errorf("loadCodeAssist body = %s", b)
	}
	if b, _ := json.Marshal(f.bodies[1]); string(b) != `{"metadata":{"ideType":"ANTIGRAVITY"},"tierId":"free-tier"}` {
		t.Errorf("onboardUser body = %s", b)
	}
	if b, _ := json.Marshal(f.bodies[3]); string(b) != `{"cloudaicompanionProject":"proj-x","metadata":{"ideType":"ANTIGRAVITY"}}` {
		t.Errorf("second loadCodeAssist body = %s", b)
	}
	if !strings.Contains(out.String(), "Provisioning the Antigravity free tier") {
		t.Errorf("progress output = %q", out.String())
	}
}

func TestAntigravityLoginRequiresRefreshToken(t *testing.T) {
	cfg := withFakeGoogle(t, &fakeGoogle{noRefTok: true})
	_, err := cfg.exchange(context.Background(), "c", "s", "http://localhost:51121/oauth-callback", "v", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no refresh token") {
		t.Fatalf("err = %v", err)
	}
}

func TestAntigravityRefresh(t *testing.T) {
	f := &fakeGoogle{}
	cfg := withFakeGoogle(t, f)
	old := Credential{Type: "oauth", Access: "at-old", Refresh: "rt-keep", Expires: 1, ProjectID: "proj-9", Email: "me@example.com"}
	nc, err := cfg.refresh(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if nc.Access != "at-new" || nc.Refresh != "rt-keep" || nc.ProjectID != "proj-9" || nc.Email != "me@example.com" {
		t.Fatalf("refreshed = %+v", nc)
	}
	form := f.forms[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "rt-keep" ||
		form.Get("client_id") != cfg.clientID || form.Get("client_secret") != cfg.clientSecret {
		t.Errorf("refresh form = %v", form)
	}
	if len(f.calls) != 1 {
		t.Errorf("refresh with a known project made extra calls: %v", f.calls)
	}

	// A credential without a project re-discovers it.
	old.ProjectID = ""
	if nc, err = cfg.refresh(context.Background(), old); err != nil || nc.ProjectID != "proj-42" {
		t.Fatalf("rediscovery: %+v %v", nc, err)
	}
}

func TestAntigravityIneligible(t *testing.T) {
	r := loadCodeAssistResponse{}
	json.Unmarshal([]byte(`{"ineligibleTiers":[{"tierId":"free-tier","reasonMessage":"Not available in your region","validationUrl":"https://v"}]}`), &r)
	if err := freeTierEligible(r); err == nil || err.Error() != "Not available in your region\nhttps://v" {
		t.Fatalf("err = %v", err)
	}
	json.Unmarshal([]byte(`{"allowedTiers":[{"id":"free-tier"}]}`), &r)
	if err := freeTierEligible(r); err != nil {
		t.Fatalf("allowed tier rejected: %v", err)
	}
}
