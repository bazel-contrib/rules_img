package deploy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/api"
)

// A deploy manifest with no operations is normally a mistake worth reporting:
// it is what a deploy_operations filter that matched nothing looks like, and
// that is only visible once the manifests are merged. AllowEmpty is how a
// caller that knows better -- multi_deploy, whose operations all expanded to an
// empty MultipleDeployInfo -- asks for a no-op instead.
func TestDeployWithExtrasEmptyManifest(t *testing.T) {
	raw, err := json.Marshal(api.DeployManifest{
		Settings: api.DeploySettings{PushStrategy: "eager", LoadStrategy: "eager"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := DeployWithExtras(context.Background(), raw, DeployOptions{Jobs: 1, AllowEmpty: true}); err != nil {
		t.Errorf("DeployWithExtras with AllowEmpty: got error %v, want success", err)
	}

	err = DeployWithExtras(context.Background(), raw, DeployOptions{Jobs: 1})
	if err == nil {
		t.Fatal("DeployWithExtras without AllowEmpty: got success, want error")
	}
	if !strings.Contains(err.Error(), "no push, load, or registry_tag operations") {
		t.Errorf("DeployWithExtras without AllowEmpty: got error %v, want it to name the missing operations", err)
	}
}
