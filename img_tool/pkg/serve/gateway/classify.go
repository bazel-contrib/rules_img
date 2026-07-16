package gateway

import (
	"net/http"
	"regexp"
)

// Policy controls which registry operations the gateway is willing to forward.
// Each dimension can be toggled independently.
type Policy struct {
	// AllowBlobRead permits GET/HEAD on /v2/<name>/blobs/<digest>.
	AllowBlobRead bool
	// AllowBlobWrite permits blob uploads (POST/PATCH/PUT on
	// /v2/<name>/blobs/uploads/...) and HEAD/DELETE writes on blobs.
	AllowBlobWrite bool
	// AllowManifestRead permits GET/HEAD on /v2/<name>/manifests/<ref> as well
	// as tag listings and referrers queries.
	AllowManifestRead bool
	// AllowManifestWrite permits PUT/DELETE on /v2/<name>/manifests/<ref>.
	AllowManifestWrite bool
}

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

// allows reports whether the policy permits a request with the given
// requirement.
func (p Policy) allows(req requirement) bool {
	switch req {
	case reqBlobRead:
		return p.AllowBlobRead
	case reqBlobWrite:
		return p.AllowBlobWrite
	case reqBlobReadOrWrite:
		return p.AllowBlobRead || p.AllowBlobWrite
	case reqManifestRead:
		return p.AllowManifestRead
	case reqManifestWrite:
		return p.AllowManifestWrite
	case reqManifestReadOrWrite:
		return p.AllowManifestRead || p.AllowManifestWrite
	default:
		return false
	}
}

var (
	// The repository name follows the OCI Distribution Spec grammar.
	nameGrammar = `[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*`

	blobUploadRe = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/uploads/?.*$`)
	blobRe       = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/([^/]+)$`)
	manifestRe   = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/manifests/(.+)$`)
	tagsRe        = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/tags/list$`)
	referrersRe   = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/referrers/(.+)$`)
)

// request describes a classified registry request.
type request struct {
	repo string      // repository name (path component of /v2/<name>/...)
	req  requirement // what policy dimension gates this request
	kind string      // human-readable operation kind for logging/errors
	// write reports whether the operation mutates the registry. It selects the
	// pull vs push token scope used when authenticating upstream.
	write bool
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
		return request{repo: m[1], req: reqBlobWrite, kind: "blob upload", write: true}, true
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
