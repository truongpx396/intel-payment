package redistest

import (
	"context"
	"io"

	"github.com/testcontainers/testcontainers-go"
)

func execIn(ctx context.Context, c testcontainers.Container, cmd ...string) (int, string, error) {
	code, r, err := c.Exec(ctx, cmd)
	if err != nil {
		return code, "", err
	}
	b, _ := io.ReadAll(r)
	return code, string(b), nil
}
