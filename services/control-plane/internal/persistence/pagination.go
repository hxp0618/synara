package persistence

func NormalizeLimit(value, fallback, maximum int) int {
	if value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}
