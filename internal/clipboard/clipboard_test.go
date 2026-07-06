//go:build linux

package clipboard

import (
	"strings"
	"testing"
)

// The Windows MWB receiver reads its entire decompress buffer, and .NET's
// DeflateStream leaves replayed window bytes just past the real data. The
// trailing separator turns that garbage into a split segment MWB discards.
// Without it, pasted text gains a corrupt suffix (see clipTextPayload). Guard
// the invariant so the separator is never dropped again.
func TestClipTextPayloadHasTrailingSeparator(t *testing.T) {
	got := clipTextPayload("sudo apt install triggerhappy ydotool")
	if !strings.HasPrefix(got, "TXT") {
		t.Errorf("payload %q missing TXT prefix", got)
	}
	if !strings.HasSuffix(got, textTypeSep) {
		t.Errorf("payload must end with the GUID separator, got %q", got)
	}
	// Round-trips back to the original text via the same parse the receiver uses.
	inner := strings.TrimSuffix(strings.TrimPrefix(got, "TXT"), textTypeSep)
	if inner != "sudo apt install triggerhappy ydotool" {
		t.Errorf("unexpected inner text %q", inner)
	}
}
