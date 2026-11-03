// Package coverage turns the rootfs listings of a package's cuts into
// coverage: what the deb ships, what the slices take, what is left.
package coverage

import (
	"context"
	"fmt"
	"strconv"

	"github.com/lczyk/chisel-coverage/pkg/cuts"
	"github.com/lczyk/chisel-coverage/pkg/fold"
	"github.com/lczyk/chisel-coverage/pkg/sdf"
)

// Arch is one package's coverage on one architecture.
type Arch struct {
	Arch      string
	Available bool  // false: chisel found no such package for the architecture
	Err       error // a cut failed; nothing else is set
	Version   string
	DebArch   string
	Total     int
	Covered   []string
	Uncovered []string
	Ignored   []string
	Slices    map[string][]string // real slice -> paths in its rootfs
}

// Package is one package's coverage across the architectures asked for.
type Package struct {
	Name   string
	Slices []string // the slices considered, in file order
	Arches []Arch   // in opts.Arches order
}

// Run cuts every package on every architecture, the whole deb included,
// and returns the coverage in input order. See cuts.Run for the errors.
func Run(ctx context.Context, opts cuts.Options, pkgs []string) ([]Package, error) {
	opts.Whole = true
	res, err := cuts.Run(ctx, opts, pkgs)
	if err != nil {
		return nil, err
	}
	out := make([]Package, len(res))
	for i, p := range res {
		out[i] = Package{Name: p.Name, Slices: p.Slices, Arches: make([]Arch, len(p.Arches))}
		for j, a := range p.Arches {
			out[i].Arches[j] = Assemble(p.Slices, a)
		}
	}
	return out, nil
}

// Assemble is the coverage of one architecture from what its cuts found:
// covered is what any real slice has, ignored what is left and an ignore
// slice has, uncovered the rest of the whole deb. A path claimed by two
// slices shows under both.
func Assemble(slices []string, c cuts.Arch) Arch {
	a := Arch{Arch: c.Arch, Err: c.Err, Available: c.Available, Version: c.Version, DebArch: c.DebArch, Slices: map[string][]string{}}
	if c.Err != nil || !c.Available {
		return a
	}
	covered := map[string]bool{}
	for _, name := range slices {
		paths := c.Slices[name]
		if paths == nil {
			paths = []string{}
		}
		a.Slices[name] = paths
		for _, path := range paths {
			covered[path] = true
		}
	}
	ignore := map[string]bool{}
	for name, paths := range c.Slices {
		if !sdf.IsIgnore(name) {
			continue
		}
		for _, path := range paths {
			ignore[path] = true
		}
	}
	a.Covered, a.Uncovered, a.Ignored = []string{}, []string{}, []string{}
	for _, path := range c.Slices[sdf.AllSlice] {
		switch {
		case covered[path]:
			a.Covered = append(a.Covered, path)
		case ignore[path]:
			a.Ignored = append(a.Ignored, path)
		default:
			a.Uncovered = append(a.Uncovered, path)
		}
	}
	a.Total = len(c.Slices[sdf.AllSlice])
	return a
}

// Group is a block of the report: a set of architectures of one version
// and the paths their debs all ship, with the counts of that set alone.
// Several architectures share the normalised paths, which stand for all
// of them; a group of one holds its real paths.
type Group struct {
	Arches    []string
	Available bool
	Err       error
	Version   string
	DebArch   string
	Total     int
	Covered   []string
	Uncovered []string
	Ignored   []string
}

// Groups folds a package's architectures of the same version and
// availability on what their debs ship: every path, after fold.Normalise,
// is keyed by the architectures it is on with the same state (covered,
// ignored or uncovered: a slice may take it on some architectures only),
// and each key is a group, most architectures first. An architecture's
// groups sum to its deb. An arch: all package is one deb for every
// architecture, so its paths are compared as they are: a Go source tree
// names every architecture somewhere and would never fold otherwise.
// Failed cuts are never grouped.
func Groups(p Package) []Group {
	var members [][]Arch
	index := map[string]int{}
	for _, a := range p.Arches {
		key := fmt.Sprintf("%v\x00%s", a.Available, a.Version)
		if a.Err != nil {
			key = "err\x00" + a.Arch
		}
		if i, ok := index[key]; ok {
			members[i] = append(members[i], a)
			continue
		}
		index[key] = len(members)
		members = append(members, []Arch{a})
	}
	var groups []Group
	for _, m := range members {
		groups = append(groups, split(m)...)
	}
	return groups
}

// Ungrouped is one group per architecture, real paths, nothing folded.
func Ungrouped(p Package) []Group {
	groups := make([]Group, 0, len(p.Arches))
	for _, a := range p.Arches {
		groups = append(groups, whole(a))
	}
	return groups
}

func whole(a Arch) Group {
	return Group{
		Arches: []string{a.Arch}, Available: a.Available, Err: a.Err, Version: a.Version, DebArch: a.DebArch,
		Total: a.Total, Covered: a.Covered, Uncovered: a.Uncovered, Ignored: a.Ignored,
	}
}

const (
	covered = iota
	ignored
	uncovered
)

// split folds architectures of one version into groups by what they ship,
// the state of a path being part of what must agree.
func split(arches []Arch) []Group {
	if len(arches) == 1 {
		return []Group{whole(arches[0])}
	}
	first := arches[0]
	names := make([]string, len(arches))
	items := make([][]fold.Item, len(arches))
	for i, a := range arches {
		names[i] = a.Arch
		for state, paths := range [][]string{a.Covered, a.Ignored, a.Uncovered} {
			for _, raw := range paths {
				key := raw
				if a.DebArch != "all" {
					key = fold.Normalise(raw, a.Arch)
				}
				items[i] = append(items[i], fold.Item{Key: key, Tag: strconv.Itoa(state), Raw: raw})
			}
		}
	}
	var groups []Group
	for _, fg := range fold.Fold(names, items) {
		g := Group{
			Arches: fg.Arches, Available: first.Available, Version: first.Version, DebArch: first.DebArch,
			Total: len(fg.Entries), Covered: []string{}, Uncovered: []string{}, Ignored: []string{},
		}
		for _, e := range fg.Entries {
			switch e.Tag {
			case strconv.Itoa(covered):
				g.Covered = append(g.Covered, e.Shown())
			case strconv.Itoa(ignored):
				g.Ignored = append(g.Ignored, e.Shown())
			case strconv.Itoa(uncovered):
				g.Uncovered = append(g.Uncovered, e.Shown())
			}
		}
		groups = append(groups, g)
	}
	return groups
}
