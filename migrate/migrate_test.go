package migrate

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

const okSQL = "-- c\nBEGIN;\nCREATE TABLE t (id int);\nCOMMIT;\n"

func TestEmbeddedLoadsContiguousAndFramed(t *testing.T) {
	t.Parallel()
	ms, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations")
	}
	for i, m := range ms {
		if m.Version != i+1 {
			t.Errorf("position %d holds version %d", i+1, m.Version)
		}
		if len(m.Checksum) != 64 {
			t.Errorf("%04d checksum %q is not sha256 hex", m.Version, m.Checksum)
		}
	}
}

func TestLoadRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fs   fstest.MapFS
		want string
	}{
		"bad name":      {fstest.MapFS{"1_x.sql": {Data: []byte(okSQL)}}, "not NNNN_name.sql"},
		"gap":           {fstest.MapFS{"0001_a.sql": {Data: []byte(okSQL)}, "0003_c.sql": {Data: []byte(okSQL)}}, "without a gap"},
		"starts at two": {fstest.MapFS{"0002_a.sql": {Data: []byte(okSQL)}}, "without a gap"},
		"no commit":     {fstest.MapFS{"0001_a.sql": {Data: []byte("BEGIN;\nSELECT 1;\n")}}, "COMMIT;"},
		"no begin":      {fstest.MapFS{"0001_a.sql": {Data: []byte("SELECT 1;\nCOMMIT;\n")}}, "BEGIN;"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(c.fs)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error containing %q", err, c.want)
			}
		})
	}
}

func TestLoadIgnoresNonSQL(t *testing.T) {
	t.Parallel()
	ms, err := Load(fstest.MapFS{"README.md": {Data: []byte("x")}, "0001_a.sql": {Data: []byte(okSQL)}})
	if err != nil || len(ms) != 1 {
		t.Fatalf("got %v, %v", ms, err)
	}
}

func TestScriptRecordsVersionInsideTheTransaction(t *testing.T) {
	t.Parallel()
	ms, err := Load(fstest.MapFS{"0001_a.sql": {Data: []byte(okSQL)}})
	if err != nil {
		t.Fatal(err)
	}
	s, _ := ms[0].script()
	ins := strings.Index(s, "INSERT INTO schema_migrations")
	commit := strings.LastIndex(s, "COMMIT;")
	if ins < 0 || commit < ins {
		t.Fatalf("the version record must precede the final COMMIT:\n%s", s)
	}
	if strings.Count(s, "COMMIT;") != 1 {
		t.Fatalf("exactly one COMMIT expected:\n%s", s)
	}
}

func TestChecksumTracksContent(t *testing.T) {
	t.Parallel()
	a, _ := Load(fstest.MapFS{"0001_a.sql": {Data: []byte(okSQL)}})
	b, _ := Load(fstest.MapFS{"0001_a.sql": {Data: []byte("-- edited\n" + okSQL)}})
	if a[0].Checksum == b[0].Checksum {
		t.Fatal("an edited migration must change its checksum")
	}
}

func TestStatus(t *testing.T) {
	t.Parallel()
	ms, _ := Load(fstest.MapFS{
		"0001_a.sql": {Data: []byte(okSQL)},
		"0002_b.sql": {Data: []byte("BEGIN;\nCREATE TABLE u (id int);\nCOMMIT;\n")},
	})
	full := map[int]string{1: ms[0].Checksum, 2: ms[1].Checksum}

	t.Run("at head", func(t *testing.T) {
		t.Parallel()
		st, err := status(ms, full)
		if err != nil || st.Current != 2 || len(st.Pending) != 0 {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("empty database is behind", func(t *testing.T) {
		t.Parallel()
		st, err := status(ms, map[int]string{})
		if !errors.Is(err, ErrBehind) || len(st.Pending) != 2 {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("partially applied is behind", func(t *testing.T) {
		t.Parallel()
		st, err := status(ms, map[int]string{1: ms[0].Checksum})
		if !errors.Is(err, ErrBehind) || st.Current != 1 {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
	t.Run("edited migration is refused", func(t *testing.T) {
		t.Parallel()
		_, err := status(ms, map[int]string{1: "deadbeef", 2: ms[1].Checksum})
		if !errors.Is(err, ErrChecksum) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("ahead of this binary is ready", func(t *testing.T) {
		t.Parallel()
		ahead := map[int]string{1: ms[0].Checksum, 2: ms[1].Checksum, 3: "x"}
		st, err := status(ms, ahead)
		if err != nil || st.Ahead != 1 {
			t.Fatalf("got %+v, %v", st, err)
		}
	})
}
