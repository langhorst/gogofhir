// Command gogofhir is the FHIR server daemon.
//
// The RESTful surface arrives at milestone M2; today the binary exists to
// prove the foundation end to end — that a release's conformance index is
// embedded, loadable, and complete — and to give `make build` something to
// build.
//
// Usage:
//
//	gogofhir serve       [-fhir r5] [-addr :8080] [-db fhir.db | postgres://...]
//	                     [-validate-writes] [-strict-terminology]
//	                     [-smart -base-url https://host]
//	gogofhir conformance [-fhir r5]
//	gogofhir version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/langhorst/gogofhir/internal/conformance"
	"github.com/langhorst/gogofhir/internal/rest"
	"github.com/langhorst/gogofhir/internal/smart"
	"github.com/langhorst/gogofhir/internal/storage/postgres"
	"github.com/langhorst/gogofhir/internal/storage/sqlite"
	"github.com/langhorst/gogofhir/internal/storage/sqlstore"
)

// version is stamped at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "version"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}

	var code int
	switch cmd {
	case "version":
		fmt.Printf("gogofhir %s\n", version)
	case "conformance":
		code = runConformance(args)
	case "serve":
		code = runServe(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (expected: version, conformance, serve)\n", cmd)
		code = 2
	}
	os.Exit(code)
}

// runServe starts the FHIR server.
func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	release := fs.String("fhir", string(conformance.R5), "FHIR release to serve (r4, r5)")
	addr := fs.String("addr", ":8080", "address to listen on")
	dbPath := fs.String("db", "gogofhir.db",
		`SQLite database path, ":memory:" for an ephemeral one, or a postgres:// URL`)
	baseURL := fs.String("base-url", "", "external base URL, when behind a proxy")
	strictTerminology := fs.Bool("strict-terminology", false,
		"treat a binding this server cannot check offline as an error rather than a warning")
	validateWrites := fs.Bool("validate-writes", false,
		"reject a create or update whose resource has validation errors")
	smartAuth := fs.Bool("smart", false,
		"require SMART App Launch access tokens and enforce their scopes")
	smartClient := fs.String("smart-client", "gogofhir-test-client",
		"client id registered with the built-in authorization server")
	smartSecret := fs.String("smart-secret", "",
		"client secret; empty registers a public client, which must use PKCE")
	smartRedirect := fs.String("smart-redirect", "http://localhost:4567/inferno/redirect",
		"comma-separated redirect URIs the client may use")
	smartPatient := fs.String("smart-patient", "",
		"patient id handed to patient-scoped tokens as launch context")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	rest.SetSoftwareVersion(version)

	idx, err := conformance.Load(conformance.Release(*release))
	if err != nil {
		log.Error("loading conformance index", "error", err)
		return 1
	}
	store, err := openStore(*dbPath, idx)
	if err != nil {
		log.Error("opening database", "db", *dbPath, "error", err)
		return 1
	}
	defer store.Close()

	server := &rest.Server{
		Index: idx, Store: store, BaseURL: *baseURL, Log: log,
		StrictTerminology: *strictTerminology,
		ValidateWrites:    *validateWrites,
	}
	if *smartAuth {
		issuer := *baseURL
		if issuer == "" {
			// The issuer has to be an absolute URL a client can reach, and
			// without -base-url there is nothing to derive one from.
			log.Error("-smart needs -base-url, since tokens and discovery are written against it")
			return 1
		}
		keys, err := smart.NewKeys()
		if err != nil {
			log.Error("generating the signing key", "error", err)
			return 1
		}
		server.SMART = smart.New(smart.Config{
			Issuer: strings.TrimSuffix(issuer, "/"),
			Keys:   keys,
			Clients: map[string]smart.Client{*smartClient: {
				ID:           *smartClient,
				Secret:       *smartSecret,
				RedirectURIs: strings.Split(*smartRedirect, ","),
			}},
			Patient: *smartPatient,
		})
		log.Info("SMART App Launch enabled",
			"client", *smartClient, "confidential", *smartSecret != "",
			"launchPatient", *smartPatient)
	}
	httpServer := &http.Server{
		Addr:    *addr,
		Handler: server.Handler(),
		// A FHIR request is a document exchange, not a stream; these bounds
		// keep a stalled client from holding a connection indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("gogofhir listening",
			"addr", *addr, "release", idx.Release, "fhirVersion", idx.FHIRVersion,
			"resourceTypes", len(idx.ResourceTypes()), "engine", store.Engine())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "error", err)
		return 1
	}
	return 0
}

// openStore picks a backend from the -db value.
//
// A connection URL means PostgreSQL and anything else is a SQLite path, which
// keeps the common case -- a file, or nothing at all -- a single flag with no
// ceremony.
func openStore(db string, idx *conformance.Index) (*sqlstore.Store, error) {
	switch {
	case strings.HasPrefix(db, "postgres://"), strings.HasPrefix(db, "postgresql://"):
		return postgres.Open(db, idx)
	default:
		return sqlite.Open(db, idx)
	}
}

// runConformance summarizes the embedded index for a release. It is the
// smoke test that the generated data survived embedding: if this reports
// plausible counts, confgen, go:embed, and the loader all agree.
func runConformance(args []string) int {
	fs := flag.NewFlagSet("conformance", flag.ExitOnError)
	release := fs.String("fhir", string(conformance.R5), "FHIR release to inspect (r4, r5)")
	_ = fs.Parse(args)

	idx, err := conformance.Load(conformance.Release(*release))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}

	var datatypes, params, invariants int
	for _, t := range idx.Types {
		if t.Kind != "resource" {
			datatypes++
		}
		invariants += len(t.Invariants)
	}
	for _, ps := range idx.SearchParams {
		params += len(ps)
	}

	fmt.Printf("release        %s (%s)\n", idx.Release, idx.FHIRVersion)
	fmt.Printf("package        %s\n", idx.PackageID)
	fmt.Printf("resources      %d\n", len(idx.ResourceTypes()))
	fmt.Printf("datatypes      %d\n", datatypes)
	fmt.Printf("search params  %d bindings\n", params)
	fmt.Printf("invariants     %d\n", invariants)
	fmt.Printf("compartments   %s\n", strings.Join(compartmentCodes(idx), ", "))
	return 0
}

func compartmentCodes(idx *conformance.Index) []string {
	codes := make([]string, 0, len(idx.Compartments))
	for code := range idx.Compartments {
		codes = append(codes, code)
	}
	// Map iteration order is random; a summary that reshuffles between runs is
	// needlessly hard to diff.
	slices.Sort(codes)
	return codes
}
