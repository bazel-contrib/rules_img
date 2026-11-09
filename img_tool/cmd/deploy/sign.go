package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	registrytypes "github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/ocilayout"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/signer"
)

// signOptions carries the runtime signing configuration derived from the CLI.
type signOptions struct {
	settingFiles       []string
	defaultSetting     string
	force              bool
	targetOverride     []string
	overrideRegistry   string
	overrideRepository string

	// pushTransport and jobs configure the pusher created for registry-path
	// signing (applySignOperations). They are unused by the --sink path, which
	// captures signatures locally instead of pushing.
	pushTransport http.RoundTripper
	jobs          int

	// images, when set, lets signer.ProtocolRequest plugins be told the platform
	// of each single-platform image (read from its config).
	images imageSource
	// additionalTags are the tags added with --tag, which every push operation
	// is pushed under besides its own.
	additionalTags []string
}

// signStep is one plugin of an operation's signing chain.
type signStep struct {
	setting *api.Descriptor // explicit setting, or nil to use the default
	targets map[string]bool // effective targets (roots/child_manifests/...)
}

// signDecision is the outcome of deciding whether and how a push operation is
// signed: an ordered chain of plugins. Plugins run in this order, and a step only
// sees what the steps before it created.
type signDecision struct {
	sign       bool
	bestEffort bool
	steps      []signStep
}

// signEmitter attaches the signature artifacts produced for one subject of a
// push operation to their destination (a registry repository or a local sink).
// It is the only part of the signing flow that differs between the registry
// push and --sink paths.
type signEmitter func(ctx context.Context, op api.IndexedPushDeployOperation, subject api.Descriptor, imgs []registryv1.Image) error

// applySignOperations signs the eligible push operations after they have been
// pushed to the registry. Referrers are attached to each signed subject.
// Signing runs sequentially so interactive signer prompts (touch/PIN/OIDC) do
// not interleave.
//
// It creates its own pusher: on main the deploy push path uses an
// internally-created pusher in PushAll, and the top-level pusher is scoped to
// registry_tag operations, so neither is available here.
func applySignOperations(ctx context.Context, pushOps []api.IndexedPushDeployOperation, settings api.DeploySettings, opts signOptions) error {
	store, rfHandle, err := signStore(opts)
	if err != nil {
		return err
	}
	if !anyOpSigns(pushOps, store, opts) {
		return nil
	}

	// Referrers are attached to their subject in the registry, so a pusher is
	// required. The bes strategy has no deploy-time pusher (the build-event
	// syncer owns the push), so signing is unsupported there and is reported
	// per operation (honoring best-effort) by the precheck below.
	var pusher *remote.Pusher
	if settings.PushStrategy != "bes" {
		pusher, err = remote.NewPusher(registryopts.Default().WithTransport(opts.pushTransport).WithJobs(opts.jobs).Remote()...)
		if err != nil {
			return fmt.Errorf("creating pusher for signing: %w", err)
		}
	}

	precheck := func(op api.IndexedPushDeployOperation) error {
		if pusher != nil {
			return nil
		}
		reg, repo := opRegistry(op, opts), opRepository(op, opts)
		return fmt.Errorf("cannot sign %s/%s@%s: deploy-time signing is unsupported for push strategy %q", reg, repo, op.Root.Digest, settings.PushStrategy)
	}
	emit := func(ctx context.Context, op api.IndexedPushDeployOperation, subject api.Descriptor, imgs []registryv1.Image) error {
		reg, repo := opRegistry(op, opts), opRepository(op, opts)
		repository, err := name.NewRepository(reg+"/"+repo, registryopts.NameOptions()...)
		if err != nil {
			return fmt.Errorf("parsing repository %q: %w", reg+"/"+repo, err)
		}
		pushed, err := signer.PushReferrers(ctx, pusher, repository, imgs)
		if err != nil {
			return err
		}
		for _, p := range pushed {
			fmt.Fprintf(os.Stderr, "    signed %s/%s@%s -> %s\n", reg, repo, subject.Digest, p)
		}
		return nil
	}
	return forEachSignature(ctx, pushOps, settings, store, rfHandle, opts, precheck, emit)
}

