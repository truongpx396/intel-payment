package envcfg_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The configuration is the operator's interface, and a setting that is read and not written down is a setting nobody can find,
// while one written down and not read is a promise nothing keeps: an operator who sets PAYMENT_LEDGER_ONLINE_RETENTION expecting
// a ledger partition to be detached gets a ledger that grows for ever, and learns it from the disk. The rule in the repository's
// instructions — every variable is in .env.example and in docs/configuration.md — is held here, in both directions. A variable
// that is documented and not read says so on its own line ("Not read"), as PAYMENT_DUNNING_SUSPEND_AFTER_DAYS does.

var (
	literal = regexp.MustCompile(`"(PAYMENT_[A-Z0-9_]+)"`)
	// envLine is a definition in .env.example — or a commented example of an optional one, whose default is not a value to write.
	envLine = regexp.MustCompile(`^#?\s?(PAYMENT_[A-Z0-9_]+)=`)
	// docsRow is a table row of docs/configuration.md and its first cell, which names the variable — or two, as the TLS pair is.
	docsRow  = regexp.MustCompile(`^\|([^|]+)\|`)
	cellName = regexp.MustCompile("`(PAYMENT_[A-Z0-9_]+)`")
)

const repoRoot = "../../.."

// readByCode is every PAYMENT_* variable a binary names. Only cmd/ reads the environment, so a string literal in its non-test Go is
// the whole of it.
func readByCode(t *testing.T) map[string]bool {
	t.Helper()
	read := map[string]bool{}
	err := filepath.WalkDir(filepath.Join(repoRoot, "cmd"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			read[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(read) < 40 {
		t.Fatalf("found only %d variables in cmd/: the scan is not seeing the code", len(read))
	}
	return read
}

// listed maps each variable a file defines to whether its own line says it is not read.
func listed(t *testing.T, file string, defines func(line string) []string) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, file))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		for _, name := range defines(line) {
			out[name] = strings.Contains(strings.ToLower(line), "not read")
		}
	}
	if len(out) < 40 {
		t.Fatalf("%s lists only %d variables: the scan is not seeing the file", file, len(out))
	}
	return out
}

// files are the two places a setting is written down, and how each says that a line defines a variable.
var files = []struct {
	file    string
	defines func(line string) []string
}{
	{".env.example", func(line string) []string {
		if m := envLine.FindStringSubmatch(line); m != nil {
			return []string{m[1]}
		}
		return nil
	}},
	{"docs/configuration.md", func(line string) []string {
		row := docsRow.FindStringSubmatch(line)
		if row == nil {
			return nil
		}
		var names []string
		for _, m := range cellName.FindAllStringSubmatch(row[1], -1) {
			names = append(names, m[1])
		}
		return names
	}},
}

func sorted(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEveryVariableTheCodeReadsIsInTheExampleEnvironmentAndTheConfigurationReference(t *testing.T) {
	t.Parallel()
	read := readByCode(t)
	for _, f := range files {
		have := listed(t, f.file, f.defines)
		for _, v := range sorted(read) {
			if _, ok := have[v]; !ok {
				t.Errorf("%s is read by a binary and is not in %s: nobody can find a setting that is not written down", v, f.file)
			}
		}
	}
}

func TestEveryVariableDocumentedAsLiveIsReadByTheCodeOrSaysItIsNot(t *testing.T) {
	t.Parallel()
	read := readByCode(t)
	for _, f := range files {
		for v, notRead := range listed(t, f.file, f.defines) {
			if !read[v] && !notRead {
				t.Errorf("%s documents %s as a setting and no binary reads it: an operator who sets it gets nothing — read it, remove it, or say \"Not read\" on its line", f.file, v)
			}
		}
	}
}
