package deploymetadata

import (
	"flag"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// toolSettingsFlags are the `--tool-*` flags that record the `img` tool settings
// Bazel resolved at build time -- the remote cache lazy layers are read from,
// the credential helpers, insecure-registry access -- in the deploy manifest, so
// that a deploy launcher is not left with nothing when it runs outside
// `bazel run` (see [api.ToolSettings]).
//
// Both `img deploy-metadata` and `img deploy-merge` take them, so a merged
// deployment (multi_deploy, or an image with several push_specs) records the
// same settings as a single one.
type toolSettingsFlags struct {
	settings api.ToolSettings
}

func (f *toolSettingsFlags) register(flagSet *flag.FlagSet) {
	flagSet.StringVar(&f.settings.RemoteCache, "tool-remote-cache", "", `(Optional) Bazel remote cache / REAPI endpoint that layer blobs are read from under the lazy push strategy. Recorded in the deploy manifest as a fallback for $IMG_REAPI_ENDPOINT.`)
	flagSet.StringVar(&f.settings.RemoteInstanceName, "tool-remote-instance-name", "", `(Optional) REAPI instance name used for remote cache requests. Recorded in the deploy manifest as a fallback for $IMG_REAPI_INSTANCE_NAME.`)
	flagSet.StringVar(&f.settings.CredentialHelper, "tool-credential-helper", "", `(Optional) Bazel credential helper used for operations without a more specific helper. Recorded in the deploy manifest as a fallback for $IMG_CREDENTIAL_HELPER.`)
	flagSet.StringVar(&f.settings.CredentialHelperOCIRegistry, "tool-credential-helper-oci-registry", "", `(Optional) credential helper used for OCI registry operations. Recorded in the deploy manifest as a fallback for $IMG_CREDENTIAL_HELPER_OCI_REGISTRY.`)
	flagSet.StringVar(&f.settings.CredentialHelperRemoteCache, "tool-credential-helper-remote-cache", "", `(Optional) credential helper used to authenticate gRPC calls to the remote cache. Recorded in the deploy manifest as a fallback for $IMG_CREDENTIAL_HELPER_REMOTE_CACHE.`)
	flagSet.StringVar(&f.settings.RegistryAuthFile, "tool-registry-auth-file", "", `(Optional) Docker/Podman config file registry credentials are read from. Recorded in the deploy manifest as a fallback for $REGISTRY_AUTH_FILE.`)
	flagSet.BoolVar(&f.settings.Insecure, "tool-insecure", false, `(Optional) address registries over plain HTTP, accepting untrusted TLS certificates. Recorded in the deploy manifest as a fallback for $IMG_INSECURE. Enabling only: an explicit IMG_INSECURE=0 still wins.`)
}

// value returns the recorded settings, or nil when no `--tool-*` flag was given,
// which keeps them out of the deploy manifest entirely.
func (f *toolSettingsFlags) value() *api.ToolSettings {
	if f == nil || f.settings.Empty() {
		return nil
	}
	settings := f.settings
	return &settings
}
