// Package fault supplies contextual errors for validation and operating-system adapters.
package fault

import "fmt"

// OperationError describes a rejected operation and optionally its underlying cause.
type OperationError struct {
	Cause   error
	Message string
}

// New creates a descriptive validation error without a synthetic global sentinel.
func New(message string) *OperationError { return &OperationError{Message: message, Cause: nil} }

// Wrap preserves errors.Is/As and returns nil when an operation succeeded.
func Wrap(operation string, cause error) error {
	if cause == nil {
		return nil
	}

	return &OperationError{Message: operation, Cause: cause}
}

// Error renders context followed by the original error.
func (e *OperationError) Error() string {
	if e.Cause == nil {
		return e.Message
	}

	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

// Unwrap exposes the original error for errors.Is/As.
func (e *OperationError) Unwrap() error { return e.Cause }
