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
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/chisel-proxy/pkg/deb"
	temprelease "github.com/lczyk/chisel-proxy/pkg/temp-release"

	"github.com/lczyk/chisel-coverage/pkg/cache"
	"github.com/lczyk/chisel-coverage/pkg/cut"
	"github.com/lczyk/chisel-coverage/pkg/packageslicesfile"
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
	work     string
	prepared []*prepared
	results  []Package
	jobs     int             // cuts allowed to run at once
	took     []time.Duration // per package, the wall time of its lead arch cuts so far
	seed     time.Duration   // the last view's seeding time; 0 until one was
	done     []int           // per package, the cuts behind took
	cells    [][]cell        // per package and architecture, what its cuts found
	viewCost time.Duration   // of making and removing one view; 0 without a cache
	start    time.Time
	rm       sync.WaitGroup // rootfs dirs being removed in the background
	mu       sync.Mutex
	logMu    sync.Mutex    // the Log and Verbose writers
	seeding  sync.Mutex    // views are seeded one at a time, see worker
	views    []*cache.View // idle views, for the next worker
	cuts     int           // the totals of the run, for the summary line
	paths    int
	bytes    int64
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

// worker cuts one slice at a time on one architecture with its own view of
// chisel's cache, merged back after every cut so the others gain the blobs.
type worker struct {
	r       *run
	runner  cut.Runner
	isolate bool // wants a view of the cache, made on its first unit
	view    *cache.View
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

	r := &run{ctx: ctx, opts: opts, names: packageslicesfile.Names(opts.Prefix), jobs: Jobs(opts.Jobs), took: make([]time.Duration, len(pkgs)), done: make([]int, len(pkgs)), start: time.Now()}
	if r.names == "" {
		r.names = packageslicesfile.DefaultPrefix
	}
	defer r.cleanup()
	if r.jobs > 1 && opts.Cache != "" {
		r.viewCost = cache.Cost(opts.Cache)
	}
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
	r.work, err = os.MkdirTemp("", "chisel-coverage-*")
	if err != nil {
		return nil, err
	}
	if opts.Keep {
		r.logf("rootfs dirs: %s", r.work)
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
	r.removeViews()
	r.verbosef("%d cuts %d paths %.1fmb %.1fs", r.cuts, r.paths, float64(r.bytes)/1e6, time.Since(r.start).Seconds())
	return r.results, nil
}

// ResolveArches expands "all", defaults to the host and keeps the order
// given, first occurrence wins.
func ResolveArches(given []string) ([]string, error) {
	if len(given) == 0 {
		a, err := deb.InferArch()
		if err != nil {
			return nil, err
		}
		return []string{a}, nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	for _, a := range given {
		if a == "all" {
			for _, x := range deb.Arches() {
				add(x)
			}
			continue
		}
		if !deb.ValidArch(a) {
			return nil, fmt.Errorf("invalid architecture %q", a)
		}
		add(a)
	}
	return out, nil
}

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

// Jobs is how many cuts may run at once for a --jobs value: n itself, one
// for 0, one per cpu for a negative value.
func Jobs(n int) int {
	switch {
	case n < 0:
		return runtime.NumCPU()
	case n == 0:
		return 1
	}
	return n
}

// pass cuts the lead slice of each (package, architecture) given and, as
// each finds its package, the package's other slices, then assembles the
// results. Workers feed the queue themselves, so no slice is cut where there
// is no package and no worker waits for the others between the two.
func (r *run) pass(leads []unit) {
	var work time.Duration
	total := 0
	for _, u := range leads {
		cuts := len(r.prepared[u.pi].cuts)
		total += cuts
		work += r.avg(u.pi) * time.Duration(cuts)
	}
	n := r.workers(work, total)
	queue := make(chan unit, total)
	var pm sync.Mutex
	pending := len(leads)
	for _, u := range leads {
		queue <- u
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.worker(i > 0)
			defer w.close()
			for u := range queue {
				w.prepare(r.opts.Arches[u.ai], r.prepared[u.pi].def.Package)
				start := time.Now()
				r.cutUnit(w, u)
				if u.ai == 0 {
					r.mu.Lock()
					r.took[u.pi] += time.Since(start)
					r.done[u.pi]++
					r.mu.Unlock()
				}
				w.merge()
				more := r.followUps(u)
				pm.Lock()
				for _, f := range more {
					queue <- f
				}
				pending += len(more) - 1
				if pending == 0 {
					close(queue)
				}
				pm.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, u := range leads {
		r.results[u.pi].Arches[u.ai] = r.assemble(u.pi, u.ai)
	}
}

// followUps is what a finished unit puts on the queue: the package's other
// slices once its lead cut found it, nothing otherwise or once the run is
// being stopped.
func (r *run) followUps(u unit) []unit {
	if u.ci != 0 || r.ctx.Err() != nil {
		return nil
	}
	r.mu.Lock()
	c := r.cells[u.pi][u.ai]
	r.mu.Unlock()
	if !c.avail || c.err != nil {
		return nil
	}
	var more []unit
	for ci := 1; ci < len(r.prepared[u.pi].cuts); ci++ {
		more = append(more, unit{u.pi, u.ai, ci})
	}
	return more
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

// workers is how many workers a pass of units gets for work seconds of
// cuts: as many as cuts may run at once, but no more than pay off. The first
// worker and every pooled view are free; each further worker waits for its
// view, and views are seeded one at a time, so the k-th new one starts k
// seeds in. A worker is added only while it brings the end of the pass
// nearer.
func (r *run) workers(work time.Duration, units int) int {
	most := min(r.jobs, units)
	if most <= 1 || r.viewCost == 0 {
		return most
	}
	r.mu.Lock()
	free, seed := 1+len(r.views), r.seed
	r.mu.Unlock()
	if seed == 0 {
		seed = r.viewCost
	}
	best, end := 1, work
	for k := 2; k <= most; k++ {
		e := finish(work, k, free, seed)
		if e >= end {
			break
		}
		best, end = k, e
	}
	return best
}

// finish is when k workers are done with work seconds of cuts, free of them
// starting at once and the rest one seed apart: the workers' active time
// adds up to the work.
func finish(work time.Duration, k, free int, seed time.Duration) time.Duration {
	starts := make([]time.Duration, k)
	for i := free; i < k; i++ {
		starts[i] = time.Duration(i-free+1) * seed
	}
	var sum time.Duration
	for m := 1; m <= k; m++ {
		sum += starts[m-1]
		end := (work + sum) / time.Duration(m)
		if m == k || end <= starts[m] {
			return end
		}
	}
	return work
}

// probePhase runs the probes, one worker per architecture and no views:
// the only blob two chisels can both miss is an architecture's Packages
// index, and a worker's probes are all on one architecture, one after
// another. The full cuts that follow then find every index cached.
func (r *run) probePhase(units []unit) {
	var byArch [][]unit
	index := map[int]int{}
	for _, u := range units {
		i, ok := index[u.ai]
		if !ok {
			i = len(byArch)
			index[u.ai] = i
			byArch = append(byArch, nil)
		}
		byArch[i] = append(byArch[i], u)
	}
	n := min(r.jobs, len(byArch))
	next := make(chan []unit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.worker(false)
			for group := range next {
				for _, u := range group {
					r.results[u.pi].Arches[u.ai] = r.probe(w, u)
				}
			}
		}()
	}
groups:
	for _, group := range byArch {
		select {
		case next <- group:
		case <-r.ctx.Done():
			break groups
		}
	}
	close(next)
	wg.Wait()
}

// worker makes a worker, isolated in a view of the cache when asked to and
// there is a cache dir.
func (r *run) worker(isolate bool) *worker {
	return &worker{r: r, runner: *r.opts.Runner, isolate: isolate && r.opts.Cache != ""}
}

// prepare gives an isolated worker its view before its first unit, on arch
// for pkg: one an earlier worker left when there is one, else a new one,
// logged like a cut as "<arch> <pkg>_<prefix>-cache-view <blobs>".
// When no view can be made the worker shares chisel's cache as is.
func (w *worker) prepare(arch, pkg string) {
	if !w.isolate {
		return
	}
	w.isolate = false
	r := w.r
	r.mu.Lock()
	if n := len(r.views); n > 0 {
		w.view, r.views = r.views[n-1], r.views[:n-1]
	}
	r.mu.Unlock()
	if w.view == nil {
		// Linking into one directory serialises in the kernel anyway, so
		// seeding views one at a time costs nothing overall and gets the
		// first one out sooner.
		r.seeding.Lock()
		start := time.Now()
		view, err := cache.New(r.opts.Cache)
		r.seeding.Unlock()
		if err != nil {
			r.logf("%s %s_%s-cache-view error: %v; sharing chisel's cache instead", arch, pkg, r.names, err)
			return
		}
		r.mu.Lock()
		r.seed = time.Since(start)
		r.mu.Unlock()
		r.verbosef("%s %s_%s-cache-view %d", arch, pkg, r.names, view.Blobs())
		w.view = view
	}
	w.runner.CacheHome = w.view.Home
}

func (w *worker) cut(release, root, arch, slice string) (*cut.Result, error) {
	return w.runner.Cut(w.r.ctx, release, root, arch, slice)
}

// merge publishes what the worker's view fetched into the shared cache.
func (w *worker) merge() {
	if w.view == nil {
		return
	}
	if err := w.view.Merge(); err != nil {
		w.r.logf("cache view: %v", err)
	}
}

// close hands the view back for the next worker.
func (w *worker) close() {
	w.merge()
	if w.view != nil {
		w.r.mu.Lock()
		w.r.views = append(w.r.views, w.view)
		w.r.mu.Unlock()
	}
}

// cutUnit cuts one slice into its cell. Once a cut of the cell has failed
// the rest are skipped, since the architecture reports the error anyway.
func (r *run) cutUnit(w *worker, u unit) {
	p, arch := r.prepared[u.pi], r.opts.Arches[u.ai]
	name := p.cuts[u.ci]
	c := &r.cells[u.pi][u.ai]
	r.mu.Lock()
	failed := c.err != nil
	r.mu.Unlock()
	if failed {
		return
	}
	root := filepath.Join(r.work, p.def.Package, arch, name)
	res, err := w.cut(p.rel.Dir, root, arch, p.def.Package+"_"+name)
	r.cutf(arch, p.def.Package+"_"+name, res, err)
	if err == nil && r.opts.Visit != nil && name != r.names.Probe() {
		r.opts.Visit(Cut{Package: p.def.Package, Arch: arch, Slice: name, Root: root, Result: res})
	}
	r.discard(root)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case errors.Is(err, cut.ErrNotAvailable) && u.ci == 0:
		// Not on this architecture; the cell stays unavailable.
	case errors.Is(err, cut.ErrNoContent) && r.names.IsIgnore(name):
		// An ignore glob matching nothing in this package ignores nothing.
	case err != nil:
		if c.err == nil {
			c.err = err
		}
	default:
		if c.sets == nil {
			c.sets = map[string][]string{}
		}
		if name != r.names.Probe() {
			c.sets[name] = res.Paths
		}
		if u.ci == 0 {
			c.avail = true
			c.version, c.debArch = res.Version, res.DebArch
		}
	}
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

// removeViews deletes the idle views, all at once since each is thousands of
// unlinks.
func (r *run) removeViews() {
	var wg sync.WaitGroup
	for _, v := range r.views {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = v.Remove()
		}()
	}
	wg.Wait()
	r.views = nil
}

// probe cuts the empty probe slice of an arch: all package on another
// architecture and, when the package is there, hands back the lead
// architecture's result for it.
func (r *run) probe(w *worker, u unit) Arch {
	p, arch := r.prepared[u.pi], r.opts.Arches[u.ai]
	probe := r.names.Probe()
	root := filepath.Join(r.work, p.def.Package, arch, probe)
	res, err := w.cut(p.rel.Dir, root, arch, p.def.Package+"_"+probe)
	r.cutf(arch, p.def.Package+"_"+probe, res, err)
	r.discard(root)
	if errors.Is(err, cut.ErrNotAvailable) {
		return Arch{Arch: arch}
	}
	if err != nil {
		return Arch{Arch: arch, Err: err}
	}
	a := r.results[u.pi].Arches[0]
	a.StandIn = a.Arch
	a.Arch = arch
	if res.Version != "" {
		a.Version, a.DebArch = res.Version, res.DebArch
	}
	return a
}

// discard removes a cut's rootfs in the background, since a big one is
// thousands of unlinks the worker need not wait for. Kept with --keep.
func (r *run) discard(root string) {
	if r.opts.Keep {
		return
	}
	r.rm.Add(1)
	go func() {
		defer r.rm.Done()
		_ = os.RemoveAll(root)
	}()
}

func (r *run) cleanup() {
	r.rm.Wait()
	r.removeViews()
	if r.opts.Keep {
		return
	}
	for _, p := range r.prepared {
		_ = p.rel.Remove()
	}
	if r.work != "" {
		_ = os.RemoveAll(r.work)
	}
}

// cutf counts one finished cut and reports it on the Verbose writer as
// "<arch> <slice> <paths>"; a package missing for the architecture is
// "n/a", a glob matching nothing "0", a failure "error".
func (r *run) cutf(arch, slice string, res *cut.Result, err error) {
	r.mu.Lock()
	r.cuts++
	if res != nil {
		r.paths += len(res.Paths)
		r.bytes += res.Bytes
	}
	r.mu.Unlock()
	if r.opts.Verbose == nil {
		return
	}
	var what string
	switch {
	case errors.Is(err, cut.ErrNotAvailable):
		what = "n/a"
	case errors.Is(err, cut.ErrNoContent):
		what = "0"
	case err != nil:
		what = "error"
	default:
		what = fmt.Sprintf("%d", len(res.Paths))
	}
	r.verbosef("%s %s %s", arch, slice, what)
}

// verbosef writes a line on the Verbose writer.
func (r *run) verbosef(format string, args ...any) {
	if r.opts.Verbose != nil {
		r.stampf(r.opts.Verbose, format, args...)
	}
}

func (r *run) logf(format string, args ...any) {
	if r.opts.Log != nil {
		r.stampf(r.opts.Log, format, args...)
	}
}

// stampf writes one timestamped line in a single call under the run's
// log lock, since workers log concurrently and the writer need not be
// safe for that. See cut.Stamp for the line format.
func (r *run) stampf(w io.Writer, format string, args ...any) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	fmt.Fprintf(w, "%s "+format+"\n", append([]any{cut.Stamp()}, args...)...)
}
