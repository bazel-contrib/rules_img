package push

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"golang.org/x/sync/errgroup"

	"github.com/malt3/go-containerregistry/pkg/name"
	registryv1 "github.com/malt3/go-containerregistry/pkg/v1"
	"github.com/malt3/go-containerregistry/pkg/v1/remote"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	blobcache_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/blobcache"
	remoteexecution_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/remote-apis/build/bazel/remote/execution/v2"
)

const defaultJobs = 16

type builder struct {
	blobcacheClient    blobcache.BlobsClient
	vfs                vfs
	pusher             *remote.Pusher
	overrideRegistry   string
	overrideRepository string
	extraTags          []string
	remoteOptions      []remote.Option
	jobs               int
}

func NewBuilder(vfs vfs) *builder {
	return &builder{vfs: vfs, jobs: defaultJobs}
}

func (b *builder) WithBlobcacheClient(client blobcache.BlobsClient) *builder {
	b.blobcacheClient = client
	return b
}

func (b *builder) WithPusher(p *remote.Pusher) *builder {
	b.pusher = p
	return b
}

func (b *builder) WithOverrideRegistry(registry string) *builder {
	b.overrideRegistry = registry
	return b
}

func (b *builder) WithOverrideRepository(repository string) *builder {
	b.overrideRepository = repository
	return b
}

func (b *builder) WithExtraTags(tags []string) *builder {
	b.extraTags = tags
	return b
}

func (b *builder) WithRemoteOptions(opts ...remote.Option) *builder {
	b.remoteOptions = opts
	return b
}

func (b *builder) WithJobs(jobs int) *builder {
	if jobs > 0 {
		b.jobs = jobs
	}
	return b
}

func (b *builder) Build() *uploader {
	return &uploader{
		blobcacheClient:    b.blobcacheClient,
		vfs:                b.vfs,
		pusher:             b.pusher,
		overrideRegistry:   b.overrideRegistry,
		overrideRepository: b.overrideRepository,
		extraTags:          b.extraTags,
		remoteOptions:      b.remoteOptions,
		jobs:               b.jobs,
	}
}

type uploader struct {
	blobcacheClient    blobcache.BlobsClient
	vfs                vfs
	pusher             *remote.Pusher
	overrideRegistry   string
	overrideRepository string
	extraTags          []string
	remoteOptions      []remote.Option
	jobs               int
}

func (u *uploader) PushAll(ctx context.Context, ops []api.IndexedPushDeployOperation, strategy string) (tags []string, retErr error) {
	if strategy == "bes" {
		return nil, nil // nothing to do
	}
	if err := u.strategyPreHooks(ctx, ops, strategy); err != nil {
		return nil, err
	}

	type pushItem struct {
		ref      name.Reference
		taggable remote.Taggable
	}
	var items []pushItem
	var allTags []string

	// collect all operations
	for _, op := range ops {
		digest, err := registryv1.NewHash(op.Root.Digest)
		if err != nil {
			return nil, err
		}
		refs, err := u.tags(op)
		if err != nil {
			return nil, err
		}
		taggable, err := u.vfs.Taggable(digest)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			items = append(items, pushItem{ref: ref, taggable: taggable})
			allTags = append(allTags, ref.String())
		}
	}

	pusher := u.pusher
	if pusher == nil {
		var err error
		pusher, err = remote.NewPusher(append(u.remoteOptions, remote.WithJobs(u.jobs))...)
		if err != nil {
			return nil, fmt.Errorf("creating pusher: %w", err)
		}
	}

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(u.jobs)

	for _, item := range items {
		item := item
		g.Go(func() error {
			return pusher.Push(ctx, item.ref, item.taggable)
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return allTags, nil
}

// tags returns the list of tags to push for the given operation, applying any overrides and extra tags.
func (u *uploader) tags(op api.IndexedPushDeployOperation) ([]name.Reference, error) {
	registry := op.Registry
	if u.overrideRegistry != "" {
		registry = u.overrideRegistry
	}
	repository := op.Repository
	if u.overrideRepository != "" {
		repository = u.overrideRepository
	}
	baseRef := registry + "/" + repository

	// we always push the digest, along with any tags from the operation and any extra tags
	var refs []name.Reference
	h, err := registryv1.NewHash(op.Root.Digest)
	if err != nil {
		return nil, err
	}

	digestRef, err := name.NewDigest(baseRef + "@" + h.String())
	if err != nil {
		return nil, err
	}
	refs = append(refs, digestRef)

	allTags := append(op.Tags, u.extraTags...)
	allTags = deduplicateAndSort(allTags)
	for _, tag := range allTags {
		tagRef, err := name.NewTag(baseRef + ":" + tag)
		if err != nil {
			return nil, err
		}
		refs = append(refs, tagRef)
	}
	return refs, nil
}

func (u *uploader) strategyPreHooks(ctx context.Context, ops []api.IndexedPushDeployOperation, strategy string) error {
	switch strategy {
	case "cas_registry":
		return u.casRegistryPreHook(ctx, ops)
	}
	return nil
}

func (u *uploader) casRegistryPreHook(ctx context.Context, ops []api.IndexedPushDeployOperation) error {
	if u.blobcacheClient == nil {
		return errors.New("blobcache client is required for cas_registry push strategy")
	}

	blobs, err := u.vfs.Digests()
	if err != nil {
		return fmt.Errorf("getting list of digests from VFS for blobcache: %w", err)
	}
	blobDigests := make([]*remoteexecution_proto.Digest, len(blobs))

	for i, blob := range blobs {
		sz, err := u.vfs.SizeOf(blob)
		if err != nil {
			return fmt.Errorf("getting size of blob %s: %w", blob.String(), err)
		}
		blobDigests[i] = &remoteexecution_proto.Digest{
			Hash:      blob.Hex,
			SizeBytes: sz,
		}
	}
	// TODO: perform optional consistency check here
	// by parsing the response and comparing to the list of blobs
	// we expect to be present in the CAS registry.
	_, err = u.blobcacheClient.Commit(ctx, &blobcache_proto.CommitRequest{
		BlobDigests:    blobDigests,
		DigestFunction: remoteexecution_proto.DigestFunction_SHA256,
	})
	if err != nil {
		return fmt.Errorf("committing blobs to CAS registry: %w", err)
	}
	return nil
}

type vfs interface {
	Taggable(digest registryv1.Hash) (remote.Taggable, error)
	Digests() ([]registryv1.Hash, error)
	SizeOf(digest registryv1.Hash) (int64, error)
}

func deduplicateAndSort(tags []string) []string {
	if len(tags) == 0 {
		return tags
	}
	sort.Strings(tags)
	tags = slices.Compact(tags)
	var outTags []string
	for _, tag := range tags {
		if tag != "" {
			outTags = append(outTags, tag)
		}
	}
	return outTags
}
