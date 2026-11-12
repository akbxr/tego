package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Subscription limits shared by every tego process. Anthropic's usage
// endpoint (/api/oauth/usage) answers only a few calls per access token
// and then refuses for hours (HTTP 429), while every answer of the model
// carries its account's limits in the anthropic-ratelimit-unified-*
// headers. So the limits come from the answers and live in one file for
// all processes (~/.tego/limits.json); a usage endpoint is asked only for
// an account nothing has reported on lately, by one process at a time,
// and not again for an hour after it refused.

const (
	// LimitsEvery is how old an Anthropic account's limits get before its
	// usage endpoint is asked again. Its answers keep an account in use
	// current; one not in use changes only by its windows ending (or by
	// use outside tego).
	LimitsEvery = time.Hour
	// limitsBackoff is how long nobody asks an endpoint that refused.
	limitsBackoff = time.Hour
)

const unifiedPrefix = "anthropic-ratelimit-unified-"

// anthropicHeaderLimits reads the limits an answer reports: the 5-hour and
// weekly windows, and a limit of the model's own tier (7d_oi for Fable),
// sent only with the models it applies to. None with an API key.
func anthropicHeaderLimits(h http.Header, model string) []UsageLimit {
	var out []UsageLimit
	for k, v := range h {
		claim, ok := strings.CutPrefix(strings.ToLower(k), unifiedPrefix)
		if !ok || len(v) == 0 {
			continue
		}
		if claim, ok = strings.CutSuffix(claim, "-utilization"); !ok {
			continue
		}
		used, err := strconv.ParseFloat(strings.TrimSpace(v[0]), 64)
		if err != nil {
			continue
		}
		window, scope, scoped := strings.Cut(claim, "_")
		l := UsageLimit{Window: window, Used: min(max(used, 0), 1)}
		switch window {
		case "5h":
			l.Label = "Claude 5 Hour"
		case "7d":
			l.Label = "Claude 7 Day"
		default:
			continue
		}
		if scoped {
			// The header names the tier by a code ("oi"); the request's
			// model is the tier it applies to.
			if l.Tier = claudeTier(model); l.Tier == "" || scope == "" {
				continue
			}
			l.Label += " (" + l.Tier + ")"
		}
		if sec, err := strconv.ParseInt(strings.TrimSpace(h.Get(unifiedPrefix+claim+"-reset")), 10, 64); err == nil && sec > 0 {
			l.ResetsAt = time.Unix(sec, 0)
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Window != out[j].Window {
			return out[i].Window < out[j].Window // 5h before 7d
		}
		return out[i].Tier < out[j].Tier
	})
	return out
}

// claudeTier is the display name of model's family ("Fable", "Opus"), as
// the usage endpoint names the tier of a scoped limit.
func claudeTier(model string) string {
	m := strings.ToLower(model)
	for _, f := range []string{"fable", "mythos", "opus", "sonnet", "haiku"} {
		if strings.Contains(m, f) {
			return strings.ToUpper(f[:1]) + f[1:]
		}
	}
	return ""
}

// LimitStore keeps the subscription limits of every account, by key
// ("provider/account id"), in a file shared by all tego processes.
type LimitStore struct {
	path string
	now  func() time.Time
	mu   sync.Mutex // the file lock covers other processes
}

var (
	limitStoresMu sync.Mutex
	limitStores   = map[string]*LimitStore{}
)

// Limits returns the store kept in path, one per path in this process.
func Limits(path string) *LimitStore {
	limitStoresMu.Lock()
	defer limitStoresMu.Unlock()
	if s, ok := limitStores[path]; ok {
		return s
	}
	s := &LimitStore{path: path, now: wallNow}
	limitStores[path] = s
	return s
}

type limitEntry struct {
	Org     string        `json:"org,omitempty"`
	Limits  []storedLimit `json:"limits,omitempty"`
	Asked   int64         `json:"asked,omitempty"`   // the endpoint was last asked (unix seconds)
	Blocked int64         `json:"blocked,omitempty"` // it refused: nobody asks before this
	Refusal string        `json:"refusal,omitempty"` // what it said then
}

type storedLimit struct {
	Label  string  `json:"label"`
	Window string  `json:"window"`
	Tier   string  `json:"tier,omitempty"`
	Used   float64 `json:"used"`
	Resets int64   `json:"resets,omitempty"`
	Detail string  `json:"detail,omitempty"`
	At     int64   `json:"at"`
	From   string  `json:"from"` // "answer" or "endpoint"
}

func (e *limitEntry) merge(limits []UsageLimit, from string, now time.Time) {
	for _, l := range limits {
		s := storedLimit{Label: l.Label, Window: l.Window, Tier: l.Tier, Used: l.Used, Detail: l.Detail, At: now.Unix(), From: from}
		if !l.ResetsAt.IsZero() {
			s.Resets = l.ResetsAt.Unix()
		}
		i := 0
		for i < len(e.Limits) && e.Limits[i].Label != l.Label {
			i++
		}
		if i == len(e.Limits) {
			e.Limits = append(e.Limits, s)
		} else {
			e.Limits[i] = s
		}
	}
}

