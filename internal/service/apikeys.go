package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lgoyal6/codex-relay/internal/store"
)

// KeyHeader is where a client presents a relay key.
//
// Deliberately not Authorization: Codex sends its own ChatGPT token in that header and the
// proxy replaces it, so reusing it would make the two indistinguishable. Codex can still send
// this one, via env_http_headers in its model_providers entry.
const KeyHeader = "X-Codex-Relay-Key"

// RequireKeySetting turns the gate on. Off by default: switching it on silently would break
// every client the moment a user upgraded.
const RequireKeySetting = "require_api_key"

// ErrKeyRequired means the proxy is locked and the caller presented nothing usable.
var ErrKeyRequired = errors.New("this relay requires an API key")

// ErrKeyOverLimit means the key is real but has spent its daily allowance.
var ErrKeyOverLimit = errors.New("this API key has reached its daily limit")

// CreateAPIKey mints a key. The secret is returned once and never again.
func (s *Service) CreateAPIKey(ctx context.Context, name string, dailyLimit *int64) (store.APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.APIKey{}, "", fmt.Errorf("a key needs a name so it can be told apart later")
	}
	if dailyLimit != nil && *dailyLimit <= 0 {
		return store.APIKey{}, "", fmt.Errorf("a daily limit must be positive, or absent for no limit")
	}
	return s.DB.CreateAPIKey(ctx, uuid.NewString(), name, dailyLimit, s.Clock.Now())
}

func (s *Service) ListAPIKeys(ctx context.Context) ([]store.APIKey, error) {
	return s.DB.ListAPIKeys(ctx)
}

func (s *Service) RevokeAPIKey(ctx context.Context, id string) error {
	return s.DB.RevokeAPIKey(ctx, id, s.Clock.Now())
}

// RequireAPIKey reports whether the proxy is locked.
func (s *Service) RequireAPIKey(ctx context.Context) bool {
	v, _ := s.GetSetting(ctx, RequireKeySetting)
	return v == "true"
}

// AuthenticateRequest resolves the caller.
//
// When the gate is off this returns an empty id and no error, so an unlocked relay behaves
// exactly as before. When it is on, a caller must present a live key that is within its
// daily allowance.
//
// A presented key is honoured even when the gate is off, so usage is attributed to it and a
// user can see which client spent what before deciding to lock anything down.
func (s *Service) Authenticate(ctx context.Context, r *http.Request) (keyID string, err error) {
	presented := strings.TrimSpace(r.Header.Get(KeyHeader))
	required := s.RequireAPIKey(ctx)

	if presented == "" {
		if required {
			return "", ErrKeyRequired
		}
		return "", nil
	}

	key, err := s.DB.LookupAPIKey(ctx, presented)
	if err != nil {
		if required {
			return "", ErrKeyRequired
		}
		// Unlocked: an unrecognised key is not a reason to refuse a turn, but it is also not
		// attributable to anything, so it is recorded as anonymous.
		return "", nil
	}

	if key.DailyLimit != nil {
		since := s.Clock.Now().Add(-24 * time.Hour)
		used, cerr := s.DB.CountKeyUsageSince(ctx, key.ID, since)
		if cerr == nil && used >= *key.DailyLimit {
			return "", ErrKeyOverLimit
		}
	}
	s.DB.TouchAPIKey(ctx, key.ID, s.Clock.Now())
	return key.ID, nil
}
