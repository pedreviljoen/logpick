package ui

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	tea "github.com/charmbracelet/bubbletea"
)

// ErrPanic is the sentinel every recovered panic unwraps to, so a caller can
// tell a crash apart from an ordinary failure with errors.Is.
var ErrPanic = errors.New("panic in goroutine")

// PanicError is the error a recovered panic becomes. It unwraps to ErrPanic.
type PanicError struct {
	// Value is whatever was passed to panic.
	Value any
	// Stack is the stack trace captured at the point of recovery.
	Stack []byte
}

// Error reports the panic value. The stack is not part of the message: it is
// there for the debug log, which would otherwise be the only record of it.
func (e *PanicError) Error() string { return fmt.Sprintf("panic: %v", e.Value) }

// Unwrap returns ErrPanic.
func (e *PanicError) Unwrap() error { return ErrPanic }

// SafeGo runs fn in a goroutine that cannot take the process down.
//
// bubbletea recovers panics in its own loop, but a panic in a goroutine the
// application spawned is not its to catch, and the process dies with the
// terminal still in raw mode. Every goroutine this application spawns therefore
// goes through SafeGo.
//
// fn is given ctx and is expected to return when it is cancelled. A non-nil
// error from fn is sent on errc. A panic in fn is recovered and sent on errc as
// a *PanicError, which unwraps to ErrPanic. A nil return sends nothing.
//
// errc must not be nil and should be buffered. The send is abandoned when ctx
// is done, so a caller that has stopped reading errc cannot leak the goroutine.
func SafeGo(ctx context.Context, errc chan<- error, fn func(ctx context.Context) error) {
	go func() {
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = &PanicError{Value: r, Stack: debug.Stack()}
				}
			}()
			err = fn(ctx)
		}()

		if err == nil {
			return
		}

		select {
		case errc <- err:
		case <-ctx.Done():
		}
	}()
}

// WaitForError returns a command that takes one error off errc and reports it
// as an ErrorMsg. Like WaitForLines it is reissued by its handler, which is what
// keeps the errors from SafeGo flowing into the event loop for as long as the
// application runs.
func WaitForError(errc <-chan error) tea.Cmd {
	return func() tea.Msg {
		err, ok := <-errc
		if !ok {
			return nil
		}
		return ErrorMsg{Err: err}
	}
}
