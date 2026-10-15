package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// oauthConfig holds the login/refresh definitions for one provider.
type oauthConfig struct {
	clientID       string
	clientSecret   string // installed-app secret sent to the token endpoint (Google)
	authorizeURL   string
	tokenURL       string
	scope          string
	refreshScope   string // sent with refresh requests ("" = none)
	authorizeExtra [][2]string
	port           int // callback port; 0 = random
	path           string
	portFallback   bool // use a random port when the fixed one is busy
	jsonBody       bool // token endpoint takes JSON (else form-encoded)
	stateInToken   bool // send state with the authorization_code exchange
	tokenHeaders   map[string]string
	successURL     string // where the browser goes after the callback ("" = a local page)
	expirySkew     time.Duration
	// afterLogin completes a fresh credential (e.g. account and project
	// discovery); afterRefresh repairs a refreshed one. Both may be nil.
	afterLogin   func(ctx context.Context, c *Credential, out io.Writer) error
	afterRefresh func(ctx context.Context, c *Credential) error
}

var oauthConfigs = map[string]*oauthConfig{
	// Claude Code 2.1.296's login: claude.com authorize page, platform.claude.com
	// token endpoint, callback on a random localhost port, axios as the HTTP
	// client, and a redirect to Anthropic's success page afterwards.
	"anthropic": {
		clientID:       mustB64("OWQxYzI1MGEtZTYxYi00NGQ5LTg4ZWQtNTk0NGQxOTYyZjVl"),
		authorizeURL:   "https://claude.com/cai/oauth/authorize",
		tokenURL:       "https://platform.claude.com/v1/oauth/token",
		scope:          "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins",
		refreshScope:   "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload user:plugins",
		authorizeExtra: [][2]string{{"code", "true"}},
		path:           "/callback",
		jsonBody:       true,
		stateInToken:   true,
		tokenHeaders: map[string]string{
			"Accept":     "application/json, text/plain, */*",
			"User-Agent": "axios/1.15.2",
		},
		successURL:   "https://platform.claude.com/oauth/code/success?app=claude-code",
		expirySkew:   5 * time.Minute,
		afterLogin:   anthropicAfterLogin,
		afterRefresh: anthropicAfterRefresh,
	},
	"openai-codex": {
		clientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		authorizeURL: "https://auth.openai.com/oauth/authorize",
		tokenURL:     "https://auth.openai.com/oauth/token",
		scope:        "openid profile email offline_access api.connectors.read api.connectors.invoke",
		authorizeExtra: [][2]string{
			{"id_token_add_organizations", "true"},
			{"codex_cli_simplified_flow", "true"},
			{"originator", "omp"},
		},
		port: 1455,
		path: "/auth/callback",
	},
	// Antigravity's Google installed-app client, scopes and callback; PKCE is
	// added on top (Google accepts it).
	"google-antigravity": {
		clientID:     mustB64("MTA3MTAwNjA2MDU5MS10bWhzc2luMmgyMWxjcmUyMzV2dG9sb2poNGc0MDNlcC5hcHBzLmdvb2dsZXVzZXJjb250ZW50LmNvbQ=="),
		clientSecret: mustB64("R09DU1BYLUs1OEZXUjQ4NkxkTEoxbUxCOHNYQzR6NnFEQWY="),
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		scope: "https://www.googleapis.com/auth/cloud-platform https://www.googleapis.com/auth/userinfo.email " +
			"https://www.googleapis.com/auth/userinfo.profile https://www.googleapis.com/auth/cclog " +
			"https://www.googleapis.com/auth/experimentsandconfigs",
		authorizeExtra: [][2]string{{"access_type", "offline"}, {"prompt", "consent"}},
		port:           51121,
		path:           "/oauth-callback",
		portFallback:   true,
		expirySkew:     5 * time.Minute,
		afterLogin:     antigravityAfterLogin,
		afterRefresh:   antigravityAfterRefresh,
	},
}

