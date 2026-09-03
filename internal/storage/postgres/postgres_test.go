package postgres_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/storage"
	"github.com/langhorst/gogofhir/internal/storage/postgres"
	"github.com/langhorst/gogofhir/internal/storage/storagetest"
)

// The storage conformance suite, on PostgreSQL.
//
// It needs a server, named by GOGOFHIR_TEST_POSTGRES. Without one the suite
// skips rather than fails: a contributor without PostgreSQL installed should
// still be able to run `make check`, and CI supplies the variable. The skip
// says so out loud, because a silently skipped parity gate is not a gate.
const dsnEnv = "GOGOFHIR_TEST_POSTGRES"

// schemaCounter gives each backend its own schema, so the suite's assumption
// that every Open yields an empty database holds without dropping and
// recreating a database per test.
var schemaCounter atomic.Int64

func TestConformance(t *testing.T) {
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s is not set, so the PostgreSQL half of the storage parity gate did not run", dsnEnv)
	}
	storagetest.Run(t, func(t *testing.T) storage.Backend {
		t.Helper()
		return openInFreshSchema(t, dsn)
	})
}

// openInFreshSchema gives one test its own schema and points the connection's
// search_path at it.
func openInFreshSchema(t *testing.T, dsn string) storage.Backend {
	t.Helper()
	schema := fmt.Sprintf("gogofhir_test_%d", schemaCounter.Add(1))

	admin, err := postgres.Open(dsn, conformance.MustLoad(conformance.R5))
	if err != nil {
		t.Fatalf("connecting to PostgreSQL: %v", err)
	}
	if _, err := admin.DB().Exec("CREATE SCHEMA " + schema); err != nil {
		admin.Close()
		t.Fatalf("creating schema: %v", err)
	}
	admin.Close()

	store, err := postgres.Open(withSearchPath(dsn, schema), conformance.MustLoad(conformance.R5))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
		cleanup, err := postgres.Open(dsn, conformance.MustLoad(conformance.R5))
		if err != nil {
			return
		}
		defer cleanup.Close()
		cleanup.DB().Exec("DROP SCHEMA " + schema + " CASCADE")
	})
	return store
}

func withSearchPath(dsn, schema string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "search_path=" + schema
}

// The dollar-placeholder rewrite has to leave quoted literals alone: the
// ORDER BY subqueries embed a parameter code as a literal, and renumbering
// anything inside one would corrupt the statement.
func TestRebindSkipsLiterals(t *testing.T) {
	_ = context.Background()
	cases := []struct{ in, want string }{
		{"SELECT ? , ?", "SELECT $1 , $2"},
		{"WHERE code = 'a?b' AND x = ?", "WHERE code = 'a?b' AND x = $1"},
		{"WHERE c = 'it''s ? here' AND x = ?", "WHERE c = 'it''s ? here' AND x = $1"},
		{`LIKE ? ESCAPE '\'`, `LIKE $1 ESCAPE '\'`},
		{"no placeholders", "no placeholders"},
	}
	for _, tc := range cases {
		if got := postgres.Rebind(tc.in); got != tc.want {
			t.Errorf("Rebind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
