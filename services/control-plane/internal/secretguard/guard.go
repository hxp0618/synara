package secretguard

import "sync"

// Secret identifies one explicit secret byte sequence. BasicAuthUsername adds
// username:secret and HTTP Basic representations. New copies inputs; callers
// remain responsible for clearing their original buffers.
type Secret struct {
	Value             []byte
	BasicAuthUsername []byte
}

// Limits bound matcher memory, stream tail length, and representation growth.
type Limits struct {
	MinimumSecretBytes       int
	MaximumSecrets           int
	MaximumPatterns          int
	MaximumTotalPatternBytes int
	MaximumPatternBytes      int
}

func DefaultLimits() Limits {
	return Limits{
		MinimumSecretBytes:       8,
		MaximumSecrets:           32,
		MaximumPatterns:          1024,
		MaximumTotalPatternBytes: 8 << 20,
		MaximumPatternBytes:      256 << 10,
	}
}

// Stats contains only non-sensitive matcher sizing data.
type Stats struct {
	PatternCount         int
	TotalPatternBytes    int
	MaximumPatternLength int
}

// Guard is immutable after construction and safe for concurrent sanitization
// and independent streams. Close blocks new work and clears matcher state.
type Guard struct {
	mu      sync.RWMutex
	matcher *matcher
	closed  bool
	streams map[*Stream]struct{}
}

func New(secrets []Secret) (*Guard, error) {
	return NewWithLimits(secrets, DefaultLimits())
}

func NewWithLimits(secrets []Secret, limits Limits) (*Guard, error) {
	if limits.MinimumSecretBytes <= 0 || limits.MaximumSecrets <= 0 || limits.MaximumPatterns <= 0 ||
		limits.MaximumTotalPatternBytes <= 0 || limits.MaximumPatternBytes <= 0 ||
		limits.MinimumSecretBytes > limits.MaximumPatternBytes || len(secrets) > limits.MaximumSecrets {
		return nil, newExposureError(ReasonPatternLimit)
	}
	builder := newPatternBuilder(limits)
	defer builder.close()
	for _, secret := range secrets {
		if err := builder.addSecret(secret); err != nil {
			return nil, err
		}
	}
	compiled := newMatcher(builder.patterns, builder.total)
	if detectRepresentations(compiled, []byte(RedactionMarker)) {
		compiled.close()
		return nil, newExposureError(ReasonUnsafeReplacement)
	}
	return &Guard{matcher: compiled, streams: make(map[*Stream]struct{})}, nil
}

func (g *Guard) Stats() Stats {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed || g.matcher == nil {
		return Stats{}
	}
	return Stats{
		PatternCount: g.matcher.patternCount, TotalPatternBytes: g.matcher.totalPatternBytes,
		MaximumPatternLength: g.matcher.maximumPatternLen,
	}
}

func (g *Guard) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	streams := make([]*Stream, 0, len(g.streams))
	for stream := range g.streams {
		streams = append(streams, stream)
	}
	g.streams = nil
	if g.matcher != nil {
		g.matcher.close()
		g.matcher = nil
	}
	g.mu.Unlock()
	for _, stream := range streams {
		stream.closeFromGuard()
	}
	return nil
}

func (g *Guard) removeStream(stream *Stream) {
	g.mu.Lock()
	if g.streams != nil {
		delete(g.streams, stream)
	}
	g.mu.Unlock()
}
