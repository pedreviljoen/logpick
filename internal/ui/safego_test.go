package ui

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Mechanic 4 of the T12 test plan in app_test.go:
//
//  4. SafeGo converts a panicking goroutine into an error on the error channel
//     rather than crashing the process.
//
// The error channel is the synchronisation point. The test blocks on a receive,
// so there is no sleep and nothing to race with.
func TestSafeGo(t *testing.T) {
	errRead := errors.New("reading stream: boom")

	tests := []struct {
		name         string
		fn           func(ctx context.Context) error
		wantErr      error
		wantContains string
	}{
		{
			name:         "a panicking goroutine reports the panic as an error",
			fn:           func(context.Context) error { panic("boom in a goroutine") },
			wantErr:      ErrPanic,
			wantContains: "boom in a goroutine",
		},
		{
			name:         "an error returned by the goroutine reaches the channel",
			fn:           func(context.Context) error { return errRead },
			wantErr:      errRead,
			wantContains: "reading stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			errc := make(chan error, 1)
			SafeGo(ctx, errc, tt.fn)

			err := <-errc
			if err == nil {
				t.Fatal("SafeGo sent a nil error, want a failure")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want it to match %v", err, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantContains) {
				t.Errorf("error %q does not mention %q", err, tt.wantContains)
			}
		})
	}
}
