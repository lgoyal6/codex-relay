package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// maxCatalogBytes bounds how much of a model-catalog response we will buffer in order to
// read it. The observed response is a few kilobytes; anything larger is streamed straight
// through unparsed rather than held in memory.
const maxCatalogBytes = 1 << 20

// modelSlugs extracts the model identifiers from a /codex/models response.
//
// The catalog is the only place a workspace tells us which models it can serve, so without
// reading it the evaluator's model-eligibility rule could never fire on real data. Two
// shapes are accepted because the client itself tolerates both: an object with a "models"
// array, and a bare array.
func modelSlugs(body []byte) []string {
	type entry struct {
		Slug string `json:"slug"`
		ID   string `json:"id"`
	}
	var wrapped struct {
		Models []entry `json:"models"`
	}
	list := []entry(nil)
	if err := json.Unmarshal(body, &wrapped); err == nil && len(wrapped.Models) > 0 {
		list = wrapped.Models
	} else if err := json.Unmarshal(body, &list); err != nil {
		return nil
	}

	seen := map[string]bool{}
	out := make([]string, 0, len(list))
	for _, e := range list {
		slug := e.Slug
		if slug == "" {
			slug = e.ID
		}
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// captureCatalog buffers a successful catalog response so its slugs can be recorded, and
// returns a reader that still yields the complete body to the client. A body we cannot read
// or cannot parse is passed through unchanged and simply records nothing: an unreadable
// catalog is unknown eligibility, never "supports nothing".
func captureCatalog(resp *http.Response) (io.Reader, []string) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.Body, nil
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return io.MultiReader(bytes.NewReader(buf), resp.Body), nil
	}
	if len(buf) > maxCatalogBytes {
		return io.MultiReader(bytes.NewReader(buf), resp.Body), nil
	}
	return bytes.NewReader(buf), modelSlugs(buf)
}
