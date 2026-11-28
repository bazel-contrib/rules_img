package deployvfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/blake3sum"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

// digestKeyedCAS is a remote cache that holds blobs under one digest each, the
// way a Bazel cache does under its --digest_function. It records the digests the
// blobs were looked up by.
type digestKeyedCAS struct {
	blobs   map[string][]byte // "algorithm:hex" -> content
	lookups []string
}

func casKey(d cas.Digest) string { return d.Algorithm() + ":" + hex.EncodeToString(d.Hash) }

func (c *digestKeyedCAS) FindMissingBlobs(_ context.Context, digests []cas.Digest) ([]cas.Digest, error) {
	var missing []cas.Digest
	for _, d := range digests {
		c.lookups = append(c.lookups, casKey(d))
		if _, ok := c.blobs[casKey(d)]; !ok {
			missing = append(missing, d)
		}
	}
	return missing, nil
}

func (c *digestKeyedCAS) ReadBlob(_ context.Context, d cas.Digest) ([]byte, error) {
	blob, ok := c.blobs[casKey(d)]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", casKey(d))
	}
	return blob, nil
}

func (c *digestKeyedCAS) ReaderForBlob(ctx context.Context, d cas.Digest) (io.ReadCloser, error) {
	blob, err := c.ReadBlob(ctx, d)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(blob)), nil
}

func (c *digestKeyedCAS) ReaderForBlobs(context.Context, []cas.Digest) (io.ReadCloser, error) {
	return nil, fmt.Errorf("not implemented")
}

// testLayer returns a layer descriptor with the SHA256 digest a registry knows
// it by and the BLAKE3 digest of the same content, plus the two cache digests.
func testLayer(t *testing.T, content []byte, recordBLAKE3 bool) (api.Descriptor, cas.Digest, cas.Digest) {
	t.Helper()
	sha := sha256.Sum256(content)
	b3, size, err := blake3sum.Sum(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	desc := api.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    "sha256:" + hex.EncodeToString(sha[:]),
		Size:      int64(len(content)),
	}
	if recordBLAKE3 {
		desc.CASDigests = map[string]string{api.DigestBLAKE3: hex.EncodeToString(b3)}
	}
	return desc, cas.SHA256(sha[:], size), cas.BLAKE3(b3, size)
}

