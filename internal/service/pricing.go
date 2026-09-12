package service

import (
	"context"
	"strconv"
)

// Pricing is the assumption behind the API-equivalent cost estimate.
//
// This is NOT what a ChatGPT subscription charges. A subscription is a flat fee; these rates
// are what the same tokens would cost on the pay-as-you-go API. The estimate exists to answer
// "roughly how much work is this doing", and every surface that shows it has to say so.
//
// The rates are stored as settings rather than compiled in, because published prices change
// and a hardcoded constant presented as exact capacity is precisely the kind of invented
// number this tool avoids. The defaults are a stated starting point, not a quoted price.
type Pricing struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	CachedPerMTok float64 `json:"cached_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
	Currency      string  `json:"currency"`
	// UserSet is false while the defaults are in use, so the UI can say the figure rests on
	// an assumption the user has not confirmed.
	UserSet bool `json:"user_set"`
}

// DefaultPricing is a neutral starting point. It is deliberately round rather than a quoted
// price list, so nobody mistakes it for an authoritative rate card.
func DefaultPricing() Pricing {
	return Pricing{InputPerMTok: 1.25, CachedPerMTok: 0.125, OutputPerMTok: 10, Currency: "USD"}
}

const (
	keyInputRate  = "pricing_input_per_mtok"
	keyCachedRate = "pricing_cached_per_mtok"
	keyOutputRate = "pricing_output_per_mtok"
)

func (s *Service) Pricing(ctx context.Context) Pricing {
	p := DefaultPricing()
	get := func(key string, into *float64) bool {
		v, err := s.GetSetting(ctx, key)
		if err != nil || v == "" {
			return false
		}
		f, cerr := strconv.ParseFloat(v, 64)
		if cerr != nil || f < 0 {
			return false
		}
		*into = f
		return true
	}
	a := get(keyInputRate, &p.InputPerMTok)
	b := get(keyCachedRate, &p.CachedPerMTok)
	c := get(keyOutputRate, &p.OutputPerMTok)
	p.UserSet = a || b || c
	return p
}

func (s *Service) SetPricing(ctx context.Context, p Pricing) error {
	for key, v := range map[string]float64{
		keyInputRate:  p.InputPerMTok,
		keyCachedRate: p.CachedPerMTok,
		keyOutputRate: p.OutputPerMTok,
	} {
		if err := s.SetSetting(ctx, key, strconv.FormatFloat(v, 'f', -1, 64)); err != nil {
			return err
		}
	}
	return nil
}

// EstimateCost converts token counts into an API-equivalent figure at the configured rates.
// Cached input is charged at the cached rate, which is why it is tracked separately.
func (p Pricing) EstimateCost(input, cached, output int64) float64 {
	fresh := input - cached
	if fresh < 0 {
		fresh = 0
	}
	const perM = 1_000_000.0
	return float64(fresh)/perM*p.InputPerMTok +
		float64(cached)/perM*p.CachedPerMTok +
		float64(output)/perM*p.OutputPerMTok
}
