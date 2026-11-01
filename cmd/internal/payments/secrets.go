package payments

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SecretSchemes are the reference schemes Resolve understands. An account naming any other would have its deliveries answered
// 500 for ever, so the admin API accepts only these.
var SecretSchemes = []string{"env", "file"}

// ErrSecretUnavailable is why a reference could not be turned into a secret. Its message never holds secret material: it is
// logged.
var ErrSecretUnavailable = errors.New("secret unavailable")

// maxSecretFile is the most a secret file may hold. A credential is a few hundred bytes; a file past this is not one, and
// reading it whole is how a mistaken reference becomes an out-of-memory.
const maxSecretFile = 64 << 10

// Secrets turns the reference a provider account holds into the secret it names, at the moment of use (invariant 10). It
// understands two schemes:
//
//	env:NAME       the environment variable NAME
//	file:/abs/path the contents of the file, less one trailing line ending (a mounted secret ends in one)
//
// Any other scheme — a secret manager this build has no client for — is refused, never guessed at. Nothing is kept between
// calls, so a rotated secret takes effect on the next delivery with no restart.
type Secrets struct{ getenv func(string) string }

// NewSecrets resolves env: references through getenv, which is the process's (os.Getenv in main, a map in a test).
func NewSecrets(getenv func(string) string) *Secrets { return &Secrets{getenv: getenv} }

func unavailable(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrSecretUnavailable, fmt.Sprintf(format, a...))
}

// Resolve implements ports.SecretResolver.
func (s *Secrets) Resolve(ctx context.Context, ref string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrSecretUnavailable, err)
	}
	scheme, name, ok := strings.Cut(ref, ":")
	switch {
	case !ok:
		return "", unavailable("%q is not a reference (want scheme:name, e.g. env:STRIPE_SECRET_KEY)", ref)
	case scheme == "env":
		return s.env(name)
	case scheme == "file":
		return readSecretFile(name)
	}
	return "", unavailable("the scheme %q of %q is not one this build resolves (env: and file: are)", scheme, ref)
}

func (s *Secrets) env(name string) (string, error) {
	if name == "" {
		return "", unavailable("env: names no variable")
	}
	v := s.getenv(name)
	if strings.TrimSpace(v) == "" {
		return "", unavailable("the environment variable %s is not set", name)
	}
	return v, nil
}

func readSecretFile(path string) (string, error) {
	switch {
	case path == "":
		return "", unavailable("file: names no path")
	case !filepath.IsAbs(path) || filepath.Clean(path) != path:
		return "", unavailable("the path %q is not a clean absolute path", path)
	}
	// Before it is opened, not after: opening a FIFO waits for a writer that may never come, and this runs on a delivery.
	st, err := os.Stat(path)
	if err != nil {
		return "", unavailable("file %s: %s", path, reason(err))
	}
	if !st.Mode().IsRegular() {
		return "", unavailable("file %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", unavailable("file %s: %s", path, reason(err))
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return "", unavailable("file %s: %s", path, reason(err))
	}
	if len(b) > maxSecretFile {
		return "", unavailable("file %s is larger than %d bytes", path, maxSecretFile)
	}
	v := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if strings.TrimSpace(v) == "" {
		return "", unavailable("file %s is empty", path)
	}
	return v, nil
}

// reason is the operating system's words for a failure, without the path it quotes.
func reason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "unreadable"
}
