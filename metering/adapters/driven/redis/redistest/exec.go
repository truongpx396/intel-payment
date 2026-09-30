package redistest

import (
	"context"
	"io"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

func execIn(ctx context.Context, c testcontainers.Container, cmd ...string) (int, string, error) {
	code, r, err := c.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return code, "", err
	}
	b, _ := io.ReadAll(r)
	return code, string(b), nil
}
