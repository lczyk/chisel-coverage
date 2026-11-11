// Package cuts runs the cuts of a set of packages across architectures: one
// chisel cut per (package, architecture, slice) of a synthesised release,
// scheduled so that concurrent chisels never share a writable cache, and
// hands back what each cut's rootfs held. What is done with the rootfs
// listings (coverage arithmetic, running file over them) is the caller's.
package cuts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	temprelease "github.com/lczyk/chisel-proxy/pkg/temp-release"

	"github.com/lczyk/chisel-coverage/pkg/cut"
	"github.com/lczyk/chisel-coverage/pkg/packageslicesfile"
	"github.com/lczyk/chisel-coverage/pkg/pool"
)

// Options configures a run.
type Options struct {
	Release string
	Arches  []string // the first is cut alone before the rest
	Only    []string // --slice values, bare or <package>_<slice>; nil means all, empty means none
	Ignores []string
	Whole   bool // cut the whole-deb slice too and lead with it; else the probe leads
	// Prefix names the run's own slices (<prefix>-all, <prefix>-probe,
	// <prefix>-ignore-<n>) in the synthesised file and the logs;
	// packageslicesfile.DefaultPrefix when empty.
	Prefix string
	// Optional lets a package have no package slices file: it is cut as if
	// its file declared no slices, so only the whole-deb and ignore slices.
	Optional bool
	Keep     bool
	Jobs     int    // cuts running at once; 0 or 1 is one after another, -1 one per cpu
	Cache    string // chisel's cache dir, for per-worker views; "" shares it as is
	Runner   *cut.Runner
	Log      io.Writer // notices (--keep, cache trouble); nil discards
	Verbose  io.Writer // one line per cut; nil discards
	// Visit, when set, sees every cut that succeeded while its rootfs still
	// exists. Workers call it concurrently.
	Visit func(Cut)
}

// Cut is one finished cut, handed to Options.Visit.
type Cut struct {
	Package string
	Arch    string
	Slice   string // bare name: a real slice, the whole-deb slice or an ignore slice
	Root    string // the rootfs, gone once Visit returns unless Options.Keep
	Result  *cut.Result
}

// Arch is what one package's cuts found on one architecture.
type Arch struct {
	Arch      string
	Available bool  // false: chisel found no such package for the architecture
	Err       error // a cut failed; nothing else is set
	Version   string
	DebArch   string
	// StandIn names the lead architecture whose cuts stand for this one: an
	// arch: all package is one deb everywhere, so it is cut in full on the
	// lead architecture only and probed on the rest. Slices is the lead's
	// and Visit never ran for this architecture.
	StandIn string
	// Slices maps every slice that was to be cut (the real ones, the whole-deb
	// slice with Options.Whole, the ignore slices) to the paths of its rootfs. A
	// real slice with nothing to cut, or an ignore slice matching nothing,
	// maps to an empty list.
	Slices map[string][]string
}

// Package is one package across the architectures asked for.
type Package struct {
	Name   string
	Slices []string // the real slices considered, in file order
	Arches []Arch   // in Options.Arches order
}

type prepared struct {
	def    *packageslicesfile.File
	slices []string // real slices considered
	cuts   []string // slices to cut, the lead first
	rel    *temprelease.Release
	only   []string // the bare --slice names asked of this package, nil for all
}

type run struct {
	ctx      context.Context
	opts     Options
	names    packageslicesfile.Names
	pool     *pool.Pool
	prepared []*prepared
	results  []Package
	took     []time.Duration // per package, the wall time of its lead arch cuts so far
	done     []int           // per package, the cuts behind took
	cells    [][]cell        // per package and architecture, what its cuts found
	mu       sync.Mutex
}

// unit is one slice of one package on one architecture, ci indexing the
// package's cuts; 0 is the lead.
type unit struct{ pi, ai, ci int }

// cell collects what the cuts of one package on one architecture found.
type cell struct {
	avail   bool // the lead cut found the package
	err     error
	version string
	debArch string
	sets    map[string][]string // slice -> paths in its rootfs
}

