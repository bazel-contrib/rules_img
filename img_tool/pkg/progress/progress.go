package progress

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/logs"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/jedib0t/go-pretty/v6/progress"
)

type contextKey string

const (
	writerKey   contextKey = "progressWriter"
	trackersKey contextKey = "progressTrackers"
	logVerbKey  contextKey = "progressLogVerb"
)

// InitProgress starts progress reporting for multiple concurrent operations.
// Returns a context to hand to the reporting functions below and a stop
// function to call when done.
//
// doneMessage describes a finished transfer ("pushed", "loaded"): it labels the
// completed progress bars in ModeBar and is the verb of the per-blob lines in
// ModeLog.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx, "pushed")
//	defer stop()
func InitProgress(ctx context.Context, doneMessage string) (context.Context, func()) {
	switch CurrentMode() {
	case ModeBar:
		// fall through to the progress bar setup below
	case ModeLog:
		return context.WithValue(ctx, logVerbKey, doneMessage), func() {}
	default:
		return ctx, func() {} // no-op when progress is not reported
	}

	pw := progress.NewWriter()
	pw.SetAutoStop(false)

	style := progress.StyleDefault
	style.Visibility.Time = false
	style.Visibility.Percentage = true
	style.Visibility.Speed = true
	style.Visibility.Tracker = true
	style.Visibility.Value = true
	style.Options.DoneString = doneMessage
	pw.SetStyle(style)

	pw.SetTrackerLength(60)
	pw.SetTrackerPosition(progress.PositionRight)
	pw.SetUpdateFrequency(100 * time.Millisecond)
	pw.SetOutputWriter(os.Stderr)

	go pw.Render()

	ctx = context.WithValue(ctx, writerKey, pw)
	return ctx, func() {
		// This is a bit silly, but we see visual glitches if we don't sleep after calling Stop.
		// If the image push completes too quickly, the final progress bar
		// doesn't render properly. Adding a small delay ensures it shows up.
		pw.Stop()
		time.Sleep(110 * time.Millisecond)
	}
}

// fromContext retrieves the progress writer from the context, if any.
func fromContext(ctx context.Context) progress.Writer {
	if pw, ok := ctx.Value(writerKey).(progress.Writer); ok {
		return pw
	}
	return nil
}

// trackersFromContext retrieves the pre-declared trackers map from the context, if any.
func trackersFromContext(ctx context.Context) map[string]*progress.Tracker {
	if trackers, ok := ctx.Value(trackersKey).(map[string]*progress.Tracker); ok {
		return trackers
	}
	return nil
}

// logVerbFromContext retrieves the verb describing a finished transfer
// ("pushed", "loaded") from the context, as set by InitProgress. It reports
// false if the context was not derived from an InitProgress context, in which
// case nothing is reported - just like ModeBar reports nothing without a
// progress writer in the context.
func logVerbFromContext(ctx context.Context) (string, bool) {
	verb, ok := ctx.Value(logVerbKey).(string)
	return verb, ok
}

// DeclareTrackers pre-creates progress trackers in the specified order with DeferStart enabled.
// This allows displaying all trackers in a deterministic order, even before they start.
//
// The trackers are attached to the context and will be automatically used when calling
// progress.Writer() with a matching name. If no pre-declared tracker exists for a name,
// a new one will be created dynamically.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx, "done")
//	defer stop()
//
//	// Declare all trackers upfront in desired order
//	ctx = progress.DeclareTrackers(ctx, []string{"layer1", "layer2", "layer3"})
//
//	// Later, when actually processing each layer, the tracker will start
//	pw, _ := progress.Writer(ctx, size1, "layer1") // Uses pre-declared tracker
//	pw, _ := progress.Writer(ctx, size2, "layer2") // Uses pre-declared tracker
func DeclareTrackers(ctx context.Context, names []string, sizes []int64) context.Context {
	pw := fromContext(ctx)
	if pw == nil {
		// No progress writer, nothing to do
		return ctx
	}

	if len(names) != len(sizes) {
		panic("DeclareTrackers: names and sizes length mismatch")
	}

	trackers := make(map[string]*progress.Tracker)
	for i, name := range names {
		tracker := &progress.Tracker{
			Message:    barLabel(name),
			Total:      sizes[i],
			Units:      progress.UnitsBytes,
			DeferStart: true,
		}
		pw.AppendTracker(tracker)
		trackers[name] = tracker
	}

	return context.WithValue(ctx, trackersKey, trackers)
}

