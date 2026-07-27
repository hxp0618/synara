package cgroupv2limits

import "errors"

// Limits are finite hard limits for one untrusted Provider process tree.
type Limits struct {
	PidsMax         uint64 `json:"pidsMax"`
	MemoryMaxBytes  uint64 `json:"memoryMaxBytes"`
	CPUQuotaMicros  uint64 `json:"cpuQuotaMicros"`
	CPUPeriodMicros uint64 `json:"cpuPeriodMicros"`
}

func Validate(limits Limits) error {
	if limits.PidsMax == 0 || limits.PidsMax > 1_048_576 {
		return errors.New("protected cgroup Provider pids.max must be between 1 and 1048576")
	}
	if limits.MemoryMaxBytes == 0 || limits.MemoryMaxBytes > uint64(1<<63-1) {
		return errors.New("protected cgroup Provider memory.max must be a positive signed 64-bit byte count")
	}
	if limits.CPUPeriodMicros < 1_000 || limits.CPUPeriodMicros > 1_000_000 {
		return errors.New("protected cgroup Provider cpu.max period must be between 1000 and 1000000 microseconds")
	}
	if limits.CPUQuotaMicros < 1_000 || limits.CPUQuotaMicros > limits.CPUPeriodMicros*1_024 {
		return errors.New("protected cgroup Provider cpu.max quota must be between 1000 microseconds and 1024 CPUs")
	}
	return nil
}
