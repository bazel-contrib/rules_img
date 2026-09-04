package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/common/libimage/manifests"
	is "go.podman.io/image/v5/storage"
	"go.podman.io/image/v5/transports"
	"go.podman.io/image/v5/types"
	"go.podman.io/storage"
)

const cachePushWorkers = 4

type cachePushTask struct {
	imageID string
	dest    types.ImageReference
}

type deferredCachePusher struct {
	ctx         context.Context
	store       storage.Store
	pushOptions buildah.PushOptions
	coordinator *blobCoordinator
	wg          sync.WaitGroup

	mu     sync.Mutex
	cond   *sync.Cond
	queue  []cachePushTask
	sealed bool
	err    error
}

func newDeferredCachePusher(ctx context.Context, store storage.Store, options define.BuildOptions) *deferredCachePusher {
	pusher := &deferredCachePusher{
		ctx:   ctx,
		store: store,
		pushOptions: buildah.PushOptions{
			Compression:            options.Compression,
			CompressionFormat:      options.CompressionFormat,
			CompressionLevel:       options.CompressionLevel,
			ForceCompressionFormat: options.ForceCompressionFormat,
			SignaturePolicyPath:    options.SignaturePolicyPath,
			SignBy:                 options.SignBy,
			Store:                  store,
			SystemContext:          options.SystemContext,
			BlobDirectory:          options.BlobDirectory,
			MaxRetries:             options.MaxPullPushRetries,
			RetryDelay:             options.PullPushRetryDelay,
		},
		coordinator: newBlobCoordinator(),
	}
	pusher.pushOptions.DestinationLookupReferenceFunc = func(ref types.ImageReference) (types.ImageReference, error) {
		return &coordinatedImageReference{ImageReference: ref, coordinator: pusher.coordinator}, nil
	}
	pusher.cond = sync.NewCond(&pusher.mu)
	for range cachePushWorkers {
		pusher.wg.Add(1)
		go pusher.loop()
	}
	return pusher
}

func (p *deferredCachePusher) sourceLookup(dest types.ImageReference) manifests.LookupReferenceFunc {
	return func(src types.ImageReference) (types.ImageReference, error) {
		_, image, err := is.ResolveReference(src)
		if err != nil {
			return nil, fmt.Errorf("resolving cache push source %q: %w", transports.ImageName(src), err)
		}
		p.enqueue(cachePushTask{imageID: image.ID, dest: dest})
		return src, nil
	}
}

func (p *deferredCachePusher) enqueue(task cachePushTask) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queue = append(p.queue, task)
	p.cond.Signal()
}

func (p *deferredCachePusher) wait() error {
	p.mu.Lock()
	p.sealed = true
	p.cond.Broadcast()
	p.mu.Unlock()

	p.wg.Wait()

	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *deferredCachePusher) loop() {
	defer p.wg.Done()
	for {
		task, ok := p.next()
		if !ok {
			return
		}
		started := time.Now()
		if _, _, err := buildah.Push(p.ctx, task.imageID, task.dest, p.pushOptions); err != nil {
			p.setErr(fmt.Errorf("pushing cache to %q: %w", transports.ImageName(task.dest), err))
			return
		}
		logf("Pushed cache | dest: %s elapsed: %s", transports.ImageName(task.dest), time.Since(started).Round(time.Millisecond))
	}
}

func (p *deferredCachePusher) next() (cachePushTask, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.queue) == 0 && !p.sealed && p.err == nil {
		p.cond.Wait()
	}
	if len(p.queue) == 0 || p.err != nil {
		return cachePushTask{}, false
	}
	task := p.queue[0]
	p.queue = p.queue[1:]
	return task, true
}

func (p *deferredCachePusher) setErr(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
	}
	p.cond.Broadcast()
}
