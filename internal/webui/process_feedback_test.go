package webui

import (
	"os/exec"
	"testing"
)

func TestProcessFeedbackLifecycle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the browser state-machine regression tests")
	}
	output, err := exec.Command(node, "--test", "process_feedback_test.cjs").CombinedOutput()
	if err != nil {
		t.Fatalf("process feedback regression tests: %v\n%s", err, output)
	}
	t.Logf("%s", output)
}
