package gateway

import (
	"net/http"
	"net/url"
	"regexp"
)

// requirement describes what a request needs in order to be allowed.
type requirement int

const (
	reqUnknown requirement = iota
	reqBlobRead
	reqBlobWrite
	// reqBlobReadOrWrite is used for HEAD on a blob, which is part of both the
	// pull (does this blob exist to download) and push (can I skip re-uploading
	// this blob) flows.
	reqBlobReadOrWrite
	reqManifestRead
	reqManifestWrite
	// reqManifestReadOrWrite is used for HEAD on a manifest, which shows up in
	// both read and write flows.
	reqManifestReadOrWrite
)

var (
	// The repository name follows the OCI Distribution Spec grammar.
	nameGrammar = `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*`

	blobUploadRe = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/uploads/?.*$`)
	blobRe       = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/([^/]+)$`)
	manifestRe   = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/manifests/(.+)$`)
	tagsRe       = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/tags/list$`)
	referrersRe  = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/referrers/(.+)$`)
)

// request describes a classified registry request.
type request struct {
	repo string      // repository name (path component of /v2/<name>/...)
	req  requirement // what policy dimension gates this request
	kind string      // human-readable operation kind for logging/errors
	// write reports whether the operation mutates the registry. It selects the
	// pull vs push token scope used when authenticating upstream.
	write bool
	// mountFrom is the source repository of a cross-repo blob mount
	// (POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<mountFrom>). It is
	// empty for every other request. When set, the source repository needs read
	// access in addition to write access on the destination, so a client cannot
	// pull a blob it may not read into a repository it can write.
	mountFrom string
	// malformedQuery reports that the upload query could not be parsed
	// unambiguously (a ';' the gateway drops but the upstream might honor, or
	// duplicate mount/from values). Such a request must be rejected rather than
	// forwarded: the gateway could otherwise authorize a different mount source
	// than the one the upstream acts on.
	malformedQuery bool
}

// classify inspects the request path and method and reports the repository, the
// policy requirement, and whether it is a write. ok is false for paths the
// gateway does not understand.
func classify(r *http.Request) (request, bool) {
	path := r.URL.Path
	method := r.Method

	if m := tagsRe.FindStringSubmatch(path); m != nil {
		return request{repo: m[1], req: reqManifestRead, kind: "tag listing"}, true
	}
	if m := referrersRe.FindStringSubmatch(path); m != nil {
		return request{repo: m[1], req: reqManifestRead, kind: "referrers query"}, true
	}
	if m := blobUploadRe.FindStringSubmatch(path); m != nil {
		// Every step of the upload session (POST to start, PATCH to append,
		// PUT to finalize, GET to query status, DELETE to cancel) is a write.
		req := request{repo: m[1], req: reqBlobWrite, kind: "blob upload", write: true}
		// A cross-repo mount (POST ...?mount=<digest>&from=<repo>) copies an
		// existing blob from another repository instead of re-uploading it; the
		// source repository must be readable. Parse the query strictly and
		// authorize exactly what will be forwarded upstream. Reject anything
		// ambiguous: url.ParseQuery errors on (and drops) ';'-joined pairs that a
		// lenient upstream might still act on, and duplicate mount/from values
		// could let us authorize one source while the upstream mounts another.
		if method == http.MethodPost {
			q, err := url.ParseQuery(r.URL.RawQuery)
			switch {
			case err != nil || len(q["mount"]) > 1 || len(q["from"]) > 1:
				req.malformedQuery = true
			case len(q["mount"]) == 1 && len(q["from"]) == 1:
				req.mountFrom = q.Get("from")
			}
		}
		return req, true
	}
	if m := blobRe.FindStringSubmatch(path); m != nil {
		switch method {
		case http.MethodGet:
			return request{repo: m[1], req: reqBlobRead, kind: "blob read"}, true
		case http.MethodHead:
			return request{repo: m[1], req: reqBlobReadOrWrite, kind: "blob existence check"}, true
		default: // DELETE and anything else that mutates.
			return request{repo: m[1], req: reqBlobWrite, kind: "blob write", write: true}, true
		}
	}
	if m := manifestRe.FindStringSubmatch(path); m != nil {
		switch method {
		case http.MethodGet:
			return request{repo: m[1], req: reqManifestRead, kind: "manifest read"}, true
		case http.MethodHead:
			return request{repo: m[1], req: reqManifestReadOrWrite, kind: "manifest existence check"}, true
		default: // PUT, DELETE, ...
			return request{repo: m[1], req: reqManifestWrite, kind: "manifest write", write: true}, true
		}
	}
	return request{}, false
}
