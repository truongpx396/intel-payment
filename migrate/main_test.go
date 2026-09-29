package migrate

import (
	"fmt"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// The migrator holds a pooled connection and an advisory lock; a goroutine still alive after the
// package's tests means a connection or a lock outlived its caller. goleak runs AFTER the shared
// container (integration builds) has been stopped, so only leaks of ours are reported.
func TestMain(m *testing.M) {
	code := m.Run()
	stopShared()
	if code == 0 {
		if err := goleak.Find(leakOptions()...); err != nil {
			fmt.Fprintln(os.Stderr, "goleak:", err)
			code = 1
		}
	}
	os.Exit(code)
}
