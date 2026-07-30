package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/logs"
	"golang.org/x/term"
)

// Mode selects how progress is reported to the user.
type Mode int

const (
	// ModeAuto asks for the mode to be derived from the environment.
	// It is never the mode in effect: see SetMode and AutoMode.
	ModeAuto Mode = iota
	// ModeBar renders self-updating progress bars on stderr, one per blob.
	ModeBar
	// ModeLog writes one line per blob to stderr, in the style of crane
	// ("existing blob: sha256:...", "pushed blob: sha256:..."). The lines for
	// registry traffic come from go-containerregistry itself.
	ModeLog
	// ModeNone reports no progress at all.
	ModeNone
)

// progressEnvVar overrides the automatically detected mode.
const progressEnvVar = "IMG_PROGRESS"

// noProgressEnvVar disables progress reporting entirely.
const noProgressEnvVar = "NO_PROGRESS"

// noBarEnvVars disable the interactive progress bars, even on a terminal.
var noBarEnvVars = []string{
	"NO_INTERACTIVE",
	"NO_COLOR",
}

// String returns the flag value that selects the mode.
func (m Mode) String() string {
	switch m {
	case ModeAuto:
		return "auto"
	case ModeBar:
		return "bar"
	case ModeLog:
		return "log"
	case ModeNone:
		return "none"
	}
	return fmt.Sprintf("Mode(%d)", int(m))
}

// ParseMode parses a mode name: "auto", "bar", "log", or "none".
// The empty string parses as ModeAuto.
func ParseMode(value string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "auto":
		return ModeAuto, nil
	case "bar", "bars":
		return ModeBar, nil
	case "log", "logs", "plain":
		return ModeLog, nil
	case "none", "off", "quiet":
		return ModeNone, nil
	}
	return ModeAuto, fmt.Errorf("invalid progress mode %q: want one of auto, bar, log, none", value)
}

var (
	modeMu sync.Mutex
	// mode is the mode in effect, or ModeAuto while it is still unresolved.
	mode Mode
)

// SetMode selects how progress is reported for the remainder of the process and
// returns the mode that is now in effect. ModeAuto is resolved with
// AutoMode(ModeNone), i.e. commands that don't opt into another fallback stay
// silent when stderr is not a terminal.
//
// The mode owns the go-containerregistry loggers: they are the ones printing
// the per-blob lines for registry traffic, so they are enabled in ModeLog and
// silenced in every other mode.
//
// Call it once during startup, before any progress is reported: reporting that
// is already in flight keeps using the mode it was started with.
func SetMode(m Mode) Mode {
	if m == ModeAuto {
		m = AutoMode(ModeNone)
	}
	modeMu.Lock()
	defer modeMu.Unlock()
	mode = m
	if m == ModeLog {
		// go-containerregistry logs one line per blob it pushes, pulls, mounts
		// or skips, and one per manifest it writes - which is exactly what crane
		// prints. Its warnings (registry retries, missing credentials) are
		// useful in the same situations, so enable both, like crane does.
		logs.Progress.SetOutput(os.Stderr)
		logs.Warn.SetOutput(os.Stderr)
	} else {
		// A stray line would corrupt the progress bars, and nothing at all is
		// reported in ModeNone.
		logs.Progress.SetOutput(io.Discard)
		logs.Warn.SetOutput(io.Discard)
	}
	return m
}

// CurrentMode returns the mode in effect, resolving it from the environment on
// first use if SetMode was never called.
func CurrentMode() Mode {
	modeMu.Lock()
	resolved := mode
	modeMu.Unlock()
	if resolved != ModeAuto {
		return resolved
	}
	return SetMode(ModeAuto)
}

// AutoMode returns the mode to use when the user didn't ask for a specific one.
// notATerminal is returned when progress bars can't be rendered because stderr
// is not an interactive terminal: `img deploy` passes ModeLog to get
// crane-style output, build actions stay silent with ModeNone.
//
// $NO_PROGRESS turns progress reporting off entirely, $NO_INTERACTIVE and
// $NO_COLOR turn off the progress bars, and $IMG_PROGRESS overrides the whole
// decision with an explicit mode.
func AutoMode(notATerminal Mode) Mode {
	if raw, ok := os.LookupEnv(progressEnvVar); ok && strings.TrimSpace(raw) != "" {
		m, err := ParseMode(raw)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr, "WARNING: ignoring invalid %s=%q: %v\n", progressEnvVar, raw, err)
		case m != ModeAuto:
			return m
		}
	}
	if _, ok := os.LookupEnv(noProgressEnvVar); ok {
		return ModeNone
	}
	for _, envVar := range noBarEnvVars {
		if _, ok := os.LookupEnv(envVar); ok {
			return notATerminal
		}
	}
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return notATerminal
	}
	return ModeBar
}
