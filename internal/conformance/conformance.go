// Package conformance loads the compiled FHIR conformance index for a release.
//
// The index itself is defined in the model subpackage; this package owns the
// generated data and the embedding of it. Building gogofhir needs neither the
// published packages nor a network: tools/confgen reduces them to the files
// under data/, which are committed.
//
// One release is selected per server instance (the "one version per instance"
// decision), so no code beyond this package carries a version dimension.
package conformance

import (
	"embed"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/langhorst/gogofhir/internal/conformance/model"
)

//go:embed data
var data embed.FS

// Re-exported so callers need only this package.
type (
	Index                = model.Index
	TypeDef              = model.TypeDef
	ElementDef           = model.ElementDef
	TypeRef              = model.TypeRef
	Binding              = model.Binding
	Invariant            = model.Invariant
	SearchParam          = model.SearchParam
	SearchParamComponent = model.SearchParamComponent
	Compartment          = model.Compartment
	ValueSet             = model.ValueSet
	Profile              = model.Profile
	ProfileElement       = model.ProfileElement
	ProfileContext       = model.ProfileContext
	Slicing              = model.Slicing
	Discriminator        = model.Discriminator
	Cursor               = model.Cursor
	Child                = model.Child
	Step                 = model.Step
	Release              = model.Release
)

const (
	R4 = model.R4
	R5 = model.R5
)

// Releases lists every release with a compiled index.
func Releases() []Release { return model.Releases() }

var (
	loadMu sync.Mutex
	loaded = map[Release]*Index{}
)

// Load returns the compiled index for a release. Indexes are read-only once
// built, so repeated calls share one instance rather than re-decoding several
// megabytes of JSON.
func Load(r Release) (*Index, error) {
	if !r.Valid() {
		return nil, fmt.Errorf("conformance: unknown release %q (have %v)", r, Releases())
	}
	loadMu.Lock()
	defer loadMu.Unlock()
	if idx, ok := loaded[r]; ok {
		return idx, nil
	}
	raw, err := data.ReadFile("data/" + string(r) + "/index.json")
	if err != nil {
		return nil, fmt.Errorf("conformance: no compiled index for %s (run `make gen`): %w", r, err)
	}
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, fmt.Errorf("conformance: decoding %s index: %w", r, err)
	}
	loaded[r] = &idx
	return &idx, nil
}

// MustLoad is Load for callers that cannot proceed without an index — the
// server's own startup. A missing or corrupt index is a build problem, not a
// runtime condition to recover from.
func MustLoad(r Release) *Index {
	idx, err := Load(r)
	if err != nil {
		panic(err)
	}
	return idx
}
