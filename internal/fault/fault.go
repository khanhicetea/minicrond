// Package fault isolates panics at operation boundaries whose owners can
// release resources or report a failure. It does not restart failed operations.
package fault

import (
	"fmt"
	"runtime/debug"
)

// PanicError preserves the panic value and the stack at the recovery point.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

func (e *PanicError) Unwrap() error {
	err, _ := e.Value.(error)
	return err
}

// Call converts a panic in fn into an error on the same goroutine. Ordinary
// errors are returned unchanged. Callers remain responsible for cleanup.
func Call(fn func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			err = &PanicError{Value: value, Stack: debug.Stack()}
		}
	}()
	return fn()
}
