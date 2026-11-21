package signer

import "testing"

func TestSBOMPredicateType(t *testing.T) {
	for _, tc := range []struct {
		name, mediaType, data string
		want                  string
		wantErr               bool
	}{
		{name: "cyclonedx", mediaType: "application/vnd.cyclonedx+json", data: `{"bomFormat":"CycloneDX","specVersion":"1.6"}`, want: CycloneDXPredicateType},
		{name: "cyclonedx with version parameter", mediaType: "application/vnd.cyclonedx+json; version=1.6", data: `{"bomFormat":"CycloneDX","specVersion":"1.6"}`, want: CycloneDXPredicateType},
		{name: "spdx", mediaType: "application/spdx+json", data: `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT"}`, want: "https://spdx.dev/Document/v2.3"},
		{name: "sniffed from json", mediaType: "application/json", data: `{"spdxVersion":"SPDX-2.2","SPDXID":"SPDXRef-DOCUMENT"}`, want: "https://spdx.dev/Document/v2.2"},
		{name: "other json", mediaType: "application/json", data: `{"hello":"world"}`},
		{name: "not json", mediaType: "application/json", data: `hello`},
		{name: "signature", mediaType: "application/vnd.dev.sigstore.bundle.v0.3+json", data: `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`},
		{name: "cyclonedx media type, spdx content", mediaType: "application/vnd.cyclonedx+json", data: `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT"}`, wantErr: true},
		{name: "broken spdx", mediaType: "application/spdx+json", data: `{`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok, err := SBOMPredicateType(tc.mediaType, []byte(tc.data))
			if (err != nil) != tc.wantErr {
				t.Fatalf("SBOMPredicateType() error = %v, want error: %v", err, tc.wantErr)
			}
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("SBOMPredicateType() = %q, %v, want %q", got, ok, tc.want)
			}
		})
	}
}
