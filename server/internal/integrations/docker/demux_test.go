package docker_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ydtoloza/homarr/server/internal/integrations/docker"
)

// Demultiplexer tests — the Go port of the fixtures in
// packages/request-handler/src/docker-log-decode.spec.ts plus the streaming
// behaviors (fragmented frames, TTY pass-through, size limit).

func TestDecodeLogsMultiplexed(t *testing.T) {
	// "decodes multiplexed docker log frames"
	logs := concatBytes(encodeDockerLogFrame(1, "hello "), encodeDockerLogFrame(2, "world"))
	if got := docker.DecodeLogs(logs); got != "hello world" {
		t.Fatalf("DecodeLogs = %q", got)
	}
}

func TestDecodeLogsEmptyAndRaw(t *testing.T) {
	if got := docker.DecodeLogs(nil); got != "" {
		t.Fatalf("DecodeLogs(nil) = %q", got)
	}
	// "falls back to raw buffer text for non-multiplexed output": a first
	// byte outside {1, 2} means a TTY (raw) stream.
	if got := docker.DecodeLogs([]byte("raw docker output")); got != "raw docker output" {
		t.Fatalf("DecodeLogs = %q", got)
	}
	// Trailing partial frames are dropped like upstream's break; complete
	// content still decodes.
	logs := concatBytes(encodeDockerLogFrame(1, "complete"), []byte{1, 0, 0, 0, 9, 'x'})
	if got := docker.DecodeLogs(logs); got != "complete" {
		t.Fatalf("DecodeLogs = %q", got)
	}
}

func TestFrameReaderStream(t *testing.T) {
	// "emits decoded chunks from a stream": one chunk per frame.
	stream := concatBytes(
		encodeDockerLogFrame(1, "streamed log"),
		encodeDockerLogFrame(2, "error line\n"),
	)
	fr := docker.NewFrameReader(strings.NewReader(string(stream)))

	chunk, ok, err := fr.Next()
	if err != nil || !ok || chunk != "streamed log" {
		t.Fatalf("Next = %q ok=%v err=%v", chunk, ok, err)
	}
	chunk, ok, err = fr.Next()
	if err != nil || !ok || chunk != "error line\n" {
		t.Fatalf("Next = %q ok=%v err=%v", chunk, ok, err)
	}
	_, ok, err = fr.Next()
	if err != nil || ok {
		t.Fatalf("Next at EOF: ok=%v err=%v", ok, err)
	}
}

func TestFrameReaderFragmentedFrames(t *testing.T) {
	// A frame split across multiple reads is buffered until complete.
	frame := encodeDockerLogFrame(1, "a fragmented frame")
	fr := docker.NewFrameReader(newChunkedReader(frame, 3)) // 3 bytes per read

	var got []string
	for {
		chunk, ok, err := fr.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got = append(got, chunk)
	}
	if len(got) != 1 || got[0] != "a fragmented frame" {
		t.Fatalf("chunks = %q", got)
	}
}

func TestFrameReaderTTYPassThrough(t *testing.T) {
	// A raw (TTY) stream passes through as-is, chunked.
	fr := docker.NewFrameReader(strings.NewReader("raw docker output\nline two\n"))
	var got strings.Builder
	for {
		chunk, ok, err := fr.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got.WriteString(chunk)
	}
	if got.String() != "raw docker output\nline two\n" {
		t.Fatalf("pass-through = %q", got.String())
	}
}

func TestFrameReaderSizeLimit(t *testing.T) {
	// "returns false when message size exceeds limit" — the port aborts with
	// ErrFrameTooLarge (MAX_MESSAGE_SIZE = 1 MiB).
	payload := strings.Repeat("x", docker.MaxLogMessageSize+1)
	frame := encodeDockerLogFrame(1, payload)
	fr := docker.NewFrameReader(strings.NewReader(string(frame)))

	_, _, err := fr.Next()
	if !errors.Is(err, docker.ErrFrameTooLarge) {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
}

func TestFrameReaderChunkedRawUTF8(t *testing.T) {
	// Raw pass-through chunks never split a UTF-8 rune (the bus payloads are
	// JSON strings).
	text := "héllo wörld"
	fr := docker.NewFrameReader(newChunkedReader([]byte(text), 1))
	var got strings.Builder
	for {
		chunk, ok, err := fr.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		got.WriteString(chunk)
	}
	if got.String() != text {
		t.Fatalf("got %q, want %q", got.String(), text)
	}
}

// chunkedReader yields the input in fixed-size chunks.
type chunkedReader struct {
	data []byte
	pos  int
	n    int
}

func newChunkedReader(data []byte, n int) *chunkedReader { return &chunkedReader{data: data, n: n} }

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.n
	if len(p) < n {
		n = len(p)
	}
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}
