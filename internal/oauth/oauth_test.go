package oauth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func mintIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "none"}) + "." + enc(claims) + ".sig"
}

func TestParseIdentitySeparatesAccountFromWorkspace(t *testing.T) {
	tok := mintIDToken(t, map[string]any{
		"email": "person@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_user_id":    "user_abc",
			"chatgpt_account_id": "acct_workspace_1",
			"chatgpt_plan_type":  "plus",
		},
	})
	id, err := ParseIdentity(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.ChatGPTUserID != "user_abc" {
		t.Fatalf("account identity = %q", id.ChatGPTUserID)
	}
	if id.ChatGPTAccountID != "acct_workspace_1" {
		t.Fatalf("workspace identity = %q", id.ChatGPTAccountID)
	}
	// The two must not be conflated, and neither is the email.
	if id.ChatGPTUserID == id.ChatGPTAccountID {
		t.Fatal("account and workspace identities must stay distinct")
	}
}

func TestParseIdentityUsesProfileEmailFallback(t *testing.T) {
	tok := mintIDToken(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "fallback@example.com"},
		"https://api.openai.com/auth":    map[string]any{"chatgpt_user_id": "u"},
	})
	id, err := ParseIdentity(tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.Email != "fallback@example.com" {
		t.Fatalf("email = %q", id.Email)
	}
}

func TestParseIdentityRejectsTokenWithoutIdentity(t *testing.T) {
	tok := mintIDToken(t, map[string]any{"email": "x@y.z"})
	if _, err := ParseIdentity(tok); err == nil {
		t.Fatal("a token with no account or user id must be rejected, not accepted as anonymous")
	}
}

// TestStartBindsAnAllowlistedPort documents the constraint found in Stage A: only ports
// 1455 and 1457 are in OpenAI's redirect allow-list for this client id.
func TestStartBindsAnAllowlistedPort(t *testing.T) {
	f, err := Start("")
	if err != nil {
		t.Skipf("callback port unavailable in this environment: %v", err)
	}
	defer f.Cancel()
	if f.Port != PrimaryPort && f.Port != FallbackPort {
		t.Fatalf("bound port %d is not in the allow-list", f.Port)
	}
	if got := "http://localhost:" + itoa(f.Port) + "/auth/callback"; !contains(f.AuthURL, "code_challenge_method=S256") {
		t.Fatalf("authorize URL must use PKCE S256: %s (redirect %s)", f.AuthURL, got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