// Run cuts every package on every architecture and returns the results in
// input order, a package given twice once. It fails up front on a bad
// release, a bad package slices file or an unknown --slice; a failed cut is
// reported per architecture instead. When ctx is done the cuts stop, the
// temp dirs and cache views go, and the error is ctx's.
func Run(ctx context.Context, opts Options, pkgs []string) ([]Package, error) {
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages given")
	}
	pkgs = unique(pkgs)
	if len(opts.Arches) == 0 {
		return nil, fmt.Errorf("no architectures given")
	}
	chiselYAML, err := os.ReadFile(filepath.Join(opts.Release, "chisel.yaml"))
	if err != nil {
		return nil, fmt.Errorf("%s is not a chisel release: %w", opts.Release, err)
	}
	only, err := resolveOnly(opts.Only, pkgs)
	if err != nil {
		return nil, err
	}

	r := &run{ctx: ctx, opts: opts, names: packageslicesfile.Names(opts.Prefix), took: make([]time.Duration, len(pkgs)), done: make([]int, len(pkgs))}
	if r.names == "" {
		r.names = packageslicesfile.DefaultPrefix
	}
	defer r.cleanup()
	for _, pkg := range pkgs {
		p, err := r.prepare(chiselYAML, pkg, only[pkg])
		if err != nil {
			return nil, err
		}
		r.prepared = append(r.prepared, p)
		r.results = append(r.results, Package{Name: pkg, Slices: p.slices, Arches: make([]Arch, len(opts.Arches))})
		r.cells = append(r.cells, make([]cell, len(opts.Arches)))
	}
	// A bare --slice name is kept only where it exists, so a package may
	// end up with nothing to cut; that is an error only when no package
	// being checked has any of the names.
	for _, p := range r.prepared {
		if len(p.only) > 0 && len(p.slices) == 0 && !r.anyExists(p.only) {
			return nil, fmt.Errorf("no package being checked has a slice named %s", strings.Join(p.only, ", "))
		}
	}
	r.pool, err = pool.New(ctx, pool.Options{Jobs: opts.Jobs, Cache: opts.Cache, Runner: opts.Runner, Keep: opts.Keep, Name: string(r.names), Log: opts.Log, Verbose: opts.Verbose})
	if err != nil {
		return nil, err
	}

	// The lead architecture alone first, so the rest find the arch: all debs
	// cached and the arch: all packages known; then those packages are only
	// probed on the other architectures, since an arch: all package has the
	// same files everywhere it exists; then everything else.
	var lead, probes, rest []unit
	for pi := range r.prepared {
		lead = append(lead, unit{pi, 0, 0})
	}
	r.pass(lead)
	for ai := 1; ai < len(opts.Arches); ai++ {
		for pi := range r.prepared {
			if a := r.results[pi].Arches[0]; a.Available && a.Err == nil && a.DebArch == "all" {
				probes = append(probes, unit{pi, ai, 0})
			} else {
				rest = append(rest, unit{pi, ai, 0})
			}
		}
	}
	r.probePhase(probes)
	r.pass(rest)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.pool.Summary()
	return r.results, nil
}

// ResolveArches is pool.ResolveArches.
func ResolveArches(given []string) ([]string, error) { return pool.ResolveArches(given) }

