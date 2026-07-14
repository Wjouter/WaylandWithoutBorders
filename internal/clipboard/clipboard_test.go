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

func TestExtractClipboardText(t *testing.T) {
	sep := textTypeSep
	cases := []struct{ name, raw, want string }{
		{"plain txt section", "TXThello" + sep, "hello"},
		{"bare text no markers", "just some text", "just some text"},
		{"txt preferred over rich", "TXTdef" + sep + "RTF{\\rtf1 x}" + sep + "HTM<b>y</b>" + sep, "def"},
		// The real bug: JetBrains sends RTF+HTML but no TXT section. We must
		// recover "def" from HTML rather than paste the whole marked-up blob.
		{
			"rtf+html only, no txt (jetbrains)",
			"RTF{\\rtf1\\ansi\\deff0{\\colortbl;}\ndef\\par}" + sep +
				"HTMVersion:1.0\r\nStartHTML:0000000128\r\nSourceURL:about:blank\r\n" +
				`<html><body><pre><span style="color:#cf8e6d;">def</span></pre></body></html>` + sep,
			"def",
		},
		{"html entities unescaped", "HTM<p>a &amp; b &lt;c&gt;</p>" + sep, "a & b <c>"},
		{"rtf only", "RTF{\\rtf1\\ansi{\\fonttbl{\\f0 Arial;}}\\f0 hello\\par}" + sep, "hello"},
		{"empty txt section falls through, not blob", "TXT" + sep + "HTM<i>hi</i>" + sep, "hi"},
	}
	for _, c := range cases {
		if got := extractClipboardText(c.raw); got != c.want {
			t.Errorf("%s: extractClipboardText = %q, want %q", c.name, got, c.want)
		}
	}
}
