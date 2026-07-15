package secretguard

import "errors"

const (
	ErrorCode       = "secret_exposure_detected"
	RedactionMarker = "[REDACTED]"
)

const exposureMessage = "Sensitive credential material was blocked before persistence."

type Reason string

const (
	ReasonSecretTooShort    Reason = "secret_too_short"
	ReasonPatternLimit      Reason = "pattern_limit"
	ReasonPatternBytesLimit Reason = "pattern_bytes_limit"
	ReasonPatternTooLong    Reason = "pattern_too_long"
	ReasonInvalidBasicAuth  Reason = "invalid_basic_auth"
	ReasonUnsafeReplacement Reason = "unsafe_replacement"
	ReasonMapKeyMatch       Reason = "map_key_match"
	ReasonBinaryMatch       Reason = "binary_match"
	ReasonInvalidText       Reason = "invalid_text"
	ReasonUnsupportedValue  Reason = "unsupported_value"
	ReasonValueLimit        Reason = "value_limit"
	ReasonInvalidMode       Reason = "invalid_stream_mode"
)

// ExposureError deliberately contains no matched value or surrounding data.
// Callers may persist Code and Reason, but must not wrap it with unsafe input.
type ExposureError struct {
	Code   string
	Reason Reason
}

func (e *ExposureError) Error() string { return exposureMessage }

func newExposureError(reason Reason) error {
	return &ExposureError{Code: ErrorCode, Reason: reason}
}

func IsExposure(err error) bool {
	var exposure *ExposureError
	return errors.As(err, &exposure) && exposure.Code == ErrorCode
}

var ErrClosed = errors.New("secret guard is closed")
