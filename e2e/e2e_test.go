//go:build e2e

// Package e2e runs the tool for real: a real chisel against the real archive
// and a real chisel-releases clone. Gated behind the e2e build tag and
// CHISEL_COVERAGE_E2E because it needs the chisel binary and network
// access.
package e2e

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lczyk/chisel-coverage/internal/cli"
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
		t.Fatalf("bash has nothing uncovered, which is not what its sdf says:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(release, "slices", "bash.yaml")); err != nil {
		t.Fatalf("the checkout was modified: %v", err)
	}
}
