package coverage

import (
	"errors"
	"testing"

	"github.com/lczyk/assert"

	"github.com/lczyk/chisel-coverage/pkg/cuts"
)

func TestAssemble(t *testing.T) {
	slices := []string{"config", "bins", "copyright", "scripts"}
	a := Assemble(slices, cuts.Arch{
		Arch: "amd64", Available: true, Version: "5.2-1", DebArch: "amd64",
		Slices: map[string][]string{
			"cov-all":      {"/etc/", "/etc/foo.d/", "/etc/skel/", "/etc/skel/.bashrc", "/usr/", "/usr/bin/", "/usr/bin/bash", "/usr/lib/", "/usr/lib/bash/", "/usr/share/", "/usr/share/doc/", "/usr/share/doc/bash/", "/usr/share/doc/bash/copyright"},
			"config":       {"/etc/", "/etc/skel/", "/etc/skel/.bashrc"},
			"bins":         {"/usr/", "/usr/bin/", "/usr/bin/bash"},
			"copyright":    {},
			"cov-ignore-0": {"/usr/", "/usr/share/", "/usr/share/doc/", "/usr/share/doc/bash/", "/usr/share/doc/bash/copyright"},
			"cov-ignore-1": {},
		},
	})
	assert.That(t, a.Available)
	assert.Equal(t, a.Version, "5.2-1")
	assert.Equal(t, a.Total, 13)
	assert.EqualArrays(t, a.Covered, []string{"/etc/", "/etc/skel/", "/etc/skel/.bashrc", "/usr/", "/usr/bin/", "/usr/bin/bash"})
	// Ignored only ever shrinks uncovered: /usr/ is covered despite being an
	// implicit parent in the ignore cut too.
	assert.EqualArrays(t, a.Ignored, []string{"/usr/share/", "/usr/share/doc/", "/usr/share/doc/bash/", "/usr/share/doc/bash/copyright"})
	assert.EqualArrays(t, a.Uncovered, []string{"/etc/foo.d/", "/usr/lib/", "/usr/lib/bash/"})
	assert.Len(t, a.Slices, 4)
	assert.EqualArrays(t, a.Slices["bins"], []string{"/usr/", "/usr/bin/", "/usr/bin/bash"})
	// A slice that was not cut is empty, not missing.
	assert.EqualArrays(t, a.Slices["scripts"], []string{})

	na := Assemble(slices, cuts.Arch{Arch: "i386"})
	assert.That(t, !na.Available)
	assert.Equal(t, na.Total, 0)
	assert.Nil(t, na.Covered)

	failed := Assemble(slices, cuts.Arch{Arch: "s390x", Err: errors.New("boom")})
	assert.Error(t, failed.Err, "boom")
	assert.That(t, !failed.Available)
}

func TestGroupsSingletonKeepsRealPaths(t *testing.T) {
	arch := func(name, version, path string) Arch {
		return Arch{Arch: name, Available: true, Version: version, Uncovered: []string{path}, Ignored: []string{}, Covered: []string{}, Slices: map[string][]string{}}
	}
	g := Groups(Package{Arches: []Arch{
		arch("amd64", "1", "/usr/lib/x86_64-linux-gnu/"),
		arch("arm64", "1", "/usr/lib/aarch64-linux-gnu/"),
		arch("s390x", "2", "/usr/lib/s390x-linux-gnu/ld64.so.1"),
	}})
	assert.Len(t, g, 2)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, g[0].Uncovered, []string{"/usr/lib/<arch>-linux-<gnu>/"})
	assert.EqualArrays(t, g[1].Arches, []string{"s390x"})
	assert.EqualArrays(t, g[1].Uncovered, []string{"/usr/lib/s390x-linux-gnu/ld64.so.1"})
}

func TestGroupsFoldArchAllOnRealPaths(t *testing.T) {
	// The same files everywhere; normalising would turn "amd64" into <arch>
	// for amd64 only and keep the two apart.
	arch := func(name string) Arch {
		return Arch{Arch: name, Available: true, Version: "1", DebArch: "all", Uncovered: []string{"/usr/share/go/src/runtime/sys_amd64.s"}, Ignored: []string{}, Covered: []string{}, Slices: map[string][]string{}}
	}
	g := Groups(Package{Arches: []Arch{arch("amd64"), arch("arm64")}})
	assert.Len(t, g, 1)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, g[0].Uncovered, []string{"/usr/share/go/src/runtime/sys_amd64.s"})
}

