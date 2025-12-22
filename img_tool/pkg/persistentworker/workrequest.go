package persistentworker

// WorkRequest represents a single work request for the persistent worker.
// See https://bazel.build/remote/creating for the protocol specification.
type WorkRequest struct {
	Arguments  []string `json:"arguments"`
	Inputs     []Input  `json:"inputs"`
	RequestId  int      `json:"requestId"`
	Cancel     bool     `json:"cancel,omitempty"`
	Verbosity  int      `json:"verbosity,omitempty"`
	SandboxDir string   `json:"sandboxDir,omitempty"`
}

// WorkResponse represents the response to a work request.
type WorkResponse struct {
	ExitCode     int    `json:"exitCode"`
	Output       string `json:"output"`
	RequestId    int    `json:"requestId"`
	WasCancelled bool   `json:"was_cancelled,omitempty"`
}

// Input represents a single input file with its path and content digest.
type Input struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}