// unique drops repeats, keeping the first occurrence's place.
func unique(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// resolveOnly maps each package to the slices --slice keeps for it. A package
// absent from the map is unrestricted; a nil only leaves every package so,
// an empty one keeps no slice anywhere. A bare name applies to every
// package that has it and must exist in at least one; a <package>_<slice>
// name must name a listed package.
func resolveOnly(only, pkgs []string) (map[string][]string, error) {
	if only == nil {
		return nil, nil
	}
	listed := map[string]bool{}
	for _, p := range pkgs {
		listed[p] = true
	}
	out := map[string][]string{}
	for _, p := range pkgs {
		out[p] = []string{}
	}
	for _, s := range only {
		if pkg, name, ok := strings.Cut(s, "_"); ok {
			if !listed[pkg] {
				return nil, fmt.Errorf("--slice %s: package %s is not being checked", s, pkg)
			}
			out[pkg] = append(out[pkg], name)
			continue
		}
		for _, p := range pkgs {
			out[p] = append(out[p], s)
		}
	}
	return out, nil
}

func (r *run) prepare(chiselYAML []byte, pkg string, only []string) (*prepared, error) {
	def, err := packageslicesfile.Read(r.opts.Release, pkg)
	if errors.Is(err, fs.ErrNotExist) && r.opts.Optional {
		def, err = &packageslicesfile.File{Package: pkg}, nil
	}
	if err != nil {
		return nil, err
	}
	var keep []string
	if only != nil {
		// A bare --slice name is kept only where it exists; a prefixed one
		// must exist.
		for _, name := range only {
			if def.Slice(name) != nil {
				keep = append(keep, name)
			} else if r.prefixed(pkg + "_" + name) {
				return nil, fmt.Errorf("%s has no slice %q", pkg, name)
			}
		}
		if keep == nil {
			keep = []string{}
		}
	}
	file, cuts, err := packageslicesfile.Synthesise(def, keep, r.opts.Ignores, r.names)
	if err != nil {
		return nil, err
	}
	// Synthesise lists the whole-deb slice first. Without Whole the probe
	// leads instead: chisel fetches the deb for an empty slice too, so it
	// still tells availability, version and deb architecture, without
	// extracting anything.
	if !r.opts.Whole {
		cuts[0] = r.names.Probe()
	}
	rel, err := temprelease.New(chiselYAML)
	if err != nil {
		return nil, err
	}
	if err := rel.WriteSlice(pkg, file); err != nil {
		_ = rel.Remove()
		return nil, err
	}
	if r.opts.Keep {
		r.logf("release for %s: %s", pkg, rel.Dir)
	}
	p := &prepared{def: def, cuts: cuts, rel: rel, only: only}
	for _, s := range def.Slices {
		if keep == nil || slices.Contains(keep, s.Name) {
			p.slices = append(p.slices, s.Name)
		}
	}
	return p, nil
}

func (r *run) prefixed(name string) bool { return slices.Contains(r.opts.Only, name) }

// anyExists reports whether some package being checked has a slice of one
// of the names.
func (r *run) anyExists(names []string) bool {
	for _, name := range names {
		for _, p := range r.prepared {
			if p.def.Slice(name) != nil {
				return true
			}
		}
	}
	return false
}

// Jobs is pool.Jobs.
func Jobs(n int) int { return pool.Jobs(n) }

// pass cuts the lead slice of each (package, architecture) given and, as
// each finds its package, the package's other slices, then assembles the
// results.
func (r *run) pass(leads []unit) {
	groups := make([]pool.Group, len(leads))
	for i, u := range leads {
		cost := r.avg(u.pi)
		cuts := r.prepared[u.pi].cuts
		groups[i].Lead = r.unit(u, cuts[0], cost)
		for ci := 1; ci < len(cuts); ci++ {
			groups[i].Rest = append(groups[i].Rest, r.unit(unit{u.pi, u.ai, ci}, cuts[ci], cost))
		}
	}
	_ = r.pool.Pass(groups, r.cut)
	for _, u := range leads {
		r.results[u.pi].Arches[u.ai] = r.assemble(u.pi, u.ai)
	}
}

// unit is the cut of slice name for u as the pool takes it, its rootfs under
// <package>/<arch>/<slice>.
func (r *run) unit(u unit, name string, cost time.Duration) pool.Unit {
	p, arch := r.prepared[u.pi], r.opts.Arches[u.ai]
	return pool.Unit{
		Release: p.rel.Dir,
		Arch:    arch,
		Slice:   p.def.Package + "_" + name,
		Dir:     filepath.Join(p.def.Package, arch, name),
		Cost:    cost,
		Tag:     u,
	}
}

// avg is what a cut of the package has taken on the lead architecture so
// far, a second before anything is measured.
func (r *run) avg(pi int) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done[pi] == 0 {
		return time.Second
	}
	return r.took[pi] / time.Duration(r.done[pi])
}

