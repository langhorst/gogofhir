package sqlstore

import (
	"context"
	"strings"
	"time"

	"github.com/langhorst/gogofhir/internal/storage"
)

// _include and _revinclude.
//
// These run after the match query rather than as part of it, and that is the
// point: an include adds context to a result set without changing which
// resources matched. Folding them into the main query would multiply rows and
// blur the distinction the bundle has to preserve -- entry.search.mode tells a
// client which resources answered its question and which came along for the
// ride.

// maxIncludeRounds bounds :iterate. Reference graphs in FHIR data contain
// cycles (an Encounter's partOf, a Patient's link), and following them without
// a bound is a request that never finishes.
const maxIncludeRounds = 5

// Include resolves include specifications against a set of matches.
func (s *Store) Include(ctx context.Context, seeds []*storage.Resource, specs []storage.IncludeSpec) ([]*storage.Resource, error) {
	if len(seeds) == 0 || len(specs) == 0 {
		return nil, nil
	}

	// Resources already in the bundle are never added again, whichever
	// direction they were reached from.
	seen := map[string]bool{}
	for _, res := range seeds {
		seen[res.Type+"/"+res.ID] = true
	}

	var out []*storage.Resource
	frontier := seeds
	for round := 0; round < maxIncludeRounds && len(frontier) > 0; round++ {
		var found []*storage.Resource
		for _, spec := range specs {
			// A non-iterating include applies to the matches only, so after the
			// first round only the iterating ones keep going.
			if round > 0 && !spec.Iterate {
				continue
			}
			batch, err := s.expand(ctx, frontier, spec)
			if err != nil {
				return nil, err
			}
			for _, res := range batch {
				key := res.Type + "/" + res.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				found = append(found, res)
			}
		}
		if len(found) == 0 {
			break
		}
		out = append(out, found...)
		frontier = found
	}
	return out, nil
}

// expand resolves one specification against one set of resources.
func (s *Store) expand(ctx context.Context, from []*storage.Resource, spec storage.IncludeSpec) ([]*storage.Resource, error) {
	if spec.Reverse {
		return s.expandReverse(ctx, from, spec)
	}
	return s.expandForward(ctx, from, spec)
}

// expandForward follows references out of the given resources.
func (s *Store) expandForward(ctx context.Context, from []*storage.Resource, spec storage.IncludeSpec) ([]*storage.Resource, error) {
	pids, err := s.pidsFor(ctx, from, spec.SourceType)
	if err != nil || len(pids) == 0 {
		return nil, err
	}

	conditions := []string{"ref.pid IN (" + placeholders(len(pids)) + ")", "t.deleted = FALSE"}
	args := append([]any{}, pids...)
	if !spec.Wildcard {
		conditions = append(conditions, "ref.code = ?")
		args = append(args, spec.Code)
	}
	if spec.TargetType != "" {
		conditions = append(conditions, "ref.target_type = ?")
		args = append(args, spec.TargetType)
	}

	query := "SELECT DISTINCT t.resource_type, t.fhir_id, t.version_id, t.last_updated, t.content" +
		" FROM idx_reference ref" +
		" JOIN resource t ON t.resource_type = ref.target_type AND t.fhir_id = ref.target_id" +
		" WHERE " + strings.Join(conditions, " AND ")
	return s.queryResources(ctx, query, args)
}

// expandReverse finds resources pointing at the given ones.
func (s *Store) expandReverse(ctx context.Context, from []*storage.Resource, spec storage.IncludeSpec) ([]*storage.Resource, error) {
	if len(from) == 0 {
		return nil, nil
	}
	// The reference must match both type and id, so a Patient/1 target is not
	// satisfied by a Group/1 reference.
	var targets []string
	var args []any
	for _, res := range from {
		if spec.TargetType != "" && res.Type != spec.TargetType {
			continue
		}
		targets = append(targets, "(ref.target_type = ? AND ref.target_id = ?)")
		args = append(args, res.Type, res.ID)
	}
	if len(targets) == 0 {
		return nil, nil
	}

	conditions := []string{
		"(" + strings.Join(targets, " OR ") + ")",
		"src.resource_type = ?",
		"src.deleted = FALSE",
	}
	args = append(args, spec.SourceType)
	if !spec.Wildcard {
		conditions = append(conditions, "ref.code = ?")
		args = append(args, spec.Code)
	}

	query := "SELECT DISTINCT src.resource_type, src.fhir_id, src.version_id, src.last_updated, src.content" +
		" FROM idx_reference ref JOIN resource src ON src.pid = ref.pid" +
		" WHERE " + strings.Join(conditions, " AND ")
	return s.queryResources(ctx, query, args)
}

// pidsFor resolves resources to their surrogate keys, optionally restricted to
// one type.
func (s *Store) pidsFor(ctx context.Context, resources []*storage.Resource, onlyType string) ([]any, error) {
	var conditions []string
	var args []any
	for _, res := range resources {
		if onlyType != "" && res.Type != onlyType {
			continue
		}
		conditions = append(conditions, "(resource_type = ? AND fhir_id = ?)")
		args = append(args, res.Type, res.ID)
	}
	if len(conditions) == 0 {
		return nil, nil
	}
	rows, err := s.query(ctx,
		"SELECT pid FROM resource WHERE "+strings.Join(conditions, " OR "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pids []any
	for rows.Next() {
		var pid int64
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	return pids, rows.Err()
}

func (s *Store) queryResources(ctx context.Context, query string, args []any) ([]*storage.Resource, error) {
	rows, err := s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*storage.Resource
	for rows.Next() {
		var (
			res    storage.Resource
			micros int64
		)
		if err := rows.Scan(&res.Type, &res.ID, &res.VersionID, &micros, &res.Content); err != nil {
			return nil, err
		}
		res.LastUpdated = time.UnixMicro(micros).UTC()
		out = append(out, &res)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n == 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
