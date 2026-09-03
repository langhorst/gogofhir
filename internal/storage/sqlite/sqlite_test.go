package sqlite_test

import (
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/sqlite"
	"github.com/langhorst/gogofhir/internal/storage/storagetest"
)

// The storage conformance suite, on SQLite. The same suite runs against
// PostgreSQL next door; an assertion that passes here and fails there is a
// divergence to document or a bug to fix.
func TestConformance(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		t.Helper()
		store, err := sqlite.Open(":memory:", conformance.MustLoad(conformance.R5))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		return store
	})
}