// signIntoSink signs the eligible push operations and captures the resulting
// signature artifacts into the sink s as digest-only referrer manifests of
// their subjects. It is the --sink counterpart of applySignOperations: the
// signature manifests carry the OCI 1.1 `subject` field, so they land in the
// same layout/repository as the subject and, for distribution sinks, appear in
// the generated referrers/ listings (which are built from the on-disk manifests
// at Close). It must therefore run before the sink is finalized.
func signIntoSink(ctx context.Context, s sink, pushOps []api.IndexedPushDeployOperation, settings api.DeploySettings, opts signOptions) error {
	store, rfHandle, err := signStore(opts)
	if err != nil {
		return err
	}
	if !anyOpSigns(pushOps, store, opts) {
		return nil
	}
	emit := func(ctx context.Context, op api.IndexedPushDeployOperation, subject api.Descriptor, imgs []registryv1.Image) error {
		reg, repo := opRegistry(op, opts), opRepository(op, opts)
		for _, img := range imgs {
			si, err := signatureSinkImage(img, reg, repo)
			if err != nil {
				return err
			}
			if err := s.AddImage(ctx, si); err != nil {
				return err
			}
			d, err := img.Digest()
			if err != nil {
				return fmt.Errorf("computing signature digest: %w", err)
			}
			fmt.Fprintf(os.Stderr, "    signed %s/%s@%s -> %s (sink)\n", reg, repo, subject.Digest, d)
		}
		return nil
	}
	return forEachSignature(ctx, pushOps, settings, store, rfHandle, opts, nil, emit)
}

// signStore discovers the sign_settings available to this deploy (the runfiles
// sign_settings area plus any --sign_setting_file) and resolves the
// --default_sign_setting override. A runfiles handle is optional (settings may
// come solely from --sign_setting_file), so failing to construct one is not
// fatal.
func signStore(opts signOptions) (*signer.SettingStore, *runfiles.Runfiles, error) {
	var rfHandle *runfiles.Runfiles
	if rf, err := runfiles.New(); err == nil {
		rfHandle = rf
	}
	store, err := signer.Discover(rfHandle, opts.settingFiles, opts.defaultSetting)
	if err != nil {
		return nil, nil, fmt.Errorf("discovering sign settings: %w", err)
	}
	return store, rfHandle, nil
}

// anyOpSigns reports whether at least one push operation would be signed, so
// callers can skip pusher creation / plugin invocation entirely in the common
// case where nothing is signed.
func anyOpSigns(pushOps []api.IndexedPushDeployOperation, store *signer.SettingStore, opts signOptions) bool {
	override := normalizeTargets(opts.targetOverride)
	for _, op := range pushOps {
		if decideSigning(op, store, opts.force, override).sign {
			return true
		}
	}
	return false
}

// signSubject is one descriptor of a push operation that is to be signed.
type signSubject struct {
	desc api.Descriptor
	// artifactType is the artifactType of a plugin artifact subject.
	artifactType string
	// role is signer.RoleRoot, signer.RoleChildManifest or signer.RolePluginArtifact.
	role string
	// parent is the index a child manifest belongs to, or what a plugin artifact
	// is attached to.
	parent *api.Descriptor
}

// signCandidate is one subject of one push operation that is to be signed.
type signCandidate struct {
	op                   api.IndexedPushDeployOperation
	registry, repository string
	decision             signDecision
	cfg                  signer.SignSettingConfig
	subject              signSubject
	tags                 []string
}

// signJob is one plugin invocation. Under signer.ProtocolDescriptor it is a
// single subject of a single operation, exactly as before. Under
// signer.ProtocolRequest it is a registry, repository and digest, with the tags
// of every operation that pushes the subject there.
type signJob struct {
	signCandidate
	// ops are the indexes of the operations this job serves. A plugin artifact is
	// only offered to later steps of the operations that caused it.
	ops map[int]bool
}

