package stats

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CatalogURL is the models.dev catalog (https://github.com/anomalyco/models.dev).
const CatalogURL = "https://models.dev/api.json"

// Providers whose prices win, in order: the model's own vendor first, then
// the aggregators tego talks to, whose ids carry a vendor prefix.
var catalogProviders = []string{
	"anthropic", "openai", "google", "deepseek", "xai", "mistral", "zai", "moonshotai", "alibaba", "minimax",
	"groq", "openrouter", "opencode",
}

type catalogCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Tiers      []struct {
		Input      float64 `json:"input"`
		Output     float64 `json:"output"`
		CacheRead  float64 `json:"cache_read"`
		CacheWrite float64 `json:"cache_write"`
		Tier       struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
}

// ParseCatalog reads models.dev's api.json into prices keyed the way
// lookups normalise model ids.
func ParseCatalog(data []byte) (map[string]Price, error) {
	var cat map[string]struct {
		Models map[string]struct {
			Cost *catalogCost `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &cat); err != nil {
		return nil, fmt.Errorf("models.dev catalog: %w", err)
	}
	out := map[string]Price{}
	for _, provider := range catalogProviders {
		ids := make([]string, 0, len(cat[provider].Models))
		for id := range cat[provider].Models {
			ids = append(ids, id)
		}
		sort.Strings(ids) // deterministic winner among ids that normalise alike
		for _, id := range ids {
			c := cat[provider].Models[id].Cost
			key := priceIDs(id)[0]
			if c == nil || strings.ContainsAny(key, ":~") || (c.Input == 0 && c.Output == 0) {
				continue // unpriced, free variants and router aliases
			}
			if _, ok := out[key]; ok {
				continue
			}
			p := Price{Input: c.Input, Output: c.Output, CacheRead: c.CacheRead, CacheWrite: c.CacheWrite}
			for _, t := range c.Tiers {
				if t.Tier.Type == "context" && t.Tier.Size > 0 {
					p.Tiers = append(p.Tiers, Tier{Above: t.Tier.Size, Input: t.Input, Output: t.Output, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite})
				}
			}
			out[key] = p
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models.dev catalog has no prices")
	}
	return out, nil
}

// priceCache is <tego home>/prices.json: the parsed catalog, a few dozen
// KB instead of the 5+ MB download.
type priceCache struct {
	Fetched int64            `json:"fetched"` // unix ms
	Source  string           `json:"source"`
	Models  map[string]Price `json:"models"`
}

// FetchPrices downloads the models.dev catalog and caches the parsed prices
// in dir for later runs.
func FetchPrices(ctx context.Context, dir string) (map[string]Price, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, CatalogURL, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("models.dev: %s", res.Status)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, time.Time{}, err
	}
	prices, err := ParseCatalog(data)
	if err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now()
	b, err := json.Marshal(priceCache{Fetched: now.UnixMilli(), Source: CatalogURL, Models: prices})
	if err == nil {
		tmp := filepath.Join(dir, ".prices.json.tmp")
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, filepath.Join(dir, "prices.json"))
		}
	}
	return prices, now, nil
}

// CachedPrices returns the prices the last FetchPrices stored in dir, or
// nil when there are none.
func CachedPrices(dir string) (map[string]Price, time.Time) {
	b, err := os.ReadFile(filepath.Join(dir, "prices.json"))
	if err != nil {
		return nil, time.Time{}
	}
	var c priceCache
	if json.Unmarshal(b, &c) != nil || len(c.Models) == 0 {
		return nil, time.Time{}
	}
	return c.Models, time.UnixMilli(c.Fetched)
}