func mustB64(s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const tokenRequestTimeout = 15 * time.Second

// Login runs the browser OAuth flow for provider ("anthropic",
// "openai-codex" or "google-antigravity") and stores the resulting credential. The authorization
// code arrives either through a local callback server or pasted into in
// (redirect URL, `code#state`, or bare code), whichever comes first. A read
// on in may remain pending after Login returns if the callback won.
func Login(ctx context.Context, s *Store, provider string, in io.Reader, out io.Writer) error {
	cfg, ok := oauthConfigs[provider]
	if !ok {
		return fmt.Errorf("auth: provider %q does not support OAuth login", provider)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	verifier := base64.RawURLEncoding.EncodeToString(randomBytes(32))
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := hex.EncodeToString(randomBytes(16))

	results := make(chan authResult, 4)
	port, stop := cfg.serveCallback(state, results)
	defer stop()
	if port == 0 {
		port = cfg.port
		fmt.Fprintf(out, "Could not listen on localhost:%d; paste the redirect URL after logging in.\n", cfg.port)
	}
	redirectURI := "http://localhost:" + strconv.Itoa(port) + cfg.path

	// Parameters in Claude Code's order; extras such as code=true first.
	q := make([]string, 0, 8+len(cfg.authorizeExtra))
	for _, kv := range cfg.authorizeExtra {
		q = append(q, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	for _, kv := range [][2]string{
		{"client_id", cfg.clientID},
		{"response_type", "code"},
		{"redirect_uri", redirectURI},
		{"scope", cfg.scope},
		{"code_challenge", challenge},
		{"code_challenge_method", "S256"},
		{"state", state},
	} {
		q = append(q, url.QueryEscape(kv[0])+"="+url.QueryEscape(kv[1]))
	}
	authURL := cfg.authorizeURL + "?" + strings.Join(q, "&")

	fmt.Fprintf(out, "Open this URL to log in:\n\n  %s\n\n", authURL)
	fmt.Fprintln(out, "If the browser cannot reach this machine, paste the final redirect URL or code here:")
	openBrowser(authURL)

	go readPasted(in, state, results)

	var res authResult
	select {
	case <-ctx.Done():
		return ctx.Err()
	case res = <-results:
	}
	if res.err != nil {
		return res.err
	}
	if res.state == "" {
		res.state = state
	}

	cred, err := cfg.exchange(ctx, res.code, res.state, redirectURI, verifier, out)
	if err != nil {
		return fmt.Errorf("auth: %s login: %w", provider, err)
	}
	stored, replaced, err := s.Add(provider, cred)
	if err != nil {
		return err
	}
	n := len(s.Accounts(provider))
	switch {
	case replaced:
		fmt.Fprintf(out, "Logged in to %s as %s (updated existing account; %d account(s)).\n", provider, stored.Label(), n)
	default:
		fmt.Fprintf(out, "Logged in to %s as %s (%d account(s)).\n", provider, stored.Label(), n)
	}
	return nil
}

// exchange trades an authorization code for a credential and runs the
// provider's afterLogin step.
func (c *oauthConfig) exchange(ctx context.Context, code, state, redirectURI, verifier string, out io.Writer) (Credential, error) {
	params := [][2]string{
		{"grant_type", "authorization_code"},
		{"code", code},
		{"redirect_uri", redirectURI},
		{"client_id", c.clientID},
		{"code_verifier", verifier},
	}
	if c.stateInToken {
		params = append(params, [2]string{"state", state})
	}
	cred, err := c.tokenRequest(ctx, params, "")
	if err != nil {
		return Credential{}, fmt.Errorf("token exchange: %w", err)
	}
	if c.afterLogin != nil {
		if err := c.afterLogin(ctx, &cred, out); err != nil {
			return Credential{}, err
		}
	}
	return cred, nil
}

type authResult struct {
	code, state string
	err         error
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

const callbackPage = `<!doctype html><html><head><meta charset="utf-8"><title>tego</title></head>` +
	`<body style="font-family:sans-serif;margin:4em"><h2>%s</h2><p>You can close this window.</p></body></html>`

// serveCallback starts the local redirect listener. It returns the bound
// port (0 if none) and a stop function.
func (c *oauthConfig) serveCallback(state string, results chan<- authResult) (int, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(c.port))
	if err != nil && c.portFallback {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return 0, func() {}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	listeners := []net.Listener{ln}
	// Browsers may resolve localhost to ::1; listen there too when possible.
	if ln6, err := net.Listen("tcp", "[::1]:"+strconv.Itoa(port)); err == nil {
		listeners = append(listeners, ln6)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(c.path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var res authResult
		switch {
		case q.Get("error") != "":
			res.err = fmt.Errorf("auth: authorization failed: %s %s", q.Get("error"), q.Get("error_description"))
		case q.Get("code") == "":
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		case q.Get("state") != state:
			res.err = errors.New("auth: OAuth state mismatch")
		default:
			res = authResult{code: q.Get("code"), state: q.Get("state")}
		}
		switch {
		case res.err != nil:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, callbackPage, "Login failed")
		case c.successURL != "":
			http.Redirect(w, r, c.successURL, http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, callbackPage, "Login successful")
		}
		select {
		case results <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	for _, l := range listeners {
		go srv.Serve(l)
	}
	return port, func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}
}

// readPasted reads lines from in until one parses as an authorization code.
func readPasted(in io.Reader, state string, results chan<- authResult) {
	if in == nil {
		return
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		code, st := parsePasted(line)
		if code == "" {
			continue
		}
		res := authResult{code: code, state: st}
		if st != "" && st != state {
			res = authResult{err: errors.New("auth: OAuth state mismatch in pasted code")}
		}
		select {
		case results <- res:
		default:
		}
		return
	}
}

// parsePasted accepts a full redirect URL, a query string containing
// code=..., `code#state`, or a bare code.
func parsePasted(s string) (code, state string) {
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		q := u.Query()
		return q.Get("code"), q.Get("state")
	}
	if strings.Contains(s, "code=") {
		q, err := url.ParseQuery(strings.TrimPrefix(s, "?"))
		if err == nil {
			return q.Get("code"), q.Get("state")
		}
	}
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if cmd.Start() == nil {
		go cmd.Wait()
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"` // OpenAI: carries the account email
	Account      struct {
		UUID         string `json:"uuid"`          // Anthropic
		EmailAddress string `json:"email_address"` // Anthropic
	} `json:"account"`
}

// jwtEmail reads the unverified "email" claim of a JWT (identification only).
func jwtEmail(jwt string) string {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims struct {
		Email string `json:"email"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.Email
}

// tokenRequest posts params (in order) to the token endpoint and converts
// the reply to a Credential. oldRefresh is kept when the response omits a
// new one.
func (c *oauthConfig) tokenRequest(ctx context.Context, params [][2]string, oldRefresh string) (Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	if c.clientSecret != "" {
		params = append(params, [2]string{"client_secret", c.clientSecret})
	}
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	if c.jsonBody {
		body = append(body, '{')
		for i, kv := range params {
			if i > 0 {
				body = append(body, ',')
			}
			k, _ := json.Marshal(kv[0])
			v, _ := json.Marshal(kv[1])
			body = append(append(append(body, k...), ':'), v...)
		}
		body = append(body, '}')
		contentType = "application/json"
	} else {
		v := url.Values{}
		for _, kv := range params {
			v.Set(kv[0], kv[1])
		}
		body = []byte(v.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	for k, v := range c.tokenHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Credential{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return Credential{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Credential{}, fmt.Errorf("bad token response: %w", err)
	}
	if tr.AccessToken == "" {
		return Credential{}, errors.New("token response missing access_token")
	}
	cred := Credential{Type: "oauth", Access: tr.AccessToken, Refresh: tr.RefreshToken, Email: tr.Account.EmailAddress, AccountID: tr.Account.UUID}
	if cred.Email == "" && tr.IDToken != "" {
		cred.Email = jwtEmail(tr.IDToken)
	}
	if cred.Refresh == "" {
		cred.Refresh = oldRefresh
	}
	if tr.ExpiresIn > 0 {
		cred.Expires = time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second - c.expirySkew).UnixMilli()
	}
	return cred, nil
}

// refreshWindow: tokens are refreshed once they are this close to expiry.
const refreshWindow = 5 * time.Minute

// TokenSource returns a bearer-token source for one account of provider
// (id "" = the first account) that refreshes OAuth credentials near expiry.
// Refreshes are coordinated through the store lock and re-check the file,
// so a token refreshed by another process is reused rather than refreshed
// twice. API-key credentials are returned as-is.
func (s *Store) TokenSource(provider, id string) llm.TokenSource {
	return &tokenSource{store: s, provider: provider, id: id}
}

type tokenSource struct {
	store    *Store
	provider string
	id       string
	mu       sync.Mutex
	// account uuid (Anthropic), cached after the first AccountID call
	accountID string
	lookedUp  bool
}

func fresh(c Credential) bool {
	return c.Expires == 0 || time.Until(time.UnixMilli(c.Expires)) > refreshWindow
}

// account finds the source's account in data; callers hold store.mu.
func (t *tokenSource) account(data map[string][]Credential) (int, bool) {
	for i, c := range data[t.provider] {
		if t.id == "" || c.ID == t.id {
			return i, true
		}
	}
	return 0, false
}

func (t *tokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	notLoggedIn := fmt.Errorf("auth: not logged in to %s (run `tego login %s`)", t.provider, t.provider)
	t.store.mu.Lock()
	_ = t.store.reload()
	i, ok := t.account(t.store.data)
	var c Credential
	if ok {
		c = t.store.data[t.provider][i]
	}
	t.store.mu.Unlock()
	if !ok {
		return "", notLoggedIn
	}
	if c.Type == "api_key" {
		return c.Key, nil
	}
	if fresh(c) {
		return c.Access, nil
	}
	cfg, ok := oauthConfigs[t.provider]
	if !ok {
		return "", fmt.Errorf("auth: %s token expired and cannot be refreshed", t.provider)
	}
	var token string
	err := t.store.locked(ctx, func() error {
		i, ok := t.account(t.store.data)
		if !ok {
			return notLoggedIn
		}
		c := t.store.data[t.provider][i]
		if fresh(c) { // refreshed by another process meanwhile
			token = c.Access
			return nil
		}
		if c.Refresh == "" {
			return fmt.Errorf("auth: %s token for %s expired; run `tego login %s`", t.provider, c.Label(), t.provider)
		}
		nc, err := cfg.refresh(ctx, c)
		if err != nil {
			return fmt.Errorf("auth: refresh %s token for %s (run `tego login %s` if this persists): %w", t.provider, c.Label(), t.provider, err)
		}
		nc.ID = c.ID
		t.store.data[t.provider][i] = nc
		token = nc.Access
		return t.store.write()
	})
	return token, err
}

// refresh exchanges old's refresh token for a new credential, keeping the
// refresh token (Google does not rotate it), project and account when the
// response omits them.
func (c *oauthConfig) refresh(ctx context.Context, old Credential) (Credential, error) {
	params := [][2]string{
		{"grant_type", "refresh_token"},
		{"refresh_token", old.Refresh},
		{"client_id", c.clientID},
	}
	if c.refreshScope != "" {
		params = append(params, [2]string{"scope", c.refreshScope})
	}
	nc, err := c.tokenRequest(ctx, params, old.Refresh)
	if err != nil {
		return Credential{}, err
	}
	nc.ProjectID, nc.Email = old.ProjectID, old.Email
	if nc.AccountID == "" {
		nc.AccountID = old.AccountID
	}
	if c.afterRefresh != nil {
		if err := c.afterRefresh(ctx, &nc); err != nil {
			return Credential{}, err
		}
	}
	return nc, nil
}
