package appmeta

// In-package tests of the head reader (an unexported helper).

import (
	"strings"
	"testing"
)

// The head reader stops at <body> even without </head> and bounds the read.
func TestReadDocumentHead(t *testing.T) {
	huge := strings.Repeat("x", 300*1024) + "</head><body>"
	got, err := readDocumentHead(strings.NewReader(huge))
	if err != nil {
		t.Fatalf("readDocumentHead: %v", err)
	}
	if len(got) > 256*1024+8192 {
		t.Errorf("head read = %d bytes, want the bounded window", len(got))
	}
	noClose := "<html><head><title>t</title><body>x</body>"
	got, err = readDocumentHead(strings.NewReader(noClose))
	if err != nil {
		t.Fatalf("readDocumentHead: %v", err)
	}
	if !strings.Contains(got, "<title>t</title>") || strings.Contains(got, "<body>x") {
		t.Errorf("head = %q, want the cut before <body>", got)
	}
}

// The attribute parser reproduces the lookbehind: a suffix of a longer name
// (data-icon → icon) does not match, quoted and bare values parse.
func TestParseAttributes(t *testing.T) {
	attributes := parseAttributes(`<link data-icon="long" rel="apple-touch-icon" href='/a b.png' sizes=64x64>`)
	if attributes["rel"] != "apple-touch-icon" || attributes["href"] != "/a b.png" || attributes["sizes"] != "64x64" {
		t.Errorf("attributes = %v", attributes)
	}
	if _, ok := attributes["icon"]; ok {
		t.Error("data-icon leaked an icon attribute")
	}
}

// The entity decoder covers the numeric forms and leaves unknown names
// verbatim.
func TestDecodeEntities(t *testing.T) {
	if got := decodeEntities("A&amp;B &#65; &#x42; &copy; &nbsp;"); got != "A&B A B &copy;  " {
		t.Errorf("decodeEntities = %q", got)
	}
}
