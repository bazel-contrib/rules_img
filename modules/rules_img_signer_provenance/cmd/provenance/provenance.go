// Command provenance is a rules_img signer plugin that attaches SLSA build
// provenance to an image at push time. It implements the `sign-oci-artifact`
// protocol: it reads the subject descriptor from stdin and writes an OCI image
// layout tar to stdout. It never contacts a container registry and never signs
// anything.
//
// The artifact has the shape BuildKit produces for `docker buildx build
// --provenance` with oci-artifact=true (the default): an attestation manifest
// with artifactType application/vnd.docker.attestation.manifest.v1+json, the
// empty config, a `subject` pointing at the image manifest, and a single
// application/vnd.in-toto+json layer holding an in-toto Statement v1 with a
// https://slsa.dev/provenance/v1 predicate in BuildKit's buildType layout.
//
// Because the plugin runs at deploy time, it can record facts that change on
// every push (timestamps, CI run, git commit) without invalidating any Bazel
// action. Everything else it records is supplied by the caller (flags and an
// optional --build-info JSON file). It does not claim what it cannot know: the
// completeness fields stay false unless the build info asserts otherwise.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/plugin"
	"github.com/bazel-contrib/rules_img_signer_provenance/pkg/signerapi"
)

const (
	// statementType is the in-toto attestation framework v1 Statement type.
	statementType = "https://in-toto.io/Statement/v1"
	// predicateType is the SLSA provenance v1 predicate type.
	predicateType = "https://slsa.dev/provenance/v1"
	// buildKitBuildType is the buildType BuildKit records for SLSA v1.
	buildKitBuildType = "https://github.com/moby/buildkit/blob/master/docs/attestations/slsa-definitions.md"
	// attestationArtifactType is the artifactType of BuildKit attestation
	// manifests in the oci-artifact form; buildx imagetools uses it to find
	// attestations through the referrers API. It is the default of
	// --artifact-type.
	attestationArtifactType = "application/vnd.docker.attestation.manifest.v1+json"
	// inTotoMediaType is the media type of the attestation layer.
	inTotoMediaType = "application/vnd.in-toto+json"
	// annotationPredicateType names the predicate type on the layer.
	annotationPredicateType = "in-toto.io/predicate-type"

	modeMin = "min"
	modeMax = "max"
)

func main() {
	if err := plugin.Dispatch(context.Background(), os.Args[1:], newSigner); err != nil {
		fmt.Fprintln(os.Stderr, "provenance-plugin:", err)
		os.Exit(1)
	}
}

// buildInfo is the optional --build-info JSON file: facts the Bazel build
// already knows. Scalar flags override it; list flags append to it.
type buildInfo struct {
	BuilderID          string            `json:"builder_id,omitempty"`
	BuildType          string            `json:"build_type,omitempty"`
	Frontend           string            `json:"frontend,omitempty"`
	Targets            []string          `json:"targets,omitempty"`
	Reproducible       bool              `json:"reproducible,omitempty"`
	Source             *sourceInfo       `json:"source,omitempty"`
	Materials          []resource        `json:"materials,omitempty"`
	Parameters         map[string]string `json:"parameters,omitempty"`
	InternalParameters map[string]any    `json:"internal_parameters,omitempty"`
	// Completeness lets the caller assert what it can vouch for (BuildKit
	// definitions only). It defaults to false/false: a post-hoc emitter cannot
	// observe the build.
	Completeness *completeness `json:"completeness,omitempty"`
}

type sourceInfo struct {
	URI    string `json:"uri,omitempty"`
	Ref    string `json:"ref,omitempty"` // bazel definitions only
	Commit string `json:"commit,omitempty"`
	Path   string `json:"path,omitempty"`
}

// SLSA predicate (BuildKit's ProvenancePredicateSLSA1 layout).

type predicate struct {
	BuildDefinition buildDefinition `json:"buildDefinition"`
	RunDetails      runDetails      `json:"runDetails"`
}

