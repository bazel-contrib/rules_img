package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/signer"
)

func TestDecideSigningReferrers(t *testing.T) {
	setting := filepath.Join(t.TempDir(), "setting.json")
	if err := os.WriteFile(setting, []byte(`{"schema_version":1,"mode":"command","tool":"true"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	withDefault, err := signer.Discover(nil, nil, setting)
	if err != nil {
		t.Fatal(err)
	}
	withoutDefault, err := signer.Discover(nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	referrer := func(targets ...string) api.IndexedPushDeployOperation {
		op := api.IndexedPushDeployOperation{}
		op.Referrer = true
		op.Root = api.Descriptor{Digest: "sha256:" + strings.Repeat("1", 64)}
		if targets != nil {
			op.Sign = &api.SignConfig{Targets: targets}
		}
		return op
	}
	for _, tc := range []struct {
		name       string
		op         api.IndexedPushDeployOperation
		store      *signer.SettingStore
		override   []string
		wantSign   bool
		wantRoot   bool
		wantAttest bool
	}{
		// Recorded before there were attestations: a sign config without targets.
		{name: "legacy referrer", op: referrer([]string{}...), store: withoutDefault, wantSign: true, wantRoot: true},
		{name: "signed referrer", op: referrer(api.SignTargetReferrers), store: withoutDefault, wantSign: true, wantRoot: true},
		{name: "attested referrer", op: referrer(api.SignTargetSBOMAttestations), store: withoutDefault, wantSign: true, wantAttest: true},
		{name: "signed and attested referrer", op: referrer(api.SignTargetReferrers, api.SignTargetSBOMAttestations), store: withoutDefault, wantSign: true, wantRoot: true, wantAttest: true},
		{name: "override without referrers", op: referrer(api.SignTargetReferrers), store: withoutDefault, override: []string{api.SignTargetRoots}},
		{name: "override attests", op: referrer(), store: withDefault, override: []string{api.SignTargetSBOMAttestations}, wantSign: true, wantAttest: true},
		{name: "override attests, no default", op: referrer(), store: withoutDefault, override: []string{api.SignTargetSBOMAttestations}},
		{name: "all doesn't attest", op: referrer("all"), store: withoutDefault, wantSign: true, wantRoot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := decideSigning(tc.op, tc.store, false, normalizeTargets(tc.override))
			if decision.sign != tc.wantSign {
				t.Fatalf("sign = %v, want %v", decision.sign, tc.wantSign)
			}
			if !decision.sign {
				return
			}
			if got := len(collectSubjects(tc.op, decision.targets)) > 0; got != tc.wantRoot {
				t.Errorf("signs the referrer: %v, want %v", got, tc.wantRoot)
			}
			if got := decision.targets[api.SignTargetSBOMAttestations]; got != tc.wantAttest {
				t.Errorf("attests: %v, want %v", got, tc.wantAttest)
			}
		})
	}
}

type fakeArtifacts map[string]registryv1.Image

func (f fakeArtifacts) Image(digest registryv1.Hash) (registryv1.Image, error) {
	img, ok := f[digest.String()]
	if !ok {
		return nil, fmt.Errorf("no image %s", digest)
	}
	return img, nil
}

type attestCall struct {
	subject       registryv1.Descriptor
	predicateType string
	predicate     string
}

// fakeAttester records the attestations it's asked for, and makes artifacts
// that refer to subject (or to wrongSubject, if set).
type fakeAttester struct {
	calls        []attestCall
	wrongSubject *registryv1.Descriptor
}

func (f *fakeAttester) AttestArtifacts(_ context.Context, subject registryv1.Descriptor, predicateType string, predicate json.RawMessage) ([]registryv1.Image, error) {
	f.calls = append(f.calls, attestCall{subject: subject, predicateType: predicateType, predicate: string(predicate)})
	refersTo := subject
	if f.wrongSubject != nil {
		refersTo = *f.wrongSubject
	}
	img, err := mutate.AppendLayers(empty.Image, static.NewLayer(predicate, "application/vnd.dev.sigstore.bundle.v0.3+json"))
	if err != nil {
		return nil, err
	}
	return []registryv1.Image{mutate.Subject(img, refersTo).(registryv1.Image)}, nil
}

// sbomReferrer returns a referrer of subject that carries blob as its layer.
func sbomReferrer(t *testing.T, subject registryv1.Descriptor, mediaType types.MediaType, blob string) (api.IndexedPushDeployOperation, registryv1.Image) {
	t.Helper()
	img, err := mutate.AppendLayers(mutate.MediaType(empty.Image, types.OCIManifestSchema1), static.NewLayer([]byte(blob), mediaType))
	if err != nil {
		t.Fatal(err)
	}
	img = mutate.ConfigMediaType(img, "application/vnd.oci.empty.v1+json")
	img = mutate.Subject(img, subject).(registryv1.Image)
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	op := api.IndexedPushDeployOperation{}
	op.Referrer = true
	op.Root = api.Descriptor{Digest: digest.String()}
	return op, img
}

func TestAttestSBOMs(t *testing.T) {
	image, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := image.Digest()
	size, _ := image.Size()
	subject := registryv1.Descriptor{MediaType: types.DockerManifestSchema2, Digest: digest, Size: size}

	const sbom = `{"bomFormat":"CycloneDX","specVersion":"1.6"}`
	sbomOp, sbomImage := sbomReferrer(t, subject, "application/vnd.cyclonedx+json", sbom)
	otherOp, otherImage := sbomReferrer(t, subject, "application/vnd.example.notes+json", `{"notes":"hi"}`)
	artifacts := fakeArtifacts{sbomOp.Root.Digest: sbomImage, otherOp.Root.Digest: otherImage}

	var emitted []api.Descriptor
	emit := func(_ context.Context, _ api.IndexedPushDeployOperation, s api.Descriptor, imgs []registryv1.Image) error {
		emitted = append(emitted, s)
		return nil
	}

	plugin := &fakeAttester{}
	if err := attestSBOMs(context.Background(), sbomOp, plugin, artifacts, emit); err != nil {
		t.Fatalf("attestSBOMs() = %v", err)
	}
	if len(plugin.calls) != 1 {
		t.Fatalf("made %d attestations, want 1", len(plugin.calls))
	}
	if call := plugin.calls[0]; call.subject.Digest != digest || call.predicateType != signer.CycloneDXPredicateType || call.predicate != sbom {
		t.Errorf("attested %+v, want the SBOM as a %s predicate about %s", call, signer.CycloneDXPredicateType, digest)
	}
	if len(emitted) != 1 || emitted[0].Digest != digest.String() {
		t.Errorf("emitted attestations about %v, want one about %s", emitted, digest)
	}

	// A referrer that isn't an SBOM isn't attested.
	plugin = &fakeAttester{}
	if err := attestSBOMs(context.Background(), otherOp, plugin, artifacts, emit); err != nil {
		t.Fatalf("attestSBOMs() = %v", err)
	}
	if len(plugin.calls) != 0 {
		t.Errorf("attested a referrer that isn't an SBOM: %+v", plugin.calls)
	}

	// Blobs that can't be SBOMs aren't even read: they may not be available.
	unreadable, err := mutate.AppendLayers(mutate.MediaType(empty.Image, types.OCIManifestSchema1), unreadableLayer{static.NewLayer([]byte("ztoc"), "application/octet-stream")})
	if err != nil {
		t.Fatal(err)
	}
	unreadable = mutate.Subject(unreadable, subject).(registryv1.Image)
	unreadableDigest, err := unreadable.Digest()
	if err != nil {
		t.Fatal(err)
	}
	unreadableOp := api.IndexedPushDeployOperation{}
	unreadableOp.Referrer = true
	unreadableOp.Root = api.Descriptor{Digest: unreadableDigest.String()}
	plugin = &fakeAttester{}
	if err := attestSBOMs(context.Background(), unreadableOp, plugin, fakeArtifacts{unreadableDigest.String(): unreadable}, emit); err != nil {
		t.Errorf("attestSBOMs() of a referrer without SBOM blobs = %v, want nil", err)
	}

	// An index referrer carries no layers of its own: there is nothing to attest.
	indexOp := api.IndexedPushDeployOperation{}
	indexOp.Referrer = true
	indexOp.RootKind = "index"
	indexOp.Root = api.Descriptor{Digest: "sha256:" + strings.Repeat("8", 64)}
	if err := attestSBOMs(context.Background(), indexOp, &fakeAttester{}, fakeArtifacts{}, emit); err != nil {
		t.Errorf("attestSBOMs() of an index referrer = %v, want nil", err)
	}

	// An attestation must refer to the image the SBOM describes.
	other := registryv1.Descriptor{MediaType: types.OCIManifestSchema1, Digest: registryv1.Hash{Algorithm: "sha256", Hex: strings.Repeat("9", 64)}, Size: 1}
	err = attestSBOMs(context.Background(), sbomOp, &fakeAttester{wrongSubject: &other}, artifacts, emit)
	if err == nil || !strings.Contains(err.Error(), "doesn't refer to") {
		t.Errorf("attestSBOMs() = %v, want an error about the attestation's subject", err)
	}
}

// unreadableLayer is a layer whose content can't be read.
type unreadableLayer struct{ registryv1.Layer }

func (unreadableLayer) Compressed() (io.ReadCloser, error) {
	return nil, errors.New("the blob is not available")
}
