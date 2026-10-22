package coverage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/lczyk/assert"

	"github.com/lczyk/chisel-slice-coverage/internal/cut"
)

// fakeChisel fills --root from $FAKE_TREES/<arch>/<slice>/. A package with no
// <arch>/<pkg>_cov-all tree is not available on that arch; a slice listed in
// $FAKE_FAIL fails; a slice listed in $FAKE_NOCONTENT fails the way chisel
// fails a glob matching nothing; a <arch>/<pkg>.version file is logged the way
// chisel logs a fetch. Every cut appends "<arch> <slice>" to $FAKE_LOG and, with
// $FAKE_LOCKS set, holds a per-arch lock for a moment so overlapping cuts on
// one arch are caught.
const fakeChisel = `#!/bin/sh
while [ $# -gt 1 ]; do
  case "$1" in
    --root) root="$2"; shift 2;;
    --arch) arch="$2"; shift 2;;
    *) shift;;
  esac
done
slice="$1"
pkg="${slice%%_*}"
[ -n "$FAKE_LOG" ] && printf '%s %s\n' "$arch" "$slice" >> "$FAKE_LOG"
if [ -n "$FAKE_LOCKS" ]; then
  mkdir "$FAKE_LOCKS/$arch" 2>/dev/null || { echo "overlapping cut on $arch" >&2; exit 1; }
  sleep 0.05
  rmdir "$FAKE_LOCKS/$arch"
fi
case " $FAKE_FAIL " in *" $slice "*) echo "boom" >&2; exit 1;; esac
case " $FAKE_NOCONTENT " in *" $slice "*) echo "error: cannot extract from package \"$pkg\": no content at /nope/**" >&2; exit 1;; esac
if [ ! -d "$FAKE_TREES/$arch/${pkg}_cov-all" ]; then
  echo "error: cannot find package \"$pkg\" in archive(s)" >&2
  exit 1
fi
if [ -f "$FAKE_TREES/$arch/$pkg.version" ]; then
  echo "Fetching pool/main/x/$pkg/${pkg}_$(cat "$FAKE_TREES/$arch/$pkg.version")_$arch.deb..." >&2
fi
tree="$FAKE_TREES/$arch/$slice"
if [ -d "$tree" ]; then cp -R "$tree/." "$root/"; fi
exit 0
`

const bashYAML = `package: bash

essential:
  - bash_copyright

slices:
  config:
    contents:
      /etc/skel/.bashrc:
  bins:
    essential:
      - bash_config
    contents:
      /usr/bin/bash:
      /usr/bin/rbash:
  copyright:
    contents:
      /usr/share/doc/bash/copyright:
  scripts:
    mutate: |
      pass
`

type fixture struct {
	tmp     string // $TMPDIR for the run, so leftovers are visible
	release string
	trees   string
	log     string
	runner  *cut.Runner
}

func touch(t *testing.T, p string) {
	t.Helper()
	assert.NoError(t, os.MkdirAll(filepath.Dir(p), 0755))
	assert.NoError(t, os.WriteFile(p, []byte("x"), 0644))
}