type buildDefinition struct {
	BuildType            string         `json:"buildType"`
	ExternalParameters   externalParams `json:"externalParameters"`
	InternalParameters   map[string]any `json:"internalParameters"`
	ResolvedDependencies []resource     `json:"resolvedDependencies,omitempty"`
}

type externalParams struct {
	ConfigSource configSource `json:"configSource"`
	Request      request      `json:"request"`
}

type configSource struct {
	URI    string            `json:"uri,omitempty"`
	Digest map[string]string `json:"digest,omitempty"`
	Path   string            `json:"path,omitempty"`
}

type request struct {
	Frontend string            `json:"frontend,omitempty"`
	Args     map[string]string `json:"args,omitempty"`
}

type resource struct {
	// Name says what role the resource plays (for example "base"). Optional;
	// only --build-info can set it.
	Name   string            `json:"name,omitempty"`
	URI    string            `json:"uri"`
	Digest map[string]string `json:"digest,omitempty"`
}

type runDetails struct {
	Builder  builder  `json:"builder"`
	Metadata metadata `json:"metadata"`
}

type builder struct {
	ID string `json:"id"`
	// Version names the builder's components, here the rules_img version that
	// ran the deploy (SLSA v1 builder.version). BuildKit does not set it.
	Version map[string]string `json:"version,omitempty"`
}

type metadata struct {
	InvocationID string `json:"invocationId,omitempty"`
	StartedOn    string `json:"startedOn,omitempty"`
	FinishedOn   string `json:"finishedOn,omitempty"`

	BuildKitMetadata buildKitMetadata `json:"buildkit_metadata"`
	Completeness     completeness     `json:"buildkit_completeness"`
	Reproducible     bool             `json:"buildkit_reproducible,omitempty"`
}

type buildKitMetadata struct {
	VCS map[string]string `json:"vcs,omitempty"`
}

type completeness struct {
	Request              bool `json:"request"`
	ResolvedDependencies bool `json:"resolvedDependencies"`
}

// in-toto Statement v1.

type statement struct {
	Type          string             `json:"_type"`
	PredicateType string             `json:"predicateType"`
	Subject       []statementSubject `json:"subject"`
	Predicate     predicate          `json:"predicate"`
}

type statementSubject struct {
	Name   string            `json:"name,omitempty"`
	Digest map[string]string `json:"digest"`
}

// stringSliceFlag is a repeatable string flag.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// provenanceSigner builds the attestation. Everything except the subject is
// resolved once in newSigner, so all subjects of one deploy share an
// invocation (like the platforms of one BuildKit build).
type provenanceSigner struct {
	// pred is the BuildKit layout of the predicate; bazel the Bazel one. Exactly
	// one is used, depending on --definitions.
	pred         predicate
	bazel        *bazelBase
	artifactType string
	subjectNames []string
	// invocationID, startedOn and finishedOn are what newSigner resolved; a deploy
	// request may refine them.
	invocationID string
	startedOn    string
	finishedOn   string
	// pinnedInvocation and explicitStarted record that the invocation ID and start
	// time were settled by the caller or the environment, so facts from the deploy
	// request must not replace them.
	pinnedInvocation bool
	explicitStarted  bool
	warnNoRequest    sync.Once
	// infoMu guards info, the last build info file read: every call of a deploy
	// names the same file.
	infoMu   sync.Mutex
	infoPath string
	info     *bazelInfoFile
}

func (s *provenanceSigner) readBazelInfo(path string) (*bazelInfoFile, error) {
	s.infoMu.Lock()
	defer s.infoMu.Unlock()
	if s.info != nil && s.infoPath == path {
		return s.info, nil
	}
	info, err := readBazelInfo(path)
	if err != nil {
		return nil, err
	}
	s.infoPath, s.info = path, info
	return info, nil
}

var (
	_ signerapi.OCIArtifactSigner = (*provenanceSigner)(nil)
	_ signerapi.RequestSigner     = (*provenanceSigner)(nil)
)

