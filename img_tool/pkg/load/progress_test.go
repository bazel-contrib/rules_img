package load

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
	ocigodigest "github.com/opencontainers/go-digest"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/containerd"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/progress"
)

// TestUploadBlobsReportsCraneStyleProgress covers the incremental containerd
// load: every blob written to the content store, and every blob the store
// already had, is reported in crane's style.
func TestUploadBlobsReportsCraneStyleProgress(t *testing.T) {
	out := captureProgress(t, progress.ModeLog)

	store := newFakeContentStore()
	newBlob := blobWorkItem{
		layer:  staticLayer(t, "a new layer"),
		labels: map[string]string{"containerd.io/gc.ref.content.l.0": "sha256:cafe"},
	}
	existingBlob := blobWorkItem{layer: staticLayer(t, "a layer containerd already has")}
	store.add(t, existingBlob)

	// The containerd branch of LoadAll reports under the "loaded" verb.
	ctx, stop := progress.InitProgress(context.Background(), "loaded")
	defer stop()

	if err := uploadBlobsParallel(ctx, store, []blobWorkItem{newBlob, existingBlob}, 1); err != nil {
		t.Fatalf("uploadBlobsParallel() returned error: %v", err)
	}

	// Same shape as crane: one timestamped line per event, nothing else.
	got := out.String()
	craneLine := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} \S`)
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if !craneLine.MatchString(line) {
			t.Errorf("progress line %q is not in crane's format", line)
		}
	}
	// The full digest is reported, like crane does - the shortened form is only
	// used to label progress bars.
	if want := "loaded blob: " + digestOf(t, newBlob); !strings.Contains(got, want) {
		t.Errorf("missing %q in progress output:\n%s", want, got)
	}
	if want := "existing blob: " + digestOf(t, existingBlob); !strings.Contains(got, want) {
		t.Errorf("missing %q in progress output:\n%s", want, got)
	}

	// The blob that was missing ended up in the content store, with its labels.
	if got, want := store.content(t, digestOf(t, newBlob)), "a new layer"; got != want {
		t.Errorf("content store holds %q for the new blob, want %q", got, want)
	}
	if got := store.labelsOf(t, digestOf(t, newBlob))["containerd.io/gc.ref.content.l.0"]; got != "sha256:cafe" {
		t.Errorf("committed GC label = %q, want %q", got, "sha256:cafe")
	}
}

// TestUploadBlobsStaysSilentWithoutProgress covers `--progress=none`: the same
// load reports nothing at all.
func TestUploadBlobsStaysSilentWithoutProgress(t *testing.T) {
	out := captureProgress(t, progress.ModeNone)

	store := newFakeContentStore()
	blob := blobWorkItem{layer: staticLayer(t, "a new layer")}

	ctx, stop := progress.InitProgress(context.Background(), "loaded")
	defer stop()

	if err := uploadBlobsParallel(ctx, store, []blobWorkItem{blob}, 1); err != nil {
		t.Fatalf("uploadBlobsParallel() returned error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("reported progress with progress reporting off:\n%s", got)
	}
}

func staticLayer(t *testing.T, content string) registryv1.Layer {
	t.Helper()
	return static.NewLayer([]byte(content), types.DockerLayer)
}

func digestOf(t *testing.T, blob blobWorkItem) string {
	t.Helper()
	digest, err := blob.layer.Digest()
	if err != nil {
		t.Fatalf("digesting test layer: %v", err)
	}
	return digest.String()
}

// fakeContentStore is an in-memory stand-in for containerd's content store.
type fakeContentStore struct {
	mu     sync.Mutex
	blobs  map[ocigodigest.Digest][]byte
	labels map[ocigodigest.Digest]map[string]string
}

func newFakeContentStore() *fakeContentStore {
	return &fakeContentStore{
		blobs:  make(map[ocigodigest.Digest][]byte),
		labels: make(map[ocigodigest.Digest]map[string]string),
	}
}

// add seeds the store with a blob, as if a previous load had written it.
func (s *fakeContentStore) add(t *testing.T, blob blobWorkItem) {
	t.Helper()
	reader, err := blob.layer.Compressed()
	if err != nil {
		t.Fatalf("reading test layer: %v", err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading test layer: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobs[ocigodigest.Digest(digestOf(t, blob))] = content
}

// content returns a stored blob's bytes.
func (s *fakeContentStore) content(t *testing.T, digest string) string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.blobs[ocigodigest.Digest(digest)]
	if !ok {
		t.Fatalf("content store has no blob %s", digest)
	}
	return string(content)
}

// labelsOf returns the labels a blob was committed with.
func (s *fakeContentStore) labelsOf(t *testing.T, digest string) map[string]string {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.labels[ocigodigest.Digest(digest)]
}

func (s *fakeContentStore) Info(_ context.Context, digest ocigodigest.Digest) (containerd.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.blobs[digest]
	if !ok {
		return containerd.Info{}, fmt.Errorf("content %s: not found", digest)
	}
	return containerd.Info{Digest: digest, Size: int64(len(content))}, nil
}

func (s *fakeContentStore) Writer(_ context.Context, opts ...containerd.WriterOpt) (containerd.Writer, error) {
	var writerOpts containerd.WriterOpts
	for _, opt := range opts {
		opt(&writerOpts)
	}
	return &fakeContentWriter{store: s, expected: writerOpts.Digest}, nil
}

type fakeContentWriter struct {
	store    *fakeContentStore
	expected ocigodigest.Digest
	content  []byte
}

func (w *fakeContentWriter) Write(p []byte) (int, error) {
	w.content = append(w.content, p...)
	return len(p), nil
}

func (w *fakeContentWriter) Close() error { return nil }

func (w *fakeContentWriter) Commit(_ context.Context, size int64, expected ocigodigest.Digest, opts ...containerd.Opt) error {
	if size != int64(len(w.content)) {
		return fmt.Errorf("committing %s: wrote %d bytes, expected %d", expected, len(w.content), size)
	}
	if got := ocigodigest.FromBytes(w.content); got != expected {
		return fmt.Errorf("committing %s: content has digest %s", expected, got)
	}
	var commitOpts containerd.CommitOpts
	for _, opt := range opts {
		opt(&commitOpts)
	}
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	w.store.blobs[expected] = w.content
	w.store.labels[expected] = commitOpts.Labels
	return nil
}

func (w *fakeContentWriter) Status() (containerd.Status, error) {
	return containerd.Status{Offset: int64(len(w.content))}, nil
}

func (w *fakeContentWriter) Digest() ocigodigest.Digest { return w.expected }

func (w *fakeContentWriter) Truncate(int64) error {
	w.content = nil
	return nil
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
	t.Cleanup(func() {
		os.Stderr = previousStderr
		// Restoring the mode also restores the loggers it owns.
		progress.SetMode(previousMode)
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
