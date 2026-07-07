package signer

import (
	"context"
	"fmt"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// SubjectDescriptor converts an api.Descriptor (as recorded in the deploy
// manifest) into the v1.Descriptor handed to a signer plugin as the signing
// subject.
func SubjectDescriptor(d api.Descriptor) (v1.Descriptor, error) {
	h, err := v1.NewHash(d.Digest)
	if err != nil {
		return v1.Descriptor{}, fmt.Errorf("parsing subject digest %q: %w", d.Digest, err)
	}
	return v1.Descriptor{
		MediaType: types.MediaType(d.MediaType),
		Digest:    h,
		Size:      d.Size,
	}, nil
}

// PushReferrers uploads each signature artifact image to repo by digest. The
// registry attaches them as referrers of their `subject` (go-containerregistry
// maintains the referrers fallback tag from the raw manifest bytes). It returns
// the pushed references for reporting.
func PushReferrers(ctx context.Context, pusher *remote.Pusher, repo name.Repository, imgs []v1.Image) ([]string, error) {
	var pushed []string
	for _, img := range imgs {
		d, err := img.Digest()
		if err != nil {
			return pushed, fmt.Errorf("computing signature digest: %w", err)
		}
		ref := repo.Digest(d.String())
		if err := pusher.Push(ctx, ref, img); err != nil {
			return pushed, fmt.Errorf("pushing signature %s: %w", ref, err)
		}
		pushed = append(pushed, ref.String())
	}
	return pushed, nil
}
