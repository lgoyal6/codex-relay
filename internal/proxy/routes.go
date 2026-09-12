package proxy

import "strings"

// Class is how a request is allowed to be credentialed. Unknown routes fail closed: we
// never attach a pooled credential to something we have not classified.
type Class int

const (
	// ClassUnknown is anything we have not explicitly classified. Mutations are refused;
	// reads are refused too, because attaching the wrong identity to an unknown account
	// operation is exactly the failure this classification exists to prevent.
	ClassUnknown Class = iota
	// ClassGeneration is a model turn. It uses the selected pooled identity and is subject
	// to conversation ownership.
	ClassGeneration
	// ClassModelCatalog is model eligibility for one identity.
	ClassModelCatalog
	// ClassIdentityScoped is an account-specific read that describes exactly one identity
	// (usage, accounts, settings). It must use that identity's own credential, and its
	// answer describes only that identity.
	ClassIdentityScoped
	// ClassClientOwned belongs to the original client's identity and is passed through
	// untouched: we do not substitute a pooled credential.
	ClassClientOwned
)

func (c Class) String() string {
	switch c {
	case ClassGeneration:
		return "generation"
	case ClassModelCatalog:
		return "model_catalog"
	case ClassIdentityScoped:
		return "identity_scoped"
	case ClassClientOwned:
		return "client_owned"
	default:
		return "unknown"
	}
}

// Route is the classification of one request path.
type Route struct {
	Class Class
	// Why is shown in diagnostics so an unknown route is actionable rather than mysterious.
	Why string
}

// Classify maps a request path to its identity class.
//
// The table is deliberately explicit. Every entry was observed in a live codex-cli 0.154.0
// session against the compatibility harness, or read from the client's own route builders in
// codex-rs/backend-client. Anything not listed is ClassUnknown.
func Classify(path string) Route {
	p := strings.TrimSuffix(path, "/")
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	switch {
	case strings.HasSuffix(p, "/codex/responses"):
		return Route{ClassGeneration, "model turn"}
	case strings.HasSuffix(p, "/codex/models"):
		return Route{ClassModelCatalog, "model eligibility for the selected identity"}

	case strings.HasSuffix(p, "/wham/usage"),
		strings.HasSuffix(p, "/api/codex/usage"):
		return Route{ClassIdentityScoped, "usage describes exactly one identity"}
	case strings.HasSuffix(p, "/wham/accounts/check"),
		strings.HasSuffix(p, "/api/codex/accounts/check"):
		return Route{ClassIdentityScoped, "workspace list for one account"}
	case strings.HasSuffix(p, "/wham/profiles/me"),
		strings.HasSuffix(p, "/api/codex/profiles/me"):
		return Route{ClassIdentityScoped, "profile for one account"}

	// File uploads are deliberately NOT pooled, and this is a correctness decision rather
	// than an omission.
	//
	// Codex builds these from chatgpt_base_url (core/src/mcp_openai_file.rs ->
	// codex-api/src/files.rs), and the request carries ONLY auth headers: files.rs's
	// authorized_request populates the header map from auth.add_auth_headers() alone, so
	// there is no thread-id, session-id or turn metadata on it. Without conversation
	// identity we cannot know which workspace will serve the turn that references the file,
	// so any pooled attribution would be a guess. Guessing wrong uploads the file to one
	// account and then references it from another, which fails at read time inside the
	// model turn where the user cannot act on it.
	//
	// Passing the client's own credential through reproduces exactly what happens without
	// codex-relay: the file belongs to the signed-in client. That is a stated limitation, and
	// it is strictly better than failing closed, which would break MCP file inputs outright
	// for anyone who also points chatgpt_base_url here.
	case p == "/files" || strings.HasSuffix(p, "/backend-api/files"),
		strings.Contains(p, "/files/") && strings.HasSuffix(p, "/uploaded"):
		return Route{ClassClientOwned, "file upload: no conversation identity on the request, so it stays with the signed-in client"}

	case strings.HasSuffix(p, "/wham/settings/user"),
		strings.HasSuffix(p, "/api/codex/settings/user"),
		strings.Contains(p, "/ps/"),
		strings.Contains(p, "/plugins"),
		strings.Contains(p, "/analytics-events"):
		return Route{ClassClientOwned, "belongs to the signed-in client, not to the pool"}
	}
	return Route{ClassUnknown, "unclassified route: no pooled credential is attached"}
}

// IsMutation reports whether a method changes state. Unknown mutations fail closed with a
// clearer message than unknown reads.
func IsMutation(method string) bool {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return false
	}
	return true
}