func TestGroupsFoldOnWhatIsShipped(t *testing.T) {
	base := func(arch string) Arch {
		return Arch{Arch: arch, Available: true, Version: "1", Total: 3, Uncovered: []string{"/a"}, Ignored: []string{}, Covered: []string{"/b", "/d"}, Slices: map[string][]string{"bins": {"/b", "/d"}}}
	}
	same := base("arm64")
	version := base("i386")
	version.Version = "2"
	fewer := base("s390x")
	fewer.Total = 2
	fewer.Covered = []string{"/b"}
	more := base("ppc64el")
	more.Total = 5
	more.Uncovered = []string{"/a", "/usr/lib/powerpc64le-linux-gnu/e"}
	more.Ignored = []string{"/i"}
	g := Groups(Package{Arches: []Arch{base("amd64"), same, version, fewer, more}})
	assert.Len(t, g, 4)
	// What all four ship, what three ship, what one ships (real paths),
	// then the other version.
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64", "s390x", "ppc64el"})
	assert.Equal(t, g[0].Total, 2)
	assert.EqualArrays(t, g[0].Covered, []string{"/b"})
	assert.EqualArrays(t, g[0].Uncovered, []string{"/a"})
	assert.EqualArrays(t, g[0].Ignored, []string{})
	assert.EqualArrays(t, g[1].Arches, []string{"amd64", "arm64", "ppc64el"})
	assert.Equal(t, g[1].Total, 1)
	assert.EqualArrays(t, g[1].Covered, []string{"/d"})
	assert.EqualArrays(t, g[1].Uncovered, []string{})
	assert.EqualArrays(t, g[2].Arches, []string{"ppc64el"})
	assert.Equal(t, g[2].Total, 2)
	assert.EqualArrays(t, g[2].Uncovered, []string{"/usr/lib/powerpc64le-linux-gnu/e"})
	assert.EqualArrays(t, g[2].Ignored, []string{"/i"})
	assert.EqualArrays(t, g[3].Arches, []string{"i386"})
	assert.Equal(t, g[3].Total, 3)
	assert.Equal(t, g[3].Version, "2")
	assert.EqualArrays(t, g[3].Covered, []string{"/b", "/d"})
}

func TestGroupsMostArchesFirst(t *testing.T) {
	arch := func(name string, uncovered ...string) Arch {
		return Arch{Arch: name, Available: true, Version: "1", Total: len(uncovered), Uncovered: uncovered, Ignored: []string{}, Covered: []string{}, Slices: map[string][]string{}}
	}
	g := Groups(Package{Arches: []Arch{
		arch("amd64", "/a", "/x"),
		arch("arm64", "/a", "/y", "/x"),
		arch("armhf", "/a", "/y", "/z"),
		arch("i386", "/y", "/z"),
	}})
	assert.Len(t, g, 4)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64", "armhf"})
	assert.EqualArrays(t, g[0].Uncovered, []string{"/a"})
	assert.EqualArrays(t, g[1].Arches, []string{"arm64", "armhf", "i386"})
	assert.EqualArrays(t, g[1].Uncovered, []string{"/y"})
	assert.EqualArrays(t, g[2].Arches, []string{"amd64", "arm64"})
	assert.EqualArrays(t, g[2].Uncovered, []string{"/x"})
	assert.EqualArrays(t, g[3].Arches, []string{"armhf", "i386"})
	assert.EqualArrays(t, g[3].Uncovered, []string{"/z"})
}

func TestGroupsSplitOnState(t *testing.T) {
	// A slice taking a path on one architecture only: covered there,
	// uncovered elsewhere, so the path is in a group of each.
	amd64 := Arch{Arch: "amd64", Available: true, Version: "1", Total: 3, Covered: []string{"/c", "/i"}, Uncovered: []string{"/u"}, Ignored: []string{}}
	arm64 := Arch{Arch: "arm64", Available: true, Version: "1", Total: 3, Covered: []string{}, Uncovered: []string{"/c", "/u"}, Ignored: []string{"/i"}}
	g := Groups(Package{Arches: []Arch{amd64, arm64}})
	assert.Len(t, g, 3)
	assert.EqualArrays(t, g[0].Arches, []string{"amd64", "arm64"})
	assert.Equal(t, g[0].Total, 1)
	assert.EqualArrays(t, g[0].Uncovered, []string{"/u"})
	assert.EqualArrays(t, g[1].Arches, []string{"amd64"})
	assert.Equal(t, g[1].Total, 2)
	assert.EqualArrays(t, g[1].Covered, []string{"/c", "/i"})
	assert.EqualArrays(t, g[2].Arches, []string{"arm64"})
	assert.Equal(t, g[2].Total, 2)
	assert.EqualArrays(t, g[2].Uncovered, []string{"/c"})
	assert.EqualArrays(t, g[2].Ignored, []string{"/i"})
}

func TestGroupsFailedCutsStandAlone(t *testing.T) {
	ok := Arch{Arch: "amd64", Available: true, Version: "1", Covered: []string{}, Uncovered: []string{}, Ignored: []string{}}
	bad := Arch{Arch: "arm64", Err: errors.New("boom")}
	g := Groups(Package{Arches: []Arch{ok, bad}})
	assert.Len(t, g, 2)
	assert.EqualArrays(t, g[1].Arches, []string{"arm64"})
	assert.Error(t, g[1].Err, "boom")
}