// probePhase runs the probes, one worker per architecture and no views: a
// worker's probes are all on one architecture, one after another. The full
// cuts that follow then find every index cached.
func (r *run) probePhase(units []unit) {
	var byArch [][]pool.Unit
	index := map[int]int{}
	for _, u := range units {
		i, ok := index[u.ai]
		if !ok {
			i = len(byArch)
			index[u.ai] = i
			byArch = append(byArch, nil)
		}
		byArch[i] = append(byArch[i], r.unit(u, r.names.Probe(), 0))
	}
	_ = r.pool.Serial(byArch, func(c pool.Cut) {
		u := c.Tag.(unit)
		r.results[u.pi].Arches[u.ai] = r.probed(c)
	})
}

// cut takes one finished cut into its cell and says whether the cell's other
// slices are still worth cutting: not where there is no package, and not
// once a cut of the cell has failed, since the architecture reports the
// error anyway.
func (r *run) cut(c pool.Cut) bool {
	u := c.Tag.(unit)
	p := r.prepared[u.pi]
	name := p.cuts[u.ci]
	if c.Err == nil && r.opts.Visit != nil && name != r.names.Probe() {
		r.opts.Visit(Cut{Package: p.def.Package, Arch: c.Arch, Slice: name, Root: c.Root, Result: c.Result})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if u.ai == 0 {
		r.took[u.pi] += time.Since(c.Start)
		r.done[u.pi]++
	}
	cell := &r.cells[u.pi][u.ai]
	switch {
	case errors.Is(c.Err, cut.ErrNotAvailable) && u.ci == 0:
		// Not on this architecture; the cell stays unavailable.
		return false
	case errors.Is(c.Err, cut.ErrNoContent) && r.names.IsIgnore(name):
		// An ignore glob matching nothing in this package ignores nothing.
	case c.Err != nil:
		if cell.err == nil {
			cell.err = c.Err
		}
		return false
	default:
		if cell.sets == nil {
			cell.sets = map[string][]string{}
		}
		if name != r.names.Probe() {
			cell.sets[name] = c.Result.Paths
		}
		if u.ci == 0 {
			cell.avail = true
			cell.version, cell.debArch = c.Result.Version, c.Result.DebArch
		}
	}
	return true
}

// assemble turns a cell into the architecture's result: every slice that
// was to be cut has its paths, empty when it was not cut or matched nothing.
func (r *run) assemble(pi, ai int) Arch {
	p, c := r.prepared[pi], r.cells[pi][ai]
	a := Arch{Arch: r.opts.Arches[ai]}
	if c.err != nil {
		a.Err = c.err
		return a
	}
	if !c.avail {
		return a
	}
	a.Available = true
	a.Version, a.DebArch = c.version, c.debArch
	a.Slices = map[string][]string{}
	for _, name := range p.slices {
		a.Slices[name] = []string{}
	}
	for _, name := range p.cuts {
		if name != r.names.Probe() {
			a.Slices[name] = []string{}
		}
	}
	for name, paths := range c.sets {
		a.Slices[name] = paths
	}
	return a
}

// probed is what the cut of the empty probe slice of an arch: all package
// on another architecture found: when the package is there, the lead
// architecture's result for it.
func (r *run) probed(c pool.Cut) Arch {
	u := c.Tag.(unit)
	if errors.Is(c.Err, cut.ErrNotAvailable) {
		return Arch{Arch: c.Arch}
	}
	if c.Err != nil {
		return Arch{Arch: c.Arch, Err: c.Err}
	}
	a := r.results[u.pi].Arches[0]
	a.StandIn = a.Arch
	a.Arch = c.Arch
	if c.Result.Version != "" {
		a.Version, a.DebArch = c.Result.Version, c.Result.DebArch
	}
	return a
}

func (r *run) cleanup() {
	if r.pool != nil {
		r.pool.Close()
	}
	if r.opts.Keep {
		return
	}
	for _, p := range r.prepared {
		_ = p.rel.Remove()
	}
}

// logf writes one timestamped notice on the Log writer. It is only called
// before the pool and its workers exist, so it takes no lock.
func (r *run) logf(format string, args ...any) {
	if r.opts.Log != nil {
		fmt.Fprintf(r.opts.Log, "%s "+format+"\n", append([]any{cut.Stamp()}, args...)...)
	}
}
