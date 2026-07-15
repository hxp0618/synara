package secretguard

import (
	"sync"
	"unicode/utf8"
)

// StreamMode selects sanitizing text output or unchanged binary output that is
// released only after it is safe from future cross-chunk matches.
type StreamMode string

const (
	StreamText             StreamMode = "text"
	StreamBinaryDetectOnly StreamMode = "binary-detect-only"
)

// Stream retains at most MaximumPatternLength-1 unsafe tail bytes. Callers must
// call Finish and Close, and must persist only bytes returned by Transform.
type Stream struct {
	mu         sync.Mutex
	guard      *Guard
	mode       StreamMode
	pending    []byte
	finished   bool
	closed     bool
	failure    error
	redactions int
}

func (g *Guard) NewStream(mode StreamMode) (*Stream, error) {
	if mode != StreamText && mode != StreamBinaryDetectOnly {
		return nil, newExposureError(ReasonInvalidMode)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed || g.matcher == nil {
		return nil, ErrClosed
	}
	stream := &Stream{guard: g, mode: mode}
	g.streams[stream] = struct{}{}
	return stream, nil
}

// Transform returns only bytes that cannot become part of a future cross-chunk
// match. The caller must persist the returned bytes, not the original chunk.
func (s *Stream) Transform(chunk []byte) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return nil, ErrClosed
	}
	if s.failure != nil {
		return nil, s.failure
	}
	return s.transformLocked(chunk, false)
}

func (s *Stream) Finish() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.finished {
		return nil, ErrClosed
	}
	if s.failure != nil {
		return nil, s.failure
	}
	output, err := s.transformLocked(nil, true)
	if err != nil {
		return nil, err
	}
	s.finished = true
	return output, nil
}

func (s *Stream) Redactions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.redactions
}

func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	guard := s.guard
	s.closeLocked()
	s.mu.Unlock()
	if guard != nil {
		guard.removeStream(s)
	}
	return nil
}

func (s *Stream) closeFromGuard() {
	s.mu.Lock()
	s.closeLocked()
	s.mu.Unlock()
}

func (s *Stream) closeLocked() {
	zero(s.pending)
	s.pending = nil
	s.guard = nil
	s.failure = nil
	s.closed = true
}

func (s *Stream) transformLocked(chunk []byte, finish bool) ([]byte, error) {
	guard := s.guard
	if guard == nil {
		return nil, ErrClosed
	}
	guard.mu.RLock()
	defer guard.mu.RUnlock()
	if guard.closed || guard.matcher == nil {
		return nil, ErrClosed
	}
	combined := make([]byte, 0, len(s.pending)+len(chunk))
	combined = append(combined, s.pending...)
	combined = append(combined, chunk...)
	zero(s.pending)
	s.pending = nil
	defer zero(combined)

	if s.mode == StreamBinaryDetectOnly && detectRepresentations(guard.matcher, combined) {
		s.failure = newExposureError(ReasonBinaryMatch)
		return nil, s.failure
	}

	limit := len(combined)
	if !finish && guard.matcher.maximumPatternLen > 1 {
		limit = len(combined) - guard.matcher.maximumPatternLen + 1
		if limit < 0 {
			limit = 0
		}
	}
	if s.mode == StreamText {
		for limit > 0 && limit < len(combined) && !utf8.RuneStart(combined[limit]) {
			limit--
		}
		if !utf8.Valid(combined[:limit]) || finish && !utf8.Valid(combined) {
			s.failure = newExposureError(ReasonInvalidText)
			return nil, s.failure
		}
	}
	if s.mode == StreamBinaryDetectOnly {
		output := append([]byte(nil), combined[:limit]...)
		if limit < len(combined) {
			s.pending = append([]byte(nil), combined[limit:]...)
		}
		return output, nil
	}
	best, found := representationMatches(guard.matcher, combined)
	if !found {
		output := append([]byte(nil), combined[:limit]...)
		if limit < len(combined) {
			s.pending = append([]byte(nil), combined[limit:]...)
		}
		return output, nil
	}

	output := make([]byte, 0, limit)
	position := 0
	for position < limit {
		if s.mode == StreamText && best[position] > 0 {
			output = append(output, RedactionMarker...)
			s.redactions++
			position += int(best[position])
			continue
		}
		output = append(output, combined[position])
		position++
	}
	if s.mode == StreamText && !utf8.Valid(output) {
		zero(output)
		s.failure = newExposureError(ReasonInvalidText)
		return nil, s.failure
	}
	if position < len(combined) {
		s.pending = append([]byte(nil), combined[position:]...)
	}
	return output, nil
}
