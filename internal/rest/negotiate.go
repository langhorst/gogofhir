package rest

import (
	"net/http"
	"strings"
)

// Content negotiation.
//
// FHIR names its own media types -- application/fhir+json and
// application/fhir+xml -- and also accepts the bare application/json and
// application/xml, plus a "_format" query parameter for clients that cannot set
// headers. All three routes reach the same decision here so no handler has to
// think about it.

type format int

const (
	formatJSON format = iota
	formatXML
)

const (
	mediaJSON = "application/fhir+json"
	mediaXML  = "application/fhir+xml"
)

func (f format) mediaType() string {
	if f == formatXML {
		return mediaXML
	}
	return mediaJSON
}

// responseFormat decides how to serialize a response. JSON is the default: the
// specification lets a server choose, and every client speaks it.
func responseFormat(r *http.Request) format {
	if q := r.URL.Query().Get("_format"); q != "" {
		if f, ok := formatFor(q); ok {
			return f
		}
	}
	// Accept may list several types with weights. Rather than implement full
	// q-value negotiation, take the first entry that names something we can
	// produce -- clients that care put their preference first, and the fallback
	// is a format every client accepts.
	for _, entry := range strings.Split(r.Header.Get("Accept"), ",") {
		media := strings.TrimSpace(strings.SplitN(entry, ";", 2)[0])
		if f, ok := formatFor(media); ok {
			return f
		}
	}
	return formatJSON
}

// requestFormat reports how a request body is encoded, and whether the type is
// one we can read at all.
func requestFormat(r *http.Request) (format, bool) {
	media := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if media == "" {
		// An absent Content-Type is taken as JSON rather than refused: it is a
		// common client omission and the body is self-describing enough.
		return formatJSON, true
	}
	return formatFor(media)
}

func formatFor(media string) (format, bool) {
	switch strings.ToLower(media) {
	case mediaJSON, "application/json", "json", "text/json":
		return formatJSON, true
	case mediaXML, "application/xml", "xml", "text/xml":
		return formatXML, true
	case "*/*", "application/*":
		return formatJSON, true
	}
	return formatJSON, false
}
