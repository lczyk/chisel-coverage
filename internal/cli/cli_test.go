package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/lczyk/assert"

	"github.com/lczyk/chisel-slice-coverage/internal/coverage"
)

func TestParseArgs(t *testing.T) {
	f, pkgs, err := parseArgs([]string{"--release", "/r", "--arch", "amd64,arm64", "--arch=s390x", "-v",
		"bash", "--slice", "bins", "--ignore", "/usr/share/doc/**,/usr/share/man/**", "--json",
		"--fail-uncovered", "--keep", "--no-group", "coreutils"}, io.Discard)
	assert.NoError(t, err)
	assert.EqualArrays(t, pkgs, []string{"bash", "coreutils"})
	assert.That(t, f.NoGroup)
	assert.Equal(t, f.Release, "/r")
	assert.EqualArrays(t, f.Arches, []string{"amd64", "arm64", "s390x"})
	assert.EqualArrays(t, f.Slices, []string{"bins"})
	assert.EqualArrays(t, f.Ignores, []string{"/usr/share/doc/**", "/usr/share/man/**"})
	assert.That(t, f.JSON && f.FailUncovered && f.Keep)
	assert.Len(t, f.Verbose, 1)

	f, _, err = parseArgs([]string{"-vv", "bash"}, io.Discard)
	assert.NoError(t, err)
	assert.Len(t, f.Verbose, 2)
	assert.Equal(t, f.Jobs, -1)
	f, _, err = parseArgs([]string{"-j", "4", "bash"}, io.Discard)
	assert.NoError(t, err)
	assert.Equal(t, f.Jobs, 4)
	f, _, err = parseArgs([]string{"--jobs=-1", "bash"}, io.Discard)
	assert.NoError(t, err)
	assert.Equal(t, f.Jobs, -1)

	f, _, err = parseArgs([]string{"bash"}, io.Discard)
	assert.NoError(t, err)
	assert.Equal(t, f.Release, ".")
	assert.Nil(t, f.Arches)

	_, _, err = parseArgs([]string{"--arch"}, io.Discard)
	assert.Error(t, err, "expected argument for flag `--arch'")
	_, _, err = parseArgs([]string{"--nope", "bash"}, io.Discard)
	assert.Error(t, err, "unknown flag `nope'")
	_, _, err = parseArgs([]string{"-x", "bash"}, io.Discard)
	assert.Error(t, err, "unknown flag `x'")
	_, pkgs, err = parseArgs([]string{"--json"}, io.Discard)
	assert.NoError(t, err)
	assert.Len(t, pkgs, 0)

	var help bytes.Buffer
	f, _, err = parseArgs([]string{"--release", "/r", "-h"}, &help)
	assert.NoError(t, err)
	assert.That(t, f.help && !f.Version)
	assert.ContainsString(t, help.String(), "--release=<dir>")
	assert.ContainsString(t, help.String(), "(default: .)")
	f, _, err = parseArgs([]string{"bash", "--version"}, io.Discard)
	assert.NoError(t, err)
	assert.That(t, f.Version && !f.help)
}

func TestResolveArches(t *testing.T) {
	host, err := resolveArches(nil)
	assert.NoError(t, err)
	assert.Len(t, host, 1)

	all, err := resolveArches([]string{"s390x", "all", "amd64"})
	assert.NoError(t, err)
	assert.EqualArrays(t, all, []string{"s390x", "amd64", "arm64", "armhf", "i386", "ppc64el", "riscv64"})

	_, err = resolveArches([]string{"x86"})
	assert.Error(t, err, `invalid architecture "x86"`)
}

func sample() []coverage.Package {
	arch := func(name string) coverage.Arch {
		triplet := map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}[name]
		return coverage.Arch{
			Arch: name, Available: true, Version: "5.2-1", DebArch: name, Total: 6,
			Covered:   []string{"/usr/", "/usr/bin/", "/usr/bin/bash"},
			Uncovered: []string{"/usr/lib/", "/usr/lib/" + triplet + "/"},
			Ignored:   []string{"/usr/share/"},
			Slices:    map[string][]string{"bins": {"/usr/", "/usr/bin/", "/usr/bin/bash"}, "scripts": {}},
		}
	}
	return []coverage.Package{
		{Name: "bash", Slices: []string{"bins", "scripts"}, Arches: []coverage.Arch{
			arch("amd64"),
			{Arch: "i386"},
			arch("arm64"),
			{Arch: "s390x", Err: errors.New("chisel cut ... exited 1\n  boom")},
		}},
		{Name: "tzdata", Slices: []string{"zones"}, Arches: []coverage.Arch{{
			Arch: "amd64", Available: true, Version: "2026a-1", DebArch: "all", Total: 1,
			Covered: []string{"/usr/"}, Uncovered: []string{}, Ignored: []string{},
			Slices: map[string][]string{"zones": {"/usr/"}},
		}}},
		{Name: "empty", Arches: []coverage.Arch{{
			Arch: "amd64", Available: true, Covered: []string{}, Uncovered: []string{}, Ignored: []string{},
			Slices: map[string][]string{},
		}}},
	}
}

