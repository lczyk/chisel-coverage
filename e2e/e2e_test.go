//go:build e2e

// Package e2e runs the tool for real: a real chisel against the real archive
// and a real chisel-releases clone. Gated behind the e2e build tag and
// CHISEL_COVERAGE_E2E because it needs the chisel binary and network
// access.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lczyk/chisel-coverage/internal/cli"
	"github.com/lczyk/chisel-coverage/pkg/cache"
	"github.com/lczyk/chisel-coverage/pkg/cut"
	"github.com/lczyk/chisel-coverage/pkg/pool"
)

const releaseBranch = "ubuntu-24.04"

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("CHISEL_COVERAGE_E2E") != "1" {
		t.Skip("set CHISEL_COVERAGE_E2E=1 to run (needs chisel + network)")
	}
	chisel := os.Getenv("CHISEL")
	if chisel == "" {
		chisel = "chisel"
	}
	if _, err := exec.LookPath(chisel); err != nil {
		t.Skipf("chisel binary not found (%v); set CHISEL=/path/to/chisel", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
}

// cloneRelease shallow-clones a real chisel-releases checkout under dir.
func cloneRelease(t *testing.T, dir string) string {
	t.Helper()
	release := filepath.Join(dir, "releases")
	clone := exec.Command("git", "clone", "--quiet", "--depth", "1", "-b", releaseBranch,
		"https://github.com/canonical/chisel-releases", release)
	clone.Stdout, clone.Stderr = os.Stderr, os.Stderr
	if err := clone.Run(); err != nil {
		t.Fatalf("clone chisel-releases: %v", err)
	}
	return release
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it wrote.
func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := fn()
	os.Stdout = orig
	_ = w.Close()
	return code, <-done
}

type report struct {
	Packages []struct {
		Name   string `json:"name"`
		Arches []struct {
			Arch      string              `json:"arch"`
			Available bool                `json:"available"`
			Version   *string             `json:"version"`
			Error     *string             `json:"error"`
			Total     int                 `json:"total"`
			Covered   []string            `json:"covered"`
			Uncovered []string            `json:"uncovered"`
			Ignored   []string            `json:"ignored"`
			Slices    map[string][]string `json:"slices"`
		} `json:"arches"`
	} `json:"packages"`
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestBashCoverage(t *testing.T) {
	requireE2E(t)
	release := cloneRelease(t, t.TempDir())

	code, out := captureStdout(t, func() int {
		return cli.Main([]string{"--release", release, "--json", "--ignore", "/usr/share/man/**", "bash"})
	})
	if code != 0 {
		t.Fatalf("chisel-coverage exited %d:\n%s", code, out)
	}
	var rep report
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if len(rep.Packages) != 1 || len(rep.Packages[0].Arches) != 1 {
		t.Fatalf("unexpected shape:\n%s", out)
	}
	a := rep.Packages[0].Arches[0]
	if a.Error != nil || !a.Available {
		t.Fatalf("bash not cut cleanly:\n%s", out)
	}
	if a.Version == nil || *a.Version == "" {
		t.Fatalf("no version:\n%s", out)
	}
	if a.Total != len(a.Covered)+len(a.Uncovered)+len(a.Ignored) {
		t.Fatalf("counts do not add up:\n%s", out)
	}
	if !has(a.Covered, "/usr/bin/bash") || !has(a.Slices["bins"], "/usr/bin/bash") {
		t.Fatalf("/usr/bin/bash not covered by bins:\n%s", out)
	}
	if !has(a.Ignored, "/usr/share/man/man1/bash.1.gz") {
		t.Fatalf("man page not ignored:\n%s", out)
	}
	if len(a.Uncovered) == 0 {
		t.Fatalf("bash has nothing uncovered, which is not what its package slices file says:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(release, "slices", "bash.yaml")); err != nil {
		t.Fatalf("the checkout was modified: %v", err)
	}
}

// flaky is a transport that can be taken down, as the network can.
type flaky struct {
	down atomic.Bool
	rt   http.RoundTripper
}

func (f *flaky) RoundTrip(r *http.Request) (*http.Response, error) {
	if f.down.Load() {
		return nil, errors.New("the network is down")
	}
	return f.rt.RoundTrip(r)
}

// TestThinViews cuts real slices in views of the cache seeded with nothing,
// since no blob was used of late, twice: with the network down, where the
// cut that lacks a blob must be cut again in a whole view, and with it up,
// where the blob is fetched again and the views are made whole. Either way
// every cut ends as it did in the shared cache.
func TestThinViews(t *testing.T) {
	requireE2E(t)
	tmp := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "xdg"))
	release := cloneRelease(t, tmp)
	shared := cache.Dir()
	runner := &cut.Runner{Bin: cut.Binary()}
	net := &flaky{rt: http.DefaultTransport}
	slices := []string{"base-files_base", "libc6_libs", "libgcc-s1_libs"}
	arches := []string{"amd64", "arm64"}
	var units []pool.Unit
	var groups []pool.Group
	for _, a := range arches {
		for _, s := range slices {
			u := pool.Unit{Release: release, Arch: a, Slice: s}
			units = append(units, u)
			groups = append(groups, pool.Group{Lead: u})
		}
	}
	// age makes every blob one last used ten days ago.
	age := func() {
		old := time.Now().Add(-240 * time.Hour)
		err := filepath.WalkDir(shared, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			return os.Chtimes(p, old, old)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	paths := func(into map[string]int) func(pool.Cut) {
		return func(c pool.Cut) {
			mu.Lock()
			defer mu.Unlock()
			if c.Err != nil {
				t.Errorf("%s %s: %v", c.Arch, c.Slice, c.Err)
				return
			}
			into[c.Arch+" "+c.Slice] = len(c.Result.Paths)
		}
	}

	// In the shared cache, one after another, which also fills it and
	// leaves the pool every suite's InRelease.
	var out strings.Builder
	p, err := pool.New(context.Background(), pool.Options{Jobs: 4, Cache: shared, Runner: runner, Transport: net, Log: os.Stderr, Verbose: &out})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{}
	if err := p.Serial([][]pool.Unit{units}, paths(want)); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(units) {
		t.Fatalf("%d of %d cuts: %s", len(want), len(units), out.String())
	}

	// The network down: a view lacks every blob and nothing can be fetched.
	age()
	net.down.Store(true)
	got := map[string]int{}
	all := paths(got)
	if err := p.Pass(groups, func(c pool.Cut) bool { all(c); return true }); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if !maps.Equal(got, want) {
		t.Errorf("offline: got %v, want %v", got, want)
	}
	if !strings.Contains(out.String(), "again, in a whole cache view") {
		t.Errorf("offline: no cut was cut again:\n%s", out.String())
	}

	// The network up: what a view lacks is fetched again. The views the
	// pool kept are whole by now, so they go: the next ones are made new.
	if err := os.RemoveAll(filepath.Join(filepath.Dir(shared), "chisel-coverage")); err != nil {
		t.Fatal(err)
	}
	age()
	net.down.Store(false)
	out.Reset()
	p, err = pool.New(context.Background(), pool.Options{Jobs: 4, Cache: shared, Runner: runner, Transport: net, Log: os.Stderr, Verbose: &out})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]int{}
	all = paths(got)
	if err := p.Pass(groups, func(c pool.Cut) bool { all(c); return true }); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if !maps.Equal(got, want) {
		t.Errorf("online: got %v, want %v", got, want)
	}
	if !strings.Contains(out.String(), "cache views made whole") {
		t.Errorf("online: the views were not made whole:\n%s", out.String())
	}
}
