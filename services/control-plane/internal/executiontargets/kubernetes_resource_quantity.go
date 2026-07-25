package executiontargets

import (
	"errors"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var kubernetesQuantityPattern = regexp.MustCompile(
	`^([+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))((?:[eE][+-]?[0-9]+)|(?:[numkMGTPE]|[KMGTPE]i)?)$`,
)

type kubernetesRequestedResources struct {
	CPUMillicores         *int64
	MemoryBytes           *int64
	EphemeralStorageBytes *int64
}

func kubernetesRequestedResourceSnapshot(requests map[string]string) (kubernetesRequestedResources, error) {
	if len(requests) == 0 {
		return kubernetesRequestedResources{}, nil
	}
	cpu, err := parseKubernetesRequestedQuantity(requests["cpu"], 1000)
	if err != nil {
		return kubernetesRequestedResources{}, err
	}
	memory, err := parseKubernetesRequestedQuantity(requests["memory"], 1)
	if err != nil {
		return kubernetesRequestedResources{}, err
	}
	ephemeral, err := parseKubernetesRequestedQuantity(requests["ephemeral-storage"], 1)
	if err != nil {
		return kubernetesRequestedResources{}, err
	}
	return kubernetesRequestedResources{
		CPUMillicores: cpu, MemoryBytes: memory, EphemeralStorageBytes: ephemeral,
	}, nil
}

// parseKubernetesRequestedQuantity converts the positive subset of Kubernetes
// resource.Quantity used by Pod requests into a retained integer unit. scale
// is 1000 for CPU cores -> millicores and 1 for byte quantities. Like
// Quantity.Value/MilliValue, positive fractional results round up so a
// non-zero request never disappears from accounting.
func parseKubernetesRequestedQuantity(raw string, scale int64) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	matches := kubernetesQuantityPattern.FindStringSubmatch(raw)
	if len(matches) != 3 || scale <= 0 {
		return nil, errors.New("invalid Kubernetes resource quantity")
	}
	value, ok := positiveDecimalRat(matches[1])
	if !ok {
		return nil, errors.New("invalid Kubernetes resource quantity")
	}
	multiplier, ok := kubernetesQuantityMultiplier(matches[2])
	if !ok {
		return nil, errors.New("unsupported Kubernetes resource quantity suffix")
	}
	value.Mul(value, multiplier)
	value.Mul(value, new(big.Rat).SetInt64(scale))
	if value.Sign() == 0 {
		return nil, nil
	}
	if value.Sign() < 0 {
		return nil, errors.New("Kubernetes resource request must not be negative")
	}
	result, ok := ceilPositiveRatToInt64(value)
	if !ok || result <= 0 {
		return nil, errors.New("Kubernetes resource request exceeds the supported range")
	}
	return &result, nil
}

func positiveDecimalRat(raw string) (*big.Rat, bool) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "+")
	if raw == "" {
		return nil, false
	}
	parts := strings.Split(raw, ".")
	if len(parts) > 2 {
		return nil, false
	}
	whole := parts[0]
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if whole == "" {
		whole = "0"
	}
	digits := whole + fraction
	if digits == "" {
		return nil, false
	}
	numerator := new(big.Int)
	if _, ok := numerator.SetString(digits, 10); !ok {
		return nil, false
	}
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(fraction))), nil)
	return new(big.Rat).SetFrac(numerator, denominator), true
}

func kubernetesQuantityMultiplier(suffix string) (*big.Rat, bool) {
	if suffix == "" {
		return new(big.Rat).SetInt64(1), true
	}
	if strings.HasPrefix(suffix, "e") || strings.HasPrefix(suffix, "E") {
		exponent, err := strconv.Atoi(suffix[1:])
		if err != nil || exponent < -18 || exponent > 18 {
			return nil, false
		}
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(exponent))), nil)
		if exponent < 0 {
			return new(big.Rat).SetFrac(big.NewInt(1), power), true
		}
		return new(big.Rat).SetInt(power), true
	}
	decimalPowers := map[string]int{
		"n": -9, "u": -6, "m": -3,
		"k": 3, "M": 6, "G": 9, "T": 12, "P": 15, "E": 18,
	}
	if exponent, found := decimalPowers[suffix]; found {
		power := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(absInt(exponent))), nil)
		if exponent < 0 {
			return new(big.Rat).SetFrac(big.NewInt(1), power), true
		}
		return new(big.Rat).SetInt(power), true
	}
	binaryPowers := map[string]int{"Ki": 1, "Mi": 2, "Gi": 3, "Ti": 4, "Pi": 5, "Ei": 6}
	if exponent, found := binaryPowers[suffix]; found {
		power := new(big.Int).Exp(big.NewInt(1024), big.NewInt(int64(exponent)), nil)
		return new(big.Rat).SetInt(power), true
	}
	return nil, false
}

func ceilPositiveRatToInt64(value *big.Rat) (int64, bool) {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
