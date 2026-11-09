package access

import (
	"context"
	"os"
	"testing"
)

func TestLiveOSLogin(t *testing.T) {
	name, pw := os.Getenv("SPAWNPOINT_AUTH_TEST_USER"), os.Getenv("SPAWNPOINT_AUTH_TEST_PASSWORD")
	if name == "" || pw == "" {
		t.Skip("requires isolated native OS test identity")
	}
	expected, e := resolveOS(name)
	if e != nil {
		t.Fatal(e)
	}
	actual, e := verifyOS(context.Background(), name, pw)
	if e != nil || actual != expected {
		t.Fatalf("OS authentication/identity mismatch: %v", e)
	}
	if _, e = verifyOS(context.Background(), name, pw+"-wrong"); e == nil {
		t.Fatal("invalid OS password authenticated")
	}
}
