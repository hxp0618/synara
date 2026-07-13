package executions

import "testing"

func TestValidateCreateWorkspaceCheckpointInputRequiresPatchBaseAndBranch(t *testing.T) {
	fileCount := 1
	totalBytes := int64(10)
	baseCommit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	branch := "synara/session-test"
	input := CreateWorkspaceCheckpointInput{
		IdempotencyKey: "patch-validation", Strategy: "patch",
		Manifest:  map[string]any{"format": "synara-workspace-patch-v1"},
		FileCount: &fileCount, TotalBytes: &totalBytes,
	}
	if err := validateCreateWorkspaceCheckpointInput(input); err == nil {
		t.Fatal("Patch Checkpoint accepted missing baseCommit and currentBranch")
	}
	input.BaseCommit = &baseCommit
	if err := validateCreateWorkspaceCheckpointInput(input); err == nil {
		t.Fatal("Patch Checkpoint accepted a missing currentBranch")
	}
	input.CurrentBranch = &branch
	if err := validateCreateWorkspaceCheckpointInput(input); err != nil {
		t.Fatalf("valid Patch Checkpoint metadata was rejected: %v", err)
	}
}