func TestRenderText(t *testing.T) {
	var out bytes.Buffer
	renderText(&out, sample(), true, style{})
	assert.Equal(t, out.String(), `bash 5.2-1 [amd64, arm64]
total 6 covered 3 ignored 1 uncovered 2
/usr/lib/
/usr/lib/<arch>-linux-<gnu>/
bash [i386]: not available
bash [s390x]: error
  chisel cut ... exited 1
    boom
tzdata 2026a-1 (all) [amd64]
total 1 covered 1 ignored 0 uncovered 0
empty [amd64]
total 0 covered 0 ignored 0 uncovered 0
`)
}

func TestRenderTextUngrouped(t *testing.T) {
	var out bytes.Buffer
	renderText(&out, sample()[:1], false, style{})
	assert.Equal(t, out.String(), `bash 5.2-1 [amd64]
total 6 covered 3 ignored 1 uncovered 2
/usr/lib/
/usr/lib/x86_64-linux-gnu/
bash [i386]: not available
bash 5.2-1 [arm64]
total 6 covered 3 ignored 1 uncovered 2
/usr/lib/
/usr/lib/aarch64-linux-gnu/
bash [s390x]: error
  chisel cut ... exited 1
    boom
`)
}

func TestRenderTextColoured(t *testing.T) {
	var out bytes.Buffer
	renderText(&out, sample(), true, colours)
	assert.ContainsString(t, out.String(), "\x1b[1;34mbash\x1b[0m 5.2-1 \x1b[2m[amd64, arm64]\x1b[0m\n")
	assert.ContainsString(t, out.String(), "total 6 \x1b[32mcovered 3\x1b[0m \x1b[33mignored 1\x1b[0m \x1b[31muncovered 2\x1b[0m\n/usr/lib/\n")
	assert.ContainsString(t, out.String(), "total 1 \x1b[32mcovered 1\x1b[0m \x1b[32mignored 0\x1b[0m \x1b[32muncovered 0\x1b[0m\n")
	assert.ContainsString(t, out.String(), "total 0 \x1b[33mcovered 0\x1b[0m ")
	assert.ContainsString(t, out.String(), ": \x1b[33mnot available\x1b[0m\n")
	assert.ContainsString(t, out.String(), ": \x1b[31merror\x1b[0m\n")
}

func TestStyleFor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	assert.Equal(t, styleFor(&bytes.Buffer{}), style{})
	f, err := os.CreateTemp(t.TempDir(), "out")
	assert.NoError(t, err)
	defer f.Close()
	assert.Equal(t, styleFor(f), style{})
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		t.Skip("no /dev/tty")
	}
	defer tty.Close()
	assert.Equal(t, styleFor(tty), colours)
	t.Setenv("NO_COLOR", "1")
	assert.Equal(t, styleFor(tty), style{})
}

func TestRenderJSON(t *testing.T) {
	var out bytes.Buffer
	assert.NoError(t, renderJSON(&out, sample()))
	var doc struct {
		Packages []struct {
			Name   string `json:"name"`
			Arches []struct {
				Arch      string              `json:"arch"`
				Available bool                `json:"available"`
				Version   *string             `json:"version"`
				DebArch   *string             `json:"deb-arch"`
				Error     *string             `json:"error"`
				Total     int                 `json:"total"`
				Uncovered []string            `json:"uncovered"`
				Slices    map[string][]string `json:"slices"`
			} `json:"arches"`
		} `json:"packages"`
	}
	assert.NoError(t, json.Unmarshal(out.Bytes(), &doc))
	assert.Len(t, doc.Packages, 3)
	bash := doc.Packages[0]
	assert.Len(t, bash.Arches, 4)
	// Raw paths, every architecture on its own.
	assert.EqualArrays(t, bash.Arches[2].Uncovered, []string{"/usr/lib/", "/usr/lib/aarch64-linux-gnu/"})
	assert.Equal(t, *bash.Arches[0].Version, "5.2-1")
	assert.Nil(t, bash.Arches[1].Version)
	assert.That(t, !bash.Arches[1].Available)
	assert.Nil(t, bash.Arches[1].Error)
	assert.ContainsString(t, *bash.Arches[3].Error, "boom")
	assert.EqualArrays(t, bash.Arches[0].Slices["scripts"], []string{})
	// Empty lists stay lists.
	assert.ContainsString(t, out.String(), `"uncovered": [],`)
	assert.Equal(t, *doc.Packages[1].Arches[0].DebArch, "all")
}