// newest is when the latest of e's limits was reported.
func (e *limitEntry) newest() time.Time {
	var at int64
	for _, l := range e.Limits {
		at = max(at, l.At)
	}
	if at == 0 {
		return time.Time{}
	}
	return time.Unix(at, 0)
}

// KnownLimits is what the store knows of an account.
type KnownLimits struct {
	Report  *UsageReport
	At      time.Time // when the newest of its limits was reported
	From    string    // who reported it: "answer" or "endpoint"
	Failed  error     // the endpoint's failure when it was asked this time
	Blocked time.Time // the endpoint is not asked before this
	Refusal string    // what it said when it refused
}

func (e *limitEntry) known(now time.Time) KnownLimits {
	k := KnownLimits{Refusal: e.Refusal}
	if e.Blocked > now.Unix() {
		k.Blocked = time.Unix(e.Blocked, 0)
	}
	if len(e.Limits) == 0 {
		return k
	}
	r := &UsageReport{Org: e.Org}
	var newest int64
	for _, s := range e.Limits {
		l := UsageLimit{Label: s.Label, Window: s.Window, Tier: s.Tier, Used: s.Used, Detail: s.Detail}
		if s.Resets > 0 {
			l.ResetsAt = time.Unix(s.Resets, 0)
			if !l.ResetsAt.After(now) && l.Window != "extra" {
				// Its window ended since: nothing of the next one is used
				// yet as far as anyone saw, and its end is not known.
				l.Used, l.ResetsAt = 0, time.Time{}
			}
		}
		r.Limits = append(r.Limits, l)
		if s.At > newest {
			newest, k.From = s.At, s.From
		}
	}
	k.Report, k.At = r, time.Unix(newest, 0)
	return k
}

type limitFile map[string]*limitEntry

func (f limitFile) entry(key string) *limitEntry {
	if f[key] == nil {
		f[key] = &limitEntry{}
	}
	return f[key]
}

func (s *LimitStore) read() limitFile {
	f := limitFile{}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f
}

// update changes the file under the lock of this process and of the
// others, and writes it whole (by rename, so readers see either version).
func (s *LimitStore) update(fn func(limitFile)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	f := s.read()
	fn(f)
	b, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Record keeps limits reported for key by from ("answer" or "endpoint");
// each replaces the limit of the same label.
func (s *LimitStore) Record(key string, limits []UsageLimit, from string) {
	if s == nil || len(limits) == 0 {
		return
	}
	now := s.now()
	_ = s.update(func(f limitFile) { f.entry(key).merge(limits, from, now) })
}

// Known returns what the store knows of key, without asking anyone.
func (s *LimitStore) Known(key string) KnownLimits {
	if s == nil {
		return KnownLimits{}
	}
	if e := s.read()[key]; e != nil {
		return e.known(s.now())
	}
	return KnownLimits{}
}

// Usage returns key's limits, asking fetch (a usage endpoint) first when
// none were reported within every, no process asked it within every, and
// it is not refusing. Of all processes, the one that marks the account as
// asked asks; the others take what the store knows. A nil store asks
// every time.
func (s *LimitStore) Usage(ctx context.Context, key string, every time.Duration, fetch func(context.Context) (*UsageReport, error)) KnownLimits {
	if s == nil {
		r, err := fetch(ctx)
		if err != nil || r == nil {
			return KnownLimits{Failed: err}
		}
		return KnownLimits{Report: r, At: wallNow(), From: "endpoint"}
	}
	now := s.now()
	ask := false
	storeErr := s.update(func(f limitFile) {
		e := f.entry(key)
		if now.Unix() < e.Blocked || now.Sub(time.Unix(e.Asked, 0)) < every || now.Sub(e.newest()) < every {
			return
		}
		e.Asked, ask = now.Unix(), true
	})
	if storeErr != nil {
		ask = true // no store: ask as if it were empty
	}
	var failed error
	if ask {
		r, err := fetch(ctx)
		switch {
		case err == nil && r != nil:
			if storeErr != nil {
				return KnownLimits{Report: r, At: now, From: "endpoint"}
			}
			_ = s.update(func(f limitFile) {
				e := f.entry(key)
				e.merge(r.Limits, "endpoint", now)
				if r.Org != "" {
					e.Org = r.Org
				}
				e.Blocked, e.Refusal = 0, ""
			})
		case err != nil:
			failed = err
			if UsageRefused(err) {
				_ = s.update(func(f limitFile) {
					e := f.entry(key)
					e.Blocked, e.Refusal = now.Add(limitsBackoff).Unix(), err.Error()
				})
			}
		}
	}
	k := s.Known(key)
	k.Failed = failed
	return k
}

// UsageError is a usage endpoint's refusal.
type UsageError struct {
	Status  int
	Message string
}

func (e *UsageError) Error() string { return "HTTP " + strconv.Itoa(e.Status) + ": " + e.Message }

// UsageRefused reports a usage endpoint refusing to answer this often.
func UsageRefused(err error) bool {
	var ue *UsageError
	return errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests
}

// usageErrorMessage is the message of an error body, on one line.
func usageErrorMessage(body []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return truncate(strings.Join(strings.Fields(string(body)), " "), 200)
}
