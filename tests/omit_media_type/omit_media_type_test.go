package omit_media_type_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

const manifestMediaType = "application/vnd.oci.image.manifest.v1+json"

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

type index struct {
	Manifests []descriptor `json:"manifests"`
}

func TestOmitMediaType(t *testing.T) {
	for _, image := range []string{"CHART", "SOCI_IMAGE"} {
		t.Run(image, func(t *testing.T) {
			manifest := readRunfile(t, image+"_MANIFEST")
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(manifest, &fields); err != nil {
				t.Fatalf("parsing manifest: %v", err)
			}
			if mt, ok := fields["mediaType"]; ok {
				t.Errorf("manifest has a top-level mediaType %s", mt)
			}
			if image == "SOCI_IMAGE" && !bytes.Contains(manifest, []byte(`"com.amazon.soci.index-digest"`)) {
				t.Errorf("SOCI image manifest has no com.amazon.soci.index-digest annotation:\n%s", manifest)
			}

			want := descriptor{
				MediaType: manifestMediaType,
				Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256(manifest)),
				Size:      int64(len(manifest)),
			}
			var got descriptor
			if err := json.Unmarshal(readRunfile(t, image+"_DESCRIPTOR"), &got); err != nil {
				t.Fatalf("parsing descriptor: %v", err)
			}
			if got != want {
				t.Errorf("descriptor = %+v, want %+v", got, want)
			}

			layouts := map[string][]byte{
				"oci_tarball": readRunfile(t, image+"_OCI_TARBALL"),
				"sink":        pushToSink(t, image),
			}
			for layout, tarData := range layouts {
				files := tarFiles(t, tarData)
				var idx index
				if err := json.Unmarshal(files["index.json"], &idx); err != nil {
					t.Fatalf("%s: parsing index.json: %v", layout, err)
				}
				if len(idx.Manifests) != 1 {
					t.Fatalf("%s: index.json has %d entries, want 1", layout, len(idx.Manifests))
				}
				if d := idx.Manifests[0]; d.MediaType != want.MediaType || d.Digest != want.Digest || d.Size != want.Size {
					t.Errorf("%s: index.json entry = %+v, want %+v", layout, d, want)
				}
				if blob := files["blobs/sha256/"+want.Digest[len("sha256:"):]]; !bytes.Equal(blob, manifest) {
					t.Errorf("%s: stored manifest differs from the built one:\ngot:  %s\nwant: %s", layout, blob, manifest)
				}
			}
		})
	}
}

// pushToSink runs the image's pusher with an oci-tar sink and returns the tar.
// Running it here instead of in a genrule builds the image in the same
// configuration as the files it is compared with.
func pushToSink(t *testing.T, image string) []byte {
	t.Helper()
	pusher, err := runfiles.Rlocation(os.Getenv(image + "_PUSH"))
	if err != nil {
		t.Fatalf("locating pusher: %v", err)
	}
	env, err := runfiles.Env()
	if err != nil {
		t.Fatal(err)
	}
	sink := filepath.Join(t.TempDir(), "sink.tar")
	cmd := exec.Command(pusher, "--sink", "oci-tar:"+sink)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pushing to the sink: %v\n%s", err, out)
	}
	data, err := os.ReadFile(sink)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readRunfile(t *testing.T, env string) []byte {
	t.Helper()
	path, err := runfiles.Rlocation(os.Getenv(env))
	if err != nil {
		t.Fatalf("locating %s: %v", env, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func tarFiles(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("reading tar: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading %s: %v", hdr.Name, err)
		}
		files[hdr.Name] = content
	}
}
