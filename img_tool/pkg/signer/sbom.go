package signer

import (
	"encoding/json"
	"fmt"
	"mime"
	"strings"
)

// Predicate types of SBOM attestations, as in-toto and GitHub's
// actions/attest-sbom define them.
const (
	CycloneDXPredicateType = "https://cyclonedx.org/bom"
	// SPDXPredicateTypePrefix is followed by the document's SPDX version, as in
	// https://spdx.dev/Document/v2.3.
	SPDXPredicateTypePrefix = "https://spdx.dev/Document/v"
)

// MaxSBOMPredicateSize bounds the SBOMs that are attested. The attestation
// embeds the SBOM (base64-encoded, in a DSSE envelope), and its artifact must
// fit the plugin output limit; GitHub's attest-sbom has the same limit.
const MaxSBOMPredicateSize = 16 << 20

// sbomFormat returns the SBOM format a blob of mediaType must be ("" for plain
// JSON, which may be either), and whether a blob of mediaType can be an SBOM at
// all.
func sbomFormat(mediaType string) (string, bool) {
	base, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return "", false
	}
	switch base {
	case "application/vnd.cyclonedx+json":
		return "cyclonedx", true
	case "application/spdx+json":
		return "spdx", true
	case "application/json":
		return "", true
	}
	return "", false
}

// MaybeSBOM reports whether a blob of mediaType can be an SBOM, so that
// SBOMPredicateType is worth reading it for.
func MaybeSBOM(mediaType string) bool {
	_, ok := sbomFormat(mediaType)
	return ok
}

// SBOMPredicateType returns the predicate type of an attestation of data, a
// blob of the given media type, if it's an SBOM: a CycloneDX or SPDX JSON
// document. It reports false for anything else.
func SBOMPredicateType(mediaType string, data []byte) (string, bool, error) {
	format, ok := sbomFormat(mediaType)
	if !ok {
		return "", false, nil
	}

	var doc struct {
		// CycloneDX
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
		// SPDX
		SPDXVersion string `json:"spdxVersion"`
		SPDXID      string `json:"SPDXID"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		if format != "" {
			return "", false, fmt.Errorf("parsing the %s SBOM: %w", format, err)
		}
		return "", false, nil
	}
	switch {
	case (format == "" || format == "cyclonedx") && doc.BOMFormat == "CycloneDX" && doc.SpecVersion != "":
		return CycloneDXPredicateType, true, nil
	case (format == "" || format == "spdx") && strings.HasPrefix(doc.SPDXVersion, "SPDX-") && doc.SPDXID != "":
		return SPDXPredicateTypePrefix + strings.TrimPrefix(doc.SPDXVersion, "SPDX-"), true, nil
	case format != "":
		return "", false, fmt.Errorf("the blob of media type %s is not a %s SBOM", mediaType, format)
	}
	return "", false, nil
}
