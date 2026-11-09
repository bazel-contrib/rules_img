package signer

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// TestHelperRequestProcess is not a real test: when GO_WANT_HELPER_REQUEST=1 it
// acts as a ProtocolRequest signer plugin. It fails if stdin is not a Request and
// echoes what it received in the annotations of the artifact it returns.
func TestHelperRequestProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_REQUEST") != "1" {
		return
	}
	var req Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil || req.ProtocolVersion != ProtocolRequest {
		os.Exit(2)
	}
	annotations := map[string]string{
		"role":       req.Role,
		"registry":   req.Destination.Registry,
		"repository": req.Destination.Repository,
		"tags":       strings.Join(req.Destination.Tags, ","),
		"references": strings.Join(req.Destination.References, ","),
		"version":    req.Deploy.RulesImgVersion,
		"invocation": req.Deploy.InvocationID,
		"started":    req.Deploy.StartedAt,
		"strategy":   req.Deploy.PushStrategy,
	}
	if p := req.Subject.Platform; p != nil {
		annotations["platform"] = p.OS + "/" + p.Architecture
	}
	if req.Parent != nil {
		annotations["parent"] = req.Parent.Digest.String()
	}
	img, err := buildTestArtifact("application/vnd.test.attestation",
		[]testArtifactLayer{{MediaType: "application/octet-stream", Data: []byte("x")}}, &req.Subject, annotations)
	if err != nil {
		os.Exit(3)
	}
	if err := writeArtifactTar(os.Stdout, []v1.Image{img}); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func requestPlugin(protocol int) *Subprocess {
	return &Subprocess{
		toolPath: os.Args[0],
		args:     []string{"-test.run=TestHelperRequestProcess"},
		env:      append(os.Environ(), "GO_WANT_HELPER_REQUEST=1"),
		protocol: protocol,
	}
}

func testRequestSubject() v1.Descriptor {
	return v1.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("4", 64)},
		Size:      7,
		Platform:  &v1.Platform{OS: "linux", Architecture: "arm64"},
	}
}

func TestSignRequestDeliversContext(t *testing.T) {
	deploy, err := NewDeploy("eager")
	if err != nil {
		t.Fatal(err)
	}
	parent := v1.Descriptor{MediaType: "application/vnd.oci.image.index.v1+json", Digest: v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("5", 64)}}
	imgs, err := requestPlugin(ProtocolRequest).SignRequest(context.Background(), Request{
		Subject: testRequestSubject(),
		Role:    RoleChildManifest,
		Parent:  &parent,
		Destination: Destination{
			Registry: "ghcr.io", Repository: "example/app", Tags: []string{"latest", "v1"},
			References: []string{"ghcr.io/example/app:latest", "ghcr.io/example/app:v1"},
		},
		Deploy: deploy,
	})
	if err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	m, err := imgs[0].Manifest()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"role": RoleChildManifest, "registry": "ghcr.io", "repository": "example/app", "tags": "latest,v1",
		"references": "ghcr.io/example/app:latest,ghcr.io/example/app:v1", "platform": "linux/arm64",
		"parent": parent.Digest.String(), "strategy": "eager", "version": deploy.RulesImgVersion,
		"invocation": deploy.InvocationID, "started": deploy.StartedAt,
	}
	for k, v := range want {
		if m.Annotations[k] != v {
			t.Errorf("plugin saw %s = %q, want %q", k, m.Annotations[k], v)
		}
	}
	if deploy.RulesImgVersion == "" || len(deploy.InvocationID) != 36 {
		t.Errorf("deploy facts incomplete: %+v", deploy)
	}
}

func TestSignRequestRequiresProtocol(t *testing.T) {
	if _, err := requestPlugin(ProtocolDescriptor).SignRequest(context.Background(), Request{Subject: testRequestSubject()}); err == nil {
		t.Fatal("SignRequest on a descriptor-protocol plugin must fail: it cannot parse a Request")
	}
}

// A caller that only has a descriptor must still send a Request to a plugin that
// expects one, never the bare descriptor.
func TestSignArtifactsWrapsDescriptorForRequestPlugins(t *testing.T) {
	imgs, err := requestPlugin(ProtocolRequest).SignArtifacts(context.Background(), testRequestSubject())
	if err != nil {
		t.Fatalf("SignArtifacts: %v", err)
	}
	m, _ := imgs[0].Manifest()
	if m.Annotations["role"] != RoleRoot {
		t.Errorf("role = %q, want %q", m.Annotations["role"], RoleRoot)
	}
}

func TestNewDeployHonorsSourceDateEpoch(t *testing.T) {
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	d, err := NewDeploy("")
	if err != nil {
		t.Fatal(err)
	}
	if d.StartedAt != "2023-11-14T22:13:20Z" {
		t.Errorf("StartedAt = %q", d.StartedAt)
	}
	other, _ := NewDeploy("")
	if d.InvocationID == other.InvocationID {
		t.Error("invocation ids must be unique per deploy")
	}
	t.Setenv("SOURCE_DATE_EPOCH", "nope")
	if _, err := NewDeploy(""); err == nil {
		t.Error("invalid SOURCE_DATE_EPOCH must fail")
	}
}

func TestReferences(t *testing.T) {
	if got := strings.Join(References("ghcr.io", "o/r", []string{"a", "b"}, "sha256:x"), " "); got != "ghcr.io/o/r:a ghcr.io/o/r:b" {
		t.Errorf("tags: %q", got)
	}
	if got := References("ghcr.io", "o/r", nil, "sha256:x"); len(got) != 1 || got[0] != "ghcr.io/o/r@sha256:x" {
		t.Errorf("digest only: %q", got)
	}
}

func TestParseConfigProtocol(t *testing.T) {
	for _, tc := range []struct {
		json string
		want int
		ok   bool
	}{
		{`{"schema_version":1,"mode":"command","tool":"x"}`, ProtocolDescriptor, true},
		{`{"schema_version":1,"mode":"command","tool":"x","protocol":1}`, ProtocolDescriptor, true},
		{`{"schema_version":1,"mode":"command","tool":"x","protocol":2}`, ProtocolRequest, true},
		{`{"schema_version":1,"mode":"command","tool":"x","protocol":3}`, 0, false},
	} {
		cfg, err := parseConfig([]byte(tc.json))
		if (err == nil) != tc.ok || (tc.ok && cfg.Protocol != tc.want) {
			t.Errorf("parseConfig(%s) = protocol %d, err %v", tc.json, cfg.Protocol, err)
		}
	}
}
