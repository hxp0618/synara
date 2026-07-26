package metricfacts

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// These functions are the only thing keeping Prometheus label cardinality
// bounded: every label value on the Generation metric families comes straight
// out of a database column and is passed through one of them. The observability
// contract fixes the permitted sets, so this pins both halves of each — the
// accepted enumeration and, more importantly, the collapse of anything else.
// Dropping a `default` branch or forgetting to extend one of these would
// otherwise let a new or malformed column value become an unbounded label, and
// nothing else in the suite would notice.
func TestBoundedLabelsCollapseUnknownValues(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		bound    func(string) string
		accepted []string
		fallback string
	}{
		{
			name:     "target kind",
			bound:    BoundedTargetKind,
			accepted: []string{"local", "ssh", "docker", "kubernetes"},
			fallback: "other",
		},
		{
			name:     "recovery reason",
			bound:    BoundedRecoveryReason,
			accepted: []string{"initial-claim", "legacy-adoption", "execution-recovery", "suspend-resume"},
			fallback: "other",
		},
		{
			name:     "warm pool mode",
			bound:    BoundedWarmPoolMode,
			accepted: []string{"disabled", "balanced", "low-latency"},
			fallback: "disabled",
		},
		{
			name:     "warm pool result",
			bound:    BoundedWarmPoolResult,
			accepted: []string{"pending", "not-requested", "hit", "fallback"},
			fallback: "pending",
		},
		{
			name:     "generation outcome",
			bound:    BoundedGenerationOutcome,
			accepted: []string{"completed", "failed", "cancelled", "interrupted", "recovering"},
			fallback: "other",
		},
		{
			name:  "pod failure class",
			bound: BoundedPodFailureClass,
			accepted: []string{
				"pod-apply-failed", "pending-timeout", "unschedulable", "image-pull",
				"container-start", "evicted", "oom-killed", "pod-failed",
			},
			fallback: "other",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, accepted := range testCase.accepted {
				if got := testCase.bound(accepted); got != accepted {
					t.Fatalf("%q was not preserved: got %q", accepted, got)
				}
			}
			// Anything outside the contract set must collapse, including values
			// that would otherwise be per-Tenant or per-Execution unique.
			permitted := make(map[string]struct{}, len(testCase.accepted)+1)
			for _, accepted := range testCase.accepted {
				permitted[accepted] = struct{}{}
			}
			permitted[testCase.fallback] = struct{}{}
			for _, unknown := range []string{
				"", " ", "OTHER", "Kubernetes", "tenant-" + uuid.NewString(),
				uuid.NewString(), strings.Repeat("x", 512), "pod-apply-failed\n",
			} {
				got := testCase.bound(unknown)
				if got != testCase.fallback {
					t.Fatalf("%q collapsed to %q, want the %q fallback", unknown, got, testCase.fallback)
				}
				if _, ok := permitted[got]; !ok {
					t.Fatalf("%q produced an unbounded label %q", unknown, got)
				}
			}
		})
	}
}
