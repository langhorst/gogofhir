package rest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/langhorst/gogofhir/internal/storage"
	"net/http"
)

// Resolving what each entry acts on.
//
// Identities are settled before anything is written: server-assigned ids are
// chosen here, and conditional creates, updates and deletes are evaluated
// here, so that by execution time every entry is a plain instance-level
// interaction and every reference between entries has something to point at.

// resolveTargets settles which resource each entry acts on before anything is
// written.
//
// It has to happen first because references between entries are resolved
// against those identities: an entry cannot point at a resource whose id is
// still going to be chosen. Server-assigned ids are therefore chosen here, and
// conditional criteria are evaluated here, which turns every entry into a plain
// instance-level interaction.
func (s *Server) resolveTargets(ctx context.Context, entries []*txEntry) error {
	for _, entry := range entries {
		segments := strings.Split(entry.path, "/")

		switch entry.method {
		case http.MethodGet, http.MethodHead:
			continue // reads act on whatever the URL names

		case http.MethodPost:
			if len(segments) != 1 || !s.index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d posts to %q, which is not a resource type", entry.position, entry.path)}
			}
			// A conditional create asks the server not to create a second copy.
			// Resolving it now means a reference to this entry points at
			// whichever resource ends up being the one.
			if entry.ifNoneExist != "" {
				existing, err := s.matchOne(ctx, entry.resourceType, entry.ifNoneExist)
				switch {
				case err == nil:
					entry.id, entry.done = existing.ID, true
					entry.status = http.StatusOK
					entry.location = fmt.Sprintf("%s/%s/_history/%d",
						existing.Type, existing.ID, existing.VersionID)
					entry.etag = etagFor(existing.VersionID)
					entry.lastMod = httpDate(existing.LastUpdated)
					continue
				case errors.Is(err, storage.ErrNotFound):
					// Nothing matched, so the create proceeds.
				default:
					return entryError(entry, err)
				}
			}
			entry.id = newID()
			entry.node.SetID(entry.id)

		case http.MethodPut:
			if !s.index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d targets %q, which is not a resource type", entry.position, entry.path)}
			}
			switch {
			case len(segments) == 2:
				entry.id = segments[1]
			case entry.query != "":
				// A conditional update resolves to the single match, or creates
				// at a new id when nothing matches.
				existing, err := s.matchOne(ctx, entry.resourceType, entry.query)
				switch {
				case err == nil:
					entry.id = existing.ID
				case errors.Is(err, storage.ErrNotFound):
					entry.id = firstNonEmpty(entry.node.ID(), newID())
				default:
					return entryError(entry, err)
				}
			default:
				return &searchError{fmt.Sprintf(
					"entry %d is a PUT to %q with no id and no criteria", entry.position, entry.path)}
			}
			// The URL's id is authoritative, and a conditional update has just
			// acquired one; the body follows it.
			entry.node.SetID(entry.id)

		case http.MethodDelete:
			if !s.index.IsResource(entry.resourceType) {
				return &searchError{fmt.Sprintf(
					"entry %d targets %q, which is not a resource type", entry.position, entry.path)}
			}
			switch {
			case len(segments) == 2:
				entry.id = segments[1]
			case entry.query != "":
				existing, err := s.matchOne(ctx, entry.resourceType, entry.query)
				switch {
				case err == nil:
					entry.id = existing.ID
				case errors.Is(err, storage.ErrNotFound):
					// Delete is idempotent, so nothing to delete is a success.
					entry.done, entry.status = true, http.StatusNoContent
				default:
					return entryError(entry, err)
				}
			default:
				return &searchError{fmt.Sprintf(
					"entry %d is a DELETE to %q with no id and no criteria", entry.position, entry.path)}
			}
		}
	}
	return nil
}

