package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// githubActionsContext returns the GitHub Actions context that buildx hands
// to BuildKit as provenance.d/github_actions_context.json (see buildx
// util/ghutil): keys are merged into the predicate's internalParameters. It
// returns nil outside of GitHub Actions. Every group of keys is emitted
// all-or-nothing, exactly like buildx.
func githubActionsContext(includeEventPayload bool) (map[string]any, error) {
	if v, _ := strconv.ParseBool(os.Getenv("GITHUB_ACTIONS")); !v {
		return nil, nil
	}
	eventName := os.Getenv("GITHUB_EVENT_NAME")
	eventPath := os.Getenv("GITHUB_EVENT_PATH")
	if eventName == "" || eventPath == "" {
		return nil, nil
	}
	m := map[string]any{"github_event_name": eventName}
	if includeEventPayload {
		data, err := os.ReadFile(eventPath)
		if err != nil {
			return nil, fmt.Errorf("reading GITHUB_EVENT_PATH %q: %w", eventPath, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			return nil, fmt.Errorf("parsing GitHub event payload %q: %w", eventPath, err)
		}
		m["github_event_payload"] = payload
	}

	addGroup(m, [][2]string{
		{"github_actor", "GITHUB_ACTOR"},
		{"github_actor_id", "GITHUB_ACTOR_ID"},
		{"github_triggering_actor", "GITHUB_TRIGGERING_ACTOR"},
	})
	addGroup(m, [][2]string{
		{"github_ref", "GITHUB_REF"},
		{"github_ref_name", "GITHUB_REF_NAME"},
		{"github_ref_protected", "GITHUB_REF_PROTECTED"},
		{"github_ref_type", "GITHUB_REF_TYPE"},
	})
	addGroup(m, [][2]string{
		{"github_repository", "GITHUB_REPOSITORY"},
		{"github_repository_id", "GITHUB_REPOSITORY_ID"},
		{"github_repository_owner", "GITHUB_REPOSITORY_OWNER"},
		{"github_repository_owner_id", "GITHUB_REPOSITORY_OWNER_ID"},
	})
	addGroup(m, [][2]string{
		{"github_job", "GITHUB_JOB"},
		{"github_run_attempt", "GITHUB_RUN_ATTEMPT"},
		{"github_run_id", "GITHUB_RUN_ID"},
		{"github_run_number", "GITHUB_RUN_NUMBER"},
		{"github_server_url", "GITHUB_SERVER_URL"},
		{"github_workflow", "GITHUB_WORKFLOW"},
		{"github_workflow_ref", "GITHUB_WORKFLOW_REF"},
		{"github_workflow_sha", "GITHUB_WORKFLOW_SHA"},
	})
	addGroup(m, [][2]string{
		{"github_runner_image_os", "ImageOS"},
		{"github_runner_image_version", "ImageVersion"},
		{"github_runner_os", "RUNNER_OS"},
		{"github_runner_arch", "RUNNER_ARCH"},
		{"github_runner_environment", "RUNNER_ENVIRONMENT"},
		{"github_runner_tracking_id", "RUNNER_TRACKING_ID"},
		{"github_runner_name", "RUNNER_NAME"},
	})
	return m, nil
}

// addGroup copies env vars into m under the given keys, but only if all of
// them are set and non-empty.
func addGroup(m map[string]any, pairs [][2]string) {
	vals := make([]string, len(pairs))
	for i, p := range pairs {
		vals[i] = os.Getenv(p[1])
		if vals[i] == "" {
			return
		}
	}
	for i, p := range pairs {
		m[p[0]] = vals[i]
	}
}

// githubInvocationID derives a stable, human-followable invocation ID from the
// GitHub Actions run, or "" when it is not available.
func githubInvocationID(ctx map[string]any) string {
	server, _ := ctx["github_server_url"].(string)
	repo, _ := ctx["github_repository"].(string)
	run, _ := ctx["github_run_id"].(string)
	attempt, _ := ctx["github_run_attempt"].(string)
	if server == "" || repo == "" || run == "" || attempt == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s/actions/runs/%s/attempts/%s", server, repo, run, attempt)
}
