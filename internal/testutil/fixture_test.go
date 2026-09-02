package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewFixtureRepoCreatesCommittedMainBranch(t *testing.T) {
	r := NewFixtureRepo(t)

	require.Equal(t, "main", r.DefaultBranch)
	require.Empty(t, r.RemoteDir)
	require.Equal(t, "main", r.Git(t, "rev-parse", "--abbrev-ref", "HEAD"))
	require.Empty(t, r.Status(t), "working tree must be clean after the initial commit")
	require.Equal(t, []string{"Initial import of fixture project"}, r.Log(t))
	require.Len(t, r.Head(t), 40)

	for _, rel := range []string{"go.mod", "Makefile", "README.md", "main.go", "stats/stats.go", "stats/stats_test.go"} {
		_, err := os.Stat(filepath.Join(r.Dir, rel))
		require.NoError(t, err, rel)
	}
	tracked := strings.Split(r.Git(t, "ls-files"), "\n")
	require.Contains(t, tracked, "stats/stats.go")
	require.Equal(t, "Fixture Author", r.Git(t, "log", "-1", "--format=%an"))
}

func TestNewFixtureRepoWithBareRemote(t *testing.T) {
	r := NewFixtureRepo(t, WithBareRemote())

	require.NotEmpty(t, r.RemoteDir)
	require.Equal(t, r.RemoteDir, r.Git(t, "remote", "get-url", "origin"))
	require.Equal(t, "origin/main", r.Git(t, "rev-parse", "--abbrev-ref", "main@{upstream}"))

	remoteHead := r.Git(t, "ls-remote", "--heads", "origin", "main")
	require.True(t, strings.HasPrefix(remoteHead, r.Head(t)), "remote main must point at HEAD: %s", remoteHead)

	r.WriteFile(t, "NOTES.md", "pushed\n")
	sha := r.Commit(t, "Add notes")
	r.Git(t, "push", "-q", "origin", "main")
	require.True(t, strings.HasPrefix(r.Git(t, "ls-remote", "--heads", "origin", "main"), sha))
}

func TestFixtureRepoWriteFileAndCommit(t *testing.T) {
	r := NewFixtureRepo(t)
	r.WriteFile(t, "docs/nested/file.txt", "hello\n")
	require.Equal(t, "?? docs/", r.Status(t))

	sha := r.Commit(t, "Add nested file")
	require.Equal(t, sha, r.Head(t))
	require.Empty(t, r.Status(t))
	require.Equal(t, []string{"Add nested file", "Initial import of fixture project"}, r.Log(t))
}

func TestNewFixtureRepoWithSource(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "only.txt"), []byte("x"), 0o644))

	r := NewFixtureRepo(t, WithSource(src))
	require.Equal(t, "only.txt", r.Git(t, "ls-files"))
}

func TestNewFixtureRepoUsesRealGitDespiteShim(t *testing.T) {
	shim := FakeBin(t, "git", "echo intercepted >&2; exit 99")

	r := NewFixtureRepo(t)
	require.Empty(t, r.Status(t))
	require.Empty(t, shim.Invocations(t), "the fixture must never go through the git shim")
}

func TestFixtureSourceHasExactlyOneTODO(t *testing.T) {
	src := FixtureSourceDir(t)
	var todos []string
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "TODO") {
				todos = append(todos, strings.TrimSpace(line))
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, todos, 1, "the fixture plants exactly one TODO: %v", todos)
}

// goEnv is the environment for running the Go toolchain inside the fixture:
// the isolated git identity plus the developer's build cache, with any
// workspace file and toolchain switching disabled so the fixture module is
// built on its own.
func goEnv(r *FixtureRepo) []string {
	env := r.Env()
	for _, key := range []string{"GOCACHE", "GOPATH", "GOMODCACHE", "GOROOT", "GOFLAGS", "GOPROXY", "GOTMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return append(env, "GOWORK=off", "GOTOOLCHAIN=local", "GO111MODULE=on")
}

// goBinary returns the real go command, skipping any shim, or skips the test.
func goBinary(t *testing.T) string {
	t.Helper()
	path, err := RealBinary("go")
	if err != nil {
		t.Skip("go toolchain not available")
	}
	return path
}

func TestFixtureBuildsAndFailsExactlyOnPlantedBugs(t *testing.T) {
	r := NewFixtureRepo(t)
	goBin := goBinary(t)

	build := exec.Command(goBin, "build", "./...")
	build.Dir = r.Dir
	build.Env = goEnv(r)
	out, err := build.CombinedOutput()
	require.NoError(t, err, "go build ./... in fixture: %s", out)

	vet := exec.Command(goBin, "vet", "./...")
	vet.Dir = r.Dir
	vet.Env = goEnv(r)
	out, err = vet.CombinedOutput()
	require.NoError(t, err, "go vet ./... in fixture: %s", out)

	test := exec.Command(goBin, "test", "-json", "./...")
	test.Dir = r.Dir
	test.Env = goEnv(r)
	var stdout bytes.Buffer
	test.Stdout = &stdout
	test.Stderr = &stdout
	err = test.Run()
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr), "go test must fail on the planted bugs, got err=%v\n%s", err, stdout.String())

	var failed, passed []string
	dec := json.NewDecoder(&stdout)
	for {
		var ev struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
		}
		if err := dec.Decode(&ev); err != nil {
			break
		}
		if ev.Test == "" || strings.Contains(ev.Test, "/") {
			continue // package events and subtests
		}
		switch ev.Action {
		case "fail":
			failed = append(failed, ev.Test)
		case "pass":
			passed = append(passed, ev.Test)
		}
	}
	slices.Sort(failed)
	want := slices.Clone(PlantedBugTests)
	slices.Sort(want)
	require.Equal(t, want, failed, "exactly the planted bugs must fail")
	require.Subset(t, passed, []string{"TestSum", "TestMaxEmpty"})

	require.Empty(t, r.Status(t), "running the fixture's tests must not dirty the tree")
}

func TestFixtureMakefileTestTarget(t *testing.T) {
	makeBin, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make not available")
	}
	r := NewFixtureRepo(t)
	env := goEnv(r)

	build := exec.Command(makeBin, "build")
	build.Dir = r.Dir
	build.Env = env
	out, err := build.CombinedOutput()
	require.NoError(t, err, "make build: %s", out)

	test := exec.Command(makeBin, "test")
	test.Dir = r.Dir
	test.Env = env
	out, err = test.CombinedOutput()
	require.Error(t, err, "make test must fail on the planted bugs: %s", out)
	for _, name := range PlantedBugTests {
		require.Contains(t, string(out), "--- FAIL: "+name)
	}
	require.NotContains(t, string(out), "--- FAIL: TestSum")
	require.Empty(t, r.Status(t))
}