// fakeChisel fills --root from $FAKE_TREES/<slice>/ and fails on $FAKE_FAIL.
const fakeChisel = `#!/bin/sh
while [ $# -gt 1 ]; do
  case "$1" in --root) root="$2"; shift 2;; *) shift;; esac
done
if [ "$1" = "$FAKE_FAIL" ]; then echo boom >&2; exit 1; fi
if [ -d "$FAKE_TREES/$1" ]; then cp -R "$FAKE_TREES/$1/." "$root/"; fi
`

func fixture(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	rel := filepath.Join(tmp, "release")
	assert.NoError(t, os.MkdirAll(filepath.Join(rel, "slices"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(rel, "chisel.yaml"), []byte("format: v1\n"), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(rel, "slices", "demo.yaml"),
		[]byte("package: demo\nslices:\n  bins:\n    contents:\n      /usr/bin/demo:\n"), 0644))
	for _, p := range []string{"demo_cov-all/usr/bin/demo", "demo_cov-all/usr/share/doc/demo/README", "demo_bins/usr/bin/demo", "demo_cov-ignore-0/usr/share/doc/demo/README"} {
		assert.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(tmp, "trees", p)), 0755))
		assert.NoError(t, os.WriteFile(filepath.Join(tmp, "trees", p), []byte("x"), 0644))
	}
	bin := filepath.Join(tmp, "chisel")
	assert.NoError(t, os.WriteFile(bin, []byte(fakeChisel), 0755))
	t.Setenv("CHISEL", bin)
	t.Setenv("FAKE_TREES", filepath.Join(tmp, "trees"))
	t.Setenv("FAKE_FAIL", "")
	// Main views the real chisel cache otherwise.
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	return rel
}

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	assert.NoError(t, err)
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

func TestMainExitCodes(t *testing.T) {
	rel := fixture(t)

	code, out := captureStdout(t, func() int { return Main([]string{"--release", rel, "--arch", "amd64", "demo"}) })
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out, "demo [amd64]\ntotal 7 covered 3 ignored 0 uncovered 4\n")
	assert.ContainsString(t, out, "\n/usr/share/doc/demo/README\n")

	code, _ = captureStdout(t, func() int {
		return Main([]string{"--release", rel, "--arch", "amd64", "--fail-uncovered", "demo"})
	})
	assert.Equal(t, code, 1)

	code, _ = captureStdout(t, func() int {
		return Main([]string{"--release", rel, "--arch", "amd64", "--fail-uncovered", "--ignore", "/usr/share/**", "demo"})
	})
	assert.Equal(t, code, 0)

	t.Setenv("FAKE_FAIL", "demo_bins")
	code, out = captureStdout(t, func() int { return Main([]string{"--release", rel, "--arch", "amd64", "demo"}) })
	assert.Equal(t, code, 2)
	assert.ContainsString(t, out, "demo [amd64]: error\n")
	assert.ContainsString(t, out, "boom")
	t.Setenv("FAKE_FAIL", "")

	code, out = captureStdout(t, func() int { return Main([]string{"--release", rel, "--arch", "amd64", "--json", "demo"}) })
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out, `"arch": "amd64"`)

	for _, args := range [][]string{
		nil,
		{"--release", rel},
		{"--release", rel, "--arch", "x86", "demo"},
		{"--release", rel, "--bogus", "demo"},
		{"--release", t.TempDir(), "demo"},
		{"--release", rel, "--slice", "nope", "demo"},
	} {
		code, _ := captureStdout(t, func() int { return Main(args) })
		assert.Equal(t, code, 2, args)
	}

	code, out = captureStdout(t, func() int { return Main([]string{"version"}) })
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out, "0.1.0")
	for _, args := range [][]string{{"help"}, {"--help"}, {"--release", rel, "-h"}, {"demo", "--help"}} {
		code, out = captureStdout(t, func() int { return Main(args) })
		assert.Equal(t, code, 0, args)
		assert.ContainsString(t, out, "Usage:", args)
	}
	code, out = captureStdout(t, func() int { return Main([]string{"--release", rel, "--version"}) })
	assert.Equal(t, code, 0)
	assert.ContainsString(t, out, "0.1.0")
}

func TestFileVersion(t *testing.T) {
	assert.Equal(t, fileVersion("# c\n\n1.2.3\n"), "1.2.3")
	assert.Equal(t, fileVersion("# c\n"), "")
}
