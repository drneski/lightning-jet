package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// lockDown takes write access to the database's directory and files away, as
// for an operator who is only in the service's group, and restores it when the
// test ends.
func lockDown(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("file permissions don't bind root")
	}
	dir := filepath.Dir(path)
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.Chmod(f, 0o440); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(dir, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(dir, 0o700)
		for _, f := range files {
			os.Chmod(f, 0o600)
		}
	})
}

// readInAnotherProcess reads the latest snapshot id the way jet status does,
// from a separate process, so SQLite can't share the writer's locks or memory.
func readInAnotherProcess(t *testing.T, path string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperReadOnlyReader$", "-test.count=1")
	cmd.Env = append(os.Environ(), "JET_TEST_READ_ONLY_DB="+path)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if id, ok := strings.CutPrefix(line, "snapshot="); ok {
			return id
		}
	}
	t.Fatalf("reader: %v\n%s", err, out)
	return ""
}

func TestHelperReadOnlyReader(t *testing.T) {
	path := os.Getenv("JET_TEST_READ_ONLY_DB")
	if path == "" {
		t.Skip("run by the read-only tests")
	}
	s, err := OpenReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st, err := s.LatestStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("snapshot=%s\n", st.SnapshotID)
}

func TestReadOnlyWhileJetRunHasItOpen(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	save := func(suffix string) {
		t.Helper()
		if err := s.SaveCycle(ctx, sampleCycle(t, suffix, t0, contracts.ModeShadow)); err != nil {
			t.Fatal(err)
		}
	}
	save("1")
	lockDown(t, path) // the writer keeps the write access it opened with
	if got := readInAnotherProcess(t, path); got != "s1" {
		t.Errorf("read %q, want s1", got)
	}
	save("2")
	if got := readInAnotherProcess(t, path); got != "s2" {
		t.Errorf("after another cycle, read %q, want s2", got)
	}
}

func TestReadOnlyWhileJetRunIsStopped(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.SaveCycle(ctx, sampleCycle(t, "1", t0, contracts.ModeShadow)); err != nil {
		t.Fatal(err)
	}
	s.Close() // folds the write-ahead log into the database and removes it
	lockDown(t, path)
	if got := readInAnotherProcess(t, path); got != "s1" {
		t.Errorf("read %q, want s1", got)
	}
	if files, _ := filepath.Glob(path + "*"); len(files) != 1 {
		t.Errorf("reading left files behind: %v", files)
	}
}

func TestOpenReadOnlyChangesNothing(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "jet.db")
	if _, err := OpenReadOnly(ctx, missing); err == nil {
		t.Error("a missing database was opened")
	}
	if _, err := os.Stat(missing); err == nil {
		t.Error("opening read-only created a database")
	}

	s, path := openTemp(t)
	if _, err := s.db.ExecContext(ctx, `UPDATE meta SET value = '1' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(ctx, path); err == nil || !strings.Contains(err.Error(), "jet run upgrades it") {
		t.Errorf("an older schema: err = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'schema_version'`, len(migrations)); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.SetMeta(ctx, "k", "v"); err == nil {
		t.Error("a read-only store wrote")
	}
}
