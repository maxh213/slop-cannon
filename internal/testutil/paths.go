package testutil

import (
	"path/filepath"
	"testing"
)

// RealPath returns path with every symlink resolved. A process reports the
// resolved form of its working directory, so a test comparing what a child
// saw (its cwd, a path it printed) with a directory from t.TempDir must
// resolve that directory first: on macOS the temp root /var/folders/… is
// really /private/var/folders/….
func RealPath(t testing.TB, path string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("testutil: resolve %s: %v", path, err)
	}
	return real
}
