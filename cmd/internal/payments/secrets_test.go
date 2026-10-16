package payments_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/cmd/internal/payments"
)

// R097b: a provider account holds a REFERENCE to its secret (`env:STRIPE_SECRET_KEY`, `file:/run/secrets/stripe`), never the
// secret. The resolver is where a reference becomes a secret, at the moment of use (invariant 10): it returns the value and
// never keeps it, and no failure — a missing variable, an unreadable file, a scheme it does not know — ever puts a secret in
// the error it returns, because that error is logged.

// secret is made up. It is written in two pieces so that it is not a credential-shaped literal to a scanner: the tests need its shape (a
// provider key) and its exact bytes, not a string that looks like one in the source.
const secret = "sk_live_" + "THIS-IS-THE-SECRET-4f9a"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestAnEnvReferenceResolvesToTheVariableAndNothingElse(t *testing.T) {
	t.Parallel()
	r := payments.NewSecrets(env(map[string]string{"STRIPE_SECRET_KEY": secret, "OTHER": "other"}))
	got, err := r.Resolve(context.Background(), "env:STRIPE_SECRET_KEY")
	if err != nil || got != secret {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestAnEnvValueIsReturnedExactlyAsSet(t *testing.T) {
	t.Parallel()
	const padded = "  sk_test_padded \n"
	got, err := payments.NewSecrets(env(map[string]string{"PADDED": padded})).Resolve(context.Background(), "env:PADDED")
	if err != nil || got != padded {
		t.Fatalf("got %q, %v; want the value untouched: a secret is not the resolver's to tidy", got, err)
	}
}

func TestAFileReferenceResolvesToTheFileWithoutItsTrailingNewline(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, tc := range map[string]struct{ content, want string }{
		"no newline":                {secret, secret},
		"a newline":                 {secret + "\n", secret},
		"a CRLF":                    {secret + "\r\n", secret},
		"two newlines keep one":     {secret + "\n\n", secret + "\n"},
		"interior space is kept":    {"a b  c\n", "a b  c"},
		"leading space is the data": {"  " + secret + "\n", "  " + secret},
	} {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := payments.NewSecrets(env(nil)).Resolve(context.Background(), "file:"+path)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

func TestAReferenceThatCannotBeResolvedFailsClosedWithoutTheSecretInTheError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, []byte(strings.Repeat("S", 70<<10)+secret), 0o600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "good") // a perfectly readable secret, reached by a path that is not one we accept
	if err := os.WriteFile(good, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := payments.NewSecrets(env(map[string]string{"SET": secret, "BLANK": "   ", "EMPTY": ""}))
	for name, ref := range map[string]string{
		"an unset variable":            "env:NOT_SET_ANYWHERE",
		"an empty variable":            "env:EMPTY",
		"a blank variable":             "env:BLANK",
		"a variable with no name":      "env:",
		"a missing file":               "file:" + filepath.Join(dir, "missing"),
		"a directory":                  "file:" + dir,
		"an empty file":                "file:" + empty,
		"a file past the size we read": "file:" + big,
		"a relative path":              "file:main_test.go", // a file that exists in the working directory: only its relativeness is wrong
		"a path that climbs":           "file:" + dir + "/sub/../good",
		"a path with a dot segment":    "file:" + dir + "/./good",
		"a file with no path":          "file:",
		"a secret manager we lack":     "vault:secret/stripe#key",
		"a scheme in upper case":       "ENV:SET",
		"no scheme":                    "STRIPE_SECRET_KEY",
		"nothing":                      "",
	} {
		got, err := r.Resolve(context.Background(), ref)
		if err == nil {
			t.Errorf("%s: resolved to %q, want a refusal", name, got)
			continue
		}
		if got != "" {
			t.Errorf("%s: returned %q beside an error", name, got)
		}
		if !errors.Is(err, payments.ErrSecretUnavailable) {
			t.Errorf("%s: %v is not ErrSecretUnavailable, so a caller cannot tell a secret that is missing from one that is wrong", name, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "SSSSSSSS") {
			t.Errorf("%s: the error holds secret material: %v", name, err)
		}
	}
}

func TestAResolutionHonoursACancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := payments.NewSecrets(env(map[string]string{"SET": secret})).Resolve(ctx, "env:SET"); err == nil || got != "" {
		t.Fatalf("a cancelled call resolved %q, %v", got, err)
	}
}

func TestTheResolverKeepsNothingBetweenCalls(t *testing.T) {
	t.Parallel()
	vars := map[string]string{"K": "first"}
	r := payments.NewSecrets(func(k string) string { return vars[k] })
	if got, _ := r.Resolve(context.Background(), "env:K"); got != "first" {
		t.Fatalf("got %q", got)
	}
	vars["K"] = "rotated"
	if got, _ := r.Resolve(context.Background(), "env:K"); got != "rotated" {
		t.Fatalf("after a rotation got %q: a secret is read at the moment of use, never cached", got)
	}
}

func TestAFifoIsRefusedAtOnceAndNotWaitedOn(t *testing.T) {
	t.Parallel()
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := payments.NewSecrets(env(nil)).Resolve(context.Background(), "file:"+fifo)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, payments.ErrSecretUnavailable) {
			t.Fatalf("got %v, want a refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolving a FIFO waits for a writer: a delivery would hang on it")
	}
}

func TestEachKindOfFailureSaysWhatIsWrong(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string]string{
		"env:":                   "names no variable",
		"file:":                  "names no path",
		"env:UNSET_VAR_FOR_TEST": "UNSET_VAR_FOR_TEST",
		"vault:secret/x":         "vault",
		"nonsense":               "not a reference",
		"file:" + t.TempDir():    "not a regular file",
	} {
		_, err := payments.NewSecrets(env(nil)).Resolve(context.Background(), ref)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want a message that says %q", ref, err, want)
		}
	}
}