// mergeSignJobs turns the signing candidates, in operation order, into plugin
// invocations. Candidates of a signer.ProtocolRequest setting that push the same
// digest to the same registry and repository share one job, so the plugin is not
// asked to attest an image once per tag or per operation, and the tags are merged.
// Everything else stays one job per candidate.
func mergeSignJobs(cands []signCandidate) []*signJob {
	var jobs []*signJob
	byKey := map[string]*signJob{}
	for _, c := range cands {
		c.tags = sortedUnique(c.tags)
		if c.cfg.Protocol != signer.ProtocolRequest {
			jobs = append(jobs, &signJob{c, map[int]bool{c.op.I: true}})
			continue
		}
		cfgJSON, _ := json.Marshal(c.cfg) // map keys are sorted, so this is stable
		key := strings.Join([]string{string(cfgJSON), c.registry, c.repository, c.subject.desc.Digest}, "\x00")
		job, ok := byKey[key]
		if !ok {
			job = &signJob{c, map[int]bool{c.op.I: true}}
			byKey[key] = job
			jobs = append(jobs, job)
			continue
		}
		job.ops[c.op.I] = true
		job.tags = sortedUnique(append(job.tags, c.tags...))
		// A merged job fails the deploy unless every operation asked for best effort.
		job.decision.bestEffort = job.decision.bestEffort && c.decision.bestEffort
		// The same manifest can be pushed on its own and as part of an index; it
		// is a root if it is one anywhere.
		if c.subject.role == signer.RoleRoot && job.subject.role != signer.RoleRoot {
			job.subject = c.subject
		}
	}
	return jobs
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	w := 0
	for i, s := range out {
		if i == 0 || s != out[w-1] {
			out[w] = s
			w++
		}
	}
	return out[:w]
}

// imageSource gives access to the images of the deploy (a *deployvfs.VFS). It is
// optional: without it, requests carry no platform.
type imageSource interface {
	Image(registryv1.Hash) (registryv1.Image, error)
}

// platformOf reads the platform of a single-platform image from its config. It
// returns nil for indexes and whenever the config cannot be read, because the
// platform is a convenience for the plugin, not something signing depends on.
func platformOf(images imageSource, d api.Descriptor) *registryv1.Platform {
	if images == nil || registrytypes.MediaType(d.MediaType).IsIndex() {
		return nil
	}
	h, err := registryv1.NewHash(d.Digest)
	if err != nil {
		return nil
	}
	img, err := images.Image(h)
	if err != nil {
		return nil
	}
	cfg, err := img.ConfigFile()
	if err != nil || cfg == nil || (cfg.OS == "" && cfg.Architecture == "") {
		return nil
	}
	return &registryv1.Platform{OS: cfg.OS, Architecture: cfg.Architecture, Variant: cfg.Variant, OSVersion: cfg.OSVersion}
}

// pluginArtifact is a manifest a signer plugin created for a subject.
type pluginArtifact struct {
	desc         api.Descriptor
	artifactType string
	// subject is what the artifact is attached to.
	subject *api.Descriptor
	// registry and repository are where it was attached.
	registry, repository string
	// ops are the operations whose chain caused it.
	ops map[int]bool
}

