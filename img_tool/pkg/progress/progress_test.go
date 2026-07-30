package progress

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/logs"
)

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  Mode
	}{
		{value: "", want: ModeAuto},
		{value: "auto", want: ModeAuto},
		{value: "AUTO", want: ModeAuto},
		{value: " bar ", want: ModeBar},
		{value: "bars", want: ModeBar},
		{value: "log", want: ModeLog},
		{value: "logs", want: ModeLog},
		{value: "plain", want: ModeLog},
		{value: "none", want: ModeNone},
		{value: "off", want: ModeNone},
		{value: "quiet", want: ModeNone},
	} {
		got, err := ParseMode(tc.value)
		if err != nil {
			t.Errorf("ParseMode(%q) returned error: %v", tc.value, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}

	if _, err := ParseMode("bogus"); err == nil {
		t.Error("ParseMode(\"bogus\") = nil error, want an error")
	}
}

func TestModeString(t *testing.T) {
	for _, tc := range []struct {
		mode Mode
		want string
	}{
		{mode: ModeAuto, want: "auto"},
		{mode: ModeBar, want: "bar"},
		{mode: ModeLog, want: "log"},
		{mode: ModeNone, want: "none"},
	} {
		if got := tc.mode.String(); got != tc.want {
			t.Errorf("%d.String() = %q, want %q", int(tc.mode), got, tc.want)
		}
		// Every mode name round-trips through ParseMode.
		parsed, err := ParseMode(tc.want)
		if err != nil || parsed != tc.mode {
			t.Errorf("ParseMode(%q) = %v, %v, want %v, nil", tc.want, parsed, err, tc.mode)
		}
	}
}

func TestAutoMode(t *testing.T) {
	// Test binaries never run with stderr attached to a terminal, so the
	// caller's non-terminal fallback is what auto-detection settles on.
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Mode
	}{
		{
			name: "not a terminal",
			want: ModeLog,
		},
		{
			name: "NO_PROGRESS wins over the fallback",
			env:  map[string]string{"NO_PROGRESS": "1"},
			want: ModeNone,
		},
		{
			name: "NO_COLOR only turns off the bars",
			env:  map[string]string{"NO_COLOR": "1"},
			want: ModeLog,
		},
		{
			name: "NO_INTERACTIVE only turns off the bars",
			env:  map[string]string{"NO_INTERACTIVE": "1"},
			want: ModeLog,
		},
		{
			name: "IMG_PROGRESS overrides",
			env:  map[string]string{"IMG_PROGRESS": "none"},
			want: ModeNone,
		},
		{
			name: "IMG_PROGRESS overrides NO_PROGRESS",
			env:  map[string]string{"IMG_PROGRESS": "bar", "NO_PROGRESS": "1"},
			want: ModeBar,
		},
		{
			name: "IMG_PROGRESS=auto defers to detection",
			env:  map[string]string{"IMG_PROGRESS": "auto"},
			want: ModeLog,
		},
		{
			name: "invalid IMG_PROGRESS is ignored",
			env:  map[string]string{"IMG_PROGRESS": "bogus"},
			want: ModeLog,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearProgressEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := AutoMode(ModeLog); got != tc.want {
				t.Errorf("AutoMode(ModeLog) = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAutoModeNonInteractiveFallbackIsCallerChosen(t *testing.T) {
	clearProgressEnv(t)
	// Build actions opt out of any output when they can't render bars.
	if got := AutoMode(ModeNone); got != ModeNone {
		t.Errorf("AutoMode(ModeNone) = %v, want %v", got, ModeNone)
	}
}

func TestSetModeLogEnablesRegistryLogging(t *testing.T) {
	restoreMode(t)
	restoreLogs(t)

	logs.Progress.SetOutput(io.Discard)
	logs.Warn.SetOutput(io.Discard)
	if got := SetMode(ModeLog); got != ModeLog {
		t.Fatalf("SetMode(ModeLog) = %v, want %v", got, ModeLog)
	}
	// The per-blob lines for registry traffic are printed by
	// go-containerregistry, so its loggers have to reach the user.
	if got := logs.Progress.Writer(); got != os.Stderr {
		t.Errorf("logs.Progress writes to %v, want os.Stderr", got)
	}
	if got := logs.Warn.Writer(); got != os.Stderr {
		t.Errorf("logs.Warn writes to %v, want os.Stderr", got)
	}
}

func TestSetModeBarSilencesRegistryLogging(t *testing.T) {
	restoreMode(t)
	restoreLogs(t)

	logs.Progress.SetOutput(os.Stderr)
	logs.Warn.SetOutput(os.Stderr)
	if got := SetMode(ModeBar); got != ModeBar {
		t.Fatalf("SetMode(ModeBar) = %v, want %v", got, ModeBar)
	}
	// A stray log line would corrupt the progress bars.
	if got := logs.Progress.Writer(); got != io.Discard {
		t.Errorf("logs.Progress writes to %v, want io.Discard", got)
	}
	if got := logs.Warn.Writer(); got != io.Discard {
		t.Errorf("logs.Warn writes to %v, want io.Discard", got)
	}
}

func TestSetModeAutoResolvesToConcreteMode(t *testing.T) {
	restoreMode(t)
	restoreLogs(t)
	clearProgressEnv(t)

	if got := SetMode(ModeAuto); got != ModeNone {
		t.Errorf("SetMode(ModeAuto) = %v, want %v", got, ModeNone)
	}
	if got := CurrentMode(); got != ModeNone {
		t.Errorf("CurrentMode() = %v, want %v", got, ModeNone)
	}
}

func TestLogModeWritesCraneStyleLines(t *testing.T) {
	out := setModeForTest(t, ModeLog)

	ctx, stop := InitProgress(context.Background(), "pushed")
	defer stop()

	if err := CompletedWriter(ctx, 1024, "sha256:cafe"); err != nil {
		t.Fatalf("CompletedWriter() returned error: %v", err)
	}

	// A transfer in flight is not reported; only its completion is.
	w, err := Writer(ctx, 4, "sha256:beef")
	if err != nil {
		t.Fatalf("Writer() returned error: %v", err)
	}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}
	if got := out.String(); strings.Contains(got, "sha256:beef") {
		t.Errorf("transfer in flight reported blob as done:\n%s", got)
	}
	Transferred(ctx, "sha256:beef")

	lines := logLines(out)
	want := []string{
		"existing blob: sha256:cafe",
		"pushed blob: sha256:beef",
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d log lines, want %d:\n%s", len(lines), len(want), out.String())
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, want[i]) {
			t.Errorf("log line %d = %q, want it to end with %q", i, line, want[i])
		}
	}
}

// TestLogModeUsesTheInitProgressVerb covers the load path, which reports the
// same events with its own verb.
func TestLogModeUsesTheInitProgressVerb(t *testing.T) {
	out := setModeForTest(t, ModeLog)

	ctx, stop := InitProgress(context.Background(), "loaded")
	defer stop()

	Transferred(ctx, "sha256:empty")

	lines := logLines(out)
	if len(lines) != 1 || !strings.HasSuffix(lines[0], "loaded blob: sha256:empty") {
		t.Errorf("got log lines %q, want one line ending in %q", lines, "loaded blob: sha256:empty")
	}
}

func TestLogModeWithoutInitProgressIsSilent(t *testing.T) {
	out := setModeForTest(t, ModeLog)

	ctx := context.Background()
	w, err := Writer(ctx, 2, "sha256:cafe")
	if err != nil {
		t.Fatalf("Writer() returned error: %v", err)
	}
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}
	Transferred(ctx, "sha256:cafe")
	if err := CompletedWriter(ctx, 2, "sha256:beef"); err != nil {
		t.Fatalf("CompletedWriter() returned error: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("reported progress outside of an InitProgress scope:\n%s", got)
	}
}

func TestModeNoneIsSilent(t *testing.T) {
	out := setModeForTest(t, ModeNone)

	ctx, stop := InitProgress(context.Background(), "pushed")
	defer stop()

	w, err := Writer(ctx, 2, "sha256:cafe")
	if err != nil {
		t.Fatalf("Writer() returned error: %v", err)
	}
	if w != io.Discard {
		t.Errorf("Writer() = %T, want io.Discard", w)
	}
	if _, err := w.Write([]byte("ab")); err != nil {
		t.Fatalf("Write() returned error: %v", err)
	}
	Transferred(ctx, "sha256:cafe")
	if err := CompletedWriter(ctx, 2, "sha256:beef"); err != nil {
		t.Fatalf("CompletedWriter() returned error: %v", err)
	}
	if tracker := NewIndeterminate(ctx, "pushing"); tracker.tracker != nil {
		t.Error("NewIndeterminate() returned a live tracker, want a no-op one")
	}
	if got := out.String(); got != "" {
		t.Errorf("reported progress in ModeNone:\n%s", got)
	}
}

func TestLogModeIndeterminateIsNoOp(t *testing.T) {
	out := setModeForTest(t, ModeLog)

	ctx, stop := InitProgress(context.Background(), "pushed")
	defer stop()

	// An aggregate byte counter has no crane-style equivalent.
	tracker := NewIndeterminate(ctx, "pushing")
	tracker.SetTotal(1024)
	tracker.SetComplete(512)
	tracker.Done(nil)
	if got := out.String(); got != "" {
		t.Errorf("indeterminate tracker logged in ModeLog:\n%s", got)
	}
}

// TestBarLabel covers the labels of the progress bars: a full digest would make
// every bar wrap, so digests are shortened there (the log lines keep them whole).
func TestBarLabel(t *testing.T) {
	for _, tc := range []struct {
		desc string
		want string
	}{
		{
			desc: "sha256:1110d13720997cdb8a454b0b27ac6454ecf35d8896bed0de38b31fb66954fd90",
			want: "1110d1372099",
		},
		{desc: "1110d1372099", want: "1110d1372099"},
		{desc: "sha256:cafe", want: "sha256:cafe"},
		{desc: "downloading a layer of the base image", want: "downloading a layer of the base image"},
		{desc: "pulling: some long description", want: "pulling: some long description"},
	} {
		if got := barLabel(tc.desc); got != tc.want {
			t.Errorf("barLabel(%q) = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

// setModeForTest switches to the given mode for the duration of the test and
// returns a buffer capturing everything reported to the user.
func setModeForTest(t *testing.T, m Mode) *bytes.Buffer {
	t.Helper()
	restoreMode(t)
	restoreLogs(t)
	SetMode(m)
	var buf bytes.Buffer
	logs.Progress.SetOutput(&buf)
	return &buf
}

// restoreMode restores the process-wide reporting mode after the test.
func restoreMode(t *testing.T) {
	t.Helper()
	modeMu.Lock()
	previous := mode
	modeMu.Unlock()
	t.Cleanup(func() {
		modeMu.Lock()
		defer modeMu.Unlock()
		mode = previous
	})
}

// restoreLogs restores the go-containerregistry loggers after the test.
func restoreLogs(t *testing.T) {
	t.Helper()
	progressOut := logs.Progress.Writer()
	warnOut := logs.Warn.Writer()
	t.Cleanup(func() {
		logs.Progress.SetOutput(progressOut)
		logs.Warn.SetOutput(warnOut)
	})
}

// clearProgressEnv unsets every environment variable that influences
// auto-detection, so a test only sees the ones it sets itself.
func clearProgressEnv(t *testing.T) {
	t.Helper()
	for _, envVar := range append([]string{progressEnvVar, noProgressEnvVar}, noBarEnvVars...) {
		value, ok := os.LookupEnv(envVar)
		if !ok {
			continue
		}
		// t.Setenv registers the restore; unsetting afterwards keeps it.
		t.Setenv(envVar, value)
		os.Unsetenv(envVar)
	}
}

func logLines(out *bytes.Buffer) []string {
	trimmed := strings.TrimSuffix(out.String(), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}
