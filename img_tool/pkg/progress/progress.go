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

type Indeterminate struct{ p *progressbar.ProgressBar }

func (i *Indeterminate) SetTotal(total int64)       { i.p.ChangeMax64(total) }
func (i *Indeterminate) SetComplete(complete int64) { i.p.Set64(complete) }

func NewIndeterminate() *Indeterminate {
	p := progressbar.NewOptions64(
		-1,
		progressbar.OptionSetTheme(progressbar.ThemeASCII),
		progressbar.OptionShowCount(),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWidth(60),
		progressbar.OptionSetPredictTime(false),
		progressbar.OptionSetWriter(os.Stderr),
		progressbar.OptionOnCompletion(func() {
			fmt.Fprint(os.Stderr, "\n")
		}),
	)
	return &Indeterminate{p: p}
}
