package feed

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

type prices struct {
	Input      *json.Number `json:"input_price"`
	Output     *json.Number `json:"output_price"`
	CacheRead  *json.Number `json:"cache_read_price"`
	CacheWrite *json.Number `json:"cache_write_price"`
	Cache1h    *json.Number `json:"cache_write_1h_price"`
}

type interval struct {
	prices
	MinTokens int `json:"min_tokens"`
}

type pricing struct {
	prices
	Mode        string                 `json:"billing_mode"`
	Intervals   []interval             `json:"intervals"`
	ImageOutput *json.Number           `json:"image_output_price"`
	Reasoning   map[string]json.Number `json:"reasoning_effort_multipliers"`
}

type plazaModel struct {
	Name        string          `json:"name"`
	Pricing     *pricing        `json:"pricing"`
	TimePricing json.RawMessage `json:"time_pricing"`
}

type plazaGroup struct {
	ID        int64        `json:"id"`
	Rate      json.Number  `json:"rate_multiplier"`
	UserRate  *json.Number `json:"user_rate_multiplier"`
	Exclusive bool         `json:"is_exclusive"`
	Peak      bool         `json:"peak_rate_enabled"`
	Models    []plazaModel `json:"models"`
}

type plazaResponse struct {
	Code *int `json:"code"`
	Data *struct {
		Groups []plazaGroup `json:"groups"`
	} `json:"data"`
}

type Model struct {
	Name       string       `json:"model_name"`
	Group      string       `json:"group_name"`
	Input      json.Number  `json:"input_price"`
	Output     *json.Number `json:"output_price"`
	CacheRead  *json.Number `json:"cache_input_price"`
	CacheWrite *json.Number `json:"cache_create_price"`
	Cache1h    *json.Number `json:"cache_create_price_1h"`
	Enabled    bool         `json:"enabled"`
	Note       string       `json:"note"`
}

type Data struct {
	Currency  string    `json:"currency"`
	PriceUnit string    `json:"price_unit"`
	SiteName  string    `json:"site_name"`
	Domain    string    `json:"site_domain"`
	UpdatedAt time.Time `json:"updated_at"`
	Models    []Model   `json:"models"`
}

type Response struct {
	Version string `json:"schema_version"`
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    *Data  `json:"data"`
}

func convert(c Config, source plazaResponse, now time.Time) (Response, error) {
	if source.Code == nil || *source.Code != 0 || source.Data == nil {
		return Response{}, fmt.Errorf("invalid source envelope")
	}
	groups := make(map[int64]plazaGroup, len(source.Data.Groups))
	for _, g := range source.Data.Groups {
		if _, ok := groups[g.ID]; ok {
			return Response{}, fmt.Errorf("duplicate source group")
		}
		groups[g.ID] = g
	}
	out := Response{Version: "1.1", Success: true, Data: &Data{
		Currency: "CNY", PriceUnit: "per_1m_tokens", SiteName: c.SiteName,
		Domain: c.SiteDomain, UpdatedAt: now.UTC(), Models: make([]Model, 0),
	}}
	for _, q := range c.Quotes {
		g, found := groups[q.GroupID]
		if !found || g.Exclusive || g.UserRate != nil {
			return Response{}, fmt.Errorf("selected public group unavailable")
		}
		// v1 publishes base-context, standard-period token prices. A time-based
		// schedule needs a separate explicit contract rather than a false flat quote.
		if g.Peak {
			return Response{}, fmt.Errorf("time-based group pricing is unsupported")
		}
		rate, err := number(g.Rate)
		if err != nil || rate.Sign() <= 0 {
			return Response{}, fmt.Errorf("invalid source group rate")
		}
		cny, _ := number(c.CNYPerCredit)
		factor := new(big.Rat).Mul(rate, cny)
		factor.Mul(factor, big.NewRat(1000000, 1))
		for _, name := range q.Models {
			if !supportedTextModel(name) {
				return Response{}, fmt.Errorf("unsupported text model family")
			}
			var selected *plazaModel
			for i := range g.Models {
				if g.Models[i].Name == name {
					if selected != nil {
						return Response{}, fmt.Errorf("duplicate source model")
					}
					selected = &g.Models[i]
				}
			}
			if selected == nil || selected.Pricing == nil || selected.Pricing.Mode != "token" {
				return Response{}, fmt.Errorf("selected token price unavailable")
			}
			if selected.Pricing.ImageOutput != nil {
				imagePrice, err := number(*selected.Pricing.ImageOutput)
				if err != nil || imagePrice.Sign() != 0 {
					return Response{}, fmt.Errorf("image output pricing is unsupported")
				}
			}
			for _, multiplier := range selected.Pricing.Reasoning {
				value, err := number(multiplier)
				if err != nil || value.Cmp(big.NewRat(1, 1)) != 0 {
					return Response{}, fmt.Errorf("conditional reasoning pricing is unsupported")
				}
			}
			if raw := strings.TrimSpace(string(selected.TimePricing)); raw != "" && raw != "null" {
				return Response{}, fmt.Errorf("time-based model pricing is unsupported")
			}
			p := selected.Pricing.prices
			note := "基础上下文档价格。"
			if tiers := selected.Pricing.Intervals; len(tiers) > 0 {
				first := tiers[0]
				for _, tier := range tiers[1:] {
					if tier.MinTokens < first.MinTokens {
						first = tier
					}
				}
				if first.MinTokens != 0 {
					return Response{}, fmt.Errorf("base context tier unavailable")
				}
				p = first.prices
				if len(tiers) > 1 {
					note = "基础上下文档价格；长上下文另按模型广场阶梯计费。"
				}
			}
			vals := []*json.Number{p.Input, p.Output, p.CacheRead, p.CacheWrite, p.Cache1h}
			for i, value := range vals {
				vals[i], err = scale(value, factor)
				if err != nil {
					return Response{}, err
				}
			}
			if vals[0] == nil || *vals[0] == "0" {
				return Response{}, fmt.Errorf("positive input price required")
			}
			out.Data.Models = append(out.Data.Models, Model{Name: name, Group: q.GroupName,
				Input: *vals[0], Output: vals[1], CacheRead: vals[2], CacheWrite: vals[3], Cache1h: vals[4],
				Enabled: true, Note: note,
			})
		}
	}
	return out, nil
}

func scale(n *json.Number, factor *big.Rat) (*json.Number, error) {
	if n == nil {
		return nil, nil
	}
	r, err := number(*n)
	if err != nil {
		return nil, fmt.Errorf("invalid source price")
	}
	r.Mul(r, factor)
	s := strings.TrimRight(strings.TrimRight(r.FloatString(12), "0"), ".")
	if s == "0" && r.Sign() > 0 {
		return nil, fmt.Errorf("price below supported precision")
	}
	result := json.Number(s)
	if _, err := number(result); err != nil {
		return nil, fmt.Errorf("converted price out of range")
	}
	return &result, nil
}
