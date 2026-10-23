package archivetest

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Entry describes a single regular file entry in a synthetic test layer tar.
type Entry struct {
	Name string
	Body string
}

// MakeLayer builds an in-memory container layer containing entries.
func MakeLayer(t *testing.T, mt types.MediaType, entries ...Entry) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name:     e.Name,
			Mode:     0o644,
			Size:     int64(len(e.Body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatalf("WriteHeader(%s): %v", e.Name, err)
		}
		if _, err := tw.Write([]byte(e.Body)); err != nil {
			t.Fatalf("Write(%s): %v", e.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	raw := bytes.Clone(buf.Bytes())
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, tarball.WithMediaType(mt))
	if err != nil {
		t.Fatalf("LayerFromOpener: %v", err)
	}
	return layer
}

// MakeImage builds an OCI test image with the given platform, config customizer, and layers.
func MakeImage(t *testing.T, osName, arch, variant string, customize func(*v1.ConfigFile), layers ...v1.Layer) v1.Image {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	var err error
	img, err = mutate.AppendLayers(img, layers...)
	if err != nil {
		t.Fatalf("AppendLayers: %v", err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatalf("ConfigFile: %v", err)
	}
	cf = cf.DeepCopy()
	cf.OS = osName
	cf.Architecture = arch
	cf.Variant = variant
	if customize != nil {
		customize(cf)
	}
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatalf("mutate.ConfigFile: %v", err)
	}
	return img
}

// WriteDirAsTar writes the regular files of srcDir into an uncompressed tar archive at dstTar.
func WriteDirAsTar(t *testing.T, srcDir, dstTar string) {
	t.Helper()
	out, err := os.Create(dstTar)
	if err != nil {
		t.Fatalf("Create(%s): %v", dstTar, err)
	}
	defer out.Close()

	tw := tar.NewWriter(out)
	err = filepath.Walk(srcDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || p == srcDir || info.IsDir() {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatalf("Walk(%s): %v", srcDir, err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
}

// WriteSingleOCITar writes img as a single-platform OCI layout directory and uncompressed tar archive.
func WriteSingleOCITar(t *testing.T, img v1.Image, osName, arch, variant string) (ociDir string, ociTar string) {
	t.Helper()
	tmp := t.TempDir()
	ociDir = filepath.Join(tmp, "oci_layout")
	lp, err := layout.Write(ociDir, empty.Index)
	if err != nil {
		t.Fatalf("layout.Write: %v", err)
	}
	if err := lp.AppendImage(img, layout.WithPlatform(v1.Platform{OS: osName, Architecture: arch, Variant: variant})); err != nil {
		t.Fatalf("AppendImage: %v", err)
	}
	ociTar = filepath.Join(tmp, "oci_layout.tar")
	WriteDirAsTar(t, ociDir, ociTar)
	return ociDir, ociTar
}
