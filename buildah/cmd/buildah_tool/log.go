package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

var (
	buildLog       *os.File
	logPipe        *os.File
	logPipeDrained chan struct{}
)

func tryStealBazelOutputForStreaming() {
	pid, ok, err := findBazelBuildPID()
	if err != nil || !ok {
		return
	}

	if bazelStdout, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/1", pid), os.O_WRONLY, 0); err != nil {
		_ = syscall.Dup2(int(bazelStdout.Fd()), int(os.Stdout.Fd()))
	}
	if bazelStderr, err := os.OpenFile(fmt.Sprintf("/proc/%d/fd/2", pid), os.O_WRONLY, 0); err == nil {
		_ = syscall.Dup2(int(bazelStderr.Fd()), int(os.Stderr.Fd()))
	}
}

func setupLog() error {
	var err error
	buildLog, err = os.Create(*flagBuildLog)
	if err != nil {
		return fmt.Errorf("failed to open: %w", err)
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("failed to create log pipe: %w", err)
	}
	logPipe = pw
	logPipeDrained = make(chan struct{})
	go func() {
		defer close(logPipeDrained)
		var out io.Writer = buildLog
		if *flagVerbose {
			out = io.MultiWriter(buildLog, os.Stderr)
		}
		tw := newTimingWriter(out)
		_, _ = io.Copy(tw, pr)
		_ = tw.Flush()
		_ = pr.Close()
	}()

	logrus.SetOutput(logPipe)
	if *flagDebug {
		logrus.SetLevel(logrus.DebugLevel)
	} else {
		logrus.SetLevel(logrus.WarnLevel)
	}
	return nil
}

func closeLogPipe() {
	if logPipe == nil {
		return
	}
	logrus.SetOutput(os.Stderr)
	_ = logPipe.Close()
	<-logPipeDrained
	logPipe = nil
}

func flushBuildLog() {
	if *flagVerbose {
		return
	}

	_, _ = buildLog.Seek(0, io.SeekStart)
	_, _ = io.Copy(os.Stderr, buildLog)
}

type timingWriter struct {
	w           io.Writer
	buf         bytes.Buffer
	atLineStart bool
}

func newTimingWriter(w io.Writer) *timingWriter {
	return &timingWriter{w: w, atLineStart: true}
}

func linePrefix() []byte {
	return []byte(time.Now().Format("2006-01-02 15:04:05.000") + " | ")
}

func logf(msg string, args ...any) {
	var buf bytes.Buffer
	buf.Write(linePrefix())
	_, _ = fmt.Fprintf(&buf, msg, args...)
	if buf.Len() == 0 || buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}

	_, _ = buildLog.Write(buf.Bytes())
	if *flagVerbose {
		os.Stderr.Write(buf.Bytes())
	}
}

func (t *timingWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if t.atLineStart {
			t.buf.Write(linePrefix())
			t.atLineStart = false
		}
		t.buf.WriteByte(b)
		if b == '\n' {
			if _, err := t.w.Write(t.buf.Bytes()); err != nil {
				t.buf.Reset()
				return 0, err //nolint:wrapcheck
			}
			t.buf.Reset()
			t.atLineStart = true
		}
	}
	return len(p), nil
}

func (t *timingWriter) Flush() error {
	if t.buf.Len() == 0 {
		return nil
	}
	_, err := t.w.Write(t.buf.Bytes())
	t.buf.Reset()
	t.atLineStart = true
	return err //nolint:wrapcheck
}
