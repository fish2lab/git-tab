// Package price turns token usage into its equivalent API price, from
// models.dev's catalog: a snapshot built into the binary, or a fresher copy
// `git tab prices update` keeps in the user's cache directory.
package price

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
)

// Source is where `git tab prices update` reads the catalog.
const Source = "https://models.dev/api.json"

//go:embed snapshot.json
var snapshot []byte

// Flat is one price, in USD per million tokens.
type Flat struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite   float64 `json:"cache_write,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
}

// Tier is the price a whole request is billed at once its prompt is over
// Above tokens.
type Tier struct {
	Above int64 `json:"above"`
	Flat
}

type Price struct {
	Flat
	Tiers []Tier `json:"tiers,omitempty"`
}

// At is the price of a request whose prompt is that many tokens: that of
// the highest tier it is over, else the base price.
func (p Price) At(prompt int64) Flat {
	f := p.Flat
	for _, t := range p.Tiers {
		if prompt > t.Above {
			f = t.Flat
		}
	}
	return f
}

// Billed is f as a bill charges it: a part the catalog leaves out costs
// what input does, since a vendor that lists no cache price bills those
// tokens as input.
func (f Flat) Billed() Flat {
	or := func(v, base float64) float64 {
		if v > 0 {
			return v
		}
		return base
	}
	f.CacheRead = or(f.CacheRead, f.Input)
	f.CacheWrite = or(f.CacheWrite, f.Input)
	f.CacheWrite1h = or(f.CacheWrite1h, f.CacheWrite)
	return f
}

// Cost is one request's price. Fast mode is twice the standard price
// (Anthropic's Opus 5 and 5.5).
func (p Price) Cost(u agents.Usage, speed string) float64 {
	f := p.At(u.Prompt()).Billed()
	c := float64(u.Input)*f.Input + float64(u.Output)*f.Output + float64(u.CacheRead)*f.CacheRead +
		float64(u.CacheWrite)*f.CacheWrite + float64(u.CacheWrite1h)*f.CacheWrite1h
	c /= 1e6
	if speed == "fast" {
		c *= 2
	}
	return c
}

// Catalog is prices by provider, then model id.
type Catalog struct {
	Source    string                      `json:"source"`
	Fetched   time.Time                   `json:"fetched"`
	Providers map[string]map[string]Price `json:"providers"`
	From      string                      `json:"-"` // "cache" or "built-in"
}

func CachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	return filepath.Join(dir, "git-tab", "prices.json")
}

// Load is the cached catalog when there is a good one, else the built-in.
func Load() *Catalog {
	if b, err := os.ReadFile(CachePath()); err == nil {
		var c Catalog
		if json.Unmarshal(b, &c) == nil && len(c.Providers) > 0 {
			c.From = "cache"
			return &c
		}
	}
	var c Catalog
	json.Unmarshal(snapshot, &c)
	c.From = "built-in"
	return &c
}

// providerHints name the provider whose own price a model id most likely
// means: its maker. Resellers list the same ids at other prices.
var providerHints = []struct{ prefix, provider string }{
	{"claude", "anthropic"},
	{"gpt", "openai"}, {"o1", "openai"}, {"o3", "openai"}, {"o4", "openai"}, {"codex", "openai"},
	{"gemini", "google"}, {"deepseek", "deepseek"}, {"kimi", "moonshotai"}, {"glm", "zai"}, {"glm", "zhipuai"},
	{"grok", "xai"}, {"qwen", "alibaba"}, {"mistral", "mistral"}, {"devstral", "mistral"}, {"minimax", "minimax"},
	{"mimo", "xiaomi"},
}

// Maker is the provider that makes a model, from its id; "" when unknown.
func Maker(model string) string {
	for _, id := range candidates(model) {
		for _, h := range providerHints {
			if strings.HasPrefix(id, h.prefix) {
				return h.provider
			}
		}
	}
	return ""
}

var (
	dateSuffix = regexp.MustCompile(`-\d{8}$`)
	versionTag = regexp.MustCompile(`-v\d+(:\d+)?$`)
	// DeepSeek serves and bills every V4 Flash name as deepseek-flash
	// (api-docs.deepseek.com/quick_start/pricing).
	deepseekFlash = regexp.MustCompile(`^deepseek-v4[.\d]*-flash$`)
)

// candidates are the ids a model may be listed under: as logged, then
// without a provider prefix, Bedrock/Vertex decorations, a [1m] context tag
// or a date snapshot suffix.
func candidates(model string) []string {
	id := strings.ToLower(strings.TrimSpace(model))
	out := []string{id}
	if i := strings.LastIndexByte(id, '/'); i >= 0 {
		id = id[i+1:]
		out = append(out, id)
	}
	for _, p := range []string{"anthropic.", "us.anthropic.", "eu.anthropic.", "global.anthropic."} {
		id = strings.TrimPrefix(id, p)
	}
	id = strings.TrimSuffix(id, "[1m]")
	if i := strings.IndexByte(id, '@'); i >= 0 {
		id = id[:i]
	}
	id = versionTag.ReplaceAllString(id, "")
	out = append(out, id, dateSuffix.ReplaceAllString(id, ""))
	if deepseekFlash.MatchString(id) {
		out = append(out, "deepseek-flash")
	}
	return out
}

// Lookup finds a model's price and the provider whose listing it is: the
// maker's own, else that of the provider the agent says it called, else
// the first listing found.
func (c *Catalog) Lookup(model, provider string) (Price, string, bool) {
	if c == nil {
		return Price{}, "", false
	}
	for _, id := range candidates(model) {
		for _, h := range providerHints {
			if strings.HasPrefix(id, h.prefix) {
				if p, ok := c.Providers[h.provider][id]; ok {
					return p, h.provider, true
				}
			}
		}
	}
	if provider = strings.ToLower(provider); provider != "" {
		for _, id := range candidates(model) {
			if p, ok := c.Providers[provider][id]; ok {
				return p, provider, true
			}
		}
	}
	names := make([]string, 0, len(c.Providers))
	for n := range c.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, id := range candidates(model) {
		for _, n := range names {
			if p, ok := c.Providers[n][id]; ok {
				return p, n, true
			}
		}
	}
	return Price{}, "", false
}

// Cost prices one request; false when neither the price table nor the
// agent knows the model.
func (c *Catalog) Cost(e agents.Event) (float64, bool) {
	p, from, ok := c.Lookup(e.Model, e.Provider)
	if !ok {
		return e.Reported, e.Reported > 0
	}
	cost := p.Cost(e.Usage, e.Speed)
	if from == "deepseek" && deepseekPeak(e.Time) {
		cost *= 2
	}
	return cost, true
}

// beijing is UTC+8, without daylight saving.
var beijing = time.FixedZone("CST", 8*3600)

// deepseekPeak says whether DeepSeek bills a request at its peak rate:
// weekdays 9:00-12:00 and 14:00-18:00 Beijing time cost twice the idle
// rate, which is the one models.dev lists (api-docs.deepseek.com/zh-cn/
// quick_start/pricing, read 2026-10-26). Public holidays, billed idle, are
// not known here and count as weekdays.
func deepseekPeak(t time.Time) bool {
	b := t.In(beijing)
	if wd := b.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	h := b.Hour()
	return h >= 9 && h < 12 || h >= 14 && h < 18
}

// FromModelsDev converts models.dev's api.json into a Catalog, keeping only
// models with a price.
func FromModelsDev(b []byte) (*Catalog, error) {
	var raw map[string]struct {
		Models map[string]struct {
			Cost *struct {
				Input        *float64 `json:"input"`
				Output       *float64 `json:"output"`
				CacheRead    *float64 `json:"cache_read"`
				CacheWrite   *float64 `json:"cache_write"`
				CacheWrite1h *float64 `json:"cache_write_1h"`
				Tiers        []struct {
					Input        *float64 `json:"input"`
					Output       *float64 `json:"output"`
					CacheRead    *float64 `json:"cache_read"`
					CacheWrite   *float64 `json:"cache_write"`
					CacheWrite1h *float64 `json:"cache_write_1h"`
					Tier         *struct {
						Type string `json:"type"`
						Size int64  `json:"size"`
					} `json:"tier"`
				} `json:"tiers"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	or := func(v *float64, base float64) float64 {
		if v != nil {
			return *v
		}
		return base
	}
	c := &Catalog{Source: Source, Fetched: time.Now().UTC().Truncate(time.Second), Providers: map[string]map[string]Price{}}
	for prov, pv := range raw {
		for id, m := range pv.Models {
			k := m.Cost
			if k == nil || k.Input == nil || k.Output == nil {
				continue
			}
			p := Price{Flat: Flat{Input: *k.Input, Output: *k.Output, CacheRead: or(k.CacheRead, 0), CacheWrite: or(k.CacheWrite, 0), CacheWrite1h: or(k.CacheWrite1h, 0)}}
			for _, t := range k.Tiers {
				if t.Tier == nil || t.Tier.Type != "context" || t.Tier.Size <= 0 {
					continue
				}
				p.Tiers = append(p.Tiers, Tier{Above: t.Tier.Size, Flat: Flat{
					Input: or(t.Input, p.Input), Output: or(t.Output, p.Output), CacheRead: or(t.CacheRead, p.CacheRead),
					CacheWrite: or(t.CacheWrite, p.CacheWrite), CacheWrite1h: or(t.CacheWrite1h, 0)}})
			}
			sort.Slice(p.Tiers, func(i, j int) bool { return p.Tiers[i].Above < p.Tiers[j].Above })
			oneHour(id, &p)
			if c.Providers[prov] == nil {
				c.Providers[prov] = map[string]Price{}
			}
			c.Providers[prov][strings.ToLower(id)] = p
		}
	}
	if len(c.Providers) == 0 {
		return nil, errors.New("no priced models")
	}
	return c, nil
}

// oneHour gives a Claude model its 1-hour cache write price, which
// models.dev leaves out: 2x input, as Anthropic bills it for every model.
func oneHour(id string, p *Price) {
	if !strings.Contains(strings.ToLower(id), "claude") || p.CacheWrite <= 0 {
		return
	}
	if p.CacheWrite1h <= 0 {
		p.CacheWrite1h = 2 * p.Input
	}
	for i := range p.Tiers {
		if p.Tiers[i].CacheWrite1h <= 0 {
			p.Tiers[i].CacheWrite1h = 2 * p.Tiers[i].Input
		}
	}
}

// Update fetches the catalog and writes it to the cache.
func Update(timeout time.Duration) (*Catalog, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(Source)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	c, err := FromModelsDev(b)
	if err != nil {
		return nil, err
	}
	out, _ := json.Marshal(c)
	path := CachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return nil, err
	}
	c.From = "cache"
	return c, os.Rename(tmp, path)
}

// Stale says whether the cached catalog is missing or older than age.
func Stale(age time.Duration) bool {
	info, err := os.Stat(CachePath())
	return err != nil || time.Since(info.ModTime()) > age
}
