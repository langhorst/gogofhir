package rest

import (
	"fmt"

	"net/http"
)

// Building the transaction-response or batch-response bundle.

func (s *Server) writeBundleResponse(w http.ResponseWriter, r *http.Request, bundleType string, entries []*txEntry) {
	built := make([]bundleEntry, 0, len(entries))
	for _, entry := range entries {
		out := bundleEntry{
			status:   statusLine(entry.status),
			location: entry.location,
			etag:     entry.etag,
			lastMod:  entry.lastMod,
		}
		if entry.carry {
			out.content = entry.body
		}
		// A batch never pre-assigns ids, so its entries learn what was written
		// from the Location the interaction returned.
		if reference := entry.reference(); reference != "" {
			out.fullURL = s.base(r) + "/" + reference
		}
		built = append(built, out)
	}
	bundle, err := buildBundle(s.index, bundleType, nil, built, nil, searchOptions{})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "building the %s bundle failed: %v", bundleType, err)
		return
	}
	s.write(w, r, http.StatusOK, bundle, nil)
}

// statusLine renders a status the way Bundle.entry.response.status wants it:
// the code and its reason phrase.
func statusLine(code int) string {
	if code == 0 {
		code = http.StatusOK
	}
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("%d", code)
}