func newPluginArtifact(img registryv1.Image, job *signJob) (*pluginArtifact, error) {
	digest, err := img.Digest()
	if err != nil {
		return nil, fmt.Errorf("computing artifact digest: %w", err)
	}
	raw, err := img.RawManifest()
	if err != nil {
		return nil, fmt.Errorf("reading artifact manifest: %w", err)
	}
	mediaType, err := img.MediaType()
	if err != nil {
		return nil, fmt.Errorf("reading artifact media type: %w", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, fmt.Errorf("parsing artifact manifest: %w", err)
	}
	artifact := &pluginArtifact{
		desc:         api.Descriptor{MediaType: string(mediaType), Digest: digest.String(), Size: int64(len(raw))},
		artifactType: manifest.ArtifactType,
		registry:     job.registry,
		repository:   job.repository,
		ops:          job.ops,
	}
	if manifest.Subject != nil {
		artifact.subject = &api.Descriptor{MediaType: string(manifest.Subject.MediaType), Digest: manifest.Subject.Digest.String(), Size: manifest.Subject.Size}
	}
	return artifact, nil
}

// plannedOp is a push operation that is to be signed, with its chain resolved.
type plannedOp struct {
	op                   api.IndexedPushDeployOperation
	registry, repository string
	decision             signDecision
	// cfgs are the resolved plugin configs, one per step.
	cfgs []signer.SignSettingConfig
	tags []string
}

// forEachSignature signs every eligible push operation and invokes emit once
// per signed subject with the signer plugin's artifact images. It centralizes
// the sign decision, plugin invocation and best-effort error handling shared by
// the registry push and --sink paths. Signing is sequential so interactive
// plugin prompts do not interleave. precheck (optional) runs before the plugin
// is invoked so an operation that cannot be signed is skipped without prompting.
//
// An operation's plugins form a chain that runs in order: every plugin of step 0
// first, then step 1, and so on, across all operations. A step with the
// plugin_artifacts target also processes the manifests the earlier steps created
// for that operation. It never sees its own output or that of later steps, so a
// chain cannot recurse, and each artifact is pushed before the next step is asked
// to attach something to it.
//
// All operations are planned before the first plugin runs, so a configuration
// error in a later operation is reported before anything is signed.
func forEachSignature(ctx context.Context, pushOps []api.IndexedPushDeployOperation, settings api.DeploySettings, store *signer.SettingStore, rfHandle *runfiles.Runfiles, opts signOptions, precheck func(op api.IndexedPushDeployOperation) error, emit signEmitter) error {
	override := normalizeTargets(opts.targetOverride)

	var planned []plannedOp
	chainLen := 0
	for _, op := range pushOps {
		decision := decideSigning(op, store, opts.force, override)
		if !decision.sign {
			continue
		}
		reg, repo := opRegistry(op, opts), opRepository(op, opts)
		skipOrFail := func(err error) error {
			if decision.bestEffort {
				fmt.Fprintf(os.Stderr, "warning: %s\n", err)
				return nil
			}
			return err
		}
		if precheck != nil {
			if err := precheck(op); err != nil {
				if err := skipOrFail(err); err != nil {
					return err
				}
				continue
			}
		}
		cfgs := make([]signer.SignSettingConfig, 0, len(decision.steps))
		var resolveErr error
		for _, step := range decision.steps {
			cfg, err := store.Resolve(step.setting, settings.DefaultSignSetting)
			if err != nil {
				resolveErr = err
				break
			}
			cfgs = append(cfgs, cfg)
		}
		if resolveErr != nil {
			if err := skipOrFail(fmt.Errorf("signing %s/%s@%s skipped: %v", reg, repo, op.Root.Digest, resolveErr)); err != nil {
				return fmt.Errorf("signing %s/%s@%s: %w", reg, repo, op.Root.Digest, resolveErr)
			}
			continue
		}
		planned = append(planned, plannedOp{
			op: op, registry: reg, repository: repo, decision: decision, cfgs: cfgs,
			tags: append(append([]string(nil), op.Tags...), opts.additionalTags...),
		})
		if len(cfgs) > chainLen {
			chainLen = len(cfgs)
		}
	}

	var deploy *signer.Deploy
	deployInfo := func() (signer.Deploy, error) {
		if deploy == nil {
			d, err := signer.NewDeploy(settings.PushStrategy)
			if err != nil {
				return signer.Deploy{}, err
			}
			deploy = &d
		}
		return *deploy, nil
	}
	plugins := map[string]*signer.Subprocess{}
	var produced []*pluginArtifact
	for step := 0; step < chainLen; step++ {
		var cands []signCandidate
		for _, p := range planned {
			if step >= len(p.cfgs) {
				continue
			}
			targets := p.decision.steps[step].targets
			base := signCandidate{
				op: p.op, registry: p.registry, repository: p.repository,
				decision: p.decision, cfg: p.cfgs[step], tags: p.tags,
			}
			for _, subject := range collectSubjects(p.op, targets) {
				c := base
				c.subject = subject
				cands = append(cands, c)
			}
			if !targets[api.SignTargetPluginArtifacts] {
				continue
			}
			// produced only holds what earlier steps created: a step's output is
			// added after the whole step ran, so a step never sees its own.
			for _, a := range produced {
				if !a.ops[p.op.I] || a.registry != p.registry || a.repository != p.repository {
					continue
				}
				c := base
				c.tags = nil // artifacts are attached by digest
				c.subject = signSubject{desc: a.desc, artifactType: a.artifactType, role: signer.RolePluginArtifact, parent: a.subject}
				cands = append(cands, c)
			}
		}

		for _, job := range mergeSignJobs(cands) {
			imgs, err := runSignJob(ctx, job, rfHandle, plugins, deployInfo, opts.images, emit)
			if err == nil {
				// What this step created is offered to the steps after it only.
				for _, img := range imgs {
					var artifact *pluginArtifact
					if artifact, err = newPluginArtifact(img, job); err != nil {
						break
					}
					produced = append(produced, artifact)
				}
			}
			if err != nil {
				if job.decision.bestEffort {
					fmt.Fprintf(os.Stderr, "warning: signing %s/%s@%s skipped: %v\n", job.registry, job.repository, job.subject.desc.Digest, err)
					continue
				}
				return fmt.Errorf("signing %s/%s@%s: %w", job.registry, job.repository, job.subject.desc.Digest, err)
			}
		}
	}
	return nil
}

// runSignJob invokes the plugin for one job, hands the resulting artifacts to
// emit, and returns them.
func runSignJob(ctx context.Context, job *signJob, rf *runfiles.Runfiles, plugins map[string]*signer.Subprocess, deployInfo func() (signer.Deploy, error), images imageSource, emit signEmitter) ([]registryv1.Image, error) {
	cfgJSON, _ := json.Marshal(job.cfg)
	sub, ok := plugins[string(cfgJSON)]
	if !ok {
		var err error
		if sub, err = signer.NewSubprocess(job.cfg, rf); err != nil {
			return nil, err
		}
		plugins[string(cfgJSON)] = sub
	}

	subject, err := signer.SubjectDescriptor(job.subject.desc)
	if err != nil {
		return nil, err
	}
	subject.ArtifactType = job.subject.artifactType
	var imgs []registryv1.Image
	if sub.Protocol() == signer.ProtocolRequest {
		deploy, err := deployInfo()
		if err != nil {
			return nil, err
		}
		if job.subject.role != signer.RolePluginArtifact {
			subject.Platform = platformOf(images, job.subject.desc)
		}
		req := signer.Request{
			Subject: subject,
			Role:    job.subject.role,
			Destination: signer.Destination{
				Registry:   job.registry,
				Repository: job.repository,
				Tags:       job.tags,
				References: signer.References(job.registry, job.repository, job.tags, job.subject.desc.Digest),
			},
			Deploy: deploy,
		}
		if job.subject.parent != nil {
			parent, err := signer.SubjectDescriptor(*job.subject.parent)
			if err != nil {
				return nil, err
			}
			req.Parent = &parent
		}
		imgs, err = sub.SignRequest(ctx, req)
	} else {
		imgs, err = sub.SignArtifacts(ctx, subject)
	}
	if err != nil {
		return nil, fmt.Errorf("signing subject %s: %w", job.subject.desc.Digest, err)
	}
	if err := emit(ctx, job.op, job.subject.desc, imgs); err != nil {
		return nil, err
	}
	return imgs, nil
}

// signatureSinkImage converts a signature artifact image (produced by a signer
// plugin) into a sinkImage. The signature is a self-contained OCI image
// manifest carrying a `subject` field; its blobs are held in memory (signature
// artifacts are tiny). It is captured with no tags (Refs empty) and the
// subject's registry/repository, so distribution sinks place it under the
// subject's repository and pick it up in the referrers listing.
func signatureSinkImage(img registryv1.Image, registry, repository string) (sinkImage, error) {
	raw, err := img.RawManifest()
	if err != nil {
		return sinkImage{}, fmt.Errorf("reading signature manifest: %w", err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return sinkImage{}, fmt.Errorf("parsing signature manifest: %w", err)
	}
	mediaType, err := img.MediaType()
	if err != nil {
		return sinkImage{}, fmt.Errorf("reading signature media type: %w", err)
	}

	mem := ocilayout.NewMemBlobSource()
	config, err := img.RawConfigFile()
	if err != nil {
		return sinkImage{}, fmt.Errorf("reading signature config: %w", err)
	}
	mem.Add(manifest.Config.Digest.Hex, config)
	layers, err := img.Layers()
	if err != nil {
		return sinkImage{}, fmt.Errorf("reading signature layers: %w", err)
	}
	for _, l := range layers {
		ld, err := l.Digest()
		if err != nil {
			return sinkImage{}, fmt.Errorf("computing signature layer digest: %w", err)
		}
		rc, err := l.Compressed()
		if err != nil {
			return sinkImage{}, fmt.Errorf("opening signature layer %s: %w", ld, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return sinkImage{}, fmt.Errorf("reading signature layer %s: %w", ld, err)
		}
		mem.Add(ld.Hex, data)
	}

	child := ocilayout.ManifestInputFromVFS(mem, manifest, raw, nil)
	return sinkImage{
		Registry:   registry,
		Repository: repository,
		Root: resolvedRoot{
			RootData:     raw,
			MediaType:    mediaType,
			ArtifactType: manifest.ArtifactType,
			IsIndex:      false,
			Children:     []ocilayout.ManifestInput{child},
		},
	}, nil
}

// decideSigning determines whether op should be signed and with which chain of
// plugins. Precedence: an operation with an explicit Sign config is always
// signed; --sign_force signs any op using the default; and a --sign_targets
// override that includes "referrers" signs referrer ops using the default. The
// runtime --sign_targets override replaces the targets of every step.
func decideSigning(op api.IndexedPushDeployOperation, store *signer.SettingStore, force bool, override map[string]bool) signDecision {
	defaultStep := func() []signStep {
		return []signStep{{targets: chooseTargets(nil, override)}}
	}
	switch {
	case op.Sign != nil:
		var steps []signStep
		for _, step := range op.Sign.EffectiveSteps() {
			steps = append(steps, signStep{setting: step.Setting, targets: chooseTargets(step.Targets, override)})
		}
		return signDecision{sign: true, bestEffort: op.Sign.BestEffort, steps: steps}
	case force && store.HasDefault():
		return signDecision{sign: true, steps: defaultStep()}
	case op.Referrer && override[api.SignTargetReferrers] && store.HasDefault():
		return signDecision{sign: true, steps: defaultStep()}
	default:
		return signDecision{}
	}
}

// collectSubjects returns the descriptors of op to sign, honoring the effective
// target selection. Roots are always included; index children are added when
// the child_manifests target is selected.
func collectSubjects(op api.IndexedPushDeployOperation, targets map[string]bool) []signSubject {
	var subjects []signSubject
	seen := map[string]bool{}
	add := func(s signSubject) {
		if s.desc.Digest == "" || seen[s.desc.Digest] {
			return
		}
		seen[s.desc.Digest] = true
		subjects = append(subjects, s)
	}

	add(signSubject{desc: op.Root, role: signer.RoleRoot})
	if targets[api.SignTargetChildManifests] && op.RootKind == "index" {
		root := op.Root
		for _, m := range op.Manifests {
			add(signSubject{desc: m.Descriptor, role: signer.RoleChildManifest, parent: &root})
		}
	}
	return subjects
}

// chooseTargets resolves the effective subject-target set for an operation. A
// runtime override wins; otherwise the operation's build-time targets are used;
// otherwise the default is roots only. Roots are always included.
func chooseTargets(opTargets []string, override map[string]bool) map[string]bool {
	if override != nil {
		return override
	}
	if t := normalizeTargets(opTargets); t != nil {
		return t
	}
	return map[string]bool{api.SignTargetRoots: true}
}

// normalizeTargets converts a raw target list into a set, expanding "all". It
// returns nil for an empty list so callers can distinguish "no override".
func normalizeTargets(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	m := map[string]bool{}
	for _, t := range list {
		switch t {
		case "all":
			m[api.SignTargetRoots] = true
			m[api.SignTargetChildManifests] = true
			m[api.SignTargetReferrers] = true
			m[api.SignTargetPluginArtifacts] = true
		default:
			m[t] = true
		}
	}
	m[api.SignTargetRoots] = true // roots are always signed
	return m
}

func opRegistry(op api.IndexedPushDeployOperation, opts signOptions) string {
	if opts.overrideRegistry != "" {
		return opts.overrideRegistry
	}
	return op.Registry
}

func opRepository(op api.IndexedPushDeployOperation, opts signOptions) string {
	if opts.overrideRepository != "" {
		return opts.overrideRepository
	}
	return op.Repository
}

// splitCommaList splits a comma-separated flag value into trimmed, non-empty
// elements.
func splitCommaList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