// entryError wraps a resolution failure so the message names the entry.
func entryError(entry *txEntry, err error) error {
	if errors.Is(err, storage.ErrMultipleMatches) {
		return &searchError{fmt.Sprintf(
			"entry %d: the criteria %q matched more than one resource, so the server cannot tell which one you meant",
			entry.position, entry.criteria())}
	}
	var se *searchError
	if errors.As(err, &se) {
		return &searchError{fmt.Sprintf("entry %d: %s", entry.position, se.Error())}
	}
	return err
}

func (entry *txEntry) criteria() string {
	if entry.ifNoneExist != "" {
		return entry.ifNoneExist
	}
	return entry.query
}

// checkDuplicates rejects a transaction that acts on the same resource twice.
//
// Two entries writing one resource have no defined order and no defined
// outcome, so the specification makes it an error rather than letting the
// server pick a winner.
func checkDuplicates(entries []*txEntry) error {
	seen := map[string]int{}
	for _, entry := range entries {
		if entry.id == "" || entry.method == http.MethodGet || entry.method == http.MethodHead {
			continue
		}
		key := entry.resourceType + "/" + entry.id
		if first, ok := seen[key]; ok {
			return &searchError{fmt.Sprintf(
				"entries %d and %d both act on %s; a transaction may touch a resource only once",
				first, entry.position, key)}
		}
		seen[key] = entry.position
	}

	urls := map[string]int{}
	for _, entry := range entries {
		if entry.fullURL == "" {
			continue
		}
		if first, ok := urls[entry.fullURL]; ok {
			return &searchError{fmt.Sprintf(
				"entries %d and %d share the fullUrl %s, so a reference to it is ambiguous",
				first, entry.position, entry.fullURL)}
		}
		urls[entry.fullURL] = entry.position
	}
	return nil
}

// resolveReferences rewrites the references between entries.
//
// An entry that creates a Patient and an entry that creates an Observation of
// that patient are posted together precisely because neither exists yet: the
// Observation names the Patient by the placeholder in its fullUrl, and the
// server substitutes the id it assigned. Conditional references -- a reference
// written as search criteria -- are resolved here too, against the transaction
// so that a resource created by an earlier entry can satisfy one.
func (s *Server) resolveReferences(ctx context.Context, entries []*txEntry) error {
	targets := map[string]string{}
	for _, entry := range entries {
		if entry.fullURL == "" || entry.id == "" {
			continue
		}
		if entry.method != http.MethodPost && entry.method != http.MethodPut {
			continue
		}
		targets[entry.fullURL] = entry.resourceType + "/" + entry.id
	}

	var failure error
	for _, entry := range entries {
		if entry.node == nil {
			continue
		}
		entry.node.RewriteReferences(func(reference string) (string, bool) {
			if failure != nil {
				return "", false
			}
			if target, ok := targets[reference]; ok {
				return target, true
			}
			// A conditional reference is search criteria in place of an id.
			resourceType, criteria, ok := strings.Cut(reference, "?")
			if !ok || criteria == "" || !s.index.IsResource(resourceType) {
				return "", false
			}
			existing, err := s.matchOne(ctx, resourceType, criteria)
			switch {
			case err == nil:
				return existing.Type + "/" + existing.ID, true
			case errors.Is(err, storage.ErrNotFound):
				failure = &searchError{fmt.Sprintf(
					"entry %d refers to %q, which matched no resource", entry.position, reference)}
			case errors.Is(err, storage.ErrMultipleMatches):
				failure = &searchError{fmt.Sprintf(
					"entry %d refers to %q, which matched more than one resource",
					entry.position, reference)}
			default:
				failure = err
			}
			return "", false
		})
		if failure != nil {
			return failure
		}
	}

	// A placeholder that survives rewriting would be stored as a dangling
	// reference, which is a data error the client cannot see. Catch it here.
	for _, entry := range entries {
		if entry.node == nil {
			continue
		}
		for _, reference := range entry.node.References() {
			if strings.HasPrefix(reference, "urn:uuid:") || strings.HasPrefix(reference, "urn:oid:") {
				return &searchError{fmt.Sprintf(
					"entry %d refers to %s, which no entry in the bundle provides",
					entry.position, reference)}
			}
		}
	}
	return nil
}
