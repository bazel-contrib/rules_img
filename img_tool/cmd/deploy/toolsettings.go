package deploy

import (
	"fmt"
	"io"
	"os"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// applyToolSettings applies the `img` tool settings a deploy manifest recorded
// at build time (see [api.ToolSettings]): the remote cache the lazy push
// strategy reads layers from, the credential helpers, the registry auth file and
// insecure-registry access.
//
// They are the lowest-priority source of every setting. The environment is what
// each of these settings is ultimately read from -- by this process and by the
// credential helper subprocesses it starts -- so a recorded setting is applied
// by exporting it, and only where the environment says nothing. An explicit
// (non-empty) environment variable is left alone, and so is anything a flag
// already decided: the one setting that has a flag (--insecure) is applied
// through [registryopts.SetInsecureDefault], which defers to both.
//
// This is what makes a generated image_push / image_load / multi_deploy launcher
// work when it is run outside `bazel run` -- built, copied to another machine and
// executed directly -- where nothing applies the launcher's RunEnvironmentInfo.
func applyToolSettings(settings *api.ToolSettings, warn io.Writer) {
	if settings == nil {
		return
	}
	for _, s := range []struct {
		env   string
		value string
	}{
		{env: envREAPIEndpoint, value: settings.RemoteCache},
		{env: envREAPIInstanceName, value: settings.RemoteInstanceName},
		{env: registry.EnvCredentialHelper, value: settings.CredentialHelper},
		{env: registry.EnvCredentialHelperOCIRegistry, value: settings.CredentialHelperOCIRegistry},
		{env: registry.EnvCredentialHelperRemoteCache, value: settings.CredentialHelperRemoteCache},
		{env: envRegistryAuthFile, value: settings.RegistryAuthFile},
	} {
		setenvDefault(s.env, s.value, warn)
	}

	// Insecure access is enabling only (like the global --insecure flag), so a
	// recorded false says nothing and is never exported.
	if settings.Insecure {
		setenvDefault(registryopts.EnvInsecure, "1", warn)
	}
	// The in-process switch is read from a variable initialized before the
	// manifest was parsed, so exporting IMG_INSECURE above (which is for the
	// subprocesses) is not enough.
	registryopts.SetInsecureDefault(settings.Insecure)
}

// setenvDefault exports value as the environment variable name, unless the
// environment already has a non-empty value for it. An empty recorded value says
// nothing. A Bazel RunEnvironmentInfo may set a variable to the empty string, so
// empty counts as unset here just as it does everywhere the img tool reads these
// variables.
func setenvDefault(name, value string, warn io.Writer) {
	if value == "" || os.Getenv(name) != "" {
		return
	}
	if err := os.Setenv(name, value); err != nil {
		// Nothing here is fatal on its own: the deploy may well not need the
		// setting (a credential helper for a registry that needs no auth, a
		// remote cache for layers that are all in runfiles anyway), and the
		// failure it would cause further down says more than this would.
		fmt.Fprintf(warn, "WARNING: not applying %s from the deploy manifest: %v\n", name, err)
	}
}
