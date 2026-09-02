package bes

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"

	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/auth/protohelper"
	build_event_stream_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/bazel/src/main/java/com/google/devtools/build/lib/buildeventstream"
	bes_proto "github.com/bazel-contrib/rules_img/img_tool/pkg/proto/build_event_service"
	"github.com/bazel-contrib/rules_img/img_tool/pkg/serve/bes/syncer"
)

type CommitMode int

const (
	// CommitModeBackground runs commits in a global background errgroup
	// Good for throughput but may delay graceful shutdown
	CommitModeBackground CommitMode = iota

	// CommitModePerStream waits for all commits per Bazel invocation (stream)
	// Good for ensuring all images are uploaded before stream closes
	CommitModePerStream
)

type imageCommitter interface {
	Commit(context.Context, string, int64) error
}

type BES struct {
	bes_proto.UnimplementedPublishBuildEventServer
	syncer     imageCommitter
	commitMode CommitMode

	// Global errgroup for background commits
	globalErrGroup *errgroup.Group
	globalCtx      context.Context
}

// New creates a new BES server with the given syncer and commit mode.
func New(s *syncer.Syncer, mode CommitMode) *BES {
	globalCtx := context.Background()
	globalErrGroup, globalCtx := errgroup.WithContext(globalCtx)

	return &BES{
		syncer:         s,
		commitMode:     mode,
		globalErrGroup: globalErrGroup,
		globalCtx:      globalCtx,
	}
}

// Shutdown gracefully shuts down the BES server, waiting for background commits to complete
func (b *BES) Shutdown(ctx context.Context) error {
	log.Println("Shutting down BES server, waiting for background commits...")

	// Wait for all background commits to complete or context to be canceled
	done := make(chan error, 1)
	go func() {
		done <- b.globalErrGroup.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			log.Printf("Background commits completed with error: %v", err)
			return err
		}
		log.Println("All background commits completed successfully")
		return nil
	case <-ctx.Done():
		log.Println("Shutdown timeout reached, some commits may still be running")
		return ctx.Err()
	}
}

func (b *BES) PublishLifecycleEvent(ctx context.Context, req *bes_proto.PublishLifecycleEventRequest) (*emptypb.Empty, error) {
	// we don't care about lifecycle events, so we just return an empty response.
	return &emptypb.Empty{}, nil
}

func (b *BES) PublishBuildToolEventStream(stream bes_proto.PublishBuildEvent_PublishBuildToolEventStreamServer) error {
	tracker := newTracker()

	var requestErrGroup *errgroup.Group
	var commitCtx context.Context

	if b.commitMode == CommitModePerStream {
		commitCtx = stream.Context()
		requestErrGroup, commitCtx = errgroup.WithContext(commitCtx)
		defer func() {
			if err := requestErrGroup.Wait(); err != nil {
				log.Printf("Per-stream commits completed with error: %v", err)
			} else {
				log.Println("All per-stream commits completed successfully")
			}
		}()
	} else {
		commitCtx = b.globalCtx
		requestErrGroup = b.globalErrGroup
	}

	for {
		req, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				// Client closed the stream, this is normal
				return nil
			}
			log.Printf("Error receiving from stream: %v", err)
			return err
		}
		response := &bes_proto.PublishBuildToolEventStreamResponse{
			StreamId:       req.OrderedBuildEvent.StreamId,
			SequenceNumber: req.OrderedBuildEvent.SequenceNumber,
		}

		bazelEvent := req.OrderedBuildEvent.Event.GetBazelEvent()
		if bazelEvent == nil {
			// simply acknowledge non-Bazel events
			if err := stream.Send(response); err != nil {
				log.Printf("Error sending response: %v", err)
				return err
			}
			continue
		}

		// Decode the Bazel event from the protobuf bytes
		var buildEvent build_event_stream_proto.BuildEvent
		if err := proto.Unmarshal(bazelEvent.Value, &buildEvent); err != nil {
			return err
		} else {
			eventCtx := commitCtx
			if invocationID := req.GetOrderedBuildEvent().GetStreamId().GetInvocationId(); invocationID != "" {
				eventCtx = protohelper.WithRequestMetadata(eventCtx, protohelper.RequestMetadata{
					ToolInvocationID: invocationID,
					ActionID:         "rules_img:bes",
					ActionMnemonic:   "ImgBESCommit",
					TargetID:         "rules_img",
				})
			}
			if err := b.processBuildEvent(&buildEvent, tracker, requestErrGroup, eventCtx); err != nil {
				log.Printf("Error processing build event: %v", err)
				// Continue processing other events even if one fails
			}
		}

		if err := stream.Send(response); err != nil {
			log.Printf("Error sending response: %v", err)
			return err
		}
	}
}

