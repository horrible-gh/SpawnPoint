//go:build windows

package runas

import (
	"strings"
	"testing"
)

func TestPrivateDesktopACLDoesNotGrantOtherAccounts(t *testing.T) {
	logon, owner := "S-1-5-5-111-222", "S-1-5-19"
	got := desktopSDDL(logon, owner)
	if got != "D:P(A;;GA;;;SY)(A;;GA;;;"+owner+")(A;;GA;;;"+logon+")" {
		t.Fatalf("unexpected ACL: %s", got)
	}
	for _, broad := range []string{";;;WD)", ";;;AU)", ";;;BU)"} {
		if strings.Contains(got, broad) {
			t.Fatalf("broad principal: %s", got)
		}
	}
}
