package cgroupv2limits

import "testing"

func TestValidateRequiresFiniteKernelCompatibleLimits(t *testing.T) {
	valid := Limits{
		PidsMax: 512, MemoryMaxBytes: 8 << 30, CPUQuotaMicros: 400_000, CPUPeriodMicros: 100_000,
	}
	if err := Validate(valid); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*Limits)
	}{
		{name: "zero pids", mutate: func(value *Limits) { value.PidsMax = 0 }},
		{name: "excessive pids", mutate: func(value *Limits) { value.PidsMax = 1_048_577 }},
		{name: "zero memory", mutate: func(value *Limits) { value.MemoryMaxBytes = 0 }},
		{name: "memory overflow", mutate: func(value *Limits) { value.MemoryMaxBytes = 1 << 63 }},
		{name: "short cpu period", mutate: func(value *Limits) { value.CPUPeriodMicros = 999 }},
		{name: "long cpu period", mutate: func(value *Limits) { value.CPUPeriodMicros = 1_000_001 }},
		{name: "short cpu quota", mutate: func(value *Limits) { value.CPUQuotaMicros = 999 }},
		{name: "excessive cpu ratio", mutate: func(value *Limits) { value.CPUQuotaMicros = value.CPUPeriodMicros*1_024 + 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := Validate(candidate); err == nil {
				t.Fatalf("invalid limits were accepted: %#v", candidate)
			}
		})
	}
}
