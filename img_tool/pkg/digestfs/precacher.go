package digestfs

import (
	"fmt"
	"sync"
)

// Precacher manages background digest calculation for a FileSystem
type Precacher struct {
	fs         *FileSystem
	tasks      chan string
	done       chan struct{}
	wg         sync.WaitGroup
	numWorkers int
}

// NewPrecacher creates a new precacher for the given FileSystem
func NewPrecacher(fs *FileSystem, numWorkers int) *Precacher {
	if numWorkers <= 0 {
		numWorkers = 4 // Default to 4 workers
	}

	p := &Precacher{
		fs:         fs,
		tasks:      make(chan string, numWorkers*2), // Small buffer to avoid blocking
		done:       make(chan struct{}),
		numWorkers: numWorkers,
	}

	// Start worker goroutines
	for i := 0; i < numWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}

	return p
}

// PrecacheFiles schedules files for background digest calculation in order
func (p *Precacher) PrecacheFiles(files []string) {
	// Send files to workers in order
	go func() {
		for _, file := range files {
			select {
			case p.tasks <- file:
			case <-p.done:
				return
			}
		}
	}()
}

// Close shuts down the precaching workers
func (p *Precacher) Close() error {
	close(p.done)
	p.wg.Wait()
	// Don't close p.tasks - there may still be goroutines from PrecacheFiles
	// trying to send on it. The workers exit via p.done, so closing p.tasks
	// is unnecessary and can cause "send on closed channel" panics.
	return nil
}

// worker processes precaching requests
func (p *Precacher) worker() {
	defer p.wg.Done()

	for {
		select {
		case path := <-p.tasks:
			// Precalculate digest for this file
			p.precacheFile(path)
		case <-p.done:
			return
		}
	}
}

// precacheFile calculates and caches the digest for a single file
func (p *Precacher) precacheFile(path string) {
	// Open file and trigger digest calculation
	file, err := p.fs.OpenFile(path)
	if err != nil {
		// Ignore errors - precaching is optional
		return
	}
	defer file.Close()

	cachedDigestFile := file.(*cachedDigestFile)
	if err := cachedDigestFile.precache(); err != nil {
		fmt.Printf("Precaching error for %s: %v\n", path, err)
	}
}
