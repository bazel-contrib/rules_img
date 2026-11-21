package bom_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazel-contrib/rules_img_sbom/internal/bom"
)

func TestCreated(t *testing.T) {
	dir := t.TempDir()
	stable := writeFile(t, dir, "stable-status.txt", "STABLE_COMMIT_TIME 2025-02-03T04:05:06Z\nSTABLE_GIT_COMMIT abc\n")
	volatile := writeFile(t, dir, "volatile-status.txt", "BUILD_TIMESTAMP 1700000000\nFORMATTED_DATE 2023 Nov 14\n")
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 999, time.FixedZone("x", 3600)) }
	status := []string{stable, volatile}

	for _, tc := range []struct {
		name  string
		mode  string
		files []string
		key   string
		want  time.Time
	}{
		{name: "none records no time", mode: bom.TimestampNone, files: status, key: "BUILD_TIMESTAMP"},
		{name: "default is none", mode: "", files: status, key: "BUILD_TIMESTAMP"},
		{name: "stamp from Unix seconds", mode: bom.TimestampStamp, files: status, key: "BUILD_TIMESTAMP", want: time.Unix(1700000000, 0).UTC()},
		{name: "stamp from RFC 3339 in the stable status", mode: bom.TimestampStamp, files: status, key: "STABLE_COMMIT_TIME", want: time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)},
		{name: "an unstamped image records no time", mode: bom.TimestampStamp, files: nil, key: "BUILD_TIMESTAMP"},
		{name: "now, in UTC, to the second", mode: bom.TimestampNow, want: time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bom.Created(tc.mode, tc.files, tc.key, now)
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(got), "got %v, want %v", got, tc.want)
		})
	}

	_, err := bom.Created(bom.TimestampStamp, status, "STABLE_MISSING", now)
	assert.ErrorContains(t, err, "STABLE_MISSING")
	_, err = bom.Created(bom.TimestampStamp, status, "STABLE_GIT_COMMIT", now)
	assert.ErrorContains(t, err, "RFC 3339")
}

func TestNoTimestampIsReproducible(t *testing.T) {
	doc, err := bom.ComposeManifest(manifest(t, bom.BaseIgnore, ""))
	require.NoError(t, err)
	cyclonedx, err := bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	assert.Empty(t, decodeCycloneDX(t, cyclonedx).Metadata.Timestamp, "CycloneDX leaves its optional timestamp out")
	spdx, err := bom.WriteSPDX(doc, bom.SPDXNamespace(imageRef))
	require.NoError(t, err)
	assert.Contains(t, string(spdx), `"created": "1970-01-01T00:00:00Z"`, "SPDX requires a creation time: the epoch")

	doc.Created = time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	cyclonedx, err = bom.WriteCycloneDX(doc, "urn:uuid:00000000-0000-5000-8000-000000000000")
	require.NoError(t, err)
	assert.Equal(t, "2025-02-03T04:05:06Z", decodeCycloneDX(t, cyclonedx).Metadata.Timestamp)
}
