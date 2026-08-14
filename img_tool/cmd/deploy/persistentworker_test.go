package deploy

import (
	"context"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
)

func TestParseWorkerArgsInvocationID(t *testing.T) {
	for _, args := range [][]string{
		{"--request-file", "request.json", "--invocation-id", "1234"},
		{"--request-file=request.json", "--invocation_id=1234"},
	} {
		opts, err := parseWorkerArgs(args)
		if err != nil {
			t.Fatalf("parseWorkerArgs(%v): %v", args, err)
		}
		if opts.toolInvocationID != "1234" {
			t.Errorf("parseWorkerArgs(%v) invocation ID = %q, want 1234", args, opts.toolInvocationID)
		}
	}
}

func TestDeployRequestMetadataUsesLogicalRequest(t *testing.T) {
	ctx := protohelper.WithToolInvocationID(context.Background(), "invocation")
	ctx = withDeployRequestMetadata(ctx, []string{"bazel-out/bin/images/app.deploy.json"})
	got, ok := protohelper.RequestMetadataFromContext(ctx)
	if !ok {
		t.Fatal("request metadata is missing")
	}
	if got.ToolInvocationID != "invocation" {
		t.Errorf("tool invocation ID = %q, want invocation", got.ToolInvocationID)
	}
	if got.ActionMnemonic != "ImgDeploy" || got.TargetID != "bazel-out/bin/images/app.deploy.json" || got.ActionID != "rules_img:deploy:bazel-out/bin/images/app.deploy.json" {
		t.Errorf("logical deploy metadata = %+v", got)
	}
}

func TestWorkerRequestMetadataInheritsStartupInvocation(t *testing.T) {
	defaults := protohelper.RequestMetadata{
		ToolInvocationID: "startup-invocation",
		ActionID:         "rules_img:deploy",
		ActionMnemonic:   "ImgDeploy",
		TargetID:         "rules_img",
	}
	ctx := withWorkerRequestMetadata(context.Background(), defaults, []string{"app.deploy.json"}, "")
	got, ok := protohelper.RequestMetadataFromContext(ctx)
	if !ok {
		t.Fatal("request metadata is missing")
	}
	if got.ToolInvocationID != "startup-invocation" {
		t.Errorf("tool invocation ID = %q, want startup-invocation", got.ToolInvocationID)
	}
	if got.ActionID != "rules_img:deploy:app.deploy.json" || got.TargetID != "app.deploy.json" {
		t.Errorf("logical deploy metadata = %+v", got)
	}
}

func TestWorkerRequestMetadataOverridesStartupInvocation(t *testing.T) {
	defaults := protohelper.RequestMetadata{ToolInvocationID: "startup-invocation"}
	ctx := withWorkerRequestMetadata(context.Background(), defaults, []string{"app.deploy.json"}, "request-invocation")
	got, ok := protohelper.RequestMetadataFromContext(ctx)
	if !ok {
		t.Fatal("request metadata is missing")
	}
	if got.ToolInvocationID != "request-invocation" {
		t.Errorf("tool invocation ID = %q, want request-invocation", got.ToolInvocationID)
	}
}

func TestParseWorkerArgsRejectsMissingInvocationID(t *testing.T) {
	for _, args := range [][]string{
		{"--request-file=request.json", "--invocation-id"},
		{"--request-file=request.json", "--invocation-id="},
	} {
		if _, err := parseWorkerArgs(args); err == nil {
			t.Fatalf("expected %v to fail", args)
		}
	}
}
