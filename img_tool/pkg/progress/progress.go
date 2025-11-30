package progress

import (
	"fmt"
	"io"
	"os"

	"github.com/schollz/progressbar/v3"
)

// Writer constructs an io.Writer that prints a progress bar to stderr.
// Use with io.MultiWriter to provide progress updates while writing to a dest.
func Writer(size int64, desc string) io.Writer {
	return progressbar.NewOptions64(
		size,
		progressbar.OptionSetDescription(desc),
		progressbar.OptionSetTheme(progressbar.ThemeASCII),
		progressbar.OptionShowCount(),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(60),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionSetRenderBlankState(true),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
	)
}
