package main

import "strings"

// The two sets of definitions the predicate can follow. A definitions set is
// identified by the predicate's buildDefinition.buildType and decides which
// externalParameters and internalParameters exist and what they mean.
const (
	// definitionsBuildKit follows BuildKit's definitions, so that the
	// attestation reads like the one `docker buildx build --provenance` makes.
	definitionsBuildKit = "buildkit"
	// definitionsBazel is this module's own, Bazel native, set (see
	// slsa-definitions.md next to the README).
	definitionsBazel = "bazel"

	// bazelBuildType identifies version 1 of the Bazel definitions. The fragment
	// names the section of slsa-definitions.md that defines them, and changes if
	// the meaning of a field ever does.
	bazelBuildType = "https://github.com/bazel-contrib/rules_img/blob/main/modules/rules_img_signer_provenance/slsa-definitions.md#bazel-v1"
)

// The Bazel native predicate: SLSA provenance v1 without anything that only makes
// sense for a container build tool. See slsa-definitions.md for what each field
// means; a test keeps the two in sync.

type bazelPredicate struct {
	BuildDefinition bazelBuildDefinition `json:"buildDefinition"`
	RunDetails      bazelRunDetails      `json:"runDetails"`
}

type bazelBuildDefinition struct {
	BuildType            string                  `json:"buildType"`
	ExternalParameters   bazelExternalParameters `json:"externalParameters"`
	InternalParameters   map[string]any          `json:"internalParameters"`
	ResolvedDependencies []bazelResource         `json:"resolvedDependencies,omitempty"`
}

type bazelExternalParameters struct {
	Workspace       *bazelWorkspace   `json:"workspace,omitempty"`
	Targets         []string          `json:"targets,omitempty"`
	Parameters      map[string]string `json:"parameters,omitempty"`
	WorkspaceStatus map[string]string `json:"workspaceStatus,omitempty"`
}

type bazelWorkspace struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Path       string `json:"path,omitempty"`
}

// bazelResource is a SLSA ResourceDescriptor.
type bazelResource struct {
	Name        string            `json:"name,omitempty"`
	URI         string            `json:"uri,omitempty"`
	Digest      map[string]string `json:"digest,omitempty"`
	Annotations map[string]any    `json:"annotations,omitempty"`
}

type bazelRunDetails struct {
	Builder  builder       `json:"builder"`
	Metadata bazelMetadata `json:"metadata"`
}

type bazelMetadata struct {
	InvocationID string `json:"invocationId,omitempty"`
	StartedOn    string `json:"startedOn,omitempty"`
	FinishedOn   string `json:"finishedOn,omitempty"`
}

// bazelBase holds what is the same for every subject of a deploy. render adds
// what is specific to one.
type bazelBase struct {
	buildType       string
	builderID       string
	repository      string // redacted
	path            string
	commit          string
	modified        bool
	ref             string // the git ref the workspace was at
	targets         []string
	parameters      map[string]string
	workspaceStatus map[string]string
	materials       []resource
	ci              map[string]any
	reproducible    bool
	builderPlatform string
	bazelInfo       string // --bazel-info
	custom          map[string]any
}

// bazelSubject is what render takes from the subject and the deploy request.
type bazelSubject struct {
	targetPlatform  string
	rulesImgVersion string
	pushStrategy    string
	invocationID    string
	startedOn       string
	finishedOn      string
	// deployTarget and dependencies come from the build info of the deploy, if any.
	deployTarget string
	dependencies []bazelResource
}

// reservedBazelInternal are the internalParameters keys the definitions own;
// custom ones from --build-info may not reuse them.
var reservedBazelInternal = map[string]bool{
	"builderPlatform": true,
	"targetPlatform":  true,
	"pushStrategy":    true,
	"reproducible":    true,
	"ci":              true,
}

