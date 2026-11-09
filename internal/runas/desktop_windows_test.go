//go:build windows

package runas

import (
	"strings"
	"testing"
)

func TestPrivateDesktopACLDoesNotGrantOtherAccounts(t *testing.T) {
	sid := "S-1-5-21-111-222-333-1001"
	got := desktopSDDL(sid)
	if got != "D:P(A;;GA;;;SY)(A;;GA;;;"+sid+")" {
		t.Fatalf("unexpected private desktop ACL: %s", got)
	}
	for _, broad := range []string{";;;WD)", ";;;AU)", ";;;BU)"} {
		if strings.Contains(got, broad) {
			t.Fatalf("private desktop grants a broad principal: %s", got)
		}
	}
}
