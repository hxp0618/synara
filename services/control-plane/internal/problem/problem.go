package problem

import "fmt"

type Error struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
	Cause   error
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func Wrap(status int, code, message string, cause error) *Error {
	return &Error{Status: status, Code: code, Message: message, Cause: cause}
}
