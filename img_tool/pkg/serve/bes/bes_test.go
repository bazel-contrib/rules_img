package bes

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	build_event_stream_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/bazel/src/main/java/com/google/devtools/build/lib/buildeventstream"
)

type recordingCommitter struct {
	ctx context.Context
}

func (c *recordingCommitter) Commit(ctx context.Context, _ string, _ int64) error {
	c.ctx = ctx
	return nil
}

func TestProcessBuildEventPropagatesInvocationToCommit(t *testing.T) {
	completedID := &build_event_stream_proto.BuildEventId{
		Id: &build_event_stream_proto.BuildEventId_TargetCompleted{
			TargetCompleted: &build_event_stream_proto.BuildEventId_TargetCompletedId{Label: "//images:app"},
		},
	}
	fileSetID := &build_event_stream_proto.BuildEventId_NamedSetOfFilesId{Id: "outputs"}
	tracker := newTracker()
	if err := tracker.trackTargetCompleted(completedID); err != nil {
		t.Fatal(err)
	}
	tracker.namedSets[fileSetID.Id] = &build_event_stream_proto.NamedSetOfFiles{
		Files: []*build_event_stream_proto.File{{Digest: strings.Repeat("a", 64), Length: 123}},
	}
	event := &build_event_stream_proto.BuildEvent{
		Id: completedID,
		Payload: &build_event_stream_proto.BuildEvent_Completed{Completed: &build_event_stream_proto.TargetComplete{
			Success: true,
			OutputGroup: []*build_event_stream_proto.OutputGroup{{
				Name:     "default",
				FileSets: []*build_event_stream_proto.BuildEventId_NamedSetOfFilesId{fileSetID},
			}},
		}},
	}

	committer := &recordingCommitter{}
	b := &BES{syncer: committer}
	group, groupCtx := errgroup.WithContext(context.Background())
	groupCtx = protohelper.WithRequestMetadata(groupCtx, protohelper.RequestMetadata{
		ToolInvocationID: "invocation-id",
		ActionID:         "rules_img:bes",
		ActionMnemonic:   "ImgBESCommit",
		TargetID:         "rules_img",
	})
	if err := b.processBuildEvent(event, tracker, group, groupCtx); err != nil {
		t.Fatal(err)
	}
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}

	got, ok := protohelper.RequestMetadataFromContext(committer.ctx)
	if !ok {
		t.Fatal("commit context is missing request metadata")
	}
	if got.ToolInvocationID != "invocation-id" || got.ActionMnemonic != "ImgBESCommit" || got.TargetID != "//images:app" {
		t.Errorf("commit request metadata = %+v", got)
	}
	if !strings.HasPrefix(got.ActionID, "rules_img:bes:") || len(got.ActionID) != len("rules_img:bes:")+12 {
		t.Errorf("action ID = %q, want logical target event ID", got.ActionID)
	}
}
