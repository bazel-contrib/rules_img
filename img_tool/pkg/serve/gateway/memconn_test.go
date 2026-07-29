package gateway

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// This file provides an in-memory net.Conn pair and net.Listener, so the tests
// can exercise real TLS handshakes and real HTTP/2 connections without binding a
// port (which this repository's tests deliberately never do).
//
// net.Pipe is not usable for either: it is synchronous and unbuffered, so any
// protocol where both sides write before either reads deadlocks. A TLS 1.3 server
// sends its session tickets immediately after the handshake, and an HTTP/2 server
// sends SETTINGS immediately on connect — both would block forever. These
// connections buffer instead.

// memBuffer is one direction of a memConn: an unbounded byte queue that blocks
// readers until data arrives or the writer closes.
type memBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      bytes.Buffer
	closed   bool
	deadline time.Time
	timer    *time.Timer
}

func newMemBuffer() *memBuffer {
	b := &memBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *memBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 {
		if b.closed {
			return 0, io.EOF
		}
		if b.expiredLocked() {
			return 0, os.ErrDeadlineExceeded
		}
		b.cond.Wait()
	}
	return b.buf.Read(p)
}

func (b *memBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return 0, net.ErrClosed
	}
	n, err := b.buf.Write(p)
	b.cond.Broadcast()
	return n, err
}

func (b *memBuffer) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
	return nil
}

// setDeadline wakes any blocked reader once t has passed.
func (b *memBuffer) setDeadline(t time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deadline = t
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if t.IsZero() {
		return
	}
	if delay := time.Until(t); delay > 0 {
		b.timer = time.AfterFunc(delay, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.cond.Broadcast()
		})
	}
	b.cond.Broadcast()
}

func (b *memBuffer) expiredLocked() bool {
	return !b.deadline.IsZero() && !time.Now().Before(b.deadline)
}

// memAddr is the address of an in-memory connection.
type memAddr string

func (a memAddr) Network() string { return "mem" }
func (a memAddr) String() string  { return string(a) }

// memConn is one end of an in-memory connection.
type memConn struct {
	read  *memBuffer
	write *memBuffer
	local memAddr
	peer  memAddr
	once  sync.Once
}

func (c *memConn) Read(p []byte) (int, error)  { return c.read.Read(p) }
func (c *memConn) Write(p []byte) (int, error) { return c.write.Write(p) }
func (c *memConn) LocalAddr() net.Addr         { return c.local }
func (c *memConn) RemoteAddr() net.Addr        { return c.peer }

func (c *memConn) Close() error {
	c.once.Do(func() {
		// Closing both directions models a TCP close: the peer's reads end too.
		_ = c.write.Close()
		_ = c.read.Close()
	})
	return nil
}

func (c *memConn) SetDeadline(t time.Time) error {
	c.read.setDeadline(t)
	c.write.setDeadline(t)
	return nil
}

func (c *memConn) SetReadDeadline(t time.Time) error  { c.read.setDeadline(t); return nil }
func (c *memConn) SetWriteDeadline(t time.Time) error { c.write.setDeadline(t); return nil }

// memPipe returns a connected pair of buffered in-memory connections.
func memPipe(name string) (client, server net.Conn) {
	toServer, toClient := newMemBuffer(), newMemBuffer()
	return &memConn{read: toClient, write: toServer, local: memAddr(name + "-client"), peer: memAddr(name)},
		&memConn{read: toServer, write: toClient, local: memAddr(name), peer: memAddr(name + "-client")}
}

// memListener is a net.Listener served over in-memory connections. Dial is how a
// client obtains one, and the dial count is what proves that several concurrent
// requests shared a single connection.
type memListener struct {
	name    string
	conns   chan net.Conn
	dialed  atomic.Int64
	closing chan struct{}
	once    sync.Once
}

func newMemListener(name string) *memListener {
	return &memListener{
		name:    name,
		conns:   make(chan net.Conn),
		closing: make(chan struct{}),
	}
}

func (l *memListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closing:
		return nil, net.ErrClosed
	}
}

// Dial creates a new connection to the listener, counting it.
func (l *memListener) Dial() (net.Conn, error) {
	client, server := memPipe(l.name)
	select {
	case l.conns <- server:
		l.dialed.Add(1)
		return client, nil
	case <-l.closing:
		return nil, net.ErrClosed
	}
}

func (l *memListener) Close() error {
	l.once.Do(func() { close(l.closing) })
	return nil
}

func (l *memListener) Addr() net.Addr { return memAddr(l.name) }

// dials reports how many connections have been established.
func (l *memListener) dials() int { return int(l.dialed.Load()) }

// errListenerClosed is returned by a server whose listener was closed, which is
// the normal way these tests stop one.
func isListenerClosed(err error) bool {
	return err == nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF)
}
