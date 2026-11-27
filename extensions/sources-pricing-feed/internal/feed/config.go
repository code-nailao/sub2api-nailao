package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/url"
	"os"
	"strings"
)

type Quote struct {
	GroupID   int64    `json:"group_id"`
	GroupName string   `json:"group_name"`
	Models    []string `json:"models"`
}

type Config struct {
	SourceURL    string      `json:"source_url"`
	SiteName     string      `json:"site_name"`
	SiteDomain   string      `json:"site_domain"`
	CNYPerCredit json.Number `json:"cny_per_credit"`
	Quotes       []Quote     `json:"quotes"`
}

func ReadConfig(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	var c Config
	d := json.NewDecoder(io.LimitReader(f, 256<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid config JSON")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, fmt.Errorf("config must contain a single JSON object")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	u, err := url.Parse(c.SourceURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/api/v1/model-plaza" {
		return fmt.Errorf("source_url must be an HTTPS model-plaza endpoint without credentials, query or fragment")
	}
	if strings.TrimSpace(c.SiteName) == "" || len(c.SiteName) > 160 || strings.TrimSpace(c.SiteDomain) == "" || len(c.SiteDomain) > 253 || strings.ContainsAny(c.SiteDomain, "/?#@ \t\r\n") {
		return fmt.Errorf("site_name and a plain site_domain are required")
	}
	rate, err := number(c.CNYPerCredit)
	if err != nil || rate.Sign() <= 0 {
		return fmt.Errorf("cny_per_credit must be a positive finite number")
	}
	if len(c.Quotes) == 0 || len(c.Quotes) > 100 {
		return fmt.Errorf("quotes must contain between 1 and 100 groups")
	}
	pairs, groups, names := map[string]bool{}, map[int64]bool{}, map[string]bool{}
	for _, q := range c.Quotes {
		if q.GroupID <= 0 || strings.TrimSpace(q.GroupName) != q.GroupName || q.GroupName == "" || len(q.GroupName) > 256 || strings.ContainsAny(q.GroupName, "\r\n\x00") || len(q.Models) == 0 || len(q.Models) > 200 {
			return fmt.Errorf("each quote requires a group ID, stable name and model list")
		}
		if groups[q.GroupID] || names[q.GroupName] {
			return fmt.Errorf("duplicate group ID or public group name")
		}
		groups[q.GroupID], names[q.GroupName] = true, true
		for _, model := range q.Models {
			key := q.GroupName + "\x00" + model
			if model == "" || strings.TrimSpace(model) != model || len(model) > 256 || strings.ContainsAny(model, "*\r\n\x00") || pairs[key] {
				return fmt.Errorf("model IDs must be concrete, non-empty and unique within each group")
			}
			if !supportedTextModel(model) {
				return fmt.Errorf("only GPT, Codex and Claude text models are supported")
			}
			pairs[key] = true
		}
	}
	return nil
}

// The plaza does not expose every provider's implicit schedules or modality
// charges. Keep the first version within the text families whose base prices
// it can represent; adding another provider needs an explicit pricing review.
func supportedTextModel(model string) bool {
	if !strings.HasPrefix(model, "gpt-") && !strings.HasPrefix(model, "claude-") && !strings.HasPrefix(model, "codex-") {
		return false
	}
	for _, modality := range []string{"image", "audio", "realtime", "transcribe", "tts"} {
		if strings.Contains(model, modality) {
			return false
		}
	}
	return true
}

func (c Config) fingerprint() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Bound numeric inputs before using exact rational arithmetic. This also rejects
// non-finite values and pathological exponents without allocating huge integers.
func number(n json.Number) (*big.Rat, error) {
	if len(n) == 0 || len(n) > 64 {
		return nil, fmt.Errorf("invalid decimal")
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1e12 {
		return nil, fmt.Errorf("invalid decimal")
	}
	r, ok := new(big.Rat).SetString(string(n))
	if !ok || r.Sign() < 0 || (f == 0 && r.Sign() != 0) {
		return nil, fmt.Errorf("invalid decimal")
	}
	return r, nil
}