func readLayer(t *testing.T, entry blobEntry) []byte {
	t.Helper()
	reader, err := entry.Opener()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBlobFromCASDigestFunctions(t *testing.T) {
	content := []byte("layer content")

	for _, tc := range []struct {
		name         string
		functions    []string // nil: not configured
		recordBLAKE3 bool
		heldUnder    string // "sha256" or "blake3"
		wantLookups  []string
		wantErr      string // substring; empty: the layer is found
	}{
		{
			name:        "default is sha256 and ignores the recorded blake3 digest",
			heldUnder:   "sha256",
			wantLookups: []string{"sha256"},
		},
		{
			name:         "sha256 cache, sha256 configured",
			functions:    []string{"sha256"},
			recordBLAKE3: true,
			heldUnder:    "sha256",
			wantLookups:  []string{"sha256"},
		},
		{
			name:         "blake3 cache, blake3 configured",
			functions:    []string{"blake3"},
			recordBLAKE3: true,
			heldUnder:    "blake3",
			wantLookups:  []string{"blake3"},
		},
		{
			name:         "both configured, blake3 cache falls back after sha256 misses",
			functions:    []string{"sha256", "blake3"},
			recordBLAKE3: true,
			heldUnder:    "blake3",
			wantLookups:  []string{"sha256", "blake3"},
		},
		{
			name:         "both configured, sha256 cache stops at the first hit",
			functions:    []string{"sha256", "blake3"},
			recordBLAKE3: true,
			heldUnder:    "sha256",
			wantLookups:  []string{"sha256"},
		},
		{
			name:         "both configured, layer without a blake3 digest uses sha256 only",
			functions:    []string{"sha256", "blake3"},
			recordBLAKE3: false,
			heldUnder:    "sha256",
			wantLookups:  []string{"sha256"},
		},
		{
			name:         "blake3 configured but the layer recorded none",
			functions:    []string{"blake3"},
			recordBLAKE3: false,
			heldUnder:    "blake3",
			wantErr:      "no BLAKE3 digest recorded",
		},
		{
			name:         "blob held under neither digest",
			functions:    []string{"sha256", "blake3"},
			recordBLAKE3: true,
			heldUnder:    "",
			wantLookups:  []string{"sha256", "blake3"},
			wantErr:      "blob not found in remote CAS (looked up as sha256, blake3)",
		},
		{
			name:         "sha256 only misses a blake3 cache and says how to fix it",
			recordBLAKE3: true,
			heldUnder:    "blake3",
			wantLookups:  []string{"sha256"},
			wantErr:      "IMG_DIGEST_FUNCTION=blake3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, sha, b3 := testLayer(t, content, tc.recordBLAKE3)
			remote := &digestKeyedCAS{blobs: map[string][]byte{}}
			switch tc.heldUnder {
			case "sha256":
				remote.blobs[casKey(sha)] = content
			case "blake3":
				remote.blobs[casKey(b3)] = content
			}
			b := NewBuilder(api.DeployManifest{}).WithCASReader(remote)
			if tc.functions != nil {
				b = b.WithCASDigestFunctions(tc.functions...)
			}

			entry, err := b.blobFromCAS(desc)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("blobFromCAS error = %v, want one containing %q", err, tc.wantErr)
				}
			} else {
				if err != nil {
					t.Fatalf("blobFromCAS: %v", err)
				}
				if got := readLayer(t, entry); !bytes.Equal(got, content) {
					t.Errorf("read %q, want %q", got, content)
				}
				// The registry still sees the SHA256 digest, whatever the cache holds it by.
				if entry.Descriptor.Digest != desc.Digest {
					t.Errorf("entry digest = %s, want %s", entry.Descriptor.Digest, desc.Digest)
				}
			}

			var got []string
			for _, lookup := range remote.lookups {
				got = append(got, strings.SplitN(lookup, ":", 2)[0])
			}
			if !slices.Equal(got, tc.wantLookups) {
				t.Errorf("looked up by %v, want %v", got, tc.wantLookups)
			}
		})
	}
}

func TestBlobFromCASRejectsAMalformedBLAKE3Digest(t *testing.T) {
	desc, _, _ := testLayer(t, []byte("x"), true)
	desc.CASDigests[api.DigestBLAKE3] = "not-hex"
	b := NewBuilder(api.DeployManifest{}).WithCASReader(&digestKeyedCAS{}).WithCASDigestFunctions("blake3")
	if _, err := b.blobFromCAS(desc); err == nil || !strings.Contains(err.Error(), "invalid BLAKE3 digest") {
		t.Fatalf("blobFromCAS error = %v, want an invalid BLAKE3 digest error", err)
	}
}

func TestBlobFromDiskCacheFindsBLAKE3Blobs(t *testing.T) {
	content := []byte("layer content")
	desc, sha, b3 := testLayer(t, content, true)

	for _, tc := range []struct {
		name      string
		functions []string
		namedBy   cas.Digest // the digest the file is named by
		wantFound bool
	}{
		{"sha256 name, default", nil, sha, true},
		{"blake3 name, blake3 configured", []string{"blake3"}, b3, true},
		{"blake3 name, default ignores it", nil, b3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBlobFile(t, cas.DiskCacheBlobPath(dir, hex.EncodeToString(tc.namedBy.Hash)), content)
			b := NewBuilder(api.DeployManifest{}).WithDiskCache(dir)
			if tc.functions != nil {
				b = b.WithCASDigestFunctions(tc.functions...)
			}
			entry, err := b.blobFromDiskCache(desc)
			if (err == nil) != tc.wantFound {
				t.Fatalf("blobFromDiskCache error = %v, want found = %v", err, tc.wantFound)
			}
			if err == nil {
				if got := readLayer(t, entry); !bytes.Equal(got, content) {
					t.Errorf("read %q, want %q", got, content)
				}
			}
		})
	}
}
