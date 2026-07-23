package deploy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/go-containerregistry/pkg/name"
	registryv1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"

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
}

// signDecision is the outcome of deciding whether and how a push operation is
// signed.
type signDecision struct {
	sign       bool
	bestEffort bool
	setting    *api.Descriptor // explicit op setting, or nil to use the default
	targets    map[string]bool // effective subject targets (roots/child_manifests)
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
		repository, err := name.NewRepository(reg + "/" + repo)
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

// forEachSignature signs every eligible push operation and invokes emit once
// per signed subject with the signer plugin's artifact images. It centralizes
// the sign decision, plugin invocation and best-effort error handling shared by
// the registry push and --sink paths. Signing is sequential so interactive
// plugin prompts do not interleave. precheck (optional) runs before the plugin
// is invoked so an operation that cannot be signed is skipped without prompting.
func forEachSignature(ctx context.Context, pushOps []api.IndexedPushDeployOperation, settings api.DeploySettings, store *signer.SettingStore, rfHandle *runfiles.Runfiles, opts signOptions, precheck func(op api.IndexedPushDeployOperation) error, emit signEmitter) error {
	override := normalizeTargets(opts.targetOverride)

	for _, op := range pushOps {
		decision := decideSigning(op, store, opts.force, override)
		if !decision.sign {
			continue
		}
		reg, repo := opRegistry(op, opts), opRepository(op, opts)
		if precheck != nil {
			if err := precheck(op); err != nil {
				if decision.bestEffort {
					fmt.Fprintf(os.Stderr, "warning: %s\n", err)
					continue
				}
				return err
			}
		}
		if err := signOneEmit(ctx, op, decision, store, settings.DefaultSignSetting, rfHandle, emit); err != nil {
			if decision.bestEffort {
				fmt.Fprintf(os.Stderr, "warning: signing %s/%s@%s skipped: %v\n", reg, repo, op.Root.Digest, err)
				continue
			}
			return fmt.Errorf("signing %s/%s@%s: %w", reg, repo, op.Root.Digest, err)
		}
	}
	return nil
}

// signOneEmit signs every selected subject of op and hands the resulting
// artifacts to emit.
func signOneEmit(ctx context.Context, op api.IndexedPushDeployOperation, decision signDecision, store *signer.SettingStore, manifestDefault *api.Descriptor, rf *runfiles.Runfiles, emit signEmitter) error {
	cfg, err := store.Resolve(decision.setting, manifestDefault)
	if err != nil {
		return err
	}
	sub, err := signer.NewSubprocess(cfg, rf)
	if err != nil {
		return err
	}

	for _, subjectDesc := range collectSubjects(op, decision.targets) {
		vDesc, err := signer.SubjectDescriptor(subjectDesc)
		if err != nil {
			return err
		}
		imgs, err := sub.SignArtifacts(ctx, vDesc)
		if err != nil {
			return fmt.Errorf("signing subject %s: %w", subjectDesc.Digest, err)
		}
		if err := emit(ctx, op, subjectDesc, imgs); err != nil {
			return err
		}
	}
	return nil
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

// decideSigning determines whether op should be signed and with which effective
// targets. Precedence: an operation with an explicit Sign config is always
// signed; --sign_force signs any op using the default; and a --sign_targets
// override that includes "referrers" signs referrer ops using the default.
func decideSigning(op api.IndexedPushDeployOperation, store *signer.SettingStore, force bool, override map[string]bool) signDecision {
	switch {
	case op.Sign != nil:
		return signDecision{
			sign:       true,
			bestEffort: op.Sign.BestEffort,
			setting:    op.Sign.Setting,
			targets:    chooseTargets(op.Sign.Targets, override),
		}
	case force && store.HasDefault():
		return signDecision{sign: true, targets: chooseTargets(nil, override)}
	case op.Referrer && override[api.SignTargetReferrers] && store.HasDefault():
		return signDecision{sign: true, targets: chooseTargets(nil, override)}
	default:
		return signDecision{}
	}
}

// collectSubjects returns the descriptors of op to sign, honoring the effective
// target selection. Roots are always included; index children are added when
// the child_manifests target is selected.
func collectSubjects(op api.IndexedPushDeployOperation, targets map[string]bool) []api.Descriptor {
	var subjects []api.Descriptor
	seen := map[string]bool{}
	add := func(d api.Descriptor) {
		if d.Digest == "" || seen[d.Digest] {
			return
		}
		seen[d.Digest] = true
		subjects = append(subjects, d)
	}

	add(op.Root)
	if targets[api.SignTargetChildManifests] && op.RootKind == "index" {
		for _, m := range op.Manifests {
			add(m.Descriptor)
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
