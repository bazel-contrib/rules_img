package registryopts

import (
	"net/http"
	"strings"
)

// WrapMountOrigin wraps base so that a cross-repository blob mount goes out
// without go-containerregistry's "origin" parameter.
//
// This is a workaround for a bug in go-containerregistry, and it is a bad place to
// fix it: rewriting a request URL underneath the library that built it hides the
// behavior from every caller reading remote.MountableLayer. Delete this wrapper as
// soon as there is any other way to keep the parameter from being emitted -- an
// upstream option to suppress it, an upstream fix that stops sending it for a
// source on the destination's own registry, or a way to hand go-cr a mount source
// that keeps its registry for authorization but not for the request.
//
// The bug: go-cr derives origin=<registry> from the reference of every mount source
// that names its registry, and sends it alongside mount/from. The parameter is
// go-cr's own invention -- the distribution spec knows only mount and from -- and
// it tells a registry nothing when the source is a repository it already serves,
// which is what every mount rules_img sends is. A registry that mounts only within
// itself may nevertheless read it as a mount it cannot serve, and answer 202
// Accepted asking for the bytes instead; for a layer that may not be uploaded (a
// deduplicated push's mount-only layer, forbid_layer_push) that costs the push.
//
// Removing it here rather than at the reference keeps go-cr's own use of that
// reference intact: it still asks the token service for read access to the source
// repository (remote.maybeUpdateScopes), and still sends the mount at all for a
// Docker Hub destination, which it drops when the origin does not name Docker Hub
// (google/go-containerregistry#1741).
//
// The one thing this gives up is a mount whose source really is on another
// registry (`cross_mount_from` with a base image elsewhere): without the origin
// the destination reads "from" as one of its own repositories, does not find the
// blob there, and asks for the bytes -- which is what a registry that does not
// implement cross-registry mounts does anyway.
//
// It has to sit above any transport that re-addresses the request (the
// oci-distribution-gateway), so what the gateway forwards is already free of the
// parameter. Installing a transport through [Options.WithTransport] does that.
func WrapMountOrigin(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &mountOriginTransport{inner: base}
}

type mountOriginTransport struct {
	inner http.RoundTripper
}

func (t *mountOriginTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if stripped, ok := withoutMountOrigin(req); ok {
		req = stripped
	}
	return t.inner.RoundTrip(req)
}

// withoutMountOrigin returns a copy of req with the "origin" parameter removed,
// and reports whether it changed anything. Only a blob-upload POST that asks for a
// mount is rewritten, and the copy leaves req itself untouched -- go-cr hands the
// same request to the transport again when it retries.
func withoutMountOrigin(req *http.Request) (*http.Request, bool) {
	if req.Method != http.MethodPost || req.URL == nil {
		return nil, false
	}
	if !strings.HasSuffix(strings.TrimSuffix(req.URL.Path, "/"), "/blobs/uploads") {
		return nil, false
	}
	query := req.URL.Query()
	if query.Get("mount") == "" || query.Get("origin") == "" {
		return nil, false
	}
	stripped := req.Clone(req.Context())
	query.Del("origin")
	stripped.URL.RawQuery = query.Encode()
	return stripped, true
}
