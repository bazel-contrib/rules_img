package scan

import (
	"archive/tar"
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazel-contrib/rules_img_sbom/internal/layerscan"
)

type entry struct {
	hdr     tar.Header
	content string
}

func layerTar(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		hdr.Size = int64(len(e.content))
		require.NoError(t, tw.WriteHeader(&hdr))
		_, err := tw.Write([]byte(e.content))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return &buf
}

const dpkgStatus = `Package: libc6
Status: install ok installed
Priority: optional
Section: libs
Installed-Size: 12000
Maintainer: GNU Libc Maintainers <debian-glibc@lists.debian.org>
Architecture: amd64
Multi-Arch: same
Source: glibc
Version: 2.36-9+deb12u4
Description: GNU C Library: Shared libraries

Package: tzdata
Status: install ok installed
Priority: required
Section: localization
Installed-Size: 3000
Maintainer: GNU Libc Maintainers <debian-glibc@lists.debian.org>
Architecture: all
Version: 2024a-0+deb12u1
Description: time zone and daylight-saving time data
`

const osRelease = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
ID=debian
`

const pythonMetadata = `Metadata-Version: 2.1
Name: requests
Version: 2.31.0
License: Apache 2.0
`

func byName(result *layerscan.Result) map[string]layerscan.Package {
	out := map[string]layerscan.Package{}
	for _, p := range result.Packages {
		out[p.Name] = p
	}
	return out
}

func TestScanLayerWithoutDistro(t *testing.T) {
	layer := layerTar(t,
		entry{hdr: tar.Header{Name: "var/", Typeflag: tar.TypeDir, Mode: 0o755}},
		entry{hdr: tar.Header{Name: "var/lib/dpkg/status"}, content: dpkgStatus},
		entry{hdr: tar.Header{Name: "usr/lib/python3/dist-packages/requests-2.31.0.dist-info/METADATA"}, content: pythonMetadata},
		entry{hdr: tar.Header{Name: "usr/lib/python3/dist-packages/requests-2.31.0.dist-info/LICENSE", Typeflag: tar.TypeLink, Linkname: "usr/lib/python3/dist-packages/requests-2.31.0.dist-info/METADATA"}},
		entry{hdr: tar.Header{Name: "etc/.wh.removed"}},
		entry{hdr: tar.Header{Name: "opt/.wh..wh..opq"}},
		entry{hdr: tar.Header{Name: "usr/bin/python3", Typeflag: tar.TypeSymlink, Linkname: "python3.11"}},
	)
	result, err := Scan(context.Background(), layer, Options{TempDir: t.TempDir(), Layer: layerscan.Layer{Digest: "sha256:abc"}})
	require.NoError(t, err)

	assert.Equal(t, layerscan.Version, result.Version)
	assert.Equal(t, "syft", result.Scanner.Name)
	assert.Equal(t, "sha256:abc", result.Layer.Digest)
	assert.Nil(t, result.Distro, "the layer has no os-release")
	assert.Equal(t, []string{
		"/usr/bin/python3",
		"/usr/lib/python3/dist-packages/requests-2.31.0.dist-info/LICENSE",
		"/usr/lib/python3/dist-packages/requests-2.31.0.dist-info/METADATA",
		"/var/lib/dpkg/status",
	}, result.Paths)
	assert.Equal(t, []string{"/etc/removed"}, result.Whiteouts)
	assert.Equal(t, []string{"/opt"}, result.OpaqueDirs)

	packages := byName(result)
	libc := packages["libc6"]
	assert.Equal(t, "deb", libc.Type)
	assert.Equal(t, "2.36-9+deb12u4", libc.Version)
	assert.Empty(t, libc.PURL, "a deb's package URL names its distribution")
	require.NotNil(t, libc.PURLTemplate)
	assert.Equal(t, []string{"/var/lib/dpkg/status"}, libc.Locations)
	assert.Equal(t,
		"pkg:deb/debian/libc6@2.36-9%2Bdeb12u4?arch=amd64&distro=debian-12&upstream=glibc",
		layerscan.FillPURL(*libc.PURLTemplate, &layerscan.Distro{ID: "debian", VersionID: "12"}))
	assert.Contains(t, packages, "tzdata")

	requests := packages["requests"]
	assert.Equal(t, "python", requests.Type)
	assert.Equal(t, "pkg:pypi/requests@2.31.0", requests.PURL)
	assert.Nil(t, requests.PURLTemplate)
}

func TestScanLayerWithDistro(t *testing.T) {
	layer := layerTar(t,
		entry{hdr: tar.Header{Name: "etc/os-release"}, content: osRelease},
		entry{hdr: tar.Header{Name: "var/lib/dpkg/status"}, content: dpkgStatus},
	)
	result, err := Scan(context.Background(), layer, Options{TempDir: t.TempDir()})
	require.NoError(t, err)
	require.NotNil(t, result.Distro)
	assert.Equal(t, "debian", result.Distro.ID)
	assert.Equal(t, "12", result.Distro.VersionID)
	libc := byName(result)["libc6"]
	assert.Equal(t, "pkg:deb/debian/libc6@2.36-9%2Bdeb12u4?arch=amd64&distro=debian-12&upstream=glibc", libc.PURL)
	assert.Nil(t, libc.PURLTemplate)
}
