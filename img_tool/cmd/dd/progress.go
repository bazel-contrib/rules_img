package ddcmd

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bazel-contrib/rules_img/img_tool/pkg/bytesize"
)

// reportProgress prints the progress to w once a second, until the returned
// function is called. total is the number of bytes to copy, or -1.
func reportProgress(w io.Writer, done *atomic.Int64, total int64) func() {
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	start := time.Now()
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				fmt.Fprint(w, "\r"+progressLine(done.Load(), total, time.Since(start)))
			}
		}
	}()
	return func() {
		close(stop)
		wg.Wait()
		fmt.Fprintln(w, "\r"+progressLine(done.Load(), total, time.Since(start)))
	}
}

func progressLine(done, total int64, elapsed time.Duration) string {
	s := fmt.Sprintf("%d bytes (%s)", done, bytesize.Format(done))
	if total > 0 {
		s += fmt.Sprintf(" of %s, %d%%", bytesize.Format(total), done*100/total)
	}
	return s + fmt.Sprintf(" copied, %.0f s, %s", elapsed.Seconds(), rate(done, elapsed))
}
