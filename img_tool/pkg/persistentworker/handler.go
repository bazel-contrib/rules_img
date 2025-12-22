package persistentworker

import "context"

// Handler processes individual work requests.
// Implementations should be thread-safe as HandleRequest may be called
// concurrently from multiple goroutines.
type Handler interface {
	// HandleRequest processes a single work request and returns a response.
	// The context will be cancelled if Bazel sends a cancel request for this work.
	// Handlers should check ctx.Done() periodically for long-running operations
	// and return early when cancelled. The Worker will automatically set
	// WasCancelled=true in the response if the context was cancelled.
	HandleRequest(ctx context.Context, req WorkRequest) WorkResponse
}

// HandlerFunc is a function adapter that implements Handler.
type HandlerFunc func(context.Context, WorkRequest) WorkResponse

// HandleRequest calls the function itself.
func (f HandlerFunc) HandleRequest(ctx context.Context, req WorkRequest) WorkResponse {
	return f(ctx, req)
}
