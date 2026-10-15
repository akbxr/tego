package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Endpoints and the onboarding poll interval used by the google-antigravity
// login (variables for tests).
var (
	codeAssistBase         = "https://daily-cloudcode-pa.googleapis.com"
	googleUserinfoURL      = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"
	antigravityOnboardPoll = time.Second
)

const (
	antigravityFreeTier       = "free-tier"
	antigravityOnboardTimeout = 30 * time.Second
)

var antigravityMetadata = map[string]string{"ideType": "ANTIGRAVITY"}

// antigravityAfterLogin requires a refresh token, looks up the Google
// account email and resolves (provisioning if needed) the Cloud Code
// Assist project.
func antigravityAfterLogin(ctx context.Context, c *Credential, out io.Writer) error {
	if c.Refresh == "" {
		return errors.New("no refresh token received; run the login again and allow offline access")
	}
	c.Email = googleEmail(ctx, c.Access)
	project, err := discoverAntigravityProject(ctx, c.Access, c.Email, out)
	if err != nil {
		return err
	}
	c.ProjectID = project
	return nil
}

// antigravityAfterRefresh re-discovers the project only when the stored
// credential lacks one (the stored project id is kept on refresh).
func antigravityAfterRefresh(ctx context.Context, c *Credential) error {
	if c.ProjectID != "" {
		return nil
	}
	project, err := discoverAntigravityProject(ctx, c.Access, c.Email, io.Discard)
	if err != nil {
		return err
	}
	c.ProjectID = project
	return nil
}

