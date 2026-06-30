//go:build linux

package clipboard

import (
	"strconv"
	"strings"
	"testing"
)

func TestWinBase(t *testing.T) {
	cases := map[string]string{
		`C:\Users\me\Desktop\report.pdf`: "report.pdf",
		"/home/me/file.txt":              "file.txt",
		`file.txt`:                       "file.txt",
	}
	for in, want := range cases {
		if got := winBase(in); got != want {
			t.Errorf("winBase(%q) = %q, want %q", in, got, want)
		}
	}
}

// The 1024-byte file header is the UTF-16LE string "<size>*<name>", null-padded.
// Verify our encode + the shared decode round-trips and parses.
func TestFileHeaderRoundTrip(t *testing.T) {
	name := `C:\Users\me\report.pdf`
	const size int64 = 4096
	header := make([]byte, clipHeaderSize)
	copy(header, encodeUTF16LE(strconv.FormatInt(size, 10)+"*"+name))

	got := decodeUTF16LE(header) // stops at the null padding
	parts := strings.SplitN(got, "*", 2)
	if len(parts) != 2 {
		t.Fatalf("header did not split: %q", got)
	}
	if parts[0] != "4096" || parts[1] != name {
		t.Errorf("parsed (%q,%q), want (4096,%q)", parts[0], parts[1], name)
	}
	if winBase(parts[1]) != "report.pdf" {
		t.Errorf("winBase = %q", winBase(parts[1]))
	}
}
