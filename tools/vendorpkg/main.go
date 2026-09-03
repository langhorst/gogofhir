// Command vendorpkg fetches the FHIR conformance packages pinned in
// third_party/packages.lock into a destination directory, one subdirectory per
// FHIR version. It is a maintainer tool, not part of the build: the compiled
// indexes under internal/conformance/data are committed, so an ordinary
// `go build` needs neither this tool nor a network.
//
// Two source kinds are supported, because the two packages are published
// differently:
//
//   - "tarball": an npm-style .tgz, verified against a pinned SHA-256 digest.
//   - "git-sparse": a subdirectory of a large repository, fetched at a pinned
//     commit with a blobless sparse checkout so only the wanted files transfer.
//
// Usage:
//
//	go run ./tools/vendorpkg -lock third_party/packages.lock -dest third_party/packages
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// lockFile is the pinned package set. Fields prefixed with "_" are commentary
// for human readers and are ignored here.
type lockFile struct {
	Packages   []pkg       `json:"packages"`
	TestSuites []testSuite `json:"testSuites"`
}

// pkg is one pinned conformance package.
type pkg struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	FHIRVersion string `json:"fhirVersion"`
	License     string `json:"license"`
	Source      string `json:"source"`

	// tarball
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`

	// git-sparse
	Repo   string `json:"repo"`
	Commit string `json:"commit"`

	// Subdir is the directory within the archive or repository whose contents
	// become the package root.
	Subdir string `json:"subdir"`

	// Dir overrides the destination subdirectory, which defaults to
	// FHIRVersion. It exists so a release can vendor more than one package --
	// the core definitions and the terminology that goes with them.
	Dir string `json:"dir,omitempty"`
}

// target returns the subdirectory a package is vendored into.
func (p pkg) target(dest string) string {
	name := p.Dir
	if name == "" {
		name = p.FHIRVersion
	}
	return filepath.Join(dest, name)
}

func main() {
	lockPath := flag.String("lock", "third_party/packages.lock", "path to the package lock file")
	dest := flag.String("dest", "third_party/packages", "directory to vendor packages into")
	testdata := flag.String("testdata", "internal/fhirpath/testdata", "directory to vendor the FHIRPath conformance suites into")
	only := flag.String("only", "", "fetch just this FHIR version (r4, r5); empty means all")
	flag.Parse()

	if err := run(*lockPath, *dest, *testdata, *only); err != nil {
		fmt.Fprintf(os.Stderr, "vendorpkg: %v\n", err)
		os.Exit(1)
	}
}

func run(lockPath, dest, testdata, only string) error {
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	var lock lockFile
	if err := json.Unmarshal(raw, &lock); err != nil {
		return fmt.Errorf("parsing %s: %w", lockPath, err)
	}
	if len(lock.Packages) == 0 {
		return fmt.Errorf("%s lists no packages", lockPath)
	}

	for _, p := range lock.Packages {
		if only != "" && p.FHIRVersion != only {
			continue
		}
		target := p.target(dest)
		if isPopulated(target) {
			fmt.Printf("%s %s: already vendored at %s\n", p.ID, p.Version, target)
			continue
		}
		fmt.Printf("%s %s -> %s\n", p.ID, p.Version, target)
		if err := fetch(p, target); err != nil {
			// Leave nothing half-written: a partial package produces confusing
			// generator failures rather than an obvious "not vendored" error.
			os.RemoveAll(target)
			return fmt.Errorf("%s: %w", p.ID, err)
		}
	}

	suites := lock.TestSuites
	if only != "" {
		suites = nil
		for _, s := range lock.TestSuites {
			if s.Release == only {
				suites = append(suites, s)
			}
		}
	}
	return fetchTestSuites(suites, testdata)
}

// isPopulated reports whether dir already holds a vendored package. Fetching is
// slow enough (tens of MB) that repeating it on every `make vendor` is a real
// annoyance; deleting the directory forces a refetch.
func isPopulated(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

func fetch(p pkg, target string) error {
	switch p.Source {
	case "tarball":
		return fetchTarball(p, target)
	case "git-sparse":
		return fetchGitSparse(p, target)
	default:
		return fmt.Errorf("unknown source kind %q", p.Source)
	}
}

// fetchTarball downloads a .tgz, verifies its digest before unpacking anything,
// and extracts the JSON files under p.Subdir.
func fetchTarball(p pkg, target string) error {
	if p.SHA256 == "" {
		return errors.New("tarball source requires a sha256 pin")
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(p.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", p.URL, resp.Status)
	}

	// Buffer to disk so the digest can be checked before extraction. Verifying
	// a stream we have already unpacked would defeat the point of pinning.
	tmp, err := os.CreateTemp("", "vendorpkg-*.tgz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != p.SHA256 {
		return fmt.Errorf("digest mismatch:\n  want %s\n  got  %s", p.SHA256, got)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return extractJSON(tmp, p.Subdir, target)
}

// extractJSON unpacks the *.json files directly under prefix into target.
// Package assets (images, CSS under other/) are skipped: the generator only
// ever reads conformance resources.
func extractJSON(r io.Reader, prefix, target string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()

	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name, ok := flatJSONName(hdr.Name, prefix)
		if !ok {
			continue
		}
		out, err := os.Create(filepath.Join(target, name))
		if err != nil {
			return err
		}
		// Bound the copy: a hostile archive should not be able to fill the disk
		// through an entry whose header understates its size.
		if _, err := io.CopyN(out, tr, hdr.Size); err != nil && !errors.Is(err, io.EOF) {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no .json files found under %q", prefix)
	}
	fmt.Printf("  extracted %d files\n", count)
	return nil
}

// flatJSONName maps an archive entry to its output filename, or reports false
// if the entry is not a JSON file sitting directly under prefix. Entries are
// rejected rather than sanitized when they contain path separators, so a
// "../.." traversal entry cannot escape the target directory.
func flatJSONName(entry, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(path.Clean(entry), prefix+"/")
	if !ok {
		return "", false
	}
	if strings.Contains(rest, "/") || !strings.HasSuffix(rest, ".json") {
		return "", false
	}
	return rest, true
}

// fetchGitSparse materializes one subdirectory of a repository at a pinned
// commit. A blobless, sparse, depth-1 fetch transfers only the wanted files --
// the source repository is far too large to clone whole for one directory.
func fetchGitSparse(p pkg, target string) error {
	if p.Commit == "" {
		return errors.New("git-sparse source requires a commit pin")
	}
	work, err := os.MkdirTemp("", "vendorpkg-git-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", p.Repo},
		{"sparse-checkout", "init", "--cone"},
		{"sparse-checkout", "set", p.Subdir},
		{"fetch", "--quiet", "--depth", "1", "--filter=blob:none", "origin", p.Commit},
		{"checkout", "--quiet", "FETCH_HEAD"},
	}
	for _, args := range steps {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
		}
	}

	src := filepath.Join(work, filepath.FromSlash(p.Subdir))
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", p.Subdir, err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), filepath.Join(target, e.Name())); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		return fmt.Errorf("no .json files found under %q", p.Subdir)
	}
	fmt.Printf("  copied %d files\n", count)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// ---- FHIRPath conformance suites ----

// testSuite is one pinned HL7 FHIRPath conformance suite.
type testSuite struct {
	ID      string `json:"id"`
	Release string `json:"release"`
	License string `json:"license"`
	// RawBase is a raw-content URL prefix that already includes the pinned
	// commit, so every fetch below is immutable without needing a digest each.
	RawBase string `json:"rawBase"`
	Suite   string `json:"suite"`
	// InputDir is the directory the suite's inputfile names resolve against.
	InputDir string `json:"inputDir"`
}

// fetchTestSuites downloads each suite and the example resources it references.
// The input list is read out of the suite rather than configured: every test
// carries an inputfile attribute, so the suite is its own manifest and cannot
// drift from a list we maintain by hand.
func fetchTestSuites(suites []testSuite, dest string) error {
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, s := range suites {
		dir := filepath.Join(dest, s.Release)
		if isPopulated(dir) {
			fmt.Printf("%s: already vendored at %s\n", s.ID, dir)
			continue
		}
		fmt.Printf("%s -> %s\n", s.ID, dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		body, err := get(client, s.RawBase+s.Suite)
		if err != nil {
			os.RemoveAll(dir)
			return fmt.Errorf("%s: %w", s.ID, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tests.xml"), body, 0o644); err != nil {
			return err
		}
		inputs := referencedInputs(body)
		for _, name := range inputs {
			data, err := get(client, s.RawBase+s.InputDir+"/"+name)
			if err != nil {
				os.RemoveAll(dir)
				return fmt.Errorf("%s input %s: %w", s.ID, name, err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
				return err
			}
		}
		fmt.Printf("  suite + %d input resources\n", len(inputs))
	}
	return nil
}

// referencedInputs returns the distinct inputfile names a suite mentions,
// sorted. It scans attributes directly rather than unmarshalling, because the
// R4 and R5 suites differ in XML namespace and the attribute is all we need.
func referencedInputs(suite []byte) []string {
	var names []string
	seen := map[string]bool{}
	for _, m := range inputFileRE.FindAllSubmatch(suite, -1) {
		name := string(m[1])
		// Reject any name that could escape the destination directory.
		if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

var inputFileRE = regexp.MustCompile(`inputfile="([^"]+)"`)

func get(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
