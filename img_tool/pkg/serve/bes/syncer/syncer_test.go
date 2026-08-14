package syncer

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/cas"
)

type contextRecordingCAS struct {
	contexts []context.Context
	data     []byte
}

func (c *contextRecordingCAS) ReadBlob(ctx context.Context, _ cas.Digest) ([]byte, error) {
	c.contexts = append(c.contexts, ctx)
	return append([]byte(nil), c.data...), nil
}

func (c *contextRecordingCAS) ReaderForBlob(ctx context.Context, _ cas.Digest) (io.ReadCloser, error) {
	c.contexts = append(c.contexts, ctx)
	return io.NopCloser(bytes.NewReader(c.data)), nil
}

func (c *contextRecordingCAS) ReaderForBlobs(ctx context.Context, _ []cas.Digest) (io.ReadCloser, error) {
	c.contexts = append(c.contexts, ctx)
	return io.NopCloser(bytes.NewReader(c.data)), nil
}

func TestLazyCASObjectsPreserveCommitContext(t *testing.T) {
	ctx := protohelper.WithRequestMetadata(context.Background(), protohelper.RequestMetadata{
		ToolInvocationID: "invocation",
		ActionID:         "rules_img:bes:target",
		ActionMnemonic:   "ImgBESCommit",
		TargetID:         "//images:app",
	})
	digest := v1.Hash{Algorithm: "sha256", Hex: strings.Repeat("a", 64)}
	descriptor := v1.Descriptor{Digest: digest, Size: 2, MediaType: types.OCIConfigJSON}

	tests := []struct {
		name string
		run  func(*Syncer) error
	}{
		{
			name: "image config",
			run: func(s *Syncer) error {
				img := &casImage{syncer: s, ctx: ctx, manifest: &v1.Manifest{Config: descriptor}}
				_, err := img.RawConfigFile()
				return err
			},
		},
		{
			name: "index child manifest",
			run: func(s *Syncer) error {
				idx := &casIndex{syncer: s, ctx: ctx, index: &v1.IndexManifest{Manifests: []v1.Descriptor{descriptor}}}
				_, err := idx.Image(digest)
				return err
			},
		},
		{
			name: "streaming layer",
			run: func(s *Syncer) error {
				layer := &casStreamingLayer{syncer: s, ctx: ctx, desc: api.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64), Size: 2}}
				r, err := layer.Compressed()
				if err == nil {
					err = r.Close()
				}
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &contextRecordingCAS{data: []byte("{}")}
			s := &Syncer{casClient: recorder, metadataCache: make(map[string][]byte)}
			if err := tc.run(s); err != nil {
				t.Fatal(err)
			}
			if len(recorder.contexts) != 1 {
				t.Fatalf("CAS received %d contexts, want 1", len(recorder.contexts))
			}
			got, ok := protohelper.RequestMetadataFromContext(recorder.contexts[0])
			if !ok || got.ToolInvocationID != "invocation" || got.ActionID != "rules_img:bes:target" {
				t.Errorf("CAS request metadata = %+v, present=%v", got, ok)
			}
		})
	}
}