func (b *bazelBase) render(s bazelSubject) bazelPredicate {
	internal := map[string]any{"builderPlatform": b.builderPlatform}
	for k, v := range b.custom {
		internal[k] = v
	}
	if s.targetPlatform != "" {
		internal["targetPlatform"] = s.targetPlatform
	}
	if s.pushStrategy != "" {
		internal["pushStrategy"] = s.pushStrategy
	}
	if b.reproducible {
		internal["reproducible"] = true
	}
	if b.ci != nil {
		internal["ci"] = b.ci
	}

	targets := b.targets
	if s.deployTarget != "" {
		targets = dedupe(append(append([]string(nil), targets...), s.deployTarget))
	}
	external := bazelExternalParameters{
		Targets:         targets,
		Parameters:      b.parameters,
		WorkspaceStatus: b.workspaceStatus,
	}
	if b.repository != "" || b.ref != "" || b.path != "" {
		external.Workspace = &bazelWorkspace{Repository: b.repository, Ref: b.ref, Path: b.path}
	}

	var deps []bazelResource
	if b.commit != "" {
		source := bazelResource{Name: "source", Digest: map[string]string{"gitCommit": b.commit}}
		if b.repository != "" {
			source.URI = gitURI(b.repository, b.ref)
		}
		if b.modified {
			source.Annotations = map[string]any{"modified": true}
		}
		deps = append(deps, source)
	}
	for _, m := range b.materials {
		deps = append(deps, bazelResource{Name: m.Name, URI: m.URI, Digest: m.Digest})
	}
	deps = append(deps, s.dependencies...)

	var version map[string]string
	if s.rulesImgVersion != "" {
		version = map[string]string{"rules_img": s.rulesImgVersion}
	}
	return bazelPredicate{
		BuildDefinition: bazelBuildDefinition{
			BuildType:            b.buildType,
			ExternalParameters:   external,
			InternalParameters:   internal,
			ResolvedDependencies: deps,
		},
		RunDetails: bazelRunDetails{
			Builder: builder{ID: b.builderID, Version: version},
			Metadata: bazelMetadata{
				InvocationID: s.invocationID,
				StartedOn:    s.startedOn,
				FinishedOn:   s.finishedOn,
			},
		},
	}
}

// gitURI renders a repository (and the ref it was at) as the SPDX download
// location SLSA uses for git sources: git+<url>[@<ref>]. An https or ssh URL
// gets the git+ prefix, and scp-style git@host:path becomes git+ssh://git@host/path.
func gitURI(repository, ref string) string {
	uri := repository
	switch {
	case strings.HasPrefix(uri, "git+"):
	case strings.Contains(uri, "://"):
		uri = "git+" + uri
	default:
		// scp-like syntax: [user@]host:path
		if host, path, ok := strings.Cut(uri, ":"); ok && !strings.Contains(host, "/") {
			uri = "git+ssh://" + host + "/" + strings.TrimPrefix(path, "/")
		}
	}
	if ref != "" {
		uri += "@" + ref
	}
	return uri
}

// bazelCI renders the GitHub Actions context as the internalParameters.ci
// object. Like the GitHub Actions build type, it holds only what describes the
// environment and is not already recorded elsewhere: the repository, ref, workflow
// and run are in the workspace, the source dependency, builder.id and invocationId,
// and mutable names (a user or repository can be renamed) are left out in favor of
// the numeric IDs that stay valid. It returns nil outside CI.
func bazelCI(gh map[string]any) map[string]any {
	if gh == nil {
		return nil
	}
	out := map[string]any{"system": "github-actions"}
	for from, to := range map[string]string{
		"github_event_name":          "eventName",
		"github_actor_id":            "actorId",
		"github_repository_id":       "repositoryId",
		"github_repository_owner_id": "repositoryOwnerId",
		"github_ref_type":            "refType",
		"github_ref_protected":       "refProtected",
	} {
		if v, ok := gh[from]; ok {
			out[to] = v
		}
	}
	runner := map[string]any{}
	for from, to := range map[string]string{
		"github_runner_environment":   "environment",
		"github_runner_os":            "os",
		"github_runner_arch":          "arch",
		"github_runner_image_os":      "imageOs",
		"github_runner_image_version": "imageVersion",
	} {
		if v, ok := gh[from]; ok {
			runner[to] = v
		}
	}
	if len(runner) > 0 {
		out["runner"] = runner
	}
	return out
}

// dedupe returns the strings in first-seen order, without duplicates.
func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
