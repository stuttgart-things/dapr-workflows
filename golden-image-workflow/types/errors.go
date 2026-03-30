package types

import "fmt"

// TransientError signals a retryable failure.
type TransientError struct {
	Activity string
	Err      error
}

func (e *TransientError) Error() string {
	return fmt.Sprintf("transient error in %s: %v", e.Activity, e.Err)
}

func (e *TransientError) Unwrap() error { return e.Err }

// PermanentError signals a non-retryable failure.
type PermanentError struct {
	Activity string
	Err      error
}

func (e *PermanentError) Error() string {
	return fmt.Sprintf("permanent error in %s: %v", e.Activity, e.Err)
}

func (e *PermanentError) Unwrap() error { return e.Err }
