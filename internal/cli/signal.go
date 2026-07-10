package cli

import (
	"context"
	"fmt"
	"os"
)

// ErrInterrupted indicates the operation was interrupted by the user
var ErrInterrupted = NewExitError(fmt.Errorf("operation interrupted"), ExitInterrupted)

const ExitInterrupted = 130

// CheckInterrupted returns an error if context was cancelled
func CheckInterrupted(ctx context.Context) error {
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "Interrupted by user")
		return ErrInterrupted
	default:
		return nil
	}
}
