package persistence

import "testing"

func TestNormalizeLimit(t *testing.T) {
	tests := map[string]struct {
		value int
		want  int
	}{
		"fallback": {value: 0, want: 50},
		"value":    {value: 25, want: 25},
		"maximum":  {value: 500, want: 200},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeLimit(test.value, 50, 200); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
		})
	}
}
