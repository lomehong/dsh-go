// Decode an SSE byte stream into event `data` payloads. Framing — chunk
// reassembly, UTF-8/CRLF/BOM handling, comment and non-data field skipping,
// multi-`data:` joining — follows the WHATWG server-sent-events algorithm
// as eventsource-parser implements it. Comments are reported only through
// the transport-activity callback. This module keeps the DeepSeek
// protocol: the literal `[DONE]` is yielded so the caller owns final
// flushing, and EOF before it fails with STREAM_CLOSED. Framing is
// spec-strict: an event dispatches only on its blank-line terminator, so
// an unterminated tail at EOF is truncation, not a flushable payload.
// Port of sse.ts.
package deepseek

import (
	"bufio"
	"io"
	"strings"

	"dshgo/llm"
)

// sseStream decodes one SSE byte stream.
type sseStream struct {
	reader    *bufio.Reader
	onComment func(string)

	line        strings.Builder // partial line across read boundaries
	field       strings.Builder // current event's concatenated data buffer
	started     bool            // the [DONE] sentinel has been yielded
	eof         bool
	consumedBOM bool
	err         error
}

// ParseSse parses an SSE byte stream into data payloads. Next yields each
// event's data payload in arrival order with the `[DONE]` sentinel last;
// after the sentinel is consumed, further Next calls return io.EOF. A
// stream that ends without the sentinel fails with an LlmError
// STREAM_CLOSED (truncated response — the model call cannot be trusted).
func ParseSse(stream io.Reader, onComment func(string)) PayloadStream {
	return &sseStream{reader: bufio.NewReader(stream), onComment: onComment}
}

// Next returns the next data payload, io.EOF at the sentinel or (with the
// stored error) at a premature end of stream.
func (s *sseStream) Next() (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if s.started {
		return "", io.EOF
	}
	for {
		line, ok, err := s.nextLine()
		if err != nil {
			s.err = err
			return "", err
		}
		if !ok {
			// EOF: an unterminated tail is truncation, not a flushable
			// payload (spec-strict dispatch on the blank-line terminator).
			s.err = llm.NewLlmError("SSE stream ended without [DONE]", "STREAM_CLOSED", llm.LlmFailure{})
			return "", s.err
		}
		if line == "" {
			// Blank line: dispatch the event when it carries data.
			if s.field.Len() == 0 {
				continue
			}
			data := strings.TrimSuffix(s.field.String(), "\n")
			s.field.Reset()
			if data == Done {
				s.started = true
			}
			return data, nil
		}
		if strings.HasPrefix(line, ":") {
			// Comment: transport activity only, never a payload.
			if s.onComment != nil {
				s.onComment(line[1:])
			}
			continue
		}
		field, value, hasColon := strings.Cut(line, ":")
		if hasColon && strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		if field == "data" {
			s.field.WriteString(value)
			s.field.WriteByte('\n')
		}
		// event:, id:, retry:, and unknown fields are ignored.
	}
}

// nextLine returns the next complete line (terminator stripped), handling
// CRLF, CR, and LF. ok is false at EOF.
func (s *sseStream) nextLine() (string, bool, error) {
	for {
		for {
			index := strings.IndexAny(s.line.String(), "\r\n")
			if index < 0 {
				break
			}
			full := s.line.String()
			line := full[:index]
			rest := full[index:]
			if strings.HasPrefix(rest, "\r\n") {
				rest = rest[2:]
			} else {
				rest = rest[1:]
			}
			s.line.Reset()
			s.line.WriteString(rest)
			if !s.consumedBOM {
				s.consumedBOM = true
				line = strings.TrimPrefix(line, "\xEF\xBB\xBF")
			}
			return line, true, nil
		}
		if s.eof {
			if s.line.Len() == 0 {
				return "", false, nil
			}
			// Trailing bytes without a terminator: kept, then reported as
			// truncation on the next dispatch check (never flushed).
			line := s.line.String()
			s.line.Reset()
			return line, true, nil
		}
		chunk := make([]byte, 8192)
		n, err := s.reader.Read(chunk)
		if n > 0 {
			s.line.Write(chunk[:n])
		}
		if err != nil {
			if err == io.EOF {
				s.eof = true
				continue
			}
			return "", false, err
		}
		if n == 0 {
			s.eof = true
		}
	}
}
