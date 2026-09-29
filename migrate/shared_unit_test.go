//go:build !integration

package migrate

import "go.uber.org/goleak"

func stopShared()                  {}
func leakOptions() []goleak.Option { return nil }