// Writer creates an io.Writer that tracks progress for a single operation.
// The io.Writer should be used with io.MultiWriter to track progress while writing to a destination.
//
// In ModeBar, a pre-declared tracker for the given description (via
// DeclareTrackers) is used if one exists, and a new tracker is appended
// dynamically otherwise. ModeLog doesn't report a transfer while it is in
// flight; call Transferred once it finished.
//
// Returns an error if progress bars are requested but no progress writer is in the context.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx, "pushed")
//	defer stop()
//
//	pw, err := progress.Writer(ctx, size, digest)
//	if err != nil { return err }
//	io.Copy(io.MultiWriter(destFile, pw), srcReader)
//	progress.Transferred(ctx, digest)
func Writer(ctx context.Context, size int64, desc string) (io.Writer, error) {
	if CurrentMode() != ModeBar {
		return io.Discard, nil
	}

	pw := fromContext(ctx)
	if pw == nil {
		return nil, errors.New("no progress writer in context")
	}

	// Check if a pre-declared tracker exists for this name
	trackers := trackersFromContext(ctx)
	if trackers != nil {
		if tracker, exists := trackers[desc]; exists {
			// Use the pre-declared tracker and update its total
			// The tracker will automatically start when we begin writing to it
			tracker.UpdateTotal(size)
			return &trackerWriter{tracker: tracker}, nil
		}
	}

	// No pre-declared tracker, create a new one dynamically
	tracker := &progress.Tracker{
		Message: barLabel(desc),
		Total:   size,
		Units:   progress.UnitsBytes,
	}
	pw.AppendTracker(tracker)

	return &trackerWriter{tracker: tracker}, nil
}

// Transferred reports a transfer that finished, once its bytes are committed at
// the destination: crane's "pushed blob: <desc>" line in ModeLog, with the verb
// taken from InitProgress's doneMessage ("pushed", "loaded").
//
// It is a no-op in ModeBar, where the tracker of the matching Writer completes
// as the bytes are written.
func Transferred(ctx context.Context, desc string) {
	if CurrentMode() != ModeLog {
		return
	}
	if verb, ok := logVerbFromContext(ctx); ok {
		logs.Progress.Printf("%s blob: %s", verb, desc)
	}
}

// CompletedWriter reports an operation that finished without transferring any
// bytes because the destination already had them: a full progress bar in
// ModeBar, and crane's "existing blob" line in ModeLog.
func CompletedWriter(ctx context.Context, size int64, desc string) error {
	switch CurrentMode() {
	case ModeBar:
		// fall through to the tracker setup below
	case ModeLog:
		if _, ok := logVerbFromContext(ctx); ok {
			logs.Progress.Printf("existing blob: %s", desc)
		}
		return nil
	default:
		return nil
	}

	pw := fromContext(ctx)
	if pw == nil {
		return errors.New("no progress writer in context")
	}

	// Check if a pre-declared tracker exists for this name
	trackers := trackersFromContext(ctx)
	if trackers != nil {
		if tracker, exists := trackers[desc]; exists {
			// Use the pre-declared tracker, mark as completed
			tracker.UpdateTotal(size)
			tracker.MarkAsDone()
			return nil
		}
	}

	// No pre-declared tracker, create a completed one dynamically
	tracker := &progress.Tracker{
		Message: barLabel(desc),
		Total:   size,
		Units:   progress.UnitsBytes,
	}
	pw.AppendTracker(tracker)
	tracker.SetValue(size)
	tracker.MarkAsDone()
	return nil
}

type trackerWriter struct {
	tracker *progress.Tracker
}

func (tw *trackerWriter) Write(p []byte) (int, error) {
	n := len(p)
	tw.tracker.Increment(int64(n))
	return n, nil
}

