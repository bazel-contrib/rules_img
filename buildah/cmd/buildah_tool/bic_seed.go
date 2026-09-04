package main

import (
	"fmt"

	digest "github.com/opencontainers/go-digest"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/docker/reference"
	"go.podman.io/image/v5/pkg/blobinfocache"
	"go.podman.io/image/v5/types"
)

type blobSeed struct {
	digest digest.Digest
	repo   string
}

func collectBlobSeeds(localMetas []layerMetadata, localRepos []string, baseLayers map[string]layerMetadata, localBlobs map[string]string, upstreamRegistry, upstreamRepository string) ([]blobSeed, error) {
	if len(localRepos) != 0 && len(localRepos) != len(localMetas) {
		return nil, fmt.Errorf("local layer cache repo count mismatch | repos: %d layers: %d", len(localRepos), len(localMetas))
	}

	seen := map[digest.Digest]bool{}
	var seeds []blobSeed
	add := func(dgst digest.Digest, repo string) {
		if dgst == "" || repo == "" || seen[dgst] {
			return
		}
		seen[dgst] = true
		seeds = append(seeds, blobSeed{digest: dgst, repo: repo})
	}

	for i, meta := range localMetas {
		if i < len(localRepos) {
			add(meta.Digest, localRepos[i])
		}
	}

	upstreamRepo := ""
	if upstreamRegistry != "" && upstreamRepository != "" {
		upstreamRepo = upstreamRegistry + "/" + upstreamRepository
	}
	for key, meta := range baseLayers {
		if key != meta.Digest.String() {
			continue
		}
		if _, local := localBlobs[key]; local {
			continue
		}
		add(meta.Digest, upstreamRepo)
	}

	return seeds, nil
}

func seedBlobLocations(sys *types.SystemContext, seeds []blobSeed) {
	if len(seeds) == 0 {
		return
	}
	cache := blobinfocache.DefaultCache(sys)
	seeded := 0
	for _, seed := range seeds {
		named, err := reference.ParseNormalizedNamed(seed.repo)
		if err != nil {
			logf("Skipping blob location seed | repo: %s digest: %s error: %v", seed.repo, seed.digest, err)
			continue
		}
		cache.RecordKnownLocation(
			docker.Transport,
			types.BICTransportScope{Opaque: reference.Domain(named)},
			seed.digest,
			types.BICLocationReference{Opaque: named.Name()},
		)
		seeded++
	}
	logf("Seeded blob locations for cache push reuse | count: %d", seeded)
}