func (b *BES) processBuildEvent(event *build_event_stream_proto.BuildEvent, tracker tracker, comittErrGroup *errgroup.Group, commitCtx context.Context) error {
	if event.Id == nil {
		return errors.New("event ID is nil")
	}

	switch event.Id.Id.(type) {
	case *build_event_stream_proto.BuildEventId_TargetConfigured:
		configured := event.GetConfigured()
		if configured == nil {
			return errors.New("target configured event data is nil")
		}
		if configured.TargetKind != "image_push rule" {
			return nil // we only care about image_push rules
		}
		for _, childId := range event.GetChildren() {
			if childId.GetTargetCompleted() == nil {
				// we only care about TargetCompleted events, so skip others
				return nil
			}
			// This is the ID of a configured target.
			// Remember it, so we can later identify the matching TargetCompleted event.
			if err := tracker.trackTargetCompleted(childId); err != nil {
				return fmt.Errorf("tracking child of TargetConfigured event: %w", err)
			}
		}
	case *build_event_stream_proto.BuildEventId_NamedSet:
		files := event.GetNamedSetOfFiles()
		if files == nil {
			return errors.New("named set of files event data is nil")
		}
		if err := tracker.addNamedSetOfFiles(event.Id, files); err != nil {
			return err
		}
	case *build_event_stream_proto.BuildEventId_TargetCompleted:
		completed := event.GetCompleted()
		if completed == nil {
			return errors.New("target completed event data is nil")
		}
		idHash, err := eventIDHash(event.Id)
		if err != nil {
			return fmt.Errorf("failed to hash event ID: %w", err)
		}
		targetID := event.Id.GetTargetCompleted().GetLabel()
		requestMetadata, _ := protohelper.RequestMetadataFromContext(commitCtx)
		requestMetadata.ActionMnemonic = "ImgBESCommit"
		requestMetadata.ActionID = "rules_img:bes:" + idHash[:12]
		if targetID != "" {
			requestMetadata.TargetID = targetID
		}
		commitCtx = protohelper.WithRequestMetadata(commitCtx, requestMetadata)
		// Check if this matches a TargetConfigured event
		// seen earlier.
		if !tracker.hasTargetCompleted(event.Id) {
			return nil
		}
		// We have a matching TargetConfigured event, so we can process this TargetCompleted event.
		if !completed.Success {
			log.Printf("Target %s failed to complete successfully\n", idHash)
			return nil // we only care about successful completions
		}
		if len(completed.OutputGroup) == 0 {
			return fmt.Errorf("target completed event %s has no output groups", idHash)
		}
		for _, group := range completed.OutputGroup {
			if group.Name != "default" {
				continue // we only care about the default output group
			}
			if len(group.FileSets) != 1 {
				return fmt.Errorf("target completed event %s has unexpected number of file sets in default output group: %d", idHash, len(group.FileSets))
			}
			fileSet := group.FileSets[0]
			if fileSet == nil {
				return fmt.Errorf("target completed event %s has nil file set in default output group", idHash)
			}
			namedSet := tracker.getNamedSetOfFiles(fileSet.Id)
			if namedSet == nil {
				return fmt.Errorf("target completed event %s references unknown named set of files: %s", idHash, fileSet.Id)
			}
			if len(namedSet.Files) != 1 {
				return fmt.Errorf("target completed event %s has unexpected number of files in named set %s: %d", idHash, fileSet.Id, len(namedSet.Files))
			}
			pushJSONDescriptor := namedSet.Files[0]
			if pushJSONDescriptor == nil {
				return fmt.Errorf("target completed event %s has nil file in named set %s", idHash, fileSet.Id)
			}

			// Schedule commit based on the configured commit mode
			digest := pushJSONDescriptor.Digest
			length := pushJSONDescriptor.Length

			comittErrGroup.Go(func() error {
				if err := b.syncer.Commit(commitCtx, digest, length); err != nil {
					return fmt.Errorf("failed to commit image for target %s: %w", idHash, err)
				}
				return nil
			})
		}
	}
	return nil
}

type tracker struct {
	targetCompletedIdHashes map[string]struct{}
	namedSets               map[string]*build_event_stream_proto.NamedSetOfFiles
}

func newTracker() tracker {
	return tracker{
		targetCompletedIdHashes: make(map[string]struct{}),
		namedSets:               make(map[string]*build_event_stream_proto.NamedSetOfFiles),
	}
}

func (t *tracker) trackTargetCompleted(eventID *build_event_stream_proto.BuildEventId) error {
	idHash, err := eventIDHash(eventID)
	if err != nil {
		return fmt.Errorf("hashing event ID: %w", err)
	}
	t.targetCompletedIdHashes[idHash] = struct{}{}
	return nil
}

func (t *tracker) hasTargetCompleted(eventID *build_event_stream_proto.BuildEventId) bool {
	idHash, err := eventIDHash(eventID)
	if err != nil {
		return false
	}
	_, exists := t.targetCompletedIdHashes[idHash]
	return exists
}

func (t *tracker) addNamedSetOfFiles(eventID *build_event_stream_proto.BuildEventId, namedSet *build_event_stream_proto.NamedSetOfFiles) error {
	if eventID == nil || eventID.GetNamedSet() == nil || namedSet == nil {
		return errors.New("event ID or named set is nil")
	}
	filesetId := eventID.GetNamedSet().Id
	t.namedSets[filesetId] = proto.Clone(namedSet).(*build_event_stream_proto.NamedSetOfFiles)
	return nil
}

func (t *tracker) getNamedSetOfFiles(filesetID string) *build_event_stream_proto.NamedSetOfFiles {
	return t.namedSets[filesetID]
}

func eventIDHash(eventID *build_event_stream_proto.BuildEventId) (string, error) {
	if eventID == nil {
		return "", errors.New("event ID is nil")
	}
	rawEventID, err := proto.Marshal(eventID)
	if err != nil {
		return "", fmt.Errorf("invalid event ID: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(rawEventID)), nil
}