func googleEmail(ctx context.Context, token string) string {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var v struct {
		Email string `json:"email"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
	return v.Email
}

type caTier struct {
	ID string `json:"id"`
}

type loadCodeAssistResponse struct {
	CurrentTier     *caTier  `json:"currentTier"`
	PaidTier        *caTier  `json:"paidTier"`
	AllowedTiers    []caTier `json:"allowedTiers"`
	IneligibleTiers []struct {
		TierID        string `json:"tierId"`
		ReasonMessage string `json:"reasonMessage"`
		ValidationURL string `json:"validationUrl"`
	} `json:"ineligibleTiers"`
	Project string `json:"cloudaicompanionProject"`
}

type onboardOperation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response json.RawMessage `json:"response"`
}

// discoverAntigravityProject is the project hook: loadCodeAssist (a
// second call with the returned project for non-paid tiers), the free-tier
// eligibility check, onboardUser polling when there is no current tier,
// then loadCodeAssist again for the project id.
func discoverAntigravityProject(ctx context.Context, token, email string, out io.Writer) (string, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", llm.AntigravityUserAgent(ctx))
	call := func(ctx context.Context, label, method, url string, body any, out any) error {
		var rd io.Reader
		if body != nil {
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return err
		}
		req.Header = h.Clone()
		if body == nil {
			req.Header.Del("Content-Type")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			text := strings.TrimSpace(string(raw))
			if u := validationURL(text); u != "" {
				who := ""
				if email != "" {
					who = " for " + email
				}
				return fmt.Errorf("account verification required%s. Visit %s to continue, then sign in again", who, u)
			}
			return fmt.Errorf("%s failed: HTTP %d: %s", label, resp.StatusCode, text)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%s: bad response: %w", label, err)
		}
		return nil
	}
	load := func() (loadCodeAssistResponse, error) {
		lctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
		defer cancel()
		var r loadCodeAssistResponse
		url := codeAssistBase + "/v1internal:loadCodeAssist"
		if err := call(lctx, "loadCodeAssist", http.MethodPost, url, map[string]any{"metadata": antigravityMetadata}, &r); err != nil {
			return r, err
		}
		if r.PaidTier == nil && r.Project != "" {
			var again loadCodeAssistResponse
			body := map[string]any{"cloudaicompanionProject": r.Project, "metadata": antigravityMetadata}
			if err := call(lctx, "loadCodeAssist", http.MethodPost, url, body, &again); err != nil {
				return r, err
			}
			r = again
		}
		return r, nil
	}

	fmt.Fprintln(out, "Checking Cloud Code Assist account status...")
	r, err := load()
	if err != nil {
		return "", fmt.Errorf("could not discover an Antigravity project: %w", err)
	}
	if err := freeTierEligible(r); err != nil {
		return "", err
	}
	if r.CurrentTier == nil {
		fmt.Fprintln(out, "Provisioning the Antigravity free tier...")
		if err := onboardAntigravity(ctx, call); err != nil {
			return "", err
		}
	}
	fmt.Fprintln(out, "Refreshing Cloud Code Assist project...")
	if r, err = load(); err != nil {
		return "", fmt.Errorf("could not discover an Antigravity project: %w", err)
	}
	if r.Project == "" {
		return "", errors.New("loadCodeAssist did not return a cloudaicompanionProject")
	}
	return r.Project, nil
}

func freeTierEligible(r loadCodeAssistResponse) error {
	for _, t := range r.AllowedTiers {
		if t.ID == antigravityFreeTier {
			return nil
		}
	}
	for _, t := range r.IneligibleTiers {
		if t.TierID == antigravityFreeTier && t.ReasonMessage != "" {
			if t.ValidationURL != "" {
				return fmt.Errorf("%s\n%s", t.ReasonMessage, t.ValidationURL)
			}
			return errors.New(t.ReasonMessage)
		}
	}
	return nil
}

type caCall func(ctx context.Context, label, method, url string, body any, out any) error

// onboardAntigravity starts free-tier onboarding and polls the returned
// long-running operation for up to 30 seconds.
func onboardAntigravity(ctx context.Context, call caCall) error {
	ctx, cancel := context.WithTimeout(ctx, antigravityOnboardTimeout)
	defer cancel()
	var op onboardOperation
	body := map[string]any{"tierId": antigravityFreeTier, "metadata": antigravityMetadata}
	if err := call(ctx, "onboardUser", http.MethodPost, codeAssistBase+"/v1internal:onboardUser", body, &op); err != nil {
		return onboardErr(ctx, err)
	}
	for !op.Done {
		if op.Name == "" {
			return errors.New("onboardUser returned an operation without a name")
		}
		select {
		case <-ctx.Done():
			return onboardErr(ctx, ctx.Err())
		case <-time.After(antigravityOnboardPoll):
		}
		name := op.Name
		op = onboardOperation{}
		if err := call(ctx, "onboardUser operation", http.MethodGet, codeAssistBase+"/v1internal/"+name, nil, &op); err != nil {
			return onboardErr(ctx, err)
		}
	}
	if op.Error != nil {
		if op.Error.Message != "" {
			return fmt.Errorf("onboardUser operation failed: %d: %s", op.Error.Code, op.Error.Message)
		}
		return fmt.Errorf("onboardUser operation failed: code %d", op.Error.Code)
	}
	if len(op.Response) == 0 || string(op.Response) == "null" {
		return errors.New("onboardUser operation finished without a response")
	}
	return nil
}

func onboardErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("onboardUser timed out after %s", antigravityOnboardTimeout)
	}
	return err
}

// validationURL extracts the VALIDATION_REQUIRED link from a Google error body.
func validationURL(body string) string {
	if !strings.Contains(body, "VALIDATION_REQUIRED") {
		return ""
	}
	i := strings.IndexByte(body, '{')
	if i < 0 {
		return ""
	}
	var v struct {
		Error struct {
			Details []struct {
				Reason   string `json:"reason"`
				Metadata struct {
					ValidationURL string `json:"validation_url"`
				} `json:"metadata"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body[i:]), &v) != nil {
		return ""
	}
	for _, d := range v.Error.Details {
		if d.Reason == "VALIDATION_REQUIRED" {
			return d.Metadata.ValidationURL
		}
	}
	return ""
}