// barLabel shortens a digest to the first 12 hex characters, which is what the
// progress bars are labelled with - a full digest would make every line wrap.
// Descriptions that are not digests are used as they are. The log lines always
// carry the full digest, like crane's do.
func barLabel(desc string) string {
	_, hex, ok := strings.Cut(desc, ":")
	if !ok || len(hex) <= shortDigestLength {
		return desc
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return desc
		}
	}
	return hex[:shortDigestLength]
}

// shortDigestLength is the number of hex characters of a digest shown in a
// progress bar's label.
const shortDigestLength = 12

// Indeterminate represents a progress tracker with an initially unknown total.
// The total can be updated once known using SetTotal, and progress is updated with SetComplete.
type Indeterminate struct {
	tracker *progress.Tracker
}

// SetTotal updates the total size for the indeterminate tracker.
func (i *Indeterminate) SetTotal(total int64) {
	if i.tracker != nil {
		i.tracker.UpdateTotal(total)
	}
}

// SetComplete updates the current progress value for the indeterminate tracker.
func (i *Indeterminate) SetComplete(complete int64) {
	if i.tracker != nil {
		i.tracker.SetValue(complete)
	}
}

func (i *Indeterminate) Done(err error) {
	if i.tracker == nil {
		return
	}
	if err == nil {
		i.tracker.MarkAsDone()
		return
	}
	i.tracker.MarkAsErrored()
}

// NewIndeterminate creates a new indeterminate progress tracker.
// If a progress writer is attached to the context (via InitProgress), it will add a tracker to it.
// Otherwise, returns a no-op tracker.
//
// An aggregate byte counter has no crane-style equivalent, so this is a no-op
// tracker in ModeLog: the per-blob lines are reported by go-containerregistry
// and by Writer/CompletedWriter instead.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx)
//	defer stop()
//
//	tracker := progress.NewIndeterminate(ctx, "uploading")
//	tracker.SetTotal(totalSize) // once known
//	tracker.SetComplete(bytesUploaded) // as progress is made
func NewIndeterminate(ctx context.Context, message string) *Indeterminate {
	if CurrentMode() != ModeBar {
		return &Indeterminate{} // Return empty struct when progress bars are off
	}

	pw := fromContext(ctx)
	if pw == nil {
		// No progress writer in context, return no-op tracker
		return &Indeterminate{}
	}

	tracker := &progress.Tracker{
		Message: message,
		Total:   0, // Indeterminate initially
		Units:   progress.UnitsBytes,
	}
	pw.AppendTracker(tracker)

	return &Indeterminate{tracker: tracker}
}

// transferUpdateBuffer is the capacity of the channel handed to
// remote.WithProgress. go-containerregistry sends an update per chunk written, so
// the buffer keeps a burst of uploads from blocking on the drain goroutine.
const transferUpdateBuffer = 256

// TrackTransfers starts aggregate progress reporting for a batch of registry
// transfers and returns the channel to hand to remote.WithProgress.
//
// doneVerb labels a finished transfer ("pushed", "staged"); label names the
// aggregate bar while the batch runs ("pushing", "staging blobs"). Call finish
// with the batch's error (nil on success) once every transfer has completed: it
// closes the channel, drains it, and tears the reporting down. Calling it more
// than once is a no-op, so it is safe to call it on an early-return path and from
// a deferred call.
//
// Usage:
//
//	ctx, updates, finish := progress.TrackTransfers(ctx, "pushed", "pushing")
//	pusher, err := remote.NewPusher(append(opts, remote.WithProgress(updates))...)
//	if err != nil {
//		finish(err)
//		return err
//	}
//	defer func() { finish(retErr) }()
func TrackTransfers(ctx context.Context, doneVerb, label string) (context.Context, chan v1.Update, func(error)) {
	ctx, stop := InitProgress(ctx, doneVerb)
	tracker := NewIndeterminate(ctx, label)

	updates := make(chan v1.Update, transferUpdateBuffer)
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		for update := range updates {
			tracker.SetTotal(update.Total)
			tracker.SetComplete(update.Complete)
		}
	}()

	var once sync.Once
	return ctx, updates, func(err error) {
		once.Do(func() {
			close(updates)
			drained.Wait()
			tracker.Done(err)
			stop()
		})
	}
}
