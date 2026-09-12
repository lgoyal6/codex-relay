package oauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// idClaims mirrors the subset of the id_token Codex itself reads
// (codex-rs/login/src/token_data.rs). We read the same claims so our notion of identity
// matches the client's.
type idClaims struct {
	Email   string `json:"email"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth struct {
		ChatGPTPlanType         string `json:"chatgpt_plan_type"`
		ChatGPTUserID           string `json:"chatgpt_user_id"`
		ChatGPTAccountID        string `json:"chatgpt_account_id"`
		ChatGPTAccountIsFedRAMP bool   `json:"chatgpt_account_is_fedramp"`
	} `json:"https://api.openai.com/auth"`
}

// ParseIdentity reads the identity claims out of an id_token.
//
// The signature is not verified here, matching the client's own behaviour: this token came
// directly from the issuer over TLS in the code exchange we just performed, and it is used
// to label a local row, never to authorize anything. Authorization is the access token's job.
func ParseIdentity(idToken string) (Identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return Identity{}, fmt.Errorf("identity token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Identity{}, fmt.Errorf("identity token payload is unreadable: %w", err)
	}
	var c idClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return Identity{}, fmt.Errorf("identity token payload is unreadable: %w", err)
	}
	email := c.Email
	if email == "" {
		email = c.Profile.Email
	}
	id := Identity{
		Email:            email,
		ChatGPTUserID:    c.Auth.ChatGPTUserID,
		ChatGPTAccountID: c.Auth.ChatGPTAccountID,
		PlanType:         c.Auth.ChatGPTPlanType,
		IsFedRAMP:        c.Auth.ChatGPTAccountIsFedRAMP,
	}
	if id.ChatGPTUserID == "" && id.ChatGPTAccountID == "" {
		return id, fmt.Errorf("identity token carried no ChatGPT account or user id")
	}
	return id, nil
}
