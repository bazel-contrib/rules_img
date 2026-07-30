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

	blobUploadRe = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/uploads/?(?P<reference>.*)$`)
	blobRe       = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/blobs/(?P<reference>[^/]+)$`)
	manifestRe   = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/manifests/(.+)$`)
	tagsRe       = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/tags/list$`)
	referrersRe  = regexp.MustCompile(`^/v2/(` + nameGrammar + `)/referrers/(.+)$`)

	// digestRe matches a content descriptor digest per the distribution spec's
	// grammar (algorithm ":" encoded). It gates what the blob existence cache
	// keys on: a reference that is not a digest names nothing immutable, and
	// matching bounds the key length as a side effect.
	digestRe = regexp.MustCompile(`^[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]{32,128}$`)

	// uploadRefGroup and blobRefGroup are the submatch indices of the upload
	// session reference and of the blob reference. The repository grammar contains
	// groups of its own, so they are looked up by name rather than assumed to be a
	// fixed index.
	uploadRefGroup = blobUploadRe.SubexpIndex("reference")
	blobRefGroup   = blobRe.SubexpIndex("reference")
)

// Values of the oci.operation metric attribute: a stable, low-cardinality token
// per classified operation. They are more granular than [requirement] (a HEAD is
// its own operation) and are also reported for requests the gateway rejects.
const (
	opNameUnknown       = "unknown"
	opNameVersionCheck  = "version.check"
	opNameBlobRead      = "blob.read"
	opNameBlobHead      = "blob.head"
	opNameBlobWrite     = "blob.write"
	opNameBlobUpload    = "blob.upload"
	opNameManifestRead  = "manifest.read"
	opNameManifestHead  = "manifest.head"
	opNameManifestWrite = "manifest.write"
	opNameTagsList      = "tags.list"
	opNameReferrers     = "referrers.read"
)

// Values of the http.route metric attribute: the OCI distribution endpoints with
// their variable path segments replaced by placeholders.
const (
	routeVersion       = "/v2/"
	routeBlob          = "/v2/{name}/blobs/{digest}"
	routeUploadStart   = "/v2/{name}/blobs/uploads/"
	routeUploadSession = "/v2/{name}/blobs/uploads/{reference}"
	routeManifest      = "/v2/{name}/manifests/{reference}"
	routeTags          = "/v2/{name}/tags/list"
	routeReferrers     = "/v2/{name}/referrers/{digest}"
)

// request describes a classified registry request.
type request struct {
	repo string      // repository name (path component of /v2/<name>/...)
	req  requirement // what policy dimension gates this request
	kind string      // human-readable operation kind for logging/errors
	// op is the oci.operation metric attribute value, and route the http.route
	// one. Both are fixed tokens, never derived from the request path.
	op    string
	route string
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
	// digest is the blob digest a blob existence check asked about, and is set
	// only for that operation and only when the reference really is a digest. It
	// is the part of the blob existence cache's key that identifies the content;
	// leaving it empty is how every other request opts out of the cache.
	digest string
}

// existenceCheck reports whether this is a HEAD probe for a blob or manifest,
// the request whose hit/miss ratio decides how much a push client re-uploads.
func (r request) existenceCheck() bool {
	return r.op == opNameBlobHead || r.op == opNameManifestHead
}

// classify inspects the request path and method and reports the repository, the
// policy requirement, and whether it is a write. ok is false for paths the
// gateway does not understand.
//
// The path is matched in its *escaped* form, which is the form the gateway
// forwards ([url.URL.RequestURI] returns EscapedPath). Matching the decoded
// [url.URL.Path] instead would let the two disagree: "/v2/a%2Fb/manifests/x"
// would be authorized as repository "a/b" while "a%2Fb" is what the registry
// receives. Since the repository grammar admits no "%", a percent-escape inside
// the repository segment now simply fails to match and the request is refused.
func classify(r *http.Request) (request, bool) {
	path := r.URL.EscapedPath()
	method := r.Method

	if m := tagsRe.FindStringSubmatch(path); m != nil {
		return request{repo: m[1], req: reqManifestRead, kind: "tag listing", op: opNameTagsList, route: routeTags}, true
	}
	if m := referrersRe.FindStringSubmatch(path); m != nil {
		return request{repo: m[1], req: reqManifestRead, kind: "referrers query", op: opNameReferrers, route: routeReferrers}, true
	}
	if m := blobUploadRe.FindStringSubmatch(path); m != nil {
		// Every step of the upload session (POST to start, PATCH to append,
		// PUT to finalize, GET to query status, DELETE to cancel) is a write.
		route := routeUploadStart
		if m[uploadRefGroup] != "" {
			route = routeUploadSession
		}
		req := request{repo: m[1], req: reqBlobWrite, kind: "blob upload", op: opNameBlobUpload, route: route, write: true}
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
			return request{repo: m[1], req: reqBlobRead, kind: "blob read", op: opNameBlobRead, route: routeBlob}, true
		case http.MethodHead:
			req := request{repo: m[1], req: reqBlobReadOrWrite, kind: "blob existence check", op: opNameBlobHead, route: routeBlob}
			// Carry the digest only when the reference is one, so that the blob
			// existence cache is never keyed on a reference no registry could
			// answer for.
			if digestRe.MatchString(m[blobRefGroup]) {
				req.digest = m[blobRefGroup]
			}
			return req, true
		default: // DELETE and anything else that mutates.
			return request{repo: m[1], req: reqBlobWrite, kind: "blob write", op: opNameBlobWrite, route: routeBlob, write: true}, true
		}
	}
	if m := manifestRe.FindStringSubmatch(path); m != nil {
		switch method {
		case http.MethodGet:
			return request{repo: m[1], req: reqManifestRead, kind: "manifest read", op: opNameManifestRead, route: routeManifest}, true
		case http.MethodHead:
			return request{repo: m[1], req: reqManifestReadOrWrite, kind: "manifest existence check", op: opNameManifestHead, route: routeManifest}, true
		default: // PUT, DELETE, ...
			return request{repo: m[1], req: reqManifestWrite, kind: "manifest write", op: opNameManifestWrite, route: routeManifest, write: true}, true
		}
	}
	return request{op: opNameUnknown}, false
}