func newSigner(args []string) (signerapi.OCIArtifactSigner, error) {
	fs := flag.NewFlagSet(plugin.Subcommand, flag.ContinueOnError)
	buildInfoPath := fs.String("build-info", "", "path to a JSON file with facts the build already knows: builder_id, build_type, frontend, reproducible, source{uri,commit,path}, materials[{uri,digest}], parameters, internal_parameters, completeness{request,resolvedDependencies}. Flags override scalar fields and append to lists.")
	builderID := fs.String("builder-id", "", "URI identifying the builder (runDetails.builder.id) (or $RULES_IMG_PROVENANCE_BUILDER_ID). Like BuildKit, empty by default.")
	definitions := fs.String("definitions", definitionsBuildKit, "buildkit or bazel: which set of definitions the predicate follows. buildkit mirrors the attestation `docker buildx build --provenance` makes. bazel is this plugin's own SLSA provenance without anything specific to container build tools (see slsa-definitions.md). They differ in buildType and in the externalParameters, internalParameters and resolvedDependencies they record.")
	buildType := fs.String("build-type", "", "predicate buildType (default: that of --definitions).")
	frontend := fs.String("frontend", "", "request.frontend to record, e.g. the name of the build system. buildkit definitions only.")
	mode := fs.String("mode", modeMin, "min or max. As in BuildKit, min drops build-arg:* and label:* parameters (and thereby never claims a complete request); max keeps them. buildkit definitions only.")
	sourceURI := fs.String("source-uri", "", "URI of the source repository (buildkit: configSource.uri and vcs.source; bazel: externalParameters.workspace.repository and the source resolved dependency).")
	sourceCommit := fs.String("source-commit", "", "commit of the source (buildkit: configSource.digest and vcs.revision; bazel: the source resolved dependency's gitCommit digest); a 40 or 64 character hex string.")
	sourceRef := fs.String("source-ref", "", "git ref the source was at, such as refs/heads/main (or $BUILD_SCM_REF from --status-command; with --ci=auto on GitHub Actions it defaults to the ref of the run). bazel definitions only: externalParameters.workspace.ref and the URI of the source resolved dependency.")
	sourcePath := fs.String("source-path", "", "path inside the source repository (buildkit: configSource.path, the build definition; bazel: externalParameters.workspace.path, the Bazel workspace).")
	statusCommand := fs.String("status-command", "", "command that prints Bazel workspace status style KEY VALUE lines, run at deploy time to derive source information (or $RULES_IMG_PROVENANCE_STATUS_COMMAND). Understood keys: BUILD_SCM_REVISION, BUILD_SCM_URL, BUILD_SCM_PATH, and BUILD_SCM_STATUS (anything but clean marks the revision modified: -dirty for buildkit, annotations.modified for bazel). Explicit --source-* flags win over it, and it wins over --build-info.")
	var statusArgs, statusKeys stringSliceFlag
	fs.Var(&statusArgs, "status-arg", "argument passed to --status-command (repeatable).")
	fs.Var(&statusKeys, "status-key", "additional key of the status output to record (buildkit: internalParameters.workspace_status; bazel: externalParameters.workspaceStatus) (repeatable). Nothing else of the output is recorded.")
	builderPlatform := fs.String("builder-platform", "", "platform of the builder, the machine running the deploy (internalParameters.builderPlatform); defaults to the platform this plugin runs on.")
	invocationID := fs.String("invocation-id", "", "identifier of this build invocation (or $RULES_IMG_PROVENANCE_INVOCATION_ID). By default it is $BUILD_ID, which `bazel run` sets to the ID of the Bazel invocation; else with --ci=auto on GitHub Actions the URL of the run; else an ID that `img deploy` generates.")
	ci := fs.String("ci", "auto", "auto or none. auto records the CI context in internalParameters (buildkit: the flat github_* keys buildx hands to BuildKit; bazel: the internalParameters.ci object).")
	eventPayload := fs.Bool("github-event-payload", true, "with --ci=auto on GitHub Actions, include the full webhook event payload (github_event_payload), as buildx does. buildkit definitions only.")
	artifactType := fs.String("artifact-type", attestationArtifactType, "artifactType of the attestation manifest. The default is the one BuildKit uses and the one `docker buildx imagetools` looks for, so a different value makes the attestation invisible to it; change it only if something else is meant to find the attestation by type.")
	reproducible := fs.Bool("reproducible", false, "claim that the same inputs produce the same image (buildkit: buildkit_reproducible; bazel: internalParameters.reproducible).")
	recordTimestamps := fs.Bool("record-timestamps", false, "record startedOn/finishedOn. Off by default so output is reproducible; honors $SOURCE_DATE_EPOCH, which implies recording.")
	startedOn := fs.String("started-on", "", "RFC 3339 time to record as startedOn (implies recording timestamps; defaults to finishedOn).")
	bazelInfo := fs.String("bazel-info", bazelInfoAll, "all, bases or none: which Bazel targets the deploy was built from to record as resolved dependencies, when the signing_config sets build_info (protocol 2): all records the pulled base images, the images and the layers below the deployed image; bases only the pulled base images; none none of them. The label of the deploy rule is recorded as a target either way. bazel definitions only.")
	var materials, parameters, subjectNames, targets stringSliceFlag
	fs.Var(&targets, "target", "Bazel label of what was built and deployed (repeatable), recorded as externalParameters.targets. bazel definitions only.")
	fs.Var(&materials, "material", "input of the build as URI or URI@algo:hex (repeatable), recorded in resolvedDependencies. Use a pkg:docker/... URI for base images.")
	fs.Var(&parameters, "parameter", "key=value parameter of the invocation (repeatable) (buildkit: request.args; bazel: externalParameters.parameters).")
	fs.Var(&subjectNames, "subject-name", "image reference that names the subject (repeatable); recorded as a pkg:docker purl with the subject's platform, like BuildKit's subject names.")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	switch *definitions {
	case definitionsBuildKit:
		if len(targets) > 0 || explicit["source-ref"] || explicit["bazel-info"] {
			return nil, fmt.Errorf("--target, --source-ref and --bazel-info are part of the bazel definitions only (use --definitions=bazel)")
		}
	case definitionsBazel:
		switch *bazelInfo {
		case bazelInfoAll, bazelInfoBases, bazelInfoNone:
		default:
			return nil, fmt.Errorf("unknown --bazel-info %q (want %s, %s or %s)", *bazelInfo, bazelInfoAll, bazelInfoBases, bazelInfoNone)
		}
		for _, name := range []string{"frontend", "mode", "github-event-payload"} {
			if explicit[name] {
				return nil, fmt.Errorf("--%s is part of the buildkit definitions only (--definitions=buildkit)", name)
			}
		}
	default:
		return nil, fmt.Errorf("unknown --definitions %q (want %s or %s)", *definitions, definitionsBuildKit, definitionsBazel)
	}
	if *mode != modeMin && *mode != modeMax {
		return nil, fmt.Errorf("unknown --mode %q (want min or max)", *mode)
	}
	if *ci != "auto" && *ci != "none" {
		return nil, fmt.Errorf("unknown --ci %q (want auto or none)", *ci)
	}
	if err := validateArtifactType(*artifactType); err != nil {
		return nil, err
	}

	var info buildInfo
	if *buildInfoPath != "" {
		data, err := os.ReadFile(*buildInfoPath)
		if err != nil {
			return nil, fmt.Errorf("reading --build-info: %w", err)
		}
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("parsing --build-info %s: %w", *buildInfoPath, err)
		}
	}
	switch *definitions {
	case definitionsBuildKit:
		if len(info.Targets) > 0 || (info.Source != nil && info.Source.Ref != "") {
			return nil, fmt.Errorf("--build-info: targets and source.ref are part of the bazel definitions only (use --definitions=bazel)")
		}
	case definitionsBazel:
		if info.Frontend != "" || info.Completeness != nil {
			return nil, fmt.Errorf("--build-info: frontend and completeness are part of the buildkit definitions only (--definitions=buildkit)")
		}
	}

	// Scalars: flag > build info.
	pick := func(flagValue, fromInfo string) string {
		if flagValue != "" {
			return flagValue
		}
		return fromInfo
	}
	src := sourceInfo{}
	if info.Source != nil {
		src = *info.Source
	}

	// Source information derived at deploy time: flag > status command > build info.
	var status workspaceStatus
	if cmdline := envOr(*statusCommand, "RULES_IMG_PROVENANCE_STATUS_COMMAND"); cmdline != "" {
		var err error
		if status, err = runStatusCommand(context.Background(), cmdline, statusArgs); err != nil {
			return nil, err
		}
	} else if len(statusArgs) > 0 || len(statusKeys) > 0 {
		return nil, fmt.Errorf("--status-arg and --status-key need --status-command")
	}
	src.URI = pick(*sourceURI, pick(status[statusURL], src.URI))
	src.Commit = pick(*sourceCommit, pick(status[statusRevision], src.Commit))
	src.Path = pick(*sourcePath, pick(status[statusPath], src.Path))
	src.Ref = pick(*sourceRef, pick(status[statusRef], src.Ref))
	// A flag or build info pins the revision; the status only qualifies a revision it supplied itself.
	dirty := *sourceCommit == "" && status[statusRevision] != "" && status.dirty()

	cliParams, err := parseKeyValues(parameters)
	if err != nil {
		return nil, err
	}
	reqArgs := map[string]string{}
	for k, v := range info.Parameters {
		reqArgs[k] = v
	}
	for k, v := range cliParams {
		reqArgs[k] = v
	}
	if *definitions == definitionsBuildKit && *mode == modeMin {
		for k := range reqArgs {
			if strings.HasPrefix(k, "build-arg:") || strings.HasPrefix(k, "label:") {
				delete(reqArgs, k)
			}
		}
	}
	if len(reqArgs) == 0 {
		reqArgs = nil
	}

	deps := append([]resource(nil), info.Materials...)
	for _, m := range materials {
		r, err := parseMaterial(m)
		if err != nil {
			return nil, err
		}
		deps = append(deps, r)
	}
	for i := range deps {
		deps[i].URI = redactURL(deps[i].URI)
	}

	commitAlg := ""
	if src.Commit != "" {
		if commitAlg, err = commitAlgorithm(src.Commit); err != nil {
			return nil, err
		}
	}

	var ghCtx map[string]any
	if *ci == "auto" {
		ghCtx, err = githubActionsContext(*definitions == definitionsBuildKit && *eventPayload)
		if err != nil {
			return nil, err
		}
	}

	// The invocation ID, in order of precedence: what the caller says, the ID of the
	// Bazel invocation (`bazel run` exports it as $BUILD_ID), the CI run. All of
	// them are the same for every call of one deploy. Failing those, the ID that
	// `img deploy` generates for the deploy is used, per request.
	invID := envOr(*invocationID, "RULES_IMG_PROVENANCE_INVOCATION_ID")
	if invID == "" {
		invID = os.Getenv("BUILD_ID")
	}
	if invID == "" && ghCtx != nil {
		invID = githubInvocationID(ghCtx)
	}

	started, finished, err := resolveTimes(*recordTimestamps, *startedOn)
	if err != nil {
		return nil, err
	}

	builderIDValue := pick(envOr(*builderID, "RULES_IMG_PROVENANCE_BUILDER_ID"), info.BuilderID)
	reproducibleClaim := *reproducible || info.Reproducible
	platform := pick(*builderPlatform, runtime.GOOS+"/"+runtime.GOARCH)

	signer := &provenanceSigner{
		artifactType:     *artifactType,
		pinnedInvocation: invID != "",
		explicitStarted:  *startedOn != "",
		subjectNames:     subjectNames,
		invocationID:     invID,
		startedOn:        started,
		finishedOn:       finished,
	}

	if *definitions == definitionsBazel {
		for k := range info.InternalParameters {
			if reservedBazelInternal[k] {
				return nil, fmt.Errorf("--build-info: internal parameter %q is defined by the bazel definitions", k)
			}
		}
		ref := src.Ref
		if ref == "" && ghCtx != nil {
			ref, _ = ghCtx["github_ref"].(string)
		}
		signer.bazel = &bazelBase{
			buildType:       pick(*buildType, pick(info.BuildType, bazelBuildType)),
			builderID:       builderIDValue,
			repository:      redactURL(src.URI),
			path:            src.Path,
			commit:          src.Commit,
			modified:        dirty,
			ref:             ref,
			targets:         dedupe(append(append([]string(nil), info.Targets...), targets...)),
			parameters:      reqArgs,
			workspaceStatus: status.extras(statusKeys),
			materials:       deps,
			ci:              bazelCI(ghCtx),
			reproducible:    reproducibleClaim,
			builderPlatform: platform,
			bazelInfo:       *bazelInfo,
			custom:          info.InternalParameters,
		}
		return signer, nil
	}

	cs := configSource{URI: redactURL(src.URI), Path: src.Path}
	vcs := map[string]string{}
	if src.URI != "" {
		vcs["source"] = redactURL(src.URI)
	}
	if src.Commit != "" {
		cs.Digest = map[string]string{commitAlg: src.Commit}
		vcs["revision"] = src.Commit
		if dirty {
			vcs["revision"] += "-dirty"
		}
	}
	if len(vcs) == 0 {
		vcs = nil
	}

	internal := map[string]any{"builderPlatform": platform}
	if extras := status.extras(statusKeys); extras != nil {
		internal["workspace_status"] = extras
	}
	for _, extra := range []map[string]any{info.InternalParameters, ghCtx} {
		for k, v := range extra {
			// BuildKit refuses custom keys that collide with its own.
			if _, exists := internal[k]; exists || k == "targetPlatform" || k == "dockerfileVersion" || k == "buildConfig" {
				return nil, fmt.Errorf("internal parameter %q collides with a built-in provenance key", k)
			}
			internal[k] = v
		}
	}

	comp := completeness{}
	if info.Completeness != nil {
		comp = *info.Completeness
	}
	if *mode == modeMin {
		// min scrubs args, so it can never vouch for a complete request.
		comp.Request = false
	}

	signer.pred = predicate{
		BuildDefinition: buildDefinition{
			BuildType: pick(*buildType, pick(info.BuildType, buildKitBuildType)),
			ExternalParameters: externalParams{
				ConfigSource: cs,
				Request:      request{Frontend: pick(*frontend, info.Frontend), Args: reqArgs},
			},
			InternalParameters:   internal,
			ResolvedDependencies: deps,
		},
		RunDetails: runDetails{
			Builder: builder{ID: builderIDValue},
			Metadata: metadata{
				InvocationID:     invID,
				StartedOn:        started,
				FinishedOn:       finished,
				BuildKitMetadata: buildKitMetadata{VCS: vcs},
				Completeness:     comp,
				Reproducible:     reproducibleClaim,
			},
		},
	}
	return signer, nil
}