func setup(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	f := &fixture{
		tmp:     filepath.Join(tmp, "tmpdir"),
		release: filepath.Join(tmp, "release"),
		trees:   filepath.Join(tmp, "trees"),
		log:     filepath.Join(tmp, "log"),
	}
	assert.NoError(t, os.MkdirAll(filepath.Join(f.release, "slices"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(f.release, "chisel.yaml"), []byte("format: v1\n"), 0644))
	assert.NoError(t, os.WriteFile(filepath.Join(f.release, "slices", "bash.yaml"), []byte(bashYAML), 0644))

	for arch, triplet := range map[string]string{"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"} {
		all := filepath.Join(f.trees, arch, "bash_cov-all")
		touch(t, filepath.Join(all, "usr/bin/bash"))
		touch(t, filepath.Join(all, "usr/bin/rbash"))
		touch(t, filepath.Join(all, "usr/lib", triplet, "bash/loadables.so"))
		touch(t, filepath.Join(all, "usr/share/man/man1/bash.1.gz"))
		touch(t, filepath.Join(all, "usr/share/doc/bash/copyright"))
		touch(t, filepath.Join(all, "etc/skel/.bashrc"))
		assert.NoError(t, os.MkdirAll(filepath.Join(all, "etc/foo.d"), 0755))
		touch(t, filepath.Join(f.trees, arch, "bash_bins/usr/bin/bash"))
		touch(t, filepath.Join(f.trees, arch, "bash_bins/usr/bin/rbash"))
		touch(t, filepath.Join(f.trees, arch, "bash_config/etc/skel/.bashrc"))
		touch(t, filepath.Join(f.trees, arch, "bash_cov-ignore-0/usr/share/man/man1/bash.1.gz"))
		touch(t, filepath.Join(f.trees, arch, "bash_cov-ignore-1/usr/share/doc/bash/copyright"))
		assert.NoError(t, os.WriteFile(filepath.Join(f.trees, arch, "bash.version"), []byte("5.2-1"), 0644))
	}

	bin := filepath.Join(tmp, "chisel")
	assert.NoError(t, os.WriteFile(bin, []byte(fakeChisel), 0755))
	f.runner = &cut.Runner{Bin: bin}
	assert.NoError(t, os.MkdirAll(f.tmp, 0755))
	t.Setenv("TMPDIR", f.tmp)
	t.Setenv("FAKE_TREES", f.trees)
	t.Setenv("FAKE_LOG", f.log)
	t.Setenv("FAKE_LOCKS", "")
	t.Setenv("FAKE_FAIL", "")
	t.Setenv("FAKE_NOCONTENT", "")
	return f
}

func (f *fixture) cuts(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	assert.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestRunCoverage(t *testing.T) {
	f := setup(t)
	// The third ignore matches nothing, which chisel reports as a failed cut.
	t.Setenv("FAKE_NOCONTENT", "bash_cov-ignore-2")
	res, err := Run(Options{
		Release: f.release,
		Arches:  []string{"amd64", "i386", "arm64"},
		Ignores: []string{"/usr/share/man/**", "/usr/share/doc/**", "/nope/**"},
		Runner:  f.runner,
	}, []string{"bash"})
	assert.NoError(t, err)
	assert.Len(t, res, 1)
	p := res[0]
	assert.Equal(t, p.Name, "bash")
	assert.EqualArrays(t, p.Slices, []string{"config", "bins", "copyright", "scripts"})
	assert.Len(t, p.Arches, 3)

	amd64 := p.Arches[0]
	assert.Equal(t, amd64.Arch, "amd64")
	assert.That(t, amd64.Available)
	assert.Nil(t, amd64.Err)
	assert.Equal(t, amd64.Version, "5.2-1")
	assert.Equal(t, amd64.DebArch, "amd64")
	assert.Equal(t, amd64.Total, 19)
	assert.EqualArrays(t, amd64.Covered, []string{"/etc/", "/etc/skel/", "/etc/skel/.bashrc", "/usr/", "/usr/bin/", "/usr/bin/bash", "/usr/bin/rbash"})
	// Ignored only ever shrinks uncovered: /usr/ is covered despite being an
	// implicit parent in the ignore cut too.
	assert.EqualArrays(t, amd64.Ignored, []string{"/usr/share/", "/usr/share/doc/", "/usr/share/doc/bash/", "/usr/share/doc/bash/copyright", "/usr/share/man/", "/usr/share/man/man1/", "/usr/share/man/man1/bash.1.gz"})
	assert.EqualArrays(t, amd64.Uncovered, []string{"/etc/foo.d/", "/usr/lib/", "/usr/lib/x86_64-linux-gnu/", "/usr/lib/x86_64-linux-gnu/bash/", "/usr/lib/x86_64-linux-gnu/bash/loadables.so"})
	assert.EqualArrays(t, amd64.Slices["bins"], []string{"/usr/", "/usr/bin/", "/usr/bin/bash", "/usr/bin/rbash"})
	assert.EqualArrays(t, amd64.Slices["config"], []string{"/etc/", "/etc/skel/", "/etc/skel/.bashrc"})
	// Cut, but the fake has no tree for it: empty, so its path is only ignored.
	assert.EqualArrays(t, amd64.Slices["copyright"], []string{})
	// Nothing left after stripping: not cut at all.
	assert.EqualArrays(t, amd64.Slices["scripts"], []string{})

	i386 := p.Arches[1]
	assert.Equal(t, i386.Arch, "i386")
	assert.That(t, !i386.Available)
	assert.Nil(t, i386.Err)
	assert.Equal(t, i386.Total, 0)

	arm64 := p.Arches[2]
	assert.That(t, arm64.Available)
	assert.ContainsString(t, strings.Join(arm64.Uncovered, "\n"), "/usr/lib/aarch64-linux-gnu/bash/loadables.so")

	cuts := f.cuts(t)
	assert.That(t, !strings.Contains(strings.Join(cuts, "\n"), "bash_scripts"))
	assert.Equal(t, cuts[0], "amd64 bash_cov-all")
	assert.Equal(t, cuts[4], "amd64 bash_cov-ignore-0")
	assert.Equal(t, cuts[6], "amd64 bash_cov-ignore-2")
	// i386 stops after cov-all.
	n := 0
	for _, c := range cuts {
		if strings.HasPrefix(c, "i386 ") {
			n++
		}
	}
	assert.Equal(t, n, 1)

	groups := Groups(p)
	assert.Len(t, groups, 2)
	assert.EqualArrays(t, groups[0].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, groups[0].Uncovered, []string{"/etc/foo.d/", "/usr/lib/", "/usr/lib/<arch>-linux-<gnu>/", "/usr/lib/<arch>-linux-<gnu>/bash/", "/usr/lib/<arch>-linux-<gnu>/bash/loadables.so"})
	assert.EqualArrays(t, groups[1].Arches, []string{"i386"})
	assert.That(t, !groups[1].Available)

	// Everything temporary is gone.
	left, err := os.ReadDir(f.tmp)
	assert.NoError(t, err)
	assert.Len(t, left, 0)
}

func TestRunOnlySlices(t *testing.T) {
	f := setup(t)
	res, err := Run(Options{Release: f.release, Arches: []string{"amd64"}, Only: []string{"bins"}, Runner: f.runner}, []string{"bash"})
	assert.NoError(t, err)
	assert.EqualArrays(t, res[0].Slices, []string{"bins"})
	assert.EqualArrays(t, f.cuts(t), []string{"amd64 bash_cov-all", "amd64 bash_bins"})
	assert.Len(t, res[0].Arches[0].Uncovered, 19-4)

	_, err = Run(Options{Release: f.release, Arches: []string{"amd64"}, Only: []string{"bash_nope"}, Runner: f.runner}, []string{"bash"})
	assert.Error(t, err, `bash has no slice "nope"`)
	_, err = Run(Options{Release: f.release, Arches: []string{"amd64"}, Only: []string{"nope"}, Runner: f.runner}, []string{"bash"})
	assert.Error(t, err, "no package being checked has a slice named nope")
	_, err = Run(Options{Release: f.release, Arches: []string{"amd64"}, Only: []string{"zsh_bins"}, Runner: f.runner}, []string{"bash"})
	assert.Error(t, err, "package zsh is not being checked")
}

func TestRunCutFailureIsPerArch(t *testing.T) {
	f := setup(t)
	// A real slice's path matching nothing is a failure, not an empty set.
	t.Setenv("FAKE_NOCONTENT", "bash_config")
	res, err := Run(Options{Release: f.release, Arches: []string{"amd64"}, Runner: f.runner}, []string{"bash"})
	assert.NoError(t, err)
	assert.Error(t, res[0].Arches[0].Err, "no content at /nope/**")

	t.Setenv("FAKE_NOCONTENT", "")
	t.Setenv("FAKE_FAIL", "bash_config")
	res, err = Run(Options{Release: f.release, Arches: []string{"amd64", "arm64"}, Runner: f.runner}, []string{"bash"})
	assert.NoError(t, err)
	for _, a := range res[0].Arches {
		assert.Error(t, a.Err, "bash_config: exit status 1\n  boom")
		assert.That(t, !a.Available)
	}
	assert.Len(t, Groups(res[0]), 2)
}

func TestRunErrorsUpFront(t *testing.T) {
	f := setup(t)
	_, err := Run(Options{Release: f.release, Arches: []string{"amd64"}, Runner: f.runner}, nil)
	assert.Error(t, err, "no packages")
	_, err = Run(Options{Release: f.release, Runner: f.runner}, []string{"bash"})
	assert.Error(t, err, "no architectures")
	_, err = Run(Options{Release: t.TempDir(), Arches: []string{"amd64"}, Runner: f.runner}, []string{"bash"})
	assert.Error(t, err, "not a chisel release")
	_, err = Run(Options{Release: f.release, Arches: []string{"amd64"}, Runner: f.runner}, []string{"zsh"})
	assert.Error(t, err, "zsh.yaml")
}

func TestRunLanesNeverOverlapAndLeadGoesFirst(t *testing.T) {
	f := setup(t)
	locks := t.TempDir()
	t.Setenv("FAKE_LOCKS", locks)
	for _, arch := range []string{"ppc64el", "s390x", "riscv64"} {
		assert.NoError(t, os.MkdirAll(filepath.Join(f.trees, arch, "bash_cov-all/usr/bin"), 0755))
	}
	arches := []string{"amd64", "arm64", "ppc64el", "s390x", "riscv64"}
	res, err := Run(Options{Release: f.release, Arches: arches, Runner: f.runner}, []string{"bash", "bash"})
	assert.NoError(t, err)
	for _, p := range res {
		for _, a := range p.Arches {
			assert.Nil(t, a.Err)
		}
	}
	cuts := f.cuts(t)
	// Two packages, three cuts each (cov-all, config, bins, copyright = 4; scripts not cut).
	assert.Len(t, cuts, 2*4*len(arches))
	lead := 0
	for i, c := range cuts {
		if strings.HasPrefix(c, "amd64 ") {
			assert.Equal(t, i, lead)
			lead++
		}
	}
	assert.Equal(t, lead, 2*4)
}

// stamped matches the timestamp every line of the tool's own log starts with.
var stamped = regexp.MustCompile(`^\d\d-\d\d-\d\dT\d\d:\d\d:\d\d `)

func TestRunVerbose(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_NOCONTENT", "bash_cov-ignore-0")
	t.Setenv("FAKE_FAIL", "bash_copyright")
	var out strings.Builder
	_, err := Run(Options{Release: f.release, Arches: []string{"amd64", "riscv64"}, Ignores: []string{"/nope/**"}, Runner: f.runner, Verbose: &out}, []string{"bash"})
	assert.NoError(t, err)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		assert.That(t, stamped.MatchString(l), l)
		lines = append(lines, took.ReplaceAllString(stamped.ReplaceAllString(l, ""), ""))
	}
	assert.EqualArrays(t, lines, []string{
		"amd64 bash_cov-all 19p",
		"amd64 bash_config 3p",
		"amd64 bash_bins 4p",
		"amd64 bash_copyright error",
		"riscv64 bash_cov-all n/a",
	})
}

var took = regexp.MustCompile(` \d+\.\ds$`)

func TestRunKeep(t *testing.T) {
	f := setup(t)
	var log strings.Builder
	_, err := Run(Options{Release: f.release, Arches: []string{"amd64"}, Keep: true, Runner: f.runner, Log: &log}, []string{"bash"})
	assert.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	assert.Len(t, lines, 2)
	for i, l := range lines {
		assert.That(t, stamped.MatchString(l), l)
		lines[i] = stamped.ReplaceAllString(l, "")
	}
	rel := strings.TrimPrefix(lines[0], "- - release for bash: ")
	work := strings.TrimPrefix(lines[1], "- - rootfs dirs: ")
	defer os.RemoveAll(rel)
	defer os.RemoveAll(work)
	_, err = os.Stat(filepath.Join(rel, "slices", "bash.yaml"))
	assert.NoError(t, err)
	_, err = os.Stat(filepath.Join(work, "bash", "amd64", "bins", "usr", "bin", "bash"))
	assert.NoError(t, err)
}

func TestNormalise(t *testing.T) {
	for _, tc := range []struct{ path, arch, want string }{
		{"/usr/lib/x86_64-linux-gnu/libc.so.6", "amd64", "/usr/lib/<arch>-linux-<gnu>/libc.so.6"},
		{"/usr/lib/aarch64-linux-gnu/", "arm64", "/usr/lib/<arch>-linux-<gnu>/"},
		{"/usr/lib/arm-linux-gnueabihf/bash/", "armhf", "/usr/lib/<arch>-linux-<gnu>/bash/"},
		{"/usr/bin/x86_64-linux-gnu-gcc", "amd64", "/usr/bin/<arch>-linux-gnu-gcc"},
		{"/etc/apparmor.d/arm", "armhf", "/etc/apparmor.d/arm"},
		{"/usr/lib/i386-linux-gnu/x", "amd64", "/usr/lib/i386-linux-gnu/x"},
		{"/usr/lib/x86_64-linux-gnu/x", "nope", "/usr/lib/x86_64-linux-gnu/x"},
	} {
		assert.Equal(t, Normalise(tc.path, tc.arch), tc.want)
	}
}

func TestGroupsSplitOnAnyDifference(t *testing.T) {
	base := func(arch string) Arch {
		return Arch{Arch: arch, Available: true, Version: "1", Uncovered: []string{"/a"}, Ignored: []string{}, Covered: []string{}, Slices: map[string][]string{"bins": {"/b"}}}
	}
	same := base("arm64")
	version := base("i386")
	version.Version = "2"
	slice := base("s390x")
	slice.Slices["bins"] = []string{"/c"}
	g := Groups(Package{Arches: []Arch{base("amd64"), same, version, slice}})
	assert.Len(t, g, 3)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, g[1].Arches, []string{"i386"})
	assert.EqualArrays(t, g[2].Arches, []string{"s390x"})
}
