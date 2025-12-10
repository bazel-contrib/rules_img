package progress

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/progress"
	"golang.org/x/term"
)

type contextKey string

const (
	writerKey   contextKey = "progressWriter"
	trackersKey contextKey = "progressTrackers"
)

// InitProgress creates and starts a progress writer for tracking multiple concurrent operations.
// Returns a context with the writer attached and a stop function to call when done.
// Progress bars are only displayed if stderr is a TTY.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx)
//	defer stop()
func InitProgress(ctx context.Context, doneMessage string) (context.Context, func()) {
	if !wantProgressBar() {
		return ctx, func() {} // no-op when not a TTY
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
			Message:    name,
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
// If a pre-declared tracker exists for the given description (via DeclareTrackers), it will be used
// and started. Otherwise, a new tracker is created and appended dynamically.
//
// Returns an error if no progress writer is in the context.
//
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx, "done")
//	defer stop()
//
//	pw, err := progress.Writer(ctx, size, "downloading layer")
//	if err != nil { return err }
//	io.Copy(io.MultiWriter(destFile, pw), srcReader)
func Writer(ctx context.Context, size int64, desc string) (io.Writer, error) {
	if !wantProgressBar() {
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
		Message: desc,
		Total:   size,
		Units:   progress.UnitsBytes,
	}
	pw.AppendTracker(tracker)

	return &trackerWriter{tracker: tracker}, nil
}

func CompletedWriter(ctx context.Context, size int64, desc string) error {
	if !wantProgressBar() {
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
		Message: desc,
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
// Usage:
//
//	ctx, stop := progress.InitProgress(ctx)
//	defer stop()
//
//	tracker := progress.NewIndeterminate(ctx, "uploading")
//	tracker.SetTotal(totalSize) // once known
//	tracker.SetComplete(bytesUploaded) // as progress is made
func NewIndeterminate(ctx context.Context, message string) *Indeterminate {
	if !wantProgressBar() {
		return &Indeterminate{} // Return empty struct when not a TTY
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

var noProgressEnvVars = []string{
	"NO_PROGRESS",
	"NO_INTERACTIVE",
	"NO_COLOR",
}

var wantProgressBar = sync.OnceValue(func() bool {
	for _, envVar := range noProgressEnvVars {
		if _, exists := os.LookupEnv(envVar); exists {
			return false
		}
	}
	return term.IsTerminal(int(os.Stderr.Fd()))
})
