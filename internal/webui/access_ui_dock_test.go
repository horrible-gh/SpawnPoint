package webui

import (
	"strings"
	"testing"
)

// The live access UI must keep process and log scrolling independent.
func TestAccessUIProcessLogDock(t *testing.T) {
	s := string(AccessUI())
	for _, required := range []string{
		"process-workspace",
		"process-list-panel",
		"data-process-splitter",
		"grid-template-rows:minmax(0,var(--process-list-fr",
		"log-dock>.log-body",
		"restoreLogDockRatio",
		"setLogDockRatio",
		"window.addEventListener('pointermove'",
		"window.addEventListener('pointerup'",
		"document.addEventListener('dblclick'",
		"processActionFeedback(p)",
	} {
		if !strings.Contains(s, required) {
			t.Errorf("live access UI lost process log dock behavior: %s", required)
		}
	}
}
