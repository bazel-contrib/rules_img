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
	// digest is the blob digest this request is about: the one an existence check
	// asks after, the one a read returns, the one an upload puts in the repository
	// when it succeeds, or the one a delete takes away. It is set only for those
	// requests, and only when the reference really is a digest. It is the part of the
	// blob existence cache's key that identifies the content; leaving it empty is how
	// every other request opts out of the cache.
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
		// A POST opens a session (or finishes an upload outright) and a PUT closes
		// one; those are the two steps whose query says which blob is being
		// uploaded, and the only ones the gateway parses.
		if method == http.MethodPost || method == http.MethodPut {
			q, err := url.ParseQuery(r.URL.RawQuery)
			// A cross-repo mount (POST ...?mount=<digest>&from=<repo>) copies an
			// existing blob from another repository instead of re-uploading it; the
			// source repository must be readable. Parse the query strictly and
			// authorize exactly what will be forwarded upstream. Reject anything
			// ambiguous: url.ParseQuery errors on (and drops) ';'-joined pairs that a
			// lenient upstream might still act on, and duplicate mount/from values
			// could let us authorize one source while the upstream mounts another.
			if method == http.MethodPost {
				switch {
				case err != nil || len(q["mount"]) > 1 || len(q["from"]) > 1:
					req.malformedQuery = true
				case len(q["mount"]) == 1 && len(q["from"]) == 1:
					req.mountFrom = q.Get("from")
				}
			}
			// An ambiguous query is not read for a digest either: a request the
			// gateway and the upstream could read differently must teach the cache
			// nothing.
			if err == nil && !req.malformedQuery {
				req.digest = committedDigest(method, q)
			}
		}
		return req, true
	}
	if m := blobRe.FindStringSubmatch(path); m != nil {
		// Carry the reference as a digest only when it is one, so that the blob
		// existence cache is never keyed on a reference no registry could answer
		// for. Three requests turn on it: the HEAD that asks whether the blob is
		// there, the GET that proves it is by returning it, and the DELETE that
		// takes it away.
		digest := ""
		if digestRe.MatchString(m[blobRefGroup]) {
			digest = m[blobRefGroup]
		}
		switch method {
		case http.MethodGet:
			return request{repo: m[1], req: reqBlobRead, kind: "blob read", op: opNameBlobRead, route: routeBlob, digest: digest}, true
		case http.MethodHead:
			return request{repo: m[1], req: reqBlobReadOrWrite, kind: "blob existence check", op: opNameBlobHead, route: routeBlob, digest: digest}, true
		case http.MethodDelete:
			// A delete is a blob write like any other to the policy and the metrics;
			// the digest is carried because it is the one request whose success makes
			// a cache entry false.
			return request{repo: m[1], req: reqBlobWrite, kind: "blob write", op: opNameBlobWrite, route: routeBlob, write: true, digest: digest}, true
		default: // Anything else that mutates.
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

// committedDigest returns the blob digest a POST or PUT to an upload endpoint puts
// in the repository if the registry answers 201 Created, or "" when the request
// commits nothing.
//
// Three request shapes finish an upload, and each names its blob in the query:
//
//   - PUT /v2/<name>/blobs/uploads/<ref>?digest=<digest> closes the session the
//     content was streamed to. This is what a go-containerregistry push sends.
//   - POST /v2/<name>/blobs/uploads/?digest=<digest> carries the whole blob in the
//     one request.
//   - POST /v2/<name>/blobs/uploads/?mount=<digest>&from=<repo> copies a blob that
//     is already in the registry, transferring no content at all.
//
// Every other step of a session — opening it, appending a chunk, querying its
// status, cancelling it — commits nothing, and neither does a query that names two
// blobs: the gateway will not guess which one an upstream acted on, and the price
// of guessing wrong is telling a client that a blob is present when it is not.
func committedDigest(method string, q url.Values) string {
	var reference string
	switch {
	case method == http.MethodPost && len(q["mount"]) == 1 && len(q["from"]) == 1 && len(q["digest"]) == 0:
		reference = q["mount"][0]
	case len(q["digest"]) == 1 && len(q["mount"]) == 0:
		reference = q["digest"][0]
	default:
		return ""
	}
	// Hold the cache to a real digest, exactly as an existence check is: a
	// reference that is not one names nothing immutable.
	if !digestRe.MatchString(reference) {
		return ""
	}
	return reference
}
