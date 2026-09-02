// Command gogofhir is the FHIR server daemon.
//
// The RESTful surface arrives at milestone M2; today the binary exists to
// prove the foundation end to end — that a release's conformance index is
// embedded, loadable, and complete — and to give `make build` something to
// build.
//
// Usage:
//
//	gogofhir version
//	gogofhir conformance [-fhir r5]
//	gogofhir serve       [-fhir r5]   (not yet implemented)
package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/langhorst/gogofhir/internal/conformance"
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
		fmt.Fprintln(os.Stderr, "serve: not implemented yet — the REST layer lands in M2")
		code = 2
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (expected: version, conformance, serve)\n", cmd)
		code = 2
	}
	os.Exit(code)
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