// Sign implements signerapi.OCIArtifactSigner for the original protocol, where
// all the plugin gets is the subject descriptor.
func (s *provenanceSigner) Sign(_ context.Context, subject v1.Descriptor) (v1.Image, error) {
	s.warnNoRequest.Do(func() {
		fmt.Fprintln(os.Stderr, "provenance-plugin: warning: called with only a subject descriptor, so the image name, platform and rules_img version are not recorded; set protocol = 2 on the signing_config")
	})
	return s.sign(subject, nil)
}

// SignRequest implements signerapi.RequestSigner: the destination names the
// subject, the subject carries its platform, and the deploy supplies the
// rules_img version and facts shared by every call of the deploy.
func (s *provenanceSigner) SignRequest(_ context.Context, req signerapi.Request) (v1.Image, error) {
	return s.sign(req.Subject, &req)
}

// subjectFacts is what differs between the subjects of one deploy, whichever
// definitions the predicate follows.
type subjectFacts struct {
	names           []string
	targetPlatform  string
	rulesImgVersion string
	pushStrategy    string
	invocationID    string
	startedOn       string
	// buildInfo is the build info file of the deploy, if there is one.
	buildInfo string
}

func (s *provenanceSigner) factsFor(subject v1.Descriptor, req *signerapi.Request) subjectFacts {
	f := subjectFacts{names: s.subjectNames, invocationID: s.invocationID, startedOn: s.startedOn}
	// builderPlatform is the machine; the target platform is the subject's.
	if p := subject.Platform; p != nil && p.OS != "" && p.Architecture != "" {
		f.targetPlatform = formatPlatform(normalizePlatform(*p))
	}
	if req == nil {
		return f
	}
	if len(f.names) == 0 {
		f.names = req.Destination.References
	}
	f.rulesImgVersion = req.Deploy.RulesImgVersion
	f.pushStrategy = req.Deploy.PushStrategy
	f.buildInfo = req.Deploy.BuildInfoFile
	// Every call of one deploy shares these, like the platforms of one BuildKit
	// build share an invocation. Explicit values still win.
	if id := req.Deploy.InvocationID; id != "" && !s.pinnedInvocation {
		f.invocationID = id
	}
	if started := req.Deploy.StartedAt; started != "" && !s.explicitStarted && s.finishedOn != "" {
		if at, err := time.Parse(time.RFC3339, started); err == nil {
			if finished, err := time.Parse(time.RFC3339, s.finishedOn); err == nil && !at.After(finished) {
				f.startedOn = at.UTC().Format(time.RFC3339)
			}
		}
	}
	return f
}

