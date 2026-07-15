package secretguard

type matcherNode struct {
	next    map[byte]int
	failure int
	outputs []int
}

type matcher struct {
	nodes             []matcherNode
	maximumPatternLen int
	patternCount      int
	totalPatternBytes int
}

func newMatcher(patterns [][]byte, totalPatternBytes int) *matcher {
	result := &matcher{
		nodes:             []matcherNode{{next: make(map[byte]int)}},
		patternCount:      len(patterns),
		totalPatternBytes: totalPatternBytes,
	}
	for _, pattern := range patterns {
		state := 0
		for _, value := range pattern {
			next, found := result.nodes[state].next[value]
			if !found {
				next = len(result.nodes)
				result.nodes[state].next[value] = next
				result.nodes = append(result.nodes, matcherNode{next: make(map[byte]int)})
			}
			state = next
		}
		result.nodes[state].outputs = append(result.nodes[state].outputs, len(pattern))
		if len(pattern) > result.maximumPatternLen {
			result.maximumPatternLen = len(pattern)
		}
	}
	result.buildFailures()
	return result
}

func (m *matcher) buildFailures() {
	queue := make([]int, 0, len(m.nodes))
	for _, child := range m.nodes[0].next {
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for value, child := range m.nodes[state].next {
			queue = append(queue, child)
			failure := m.nodes[state].failure
			for failure != 0 {
				if candidate, found := m.nodes[failure].next[value]; found {
					failure = candidate
					break
				}
				failure = m.nodes[failure].failure
			}
			if failure == 0 {
				if candidate, found := m.nodes[0].next[value]; found && candidate != child {
					failure = candidate
				}
			}
			m.nodes[child].failure = failure
			if len(m.nodes[failure].outputs) > 0 {
				m.nodes[child].outputs = append(m.nodes[child].outputs, m.nodes[failure].outputs...)
			}
		}
	}
}

func (m *matcher) transition(state int, value byte) int {
	for {
		if next, found := m.nodes[state].next[value]; found {
			return next
		}
		if state == 0 {
			return 0
		}
		state = m.nodes[state].failure
	}
}

func (m *matcher) detect(value []byte) bool {
	if m == nil || m.patternCount == 0 {
		return false
	}
	state := 0
	for _, current := range value {
		state = m.transition(state, current)
		if len(m.nodes[state].outputs) > 0 {
			return true
		}
	}
	return false
}

func (m *matcher) detectWithRepresentationHints(value []byte) (bool, bool) {
	if m == nil || m.patternCount == 0 {
		return false, false
	}
	state := 0
	found := false
	hasEncodedRepresentation := false
	for _, current := range value {
		if current == '%' || current == '+' || current == '\\' {
			hasEncodedRepresentation = true
		}
		state = m.transition(state, current)
		if len(m.nodes[state].outputs) > 0 {
			found = true
		}
	}
	return found, hasEncodedRepresentation
}

// matches returns the longest match starting at each byte. Iterating the
// returned slice from left to right therefore implements leftmost-longest.
func (m *matcher) matches(value []byte) []uint32 {
	best := make([]uint32, len(value))
	if m == nil || m.patternCount == 0 {
		return best
	}
	state := 0
	for index, current := range value {
		state = m.transition(state, current)
		for _, length := range m.nodes[state].outputs {
			start := index + 1 - length
			if start >= 0 && uint32(length) > best[start] {
				best[start] = uint32(length)
			}
		}
	}
	return best
}

func (m *matcher) close() {
	if m == nil {
		return
	}
	for index := range m.nodes {
		for value := range m.nodes[index].next {
			delete(m.nodes[index].next, value)
		}
		for output := range m.nodes[index].outputs {
			m.nodes[index].outputs[output] = 0
		}
		m.nodes[index].outputs = nil
		m.nodes[index].failure = 0
		m.nodes[index].next = nil
	}
	m.nodes = nil
	m.maximumPatternLen = 0
	m.patternCount = 0
	m.totalPatternBytes = 0
}
