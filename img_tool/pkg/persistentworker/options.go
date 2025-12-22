package persistentworker

import (
	"io"
	"os"
	"runtime"
)

// Option configures a Worker.
type Option func(*Worker)

// WithMaxWorkers sets the maximum number of concurrent workers.
// If not specified, defaults to runtime.NumCPU().
func WithMaxWorkers(max int) Option {
	return func(w *Worker) {
		if max > 0 {
			w.maxWorkers = max
		}
	}
}

// WithInput sets the input reader for work requests.
// If not specified, defaults to os.Stdin.
func WithInput(r io.Reader) Option {
	return func(w *Worker) {
		w.input = r
	}
}

// WithOutput sets the output writer for work responses.
// If not specified, defaults to os.Stdout.
func WithOutput(w io.Writer) Option {
	return func(worker *Worker) {
		worker.output = w
	}
}

// defaultMaxWorkers returns the default number of concurrent workers.
func defaultMaxWorkers() int {
	return runtime.NumCPU()
}

// defaultInput returns the default input reader.
func defaultInput() io.Reader {
	return os.Stdin
}

// defaultOutput returns the default output writer.
func defaultOutput() io.Writer {
	return os.Stdout
}
