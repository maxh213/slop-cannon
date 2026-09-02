package testutil

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// PlantedBugTests lists the fixture tests that fail on a pristine copy of
// testdata/fixture-repo. They are the two intentional bugs an agent is
// expected to find and fix; every other fixture test passes.
var PlantedBugTests = []string{"TestMean", "TestMax"}

// FixtureRepo is a temporary git repository created from testdata/fixture-repo.
// It is removed when the test that created it finishes.
type FixtureRepo struct {
	// Dir is the working tree, with the fixture committed on the default branch.
	Dir string
	// RemoteDir is the bare repository added as "origin", or empty when the
	// repo was created without WithBareRemote.
	RemoteDir string
	// DefaultBranch is the name of the initial branch, always "main".
	DefaultBranch string

	env []string
}

// FixtureOption customises NewFixtureRepo.
type FixtureOption func(*fixtureConfig)

type fixtureConfig struct {
	source     string
	bareRemote bool
}

// WithBareRemote adds a bare repository as the "origin" remote and pushes
// the default branch to it, so tests can exercise push and fetch paths.
func WithBareRemote() FixtureOption {
	return func(c *fixtureConfig) { c.bareRemote = true }
}

// WithSource builds the repository from dir instead of testdata/fixture-repo.
func WithSource(dir string) FixtureOption {
	return func(c *fixtureConfig) { c.source = dir }
}

// FixtureSourceDir returns the absolute path of testdata/fixture-repo in the
// slop-cannon source tree. It fails the test if the directory is missing.
func FixtureSourceDir(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("testutil: cannot locate the package source directory")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "fixture-repo")
	dir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("testutil: resolving fixture path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("testutil: fixture source %s is missing go.mod: %v", dir, err)
	}
	return dir
}

// NewFixtureRepo copies testdata/fixture-repo into a fresh temporary
// directory, runs git init with "main" as the default branch, and makes an
// initial commit containing every fixture file. Git runs with an isolated
// identity and no user or system configuration, so the result does not
// depend on the machine. With WithBareRemote a bare "origin" is created and
// the default branch is pushed to it.
//
// The real git binary is used even when a git shim from FakeBin is on PATH.
func NewFixtureRepo(t testing.TB, opts ...FixtureOption) *FixtureRepo {
	t.Helper()
	cfg := fixtureConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.source == "" {
		cfg.source = FixtureSourceDir(t)
	}

	root := t.TempDir()
	dir := filepath.Join(root, "repo")
	if err := copyTree(cfg.source, dir); err != nil {
		t.Fatalf("testutil: copying fixture: %v", err)
	}

	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("testutil: creating fake home: %v", err)
	}
	r := &FixtureRepo{Dir: dir, DefaultBranch: "main", env: gitEnv(home)}

	r.Git(t, "init", "-q", "-b", r.DefaultBranch)
	r.Git(t, "add", "-A")
	r.Git(t, "commit", "-q", "-m", "Initial import of fixture project")

	if cfg.bareRemote {
		r.RemoteDir = filepath.Join(root, "remote.git")
		r.Git(t, "init", "-q", "--bare", "-b", r.DefaultBranch, r.RemoteDir)
		r.Git(t, "remote", "add", "origin", r.RemoteDir)
		r.Git(t, "push", "-q", "-u", "origin", r.DefaultBranch)
	}
	return r
}

// gitEnv returns an environment for git that ignores the user's own
// configuration and provides a fixed identity.
func gitEnv(home string) []string {
	env := []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_AUTHOR_NAME=Fixture Author",
		"GIT_AUTHOR_EMAIL=fixture@example.com",
		"GIT_COMMITTER_NAME=Fixture Author",
		"GIT_COMMITTER_EMAIL=fixture@example.com",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	return env
}

// Env returns the environment NewFixtureRepo uses for git: an isolated HOME,
// no global or system git config, and a fixed author identity. Tests that
// run git or bd themselves in the fixture can pass it to exec.Cmd.Env so the
// result does not depend on the developer's machine.
func (r *FixtureRepo) Env() []string {
	return append([]string(nil), r.env...)
}

// Git runs git with the given arguments inside the repository and returns
// its trimmed standard output. It fails the test on a non-zero exit. The
// real git binary is used, bypassing any shim installed with FakeBin.
func (r *FixtureRepo) Git(t testing.TB, args ...string) string {
	t.Helper()
	out, err := r.git(args...)
	if err != nil {
		t.Fatalf("testutil: git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (r *FixtureRepo) git(args ...string) (string, error) {
	bin, err := RealBinary("git")
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Head returns the full SHA of HEAD.
func (r *FixtureRepo) Head(t testing.TB) string {
	t.Helper()
	return r.Git(t, "rev-parse", "HEAD")
}

// Status returns `git status --porcelain`; an empty string means clean.
func (r *FixtureRepo) Status(t testing.TB) string {
	t.Helper()
	return r.Git(t, "status", "--porcelain")
}

// Log returns the one-line subjects of every commit on HEAD, newest first.
func (r *FixtureRepo) Log(t testing.TB) []string {
	t.Helper()
	out := r.Git(t, "log", "--format=%s")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// WriteFile writes content to rel (relative to the working tree), creating
// parent directories as needed. It does not stage or commit.
func (r *FixtureRepo) WriteFile(t testing.TB, rel, content string) {
	t.Helper()
	path := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("testutil: mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("testutil: writing %s: %v", rel, err)
	}
}

// Commit stages every change and commits it with message, returning the new
// HEAD SHA.
func (r *FixtureRepo) Commit(t testing.TB, message string) string {
	t.Helper()
	r.Git(t, "add", "-A")
	r.Git(t, "commit", "-q", "-m", message)
	return r.Head(t)
}

// copyTree copies the regular files and directories under src to dst,
// preserving file modes. Symlinks and other special files are rejected so a
// fixture can never point outside its own tree.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		default:
			return fmt.Errorf("%s: unsupported file type %v", path, info.Mode().Type())
		}
	})
}

// RealBinary resolves name on PATH while skipping every directory created
// by FakeBin, so helpers that must talk to the genuine tool still can while
// a shim of the same name is installed. It returns exec.ErrNotFound wrapped
// with the name when nothing outside those directories provides it.
func RealBinary(name string) (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, shimMarker)); err == nil {
			continue
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%s: %w (outside fake bin directories)", name, exec.ErrNotFound)
}