// renderBuildKit completes the BuildKit layout of the predicate for one subject.
func (s *provenanceSigner) renderBuildKit(f subjectFacts) predicate {
	pred := s.pred
	internal := make(map[string]any, len(pred.BuildDefinition.InternalParameters)+1)
	for k, v := range pred.BuildDefinition.InternalParameters {
		internal[k] = v
	}
	if f.targetPlatform != "" {
		internal["targetPlatform"] = f.targetPlatform
	}
	pred.BuildDefinition.InternalParameters = internal
	if f.rulesImgVersion != "" {
		pred.RunDetails.Builder.Version = map[string]string{"rules_img": f.rulesImgVersion}
	}
	pred.RunDetails.Metadata.InvocationID = f.invocationID
	pred.RunDetails.Metadata.StartedOn = f.startedOn
	return pred
}

func (s *provenanceSigner) sign(subject v1.Descriptor, req *signerapi.Request) (v1.Image, error) {
	f := s.factsFor(subject, req)
	names := f.names
	var pred any
	if s.bazel != nil {
		subj := bazelSubject{
			targetPlatform:  f.targetPlatform,
			rulesImgVersion: f.rulesImgVersion,
			pushStrategy:    f.pushStrategy,
			invocationID:    f.invocationID,
			startedOn:       f.startedOn,
			finishedOn:      s.finishedOn,
		}
		if f.buildInfo != "" {
			info, err := s.readBazelInfo(f.buildInfo)
			if err != nil {
				return nil, fmt.Errorf("reading the build info of the deploy: %w", err)
			}
			subj.deployTarget = info.Deploy.Label
			subj.dependencies = info.dependenciesOf(subject.Digest.String(), s.bazel.bazelInfo)
		}
		pred = s.bazel.render(subj)
	} else {
		pred = s.renderBuildKit(f)
	}

	digest := map[string]string{subject.Digest.Algorithm: subject.Digest.Hex}
	var subjects []statementSubject
	for _, ref := range names {
		purl, err := refToPURL(ref, subject.Platform)
		if err != nil {
			return nil, err
		}
		subjects = append(subjects, statementSubject{Name: purl, Digest: digest})
	}
	if len(subjects) == 0 {
		subjects = []statementSubject{{Digest: digest}}
	}

	payload, err := json.Marshal(struct {
		Type          string             `json:"_type"`
		PredicateType string             `json:"predicateType"`
		Subject       []statementSubject `json:"subject"`
		Predicate     any                `json:"predicate"`
	}{
		Type:          statementType,
		PredicateType: predicateType,
		Subject:       subjects,
		Predicate:     pred,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling in-toto statement: %w", err)
	}
	// The manifest's subject has no platform (BuildKit does not set one).
	manifestSubject := subject
	manifestSubject.Platform = nil
	return plugin.BuildArtifact(
		s.artifactType,
		[]plugin.ArtifactLayer{{
			MediaType:   inTotoMediaType,
			Data:        payload,
			Annotations: map[string]string{annotationPredicateType: predicateType},
		}},
		&manifestSubject,
		nil,
	)
}

// validateArtifactType checks that v looks like a media type (type/subtype, RFC
// 6838), which is what the OCI spec requires an artifactType to be.
func validateArtifactType(v string) error {
	typ, sub, ok := strings.Cut(v, "/")
	if !ok || typ == "" || sub == "" || strings.ContainsAny(v, " \t\r\n") || strings.Count(v, "/") != 1 {
		return fmt.Errorf("invalid --artifact-type %q: want a media type such as %s", v, attestationArtifactType)
	}
	return nil
}

// resolveTimes returns startedOn/finishedOn. Timestamps are recorded only when
// asked for (--record-timestamps, --started-on) or when SOURCE_DATE_EPOCH pins
// them, keeping the default output a pure function of its inputs.
func resolveTimes(record bool, startedFlag string) (started, finished string, err error) {
	var finishedAt time.Time
	switch sde := os.Getenv("SOURCE_DATE_EPOCH"); {
	case sde != "":
		secs, err := strconv.ParseInt(sde, 10, 64)
		if err != nil {
			return "", "", fmt.Errorf("invalid SOURCE_DATE_EPOCH %q: %w", sde, err)
		}
		finishedAt = time.Unix(secs, 0).UTC()
	case record || startedFlag != "":
		finishedAt = time.Now().UTC()
	default:
		return "", "", nil
	}
	startedAt := finishedAt
	if startedFlag != "" {
		startedAt, err = time.Parse(time.RFC3339, startedFlag)
		if err != nil {
			return "", "", fmt.Errorf("invalid --started-on %q: %w", startedFlag, err)
		}
		startedAt = startedAt.UTC()
		if startedAt.After(finishedAt) {
			return "", "", fmt.Errorf("--started-on %s is after the finish time %s", startedAt.Format(time.RFC3339), finishedAt.Format(time.RFC3339))
		}
	}
	return startedAt.Format(time.RFC3339), finishedAt.Format(time.RFC3339), nil
}

var digestSuffix = regexp.MustCompile(`^([a-z0-9]+):([0-9a-f]+)$`)

// parseMaterial parses URI or URI@algo:hex. The split is on the last '@' and
// only counts when what follows is a digest, because purls contain '@'.
func parseMaterial(s string) (resource, error) {
	if s == "" {
		return resource{}, fmt.Errorf("empty --material")
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		if m := digestSuffix.FindStringSubmatch(s[i+1:]); m != nil {
			return resource{URI: s[:i], Digest: map[string]string{m[1]: m[2]}}, nil
		}
	}
	return resource{URI: s}, nil
}

// commitAlgorithm mirrors BuildKit: 40 hex chars are sha1, 64 are sha256.
func commitAlgorithm(commit string) (string, error) {
	if !regexp.MustCompile(`^[0-9a-f]+$`).MatchString(commit) {
		return "", fmt.Errorf("--source-commit %q is not lower-case hex", commit)
	}
	switch len(commit) {
	case 40:
		return "sha1", nil
	case 64:
		return "sha256", nil
	}
	return "", fmt.Errorf("--source-commit %q must be 40 (sha1) or 64 (sha256) hex characters", commit)
}

func parseKeyValues(kvs []string) (map[string]string, error) {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("unable to parse %q (want key=value)", kv)
		}
		m[k] = v
	}
	return m, nil
}

func envOr(flagValue, env string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(env)
}
