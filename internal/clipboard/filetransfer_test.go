//go:build linux

package clipboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// sendFileStream must 16-byte-align the body (AES-CBC requirement) by zero-
// padding the final block, while the header reports the true size.
func TestSendFileStreamPadsToBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	const size = 5014 // not a multiple of 16
	content := bytes.Repeat([]byte{0xAB}, size)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := sendFileStream(&buf, path); err != nil {
		t.Fatal(err)
	}

	body := buf.Bytes()[clipHeaderSize:] // skip the 1024-byte header
	if len(body)%16 != 0 {
		t.Errorf("body length %d not 16-aligned", len(body))
	}
	if len(body) != size+(16-size%16) {
		t.Errorf("body length %d, want %d", len(body), size+(16-size%16))
	}
	if !bytes.Equal(body[:size], content) {
		t.Error("first dataSize bytes do not match the file content")
	}
	header := decodeUTF16LE(buf.Bytes()[:clipHeaderSize])
	if !strings.HasPrefix(header, strconv.Itoa(size)+"*") {
		t.Errorf("header %q does not report true size", header)
	}
}

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
