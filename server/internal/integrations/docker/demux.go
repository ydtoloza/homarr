package docker

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Docker stream demultiplexer — the Go port of
// packages/request-handler/src/docker-log-decode.ts.
//
// When a container runs WITHOUT a TTY, the Engine prefixes every chunk of
// stdout/stderr with an 8-byte header: [streamType, 0, 0, 0, length uint32BE]
// where streamType is 1 (stdout) or 2 (stderr). With a TTY the stream is raw
// bytes. Upstream treats any first byte other than 1 or 2 as "not
// multiplexed" and passes the buffer through raw.

// dockerStreamHeaderSize is DOCKER_STREAM_HEADER_SIZE (docker-log-decode.ts:1).
const dockerStreamHeaderSize = 8

// MaxLogMessageSize is the upstream MAX_MESSAGE_SIZE of the streaming
// processor (request-handler/src/docker.ts:289, 1024 * 1024 bytes).
const MaxLogMessageSize = 1024 * 1024

// DecodeLogs ports decodeDockerLogs (docker-log-decode.ts:4-43): decode a
// COMPLETE buffer of Engine log output. Multiplexed frames are joined in
// stream order; anything that is not a valid frame stream falls back to the
// raw text (TTY containers, podman variations).
func DecodeLogs(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var parts []string
	cursor := 0
	for cursor < len(raw) {
		if cursor+dockerStreamHeaderSize > len(raw) {
			break // trailing partial header: ignored like upstream's break
		}
		streamType := raw[cursor]
		if streamType != 1 && streamType != 2 {
			return string(raw) // not multiplexed: raw text
		}
		length := int(binary.BigEndian.Uint32(raw[cursor+4 : cursor+8]))
		start := cursor + dockerStreamHeaderSize
		end := start + length
		if end > len(raw) {
			break // trailing partial payload
		}
		parts = append(parts, string(raw[start:end]))
		cursor = end
	}
	decoded := strings.Join(parts, "")
	if decoded == "" {
		return string(raw)
	}
	return decoded
}

// FrameReader ports createDockerLogStreamProcessor (docker-log-decode.ts:45-117)
// as a pull-based iterator over the live Engine stream: Next yields one
// decoded text chunk per multiplexed frame (upstream calls onData per frame;
// the docker router forwards each chunk to the tRPC subscription, so the
// chunk granularity is part of the log subscription contract). Frames may
// arrive fragmented — the reader buffers until a complete frame is available.
//
// A frame larger than MaxLogMessageSize aborts the stream with
// ErrFrameTooLarge (upstream: onError + stream.destroy,
// request-handler/src/docker.ts:290-299). A non-multiplexed stream (TTY)
// passes the remaining bytes through raw, one chunk per read.
type FrameReader struct {
	br      *bufio.Reader
	pending []byte // partial frame buffer (header + payload prefix)
	scratch []byte // reused read buffer
	ttyMode bool   // switched on when the first byte is not a frame type
	eof     bool
}

// NewFrameReader wraps the raw Engine log stream.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{br: bufio.NewReader(r)}
}

// Next returns the next decoded chunk. ok=false means the stream ended (EOF
// or raw pass-through exhausted); err is ErrFrameTooLarge for oversized
// frames or a read error.
func (fr *FrameReader) Next() (chunk string, ok bool, err error) {
	for {
		if fr.ttyMode {
			return fr.nextRaw()
		}
		if len(fr.pending) >= dockerStreamHeaderSize {
			streamType := fr.pending[0]
			if streamType != 1 && streamType != 2 {
				// Not a multiplexed stream: everything buffered plus the rest
				// of the stream is raw text (docker-log-decode.ts:82-87).
				fr.ttyMode = true
				return fr.nextRaw()
			}
			length := int(binary.BigEndian.Uint32(fr.pending[4:8]))
			if length > MaxLogMessageSize {
				return "", false, fmt.Errorf("%w (%d bytes > %d bytes)", ErrFrameTooLarge, length, MaxLogMessageSize)
			}
			if len(fr.pending) >= dockerStreamHeaderSize+length {
				payload := string(fr.pending[dockerStreamHeaderSize : dockerStreamHeaderSize+length])
				fr.pending = fr.pending[dockerStreamHeaderSize+length:]
				return payload, true, nil
			}
		}
		if fr.eof {
			if len(fr.pending) > 0 {
				// Trailing bytes that never completed a frame: upstream's
				// loop breaks and discards them (docker-log-decode.ts:78-80).
				fr.pending = nil
			}
			return "", false, nil
		}
		if err := fr.fill(); err != nil {
			return "", false, err
		}
	}
}

// nextRaw emits the buffered remainder and then the underlying stream as raw
// text chunks. The chunks stop at UTF-8 boundaries so the JSON-string frames
// stay valid (upstream emits whatever Buffer.toString yields; xterm tolerates
// partial sequences, but the bus payloads are JSON strings).
func (fr *FrameReader) nextRaw() (string, bool, error) {
	for {
		if len(fr.pending) > 0 {
			chunk := truncateAtRune(fr.pending)
			if len(chunk) > 0 {
				if len(chunk) == len(fr.pending) {
					fr.pending = nil
				} else {
					fr.pending = fr.pending[len(chunk):]
				}
				return string(chunk), true, nil
			}
			// pending holds only an incomplete rune prefix: wait for more
			// bytes before emitting anything.
			if fr.eof {
				fr.pending = nil
				return "", false, nil
			}
		} else if fr.eof {
			return "", false, nil
		}
		if err := fr.fill(); err != nil {
			return "", false, err
		}
	}
}

// fill reads more bytes into the pending buffer. In frame mode the buffer
// never exceeds the header plus one frame payload: Next rejects frames
// larger than MaxLogMessageSize as soon as their header is complete. In TTY
// mode nextRaw drains the buffer on every call.
func (fr *FrameReader) fill() error {
	buf := fr.scratch
	if buf == nil {
		buf = make([]byte, 64*1024)
		fr.scratch = buf
	}
	n, err := fr.br.Read(buf)
	if n > 0 {
		fr.pending = append(fr.pending, buf[:n]...)
	}
	if err == io.EOF {
		fr.eof = true
		return nil
	}
	return err
}

// truncateAtRune shortens b so it ends on a valid UTF-8 boundary.
func truncateAtRune(b []byte) []byte {
	for len(b) > 0 {
		r, size := utf8.DecodeLastRune(b)
		if r != utf8.RuneError || size > 1 {
			return b
		}
		// A trailing incomplete sequence: drop it for now; fill() will
		// complete it on the next read.
		b = b[:len(b)-1]
	}
	return b
}
