package persistentworker

import (
	"github.com/bazel-contrib/rules_img/img_tool/pkg/argfile"
)

// ParseArgs processes command arguments by expanding argfiles and extracting
// the persistent_worker flag. It returns the processed argument slice and
// a boolean indicating whether the persistent_worker flag was set.
func ParseArgs(args []string) ([]string, bool, error) {
	// Expand argfile if present
	expandedArgs, err := argfile.Expand(args)
	if err != nil {
		return nil, false, err
	}

	// Extract persistent_worker flag
	processedArgs, isPersistentWorker := extractPersistentWorkerFlag(expandedArgs)

	return processedArgs, isPersistentWorker, nil
}

// extractPersistentWorkerFlag searches for and removes the --persistent_worker flag.
// Returns the remaining args and whether the flag was found.
func extractPersistentWorkerFlag(args []string) ([]string, bool) {
	isPersistentWorker := false
	result := make([]string, 0, len(args))

	for _, arg := range args {
		if arg == "--persistent_worker" {
			isPersistentWorker = true
			// Skip this arg - don't add it to result
		} else {
			result = append(result, arg)
		}
	}

	return result, isPersistentWorker
}
