package push

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/referrers"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// TestPushAllReportsCraneStyleProgress pushes real images into an in-memory
// registry and asserts that the crane-style progress mode reports every blob:
// the ones it uploads, and the ones the registry already has.
func TestPushAllReportsCraneStyleProgress(t *testing.T) {
	host, transport := newTestRegistry(t)
	out := captureProgress(t, progress.ModeLog)

	image, err := random.Image(256, 2)
	if err != nil {
		t.Fatalf("creating test image: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digesting test image: %v", err)
	}
	vfs := imageVFS{}
	vfs.add(t, image)
	uploader := NewBuilder(vfs).
		WithJobs(1).
		WithRemoteOptions(transport).
		Build()

	pushed, err := uploader.PushAll(context.Background(), pushOps(host, "test/image", digest, "latest"), "eager")
	if err != nil {
		t.Fatalf("PushAll() returned error: %v", err)
	}
	wantRefs := []PushedReference{
		{Ref: host + "/test/image@" + digest.String()},
		{Ref: host + "/test/image:latest", Tag: true},
	}
	if !slices.Equal(pushed, wantRefs) {
		t.Errorf("PushAll() = %+v, want %+v", pushed, wantRefs)
	}

	// Same shape as crane: one timestamped line per event, nothing else.
	got := out.String()
	craneLine := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \S`)
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !craneLine.MatchString(line) {
			t.Errorf("progress line %q is not in crane's format", line)
		}
	}
	// Every layer and the config are reported as uploaded, and every reference
	// gets crane's "<ref>: digest: <digest> size: <size>" line.
	for _, blob := range blobDigests(t, image) {
		if want := "pushed blob: " + blob; !strings.Contains(got, want) {
			t.Errorf("missing %q in progress output:\n%s", want, got)
		}
	}
	for _, ref := range wantRefs {
		if want := fmt.Sprintf("%s: digest: %s size:", ref.Ref, digest); !strings.Contains(got, want) {
			t.Errorf("missing %q in progress output:\n%s", want, got)
		}
	}

	// Pushing an image on top of the one already in the registry reports its
	// shared layers as existing and uploads only what is new.
	extraLayer, err := random.Layer(128, types.DockerLayer)
	if err != nil {
		t.Fatalf("creating test layer: %v", err)
	}
	extended, err := mutate.AppendLayers(image, extraLayer)
	if err != nil {
		t.Fatalf("appending layer to test image: %v", err)
	}
	extendedDigest, err := extended.Digest()
	if err != nil {
		t.Fatalf("digesting extended test image: %v", err)
	}
	vfs.add(t, extended)
	extraDigest, err := extraLayer.Digest()
	if err != nil {
		t.Fatalf("digesting test layer: %v", err)
	}

	out.Reset()
	if _, err := uploader.PushAll(context.Background(), pushOps(host, "test/image", extendedDigest), "eager"); err != nil {
		t.Fatalf("second PushAll() returned error: %v", err)
	}
	got = out.String()
	for _, layer := range mustLayerDigests(t, image) {
		if want := "existing blob: " + layer; !strings.Contains(got, want) {
			t.Errorf("missing %q in progress output:\n%s", want, got)
		}
	}
	if want := "pushed blob: " + extraDigest.String(); !strings.Contains(got, want) {
		t.Errorf("missing %q in progress output:\n%s", want, got)
	}

	// Re-pushing an image the registry already serves skips it entirely.
	out.Reset()
	if _, err := uploader.PushAll(context.Background(), pushOps(host, "test/image", extendedDigest), "eager"); err != nil {
		t.Fatalf("third PushAll() returned error: %v", err)
	}
	got = out.String()
	if want := "existing manifest: " + extendedDigest.String(); !strings.Contains(got, want) {
		t.Errorf("missing %q in progress output:\n%s", want, got)
	}
	if strings.Contains(got, "pushed blob:") {
		t.Errorf("re-uploaded a blob the registry already had:\n%s", got)
	}
}

// TestPushAllStaysSilentWithoutProgress covers the mode that build actions and
// `--progress=none` use: the same push reports nothing at all.
func TestPushAllStaysSilentWithoutProgress(t *testing.T) {
	host, transport := newTestRegistry(t)
	out := captureProgress(t, progress.ModeNone)

	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("creating test image: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digesting test image: %v", err)
	}
	vfs := imageVFS{}
	vfs.add(t, image)

	uploader := NewBuilder(vfs).
		WithJobs(1).
		WithRemoteOptions(transport).
		Build()
	if _, err := uploader.PushAll(context.Background(), pushOps(host, "test/image", digest), "eager"); err != nil {
		t.Fatalf("PushAll() returned error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("reported progress with progress reporting off:\n%s", got)
	}
}

// TestPushAllMarksReferrerReferences checks that PushAll reports which of the
// manifests it pushed are attachments of another one, so that callers can tell
// an image apart from its SBOMs and signatures in the list of references.
func TestPushAllMarksReferrerReferences(t *testing.T) {
	host, transport := newTestRegistry(t)

	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("creating test image: %v", err)
	}
	imageDigest, err := image.Digest()
	if err != nil {
		t.Fatalf("digesting test image: %v", err)
	}
	attachment, err := random.Image(128, 1)
	if err != nil {
		t.Fatalf("creating test attachment: %v", err)
	}
	attachmentDigest, err := attachment.Digest()
	if err != nil {
		t.Fatalf("digesting test attachment: %v", err)
	}
	vfs := imageVFS{}
	vfs.add(t, image)
	vfs.add(t, attachment)

	// A referrer push carries no tags of its own; it is discovered through the
	// referrers API (see referrerPushOperation in cmd/deploymetadata).
	ops := pushOps(host, "test/image", imageDigest, "latest")
	referrerOps := pushOps(host, "test/image", attachmentDigest)
	referrerOps[0].Referrer = true
	ops = append(ops, referrerOps...)

	pushed, err := NewBuilder(vfs).WithJobs(1).WithRemoteOptions(transport).Build().
		PushAll(context.Background(), ops, "eager")
	if err != nil {
		t.Fatalf("PushAll() returned error: %v", err)
	}
	want := []PushedReference{
		{Ref: host + "/test/image@" + imageDigest.String()},
		{Ref: host + "/test/image:latest", Tag: true},
		{Ref: host + "/test/image@" + attachmentDigest.String(), Referrer: true},
	}
	if !slices.Equal(pushed, want) {
		t.Errorf("PushAll() = %+v, want %+v", pushed, want)
	}
}

// TestTagsSchemeFollowsInsecureMode pins the behavior behind the global
// --insecure flag for the `img deploy` push path: every reference we push to must
// address its registry over http, otherwise go-containerregistry talks HTTPS to a
// plain-HTTP registry ("server gave HTTP response to HTTPS client").
func TestTagsSchemeFollowsInsecureMode(t *testing.T) {
	digest := registryv1.Hash{Algorithm: "sha256", Hex: strings.Repeat("0", 64)}

	tests := []struct {
		name       string
		insecure   bool
		wantScheme string
	}{
		{name: "secure", insecure: false, wantScheme: "https"},
		{name: "insecure", insecure: true, wantScheme: "http"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			previous := registryopts.Insecure()
			registryopts.SetInsecure(tc.insecure)
			t.Cleanup(func() { registryopts.SetInsecure(previous) })

			ops := pushOps("registry.example.com", "my/app", digest, "latest")
			refs, err := NewBuilder(nil).Build().tags(ops[0])
			if err != nil {
				t.Fatalf("tags: %v", err)
			}
			// One digest reference plus the operation's single tag.
			if len(refs) != 2 {
				t.Fatalf("got %d references, want 2 (digest + tag)", len(refs))
			}
			for _, ref := range refs {
				if got := ref.Context().Registry.Scheme(); got != tc.wantScheme {
					t.Errorf("scheme of %s = %q, want %q", ref, got, tc.wantScheme)
				}
			}
		})
	}
}

// pushOps builds a single-manifest push operation for the given image digest.
func pushOps(host, repository string, digest registryv1.Hash, tags ...string) []api.IndexedPushDeployOperation {
	return []api.IndexedPushDeployOperation{{
		PushDeployOperation: api.PushDeployOperation{
			BaseCommandOperation: api.BaseCommandOperation{
				Command:  "push",
				RootKind: "manifest",
				Root:     api.Descriptor{Digest: digest.String()},
			},
			PushTarget: api.PushTarget{
				Registry:   host,
				Repository: repository,
				Tags:       tags,
			},
		},
	}}
}

// imageVFS serves in-memory images to the uploader, keyed by manifest digest.
type imageVFS map[registryv1.Hash]registryv1.Image

func (v imageVFS) add(t *testing.T, image registryv1.Image) {
	t.Helper()
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("digesting test image: %v", err)
	}
	v[digest] = image
}

func (v imageVFS) Taggable(digest registryv1.Hash) (remote.Taggable, error) {
	image, ok := v[digest]
	if !ok {
		return nil, fmt.Errorf("unexpected digest %s", digest)
	}
	return image, nil
}

func (v imageVFS) Digests() ([]registryv1.Hash, error) {
	var hashes []registryv1.Hash
	for _, image := range v {
		imageHashes, err := blobHashes(image)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, imageHashes...)
	}
	return hashes, nil
}

func (v imageVFS) SizeOf(digest registryv1.Hash) (int64, error) {
	for _, image := range v {
		if layer, err := image.LayerByDigest(digest); err == nil {
			return layer.Size()
		}
	}
	return 0, fmt.Errorf("unknown blob %s", digest)
}

func blobHashes(image registryv1.Image) ([]registryv1.Hash, error) {
	manifest, err := image.Manifest()
	if err != nil {
		return nil, err
	}
	hashes := []registryv1.Hash{manifest.Config.Digest}
	for _, layer := range manifest.Layers {
		hashes = append(hashes, layer.Digest)
	}
	return hashes, nil
}

// blobDigests returns the digests of the config and of every layer of the image.
func blobDigests(t *testing.T, image registryv1.Image) []string {
	t.Helper()
	hashes, err := blobHashes(image)
	if err != nil {
		t.Fatalf("listing blobs of test image: %v", err)
	}
	digests := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		digests = append(digests, hash.String())
	}
	return digests
}

// mustLayerDigests returns the digests of the image's layers.
func mustLayerDigests(t *testing.T, image registryv1.Image) []string {
	t.Helper()
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatalf("reading manifest of test image: %v", err)
	}
	digests := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		digests = append(digests, layer.Digest.String())
	}
	return digests
}

// captureProgress applies the given progress mode for the duration of the test
// with stderr redirected, so that everything the mode reports to the user can be
// inspected. The mode is applied after the redirect because it captures
// os.Stderr by value.
func captureProgress(t *testing.T, mode progress.Mode) *stderrCapture {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("creating stderr capture file: %v", err)
	}
	previousStderr := os.Stderr
	previousMode := progress.CurrentMode()
	progressOut := logs.Progress.Writer()
	warnOut := logs.Warn.Writer()
	t.Cleanup(func() {
		os.Stderr = previousStderr
		progress.SetMode(previousMode)
		logs.Progress.SetOutput(progressOut)
		logs.Warn.SetOutput(warnOut)
		file.Close()
	})

	os.Stderr = file
	progress.SetMode(mode)
	return &stderrCapture{t: t, file: file}
}

// stderrCapture reads back what was written to the redirected stderr.
type stderrCapture struct {
	t    *testing.T
	file *os.File
}

func (c *stderrCapture) String() string {
	c.t.Helper()
	data, err := os.ReadFile(c.file.Name())
	if err != nil {
		c.t.Fatalf("reading stderr capture: %v", err)
	}
	return string(data)
}

// Reset drops everything reported so far.
func (c *stderrCapture) Reset() {
	c.t.Helper()
	if err := c.file.Truncate(0); err != nil {
		c.t.Fatalf("truncating stderr capture: %v", err)
	}
	if _, err := c.file.Seek(0, io.SeekStart); err != nil {
		c.t.Fatalf("rewinding stderr capture: %v", err)
	}
}

// newTestRegistry returns an in-memory registry, the host to address it under,
// and the remote option that routes all requests to it. The registry is served
// directly from the transport instead of over a socket, because the sandboxes
// these tests run in don't allow binding a loopback port.
func newTestRegistry(t *testing.T) (string, remote.Option) {
	t.Helper()
	host, option, _ := newTestRegistryTransport(t)
	return host, option
}

// newTestRegistryTransport is newTestRegistry, and also returns the transport.
func newTestRegistryTransport(t *testing.T) (string, remote.Option, http.RoundTripper) {
	t.Helper()
	handler := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	rt := &handlerTransport{handler: handler}
	return "localhost:1234", remote.WithTransport(rt), rt
}

// handlerTransport serves HTTP requests from an in-process handler.
type handlerTransport struct {
	handler http.Handler
}

func (t *handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	// Handlers may assume the server-side invariants that net/http would
	// establish when reading the request off a connection.
	served := request.Clone(request.Context())
	if served.Body == nil {
		served.Body = http.NoBody
	}
	if served.Host == "" {
		served.Host = served.URL.Host
	}
	served.RequestURI = served.URL.RequestURI()

	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, served)
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// TestPushAllKeepsEveryReferrerOfASubject pushes several referrers of one image
// to a registry without the referrers API, where each push adds the referrer to
// the index under the image's referrers tag (sha256-<hex>). Pushed at once, the
// updates of that index would overwrite each other.
func TestPushAllKeepsEveryReferrerOfASubject(t *testing.T) {
	for _, writer := range []bool{false, true} {
		t.Run(fmt.Sprintf("writer=%v", writer), func(t *testing.T) {
			testPushAllKeepsEveryReferrerOfASubject(t, writer)
		})
	}
}

func testPushAllKeepsEveryReferrerOfASubject(t *testing.T, writer bool) {
	host, transport, rt := newTestRegistryTransport(t)

	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatalf("creating test image: %v", err)
	}
	imageDigest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	rawManifest, err := image.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	mediaType, err := image.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	subject := registryv1.Descriptor{MediaType: mediaType, Digest: imageDigest, Size: int64(len(rawManifest))}

	vfs := imageVFS{}
	vfs.add(t, image)
	ops := pushOps(host, "test/image", imageDigest, "latest")
	var want []string
	for range 8 {
		attachment, err := random.Image(64, 1)
		if err != nil {
			t.Fatal(err)
		}
		referrer := mutate.Subject(attachment, subject).(registryv1.Image)
		digest, err := referrer.Digest()
		if err != nil {
			t.Fatal(err)
		}
		vfs.add(t, referrer)
		referrerOps := pushOps(host, "test/image", digest)
		referrerOps[0].Referrer = true
		ops = append(ops, referrerOps...)
		want = append(want, digest.String())
	}

	builder := NewBuilder(vfs).WithJobs(8).WithRemoteOptions(transport)
	if writer {
		builder = builder.WithReferrerOptions(referrers.Options{Transport: rt})
	}
	if _, err := builder.Build().PushAll(context.Background(), ops, "eager"); err != nil {
		t.Fatalf("PushAll() returned error: %v", err)
	}

	tag, err := name.NewTag(host+"/test/image:sha256-"+imageDigest.Hex, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	index, err := remote.Index(tag, transport)
	if err != nil {
		t.Fatalf("reading the referrers tag: %v", err)
	}
	manifest, err := index.IndexManifest()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range manifest.Manifests {
		got = append(got, d.Digest.String())
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the referrers tag lists %d of the %d referrers:\ngot  %v\nwant %v", len(got), len(want), got, want)
	}
}

// TestPushAllRestoresReferrerMissingFromTag pushes a referrer the registry
// already has, but that the referrers tag of its subject doesn't list (because
// an earlier push lost it): the push lists it again. go-containerregistry's
// pusher alone skips the existing manifest, and with it the referrers tag.
func TestPushAllRestoresReferrerMissingFromTag(t *testing.T) {
	host, transport, rt := newTestRegistryTransport(t)

	image, err := random.Image(256, 1)
	if err != nil {
		t.Fatal(err)
	}
	imageDigest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	size, err := image.Size()
	if err != nil {
		t.Fatal(err)
	}
	mediaType, err := image.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	referrer := mutate.Subject(attachment, registryv1.Descriptor{MediaType: mediaType, Digest: imageDigest, Size: size}).(registryv1.Image)
	referrerDigest, err := referrer.Digest()
	if err != nil {
		t.Fatal(err)
	}

	vfs := imageVFS{}
	vfs.add(t, image)
	vfs.add(t, referrer)
	ops := pushOps(host, "test/image", imageDigest, "latest")
	referrerOps := pushOps(host, "test/image", referrerDigest)
	referrerOps[0].Referrer = true
	ops = append(ops, referrerOps...)

	tag, err := name.NewTag(host+"/test/image:sha256-"+imageDigest.Hex, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	listed := func() []string {
		t.Helper()
		index, err := remote.Index(tag, transport)
		if err != nil {
			t.Fatalf("reading the referrers tag: %v", err)
		}
		manifest, err := index.IndexManifest()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, d := range manifest.Manifests {
			got = append(got, d.Digest.String())
		}
		return got
	}
	pushAll := func() {
		t.Helper()
		builder := NewBuilder(vfs).WithRemoteOptions(transport).WithReferrerOptions(referrers.Options{Transport: rt})
		if _, err := builder.Build().PushAll(context.Background(), ops, "eager"); err != nil {
			t.Fatalf("PushAll() returned error: %v", err)
		}
	}

	pushAll()
	if got := listed(); !slices.Equal(got, []string{referrerDigest.String()}) {
		t.Fatalf("after the first push, the referrers tag lists %v, want [%s]", got, referrerDigest)
	}
	if err := remote.Put(tag, empty.Index, transport); err != nil {
		t.Fatal(err)
	}
	pushAll()
	if got := listed(); !slices.Equal(got, []string{referrerDigest.String()}) {
		t.Errorf("after pushing again, the referrers tag lists %v, want [%s]", got, referrerDigest)
	}
}
