package deploy

import (
	"io"
	"os"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/registry"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/registryopts"
)

// toolSettingsEnv are the variables applyToolSettings may write. Tests clear
// them through t.Setenv so that the process environment is restored afterwards.
var toolSettingsEnv = []string{
	envREAPIEndpoint,
	envREAPIInstanceName,
	registry.EnvCredentialHelper,
	registry.EnvCredentialHelperOCIRegistry,
	registry.EnvCredentialHelperRemoteCache,
	envRegistryAuthFile,
	registryopts.EnvInsecure,
}

func clearToolSettingsEnv(t *testing.T) {
	t.Helper()
	for _, name := range toolSettingsEnv {
		t.Setenv(name, "")
	}
}

// TestApplyToolSettingsFillsEmptyEnvironment covers the case the deploy manifest
// exists for: a launcher run outside `bazel run`, where nothing set any of these
// variables. The in-process effect of the insecure setting is covered by
// registryopts.TestSetInsecureDefault; here only the exported environment is
// checked, because the process-wide switch cannot be returned to "nothing
// decided it" from outside the package.
func TestApplyToolSettingsFillsEmptyEnvironment(t *testing.T) {
	clearToolSettingsEnv(t)
	previousInsecure := registryopts.Insecure()
	t.Cleanup(func() { registryopts.SetInsecure(previousInsecure) })

	applyToolSettings(&api.ToolSettings{
		RemoteCache:                 "grpcs://cache.example.com",
		RemoteInstanceName:          "instance",
		CredentialHelper:            "/helpers/generic",
		CredentialHelperOCIRegistry: "/helpers/registry",
		CredentialHelperRemoteCache: "/helpers/cache",
		RegistryAuthFile:            "/home/user/.docker/config.json",
		Insecure:                    true,
	}, io.Discard)

	for name, want := range map[string]string{
		envREAPIEndpoint:                        "grpcs://cache.example.com",
		envREAPIInstanceName:                    "instance",
		registry.EnvCredentialHelper:            "/helpers/generic",
		registry.EnvCredentialHelperOCIRegistry: "/helpers/registry",
		registry.EnvCredentialHelperRemoteCache: "/helpers/cache",
		envRegistryAuthFile:                     "/home/user/.docker/config.json",
		registryopts.EnvInsecure:                "1",
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestApplyToolSettingsEnvironmentWins covers the precedence: what the manifest
// recorded is only a fallback, so `bazel run` (which sets the environment from
// the same build settings) and an operator exporting IMG_* stay in control.
func TestApplyToolSettingsEnvironmentWins(t *testing.T) {
	clearToolSettingsEnv(t)
	t.Setenv(envREAPIEndpoint, "grpcs://from-environment.example.com")
	t.Setenv(registry.EnvCredentialHelper, "/from/environment")

	applyToolSettings(&api.ToolSettings{
		RemoteCache:        "grpcs://from-manifest.example.com",
		RemoteInstanceName: "instance",
		CredentialHelper:   "/from/manifest",
	}, io.Discard)

	if got := os.Getenv(envREAPIEndpoint); got != "grpcs://from-environment.example.com" {
		t.Errorf("%s = %q, want the environment's value", envREAPIEndpoint, got)
	}
	if got := os.Getenv(registry.EnvCredentialHelper); got != "/from/environment" {
		t.Errorf("%s = %q, want the environment's value", registry.EnvCredentialHelper, got)
	}
	// A setting the environment says nothing about still comes from the manifest.
	if got := os.Getenv(envREAPIInstanceName); got != "instance" {
		t.Errorf("%s = %q, want %q", envREAPIInstanceName, got, "instance")
	}
}

// TestApplyToolSettingsInsecureNotOverridden verifies that an explicit
// IMG_INSECURE=0 survives a manifest that recorded insecure access, and that a
// manifest never disables insecure mode the environment asked for.
func TestApplyToolSettingsInsecureNotOverridden(t *testing.T) {
	t.Run("environment disables", func(t *testing.T) {
		clearToolSettingsEnv(t)
		t.Setenv(registryopts.EnvInsecure, "0")
		previous := registryopts.Insecure()
		registryopts.SetInsecure(false)
		t.Cleanup(func() { registryopts.SetInsecure(previous) })

		applyToolSettings(&api.ToolSettings{Insecure: true}, io.Discard)

		if registryopts.Insecure() {
			t.Error("the deploy manifest enabled insecure mode against IMG_INSECURE=0")
		}
		if got := os.Getenv(registryopts.EnvInsecure); got != "0" {
			t.Errorf("%s = %q, want %q", registryopts.EnvInsecure, got, "0")
		}
	})
	t.Run("environment enables", func(t *testing.T) {
		clearToolSettingsEnv(t)
		t.Setenv(registryopts.EnvInsecure, "1")
		previous := registryopts.Insecure()
		registryopts.SetInsecure(true)
		t.Cleanup(func() { registryopts.SetInsecure(previous) })

		applyToolSettings(&api.ToolSettings{}, io.Discard)

		if !registryopts.Insecure() {
			t.Error("a manifest without insecure access disabled it")
		}
	})
}

// TestApplyToolSettingsNil verifies that a deploy manifest from before this
// existed (or one of a build-time push action, which records none) changes
// nothing.
func TestApplyToolSettingsNil(t *testing.T) {
	clearToolSettingsEnv(t)
	t.Setenv(envREAPIEndpoint, "grpcs://from-environment.example.com")

	applyToolSettings(nil, io.Discard)

	if got := os.Getenv(envREAPIEndpoint); got != "grpcs://from-environment.example.com" {
		t.Errorf("%s = %q, want the environment's value", envREAPIEndpoint, got)
	}
}
