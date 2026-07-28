package timefmt

import (
	"regexp"
	"testing"
	"time"
)

var (
	responsePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}[+-]\d{2}:\d{2}$`)
	storagePattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)
)

// kst is the offset the measured values in 0007-P 0.3 were taken in.
var kst = time.FixedZone("KST", 9*60*60)

func TestResponseMatchesMeasuredExample(t *testing.T) {
	// 0007-P 0.3: 2026-07-28T06:50:52.117482+09:00
	at := time.Date(2026, 7, 28, 6, 50, 52, 117482000, kst)
	got := Response(at)
	if got != "2026-07-28T06:50:52.117482+09:00" {
		t.Fatalf("Response = %q", got)
	}
}

func TestStorageMatchesMeasuredExample(t *testing.T) {
	// 0007-P 0.3: 2026-07-28T05:32:10.482Z
	at := time.Date(2026, 7, 28, 5, 32, 10, 482000000, time.UTC)
	got := Storage(at)
	if got != "2026-07-28T05:32:10.482Z" {
		t.Fatalf("Storage = %q", got)
	}
}

// 0007-P 0.3 keeps the fraction digits even when they are zero, unlike Python's
// isoformat(). A consumer parsing fixed-width offsets depends on it.
func TestZeroFractionIsNotOmitted(t *testing.T) {
	at := time.Date(2026, 7, 28, 6, 50, 52, 0, kst)
	if got := Response(at); got != "2026-07-28T06:50:52.000000+09:00" {
		t.Fatalf("Response = %q", got)
	}
	if got := Storage(at.UTC()); got != "2026-07-27T21:50:52.000Z" {
		t.Fatalf("Storage = %q", got)
	}
}

// 0008-L 1.6: both forms truncate. Rounding the storage form would flip the
// dedup window's string comparison at the boundary.
func TestTruncationNotRounding(t *testing.T) {
	// 999999999 ns rounds up to the next second; truncation must not.
	at := time.Date(2026, 7, 28, 5, 32, 10, 999999999, time.UTC)
	if got := Storage(at); got != "2026-07-28T05:32:10.999Z" {
		t.Fatalf("Storage = %q, want truncation to .999", got)
	}
	if got := Response(at.In(kst)); got != "2026-07-28T14:32:10.999999+09:00" {
		t.Fatalf("Response = %q, want truncation to .999999", got)
	}
}

func TestStorageIsUTCRegardlessOfInputZone(t *testing.T) {
	at := time.Date(2026, 7, 28, 14, 32, 10, 482000000, kst)
	if got := Storage(at); got != "2026-07-28T05:32:10.482Z" {
		t.Fatalf("Storage = %q", got)
	}
}

// Width is what the dedup comparison and any log parser rely on, so pin it for
// a range of instants rather than only the two documented examples.
func TestFixedWidth(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 64 {
		at := base.Add(time.Duration(i) * 37 * time.Millisecond)
		if got := Response(at); !responsePattern.MatchString(got) {
			t.Fatalf("Response(%v) = %q does not match contract shape", at, got)
		}
		if got := Storage(at); !storagePattern.MatchString(got) {
			t.Fatalf("Storage(%v) = %q does not match contract shape", at, got)
		}
	}
}
