package persistentworker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Worker is a generic persistent worker that handles Bazel work requests.
// It manages goroutine orchestration, concurrent request processing, and response ordering.
// It supports Bazel's cancellation protocol as described at:
// https://bazel.build/remote/creating#cancellation
type Worker struct {
	handler    Handler
	maxWorkers int
	input      io.Reader
	output     io.Writer

	// requestsMu protects the requests map
	requestsMu sync.Mutex
	// requests tracks in-flight requests by ID with their cancel functions
	requests map[int]context.CancelFunc
}

// NewWorker creates a new persistent worker with the given handler and options.
func NewWorker(handler Handler, opts ...Option) *Worker {
	w := &Worker{
		handler:    handler,
		maxWorkers: defaultMaxWorkers(),
		input:      defaultInput(),
		output:     defaultOutput(),
		requests:   make(map[int]context.CancelFunc),
	}

	for _, opt := range opts {
		opt(w)
	}

	return w
}

// Run starts the persistent worker, reading work requests from input and writing responses to output.
// It blocks until the input stream is closed or an error occurs.
//
// The worker uses a semaphore to limit concurrent request processing to maxWorkers.
// Each request is processed in its own goroutine for multiplex support.
// Responses are written to output in the order they are completed (not necessarily request order).
func (w *Worker) Run() error {
	reader := bufio.NewReader(w.input)
	encoder := json.NewEncoder(w.output)

	// Semaphore to limit concurrent workers
	sem := make(chan struct{}, w.maxWorkers)

	// WaitGroup to track in-flight requests
	var wg sync.WaitGroup

	// Channel for responses to ensure thread-safe writing to output
	responseChan := make(chan WorkResponse, w.maxWorkers)

	// Start response writer goroutine
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		for resp := range responseChan {
			if err := encoder.Encode(resp); err != nil {
				// Note: We can't return this error from here, so we just continue.
				// The main loop will detect EOF and exit cleanly.
				// Consider adding an error callback option in the future.
				continue
			}
		}
	}()

	// Main request loop
	decoder := json.NewDecoder(reader)
	for {
		var req WorkRequest
		if err := decoder.Decode(&req); err != nil {
			if err == io.EOF {
				// Normal shutdown - input closed
				break
			}
			// Wait for in-flight requests before returning error
			wg.Wait()
			close(responseChan)
			<-responseDone
			return fmt.Errorf("failed to decode work request: %w", err)
		}

		// Handle cancel requests
		if req.Cancel {
			w.requestsMu.Lock()
			if cancelFunc, ok := w.requests[req.RequestId]; ok {
				// Request is still in-flight, cancel it
				cancelFunc()
			}
			// If request is not found, it's already completed - ignore per spec
			w.requestsMu.Unlock()
			continue
		}

		// Acquire semaphore slot
		sem <- struct{}{}
		wg.Add(1)

		// Process request in goroutine for multiplex support
		go func(request WorkRequest) {
			defer wg.Done()
			defer func() { <-sem }() // Release semaphore

			// Create cancellable context for this request
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			// Register cancel function
			w.requestsMu.Lock()
			w.requests[request.RequestId] = cancel
			w.requestsMu.Unlock()

			// Ensure we clean up the cancel function when done
			defer func() {
				w.requestsMu.Lock()
				delete(w.requests, request.RequestId)
				w.requestsMu.Unlock()
			}()

			resp := w.handler.HandleRequest(ctx, request)

			// Check if the request was cancelled
			if ctx.Err() == context.Canceled {
				resp.WasCancelled = true
			}

			responseChan <- resp
		}(req)
	}

	// Wait for all workers to complete
	wg.Wait()

	// Close response channel and wait for writer to finish
	close(responseChan)
	<-responseDone

	return nil
}
