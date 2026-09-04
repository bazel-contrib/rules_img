package main

import (
	"flag"
	"strings"
)

var (
	flagDockerfile         = flag.String("dockerfile", "", "")
	flagArchitecture       = flag.String("architecture", "amd64", "")
	flagOperatingSystem    = flag.String("os", "linux", "")
	flagCacheFrom          = flag.String("cache-from", "", "")
	flagCacheTo            = flag.String("cache-to", "", "")
	flagUpstreamRegistry   = flag.String("upstream-registry", "", "")
	flagUpstreamRepository = flag.String("upstream-repository", "", "")
	flagUpstreamDigest     = flag.String("upstream-digest", "", "")
	flagBaseManifest       = flag.String("base-manifest", "", "")
	flagBaseConfig         = flag.String("base-config", "", "")
	flagStagingDir         = flag.String("staging-dir", "", "")

	flagOutputManifest   = flag.String("output-manifest", "", "")
	flagOutputConfig     = flag.String("output-config", "", "")
	flagOutputDescriptor = flag.String("output-descriptor", "", "")
	flagOutputDigest     = flag.String("output-digest", "", "")
	flagBuildLog         = flag.String("build-log", "", "")
	flagEstargz          = flag.Bool("estargz", false, "produce zstd:chunked layers")
	flagVerbose          = flag.Bool("verbose", false, "stream buildah output to the bazel build")
	flagDebug            = flag.Bool("debug", false, "enable debug-level logging of buildah and image operations")

	flagContextFiles         repeatedFlag
	flagBuildArgs            repeatedFlag
	flagLocalLayerBlobs      repeatedFlag
	flagLocalLayerMeta       repeatedFlag
	flagLocalLayerCacheRepos repeatedFlag
	flagBaseLayerMetadata    repeatedFlag
	flagAddHosts             repeatedFlag
	flagOutputLayerBlobs     repeatedFlag
	flagOutputLayerMetadata  repeatedFlag
)

func init() {
	flag.Var(&flagContextFiles, "context-file", "")
	flag.Var(&flagBuildArgs, "build-arg", "")
	flag.Var(&flagLocalLayerBlobs, "local-layer-blob", "")
	flag.Var(&flagLocalLayerMeta, "local-layer-metadata", "")
	flag.Var(&flagLocalLayerCacheRepos, "local-layer-cache-repo", "")
	flag.Var(&flagBaseLayerMetadata, "base-layer-metadata", "")
	flag.Var(&flagAddHosts, "add-host", "")
	flag.Var(&flagOutputLayerBlobs, "output-layer-blob", "")
	flag.Var(&flagOutputLayerMetadata, "output-layer-metadata", "")
}

type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }
func (f *repeatedFlag) Set(s string) error {
	*f = append(*f, s)
	return nil
}
