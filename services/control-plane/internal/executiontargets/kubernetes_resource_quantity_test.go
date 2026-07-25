package executiontargets

import "testing"

func TestParseKubernetesRequestedQuantity(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		scale int64
		want  *int64
	}{
		{name: "absent", raw: "", scale: 1000},
		{name: "zero", raw: "0", scale: 1},
		{name: "cpu millicores", raw: "500m", scale: 1000, want: int64TestPointer(500)},
		{name: "cpu decimal", raw: "0.25", scale: 1000, want: int64TestPointer(250)},
		{name: "cpu tiny rounds up", raw: "1n", scale: 1000, want: int64TestPointer(1)},
		{name: "memory binary", raw: "256Mi", scale: 1, want: int64TestPointer(268435456)},
		{name: "memory decimal", raw: "2G", scale: 1, want: int64TestPointer(2000000000)},
		{name: "decimal exponent", raw: "2e3", scale: 1, want: int64TestPointer(2000)},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := parseKubernetesRequestedQuantity(testCase.raw, testCase.scale)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.want == nil {
				if got != nil {
					t.Fatalf("quantity = %d, want nil", *got)
				}
				return
			}
			if got == nil || *got != *testCase.want {
				t.Fatalf("quantity = %#v, want %d", got, *testCase.want)
			}
		})
	}
}

func TestKubernetesRequestedResourceSnapshotRejectsInvalidQuantity(t *testing.T) {
	if _, err := kubernetesRequestedResourceSnapshot(map[string]string{"memory": "not-a-quantity"}); err == nil {
		t.Fatal("expected invalid Kubernetes quantity to be rejected")
	}
}

func int64TestPointer(value int64) *int64 { return &value }
