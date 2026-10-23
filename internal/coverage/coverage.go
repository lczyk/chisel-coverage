// Package coverage drives the cuts for a set of packages and architectures and
// turns their rootfs listings into coverage: what the deb ships, what the
// slices take, what is left.
package coverage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	temprelease "github.com/lczyk/chisel-proxy/pkg/temp-release"

	"github.com/lczyk/chisel-slice-coverage/internal/cache"
	"github.com/lczyk/chisel-slice-coverage/internal/cut"
	"github.com/lczyk/chisel-slice-coverage/internal/sdf"
)

// AllArches lists every architecture ubuntu builds for, in --arch all order.
var AllArches = []string{"amd64", "arm64", "armhf", "i386", "ppc64el", "riscv64", "s390x"}

// Options configures a run.
type Options struct {
	Release string
	Arches  []string // the first is cut alone before the rest
	Only    []string // --slice values, bare or <package>_<slice>; nil means all
	Ignores []string
	Keep    bool
	Jobs    int    // cuts running at once; 0 or 1 is one after another, -1 one per cpu
	Cache   string // chisel's cache dir, for per-worker views; "" shares it as is
	Runner  *cut.Runner
	Log     io.Writer // notices (--keep, cache trouble); nil discards
	Verbose io.Writer // one line per cut; nil discards
}

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
	Arches []Arch   // in Options.Arches order
}

type prepared struct {
	def    *sdf.Definition
	slices []string // real slices considered
	cuts   []string // slices to cut, sdf.AllSlice first
	rel    *temprelease.Release
}

type run struct {
	ctx      context.Context
	opts     Options
	work     string
	prepared []*prepared
	results  []Package
	jobs     int             // cuts allowed to run at once
	took     []time.Duration // per package, its lead arch's cut wall time
	viewCost time.Duration   // of making and removing one view; 0 without a cache
	start    time.Time
	rm       sync.WaitGroup // rootfs dirs being removed in the background
	mu       sync.Mutex
	seeding  sync.Mutex    // views are seeded one at a time, see worker
	views    []*cache.View // idle views, for the next worker
	cuts     int           // the totals of the run, for the summary line
	paths    int
	bytes    int64
}

// unit is one package on one architecture. A probe only checks that the
// package exists there: an arch: all package cut on the lead architecture
// has the same files everywhere it exists.
type unit struct {
	pi, ai int
	probe  bool
}

// A probe is a cut with nothing to extract, about this long on a warm cache.
const probeCost = time.Second

// worker cuts one package at a time on one architecture with its own view of
// chisel's cache, merged back after every package so the others gain the
// blobs.
type worker struct {
	r       *run
	runner  cut.Runner
	isolate bool // wants a view of the cache, made on its first unit
	view    *cache.View
}

// Run cuts every package on every architecture and returns the results in
// input order, a package given twice once. It fails up front on a bad
// release, a bad slice definition or an unknown --slice; a failed cut is
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

	r := &run{ctx: ctx, opts: opts, jobs: jobs(opts.Jobs), took: make([]time.Duration, len(pkgs)), start: time.Now()}
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
	}
	r.work, err = os.MkdirTemp("", "chisel-slice-coverage-*")
	if err != nil {
		return nil, err
	}
	if opts.Keep {
		r.logf("rootfs dirs: %s", r.work)
	}

	// The lead architecture alone first, so the rest find the arch: all debs
	// cached and the arch: all packages known, then every other architecture.
	var lead, rest []unit
	for pi := range r.prepared {
		lead = append(lead, unit{pi: pi, ai: 0})
	}
	r.phase(lead)
	for ai := 1; ai < len(opts.Arches); ai++ {
		for pi := range r.prepared {
			a := r.results[pi].Arches[0]
			rest = append(rest, unit{pi: pi, ai: ai, probe: a.Available && a.Err == nil && a.DebArch == "all"})
		}
	}
	r.phase(rest)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.removeViews()
	r.verbosef("%d cuts %d paths %.1fmb %.1fs", r.cuts, r.paths, float64(r.bytes)/1e6, time.Since(r.start).Seconds())
	return r.results, nil
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
// absent from the map is unrestricted. A bare name applies to every package
// that has it and must exist in at least one; a <package>_<slice> name must
// name a listed package.
func resolveOnly(only, pkgs []string) (map[string][]string, error) {
	if len(only) == 0 {
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
	def, err := sdf.Read(r.opts.Release, pkg)
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
		if len(keep) == 0 && !r.anyExists(only) {
			return nil, fmt.Errorf("no package being checked has a slice named %s", strings.Join(only, ", "))
		}
	}
	file, cuts, err := sdf.Synthesise(def, keep, r.opts.Ignores)
	if err != nil {
		return nil, err
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
	p := &prepared{def: def, cuts: cuts, rel: rel}
	for _, s := range def.Slices {
		if keep == nil || slices.Contains(keep, s.Name) {
			p.slices = append(p.slices, s.Name)
		}
	}
	return p, nil
}

func (r *run) prefixed(name string) bool { return slices.Contains(r.opts.Only, name) }

// anyExists reports whether any of the bare names exists in some package
// already prepared or still to come. Checked lazily: the error only matters
// when a package ends up with nothing to cut.
func (r *run) anyExists(names []string) bool {
	for _, name := range names {
		if r.prefixed(name) {
			continue
		}
		for _, p := range r.prepared {
			if p.def.Slice(name) != nil {
				return true
			}
		}
	}
	return false
}

// jobs is how many cuts may run at once for a --jobs value.
func jobs(n int) int {
	switch {
	case n < 0:
		return runtime.NumCPU()
	case n == 0:
		return 1
	}
	return n
}

// phase cuts the units in order over the workers it gets. The first worker
// shares chisel's cache: a view's blobs enter it only as complete files
// linked into place, which never trips a chisel reading or writing beside
// them. Every other worker has a view.
func (r *run) phase(units []unit) {
	n := r.workers(units)
	next := make(chan unit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := r.worker(i > 0)
			defer w.close()
			for u := range next {
				w.prepare(r.opts.Arches[u.ai], r.prepared[u.pi].def.Package)
				start := time.Now()
				if u.probe {
					r.results[u.pi].Arches[u.ai] = r.probe(w, u)
				} else {
					r.results[u.pi].Arches[u.ai] = r.cutPackage(w, r.prepared[u.pi], r.opts.Arches[u.ai])
				}
				if u.ai == 0 {
					r.took[u.pi] = time.Since(start)
				}
				w.merge()
			}
		}()
	}
units:
	for _, u := range units {
		select {
		case next <- u:
		case <-r.ctx.Done():
			break units
		}
	}
	close(next)
	wg.Wait()
}

// workers is how many workers a phase gets: as many as cuts may run at once,
// but no more than its work pays for, since every worker past the first
// costs a view, and a view of a big cache costs about as much as cutting a
// small package. The lead phase has nothing measured yet and gets them all.
func (r *run) workers(units []unit) int {
	n := min(r.jobs, len(units))
	var work time.Duration
	for _, u := range units {
		if u.probe {
			work += probeCost
		} else {
			work += r.took[u.pi]
		}
	}
	if n <= 1 || r.viewCost == 0 || work == 0 {
		return n
	}
	return min(n, 1+int(work/r.viewCost))
}

// worker makes a worker, isolated in a view of the cache when asked to and
// there is a cache dir.
func (r *run) worker(isolate bool) *worker {
	return &worker{r: r, runner: *r.opts.Runner, isolate: isolate && r.opts.Cache != ""}
}

// prepare gives an isolated worker its view before its first unit, on arch
// for pkg: one an earlier worker left when there is one, else a new one,
// logged like a cut as "<arch> <pkg>_cov-cache-view <blobs>".
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
		view, err := cache.New(r.opts.Cache)
		r.seeding.Unlock()
		if err != nil {
			r.logf("%s %s_cov-cache-view error: %v; sharing chisel's cache instead", arch, pkg, err)
			return
		}
		r.verbosef("%s %s_cov-cache-view %d", arch, pkg, view.Blobs())
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

func (r *run) cutPackage(w *worker, p *prepared, arch string) Arch {
	a := Arch{Arch: arch, Slices: map[string][]string{}}
	sets := map[string][]string{}
	for _, name := range p.cuts {
		root := filepath.Join(r.work, p.def.Package, arch, name)
		res, err := w.cut(p.rel.Dir, root, arch, p.def.Package+"_"+name)
		r.cutf(arch, p.def.Package+"_"+name, res, err)
		r.discard(root)
		if errors.Is(err, cut.ErrNotAvailable) && name == sdf.AllSlice {
			return a
		}
		// An ignore glob matching nothing in this package ignores nothing.
		if errors.Is(err, cut.ErrNoContent) && sdf.IsIgnore(name) {
			continue
		}
		if err != nil {
			a.Err = err
			return a
		}
		sets[name] = res.Paths
		if name == sdf.AllSlice {
			a.Version, a.DebArch = res.Version, res.DebArch
		}
	}
	a.Available = true

	covered := map[string]bool{}
	for _, name := range p.slices {
		paths := sets[name]
		if paths == nil {
			paths = []string{}
		}
		a.Slices[name] = paths
		for _, path := range paths {
			covered[path] = true
		}
	}
	ignore := map[string]bool{}
	for name, paths := range sets {
		if !sdf.IsIgnore(name) {
			continue
		}
		for _, path := range paths {
			ignore[path] = true
		}
	}
	a.Covered, a.Uncovered, a.Ignored = []string{}, []string{}, []string{}
	for _, path := range sets[sdf.AllSlice] {
		switch {
		case covered[path]:
			a.Covered = append(a.Covered, path)
		case ignore[path]:
			a.Ignored = append(a.Ignored, path)
		default:
			a.Uncovered = append(a.Uncovered, path)
		}
	}
	a.Total = len(sets[sdf.AllSlice])
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
	root := filepath.Join(r.work, p.def.Package, arch, sdf.ProbeSlice)
	res, err := w.cut(p.rel.Dir, root, arch, p.def.Package+"_"+sdf.ProbeSlice)
	r.cutf(arch, p.def.Package+"_"+sdf.ProbeSlice, res, err)
	r.discard(root)
	if errors.Is(err, cut.ErrNotAvailable) {
		return Arch{Arch: arch}
	}
	if err != nil {
		return Arch{Arch: arch, Err: err}
	}
	a := r.results[u.pi].Arches[0]
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
	stampf(r.opts.Verbose, "%s %s %s", arch, slice, what)
}

// verbosef writes a line on the Verbose writer.
func (r *run) verbosef(format string, args ...any) {
	if r.opts.Verbose != nil {
		stampf(r.opts.Verbose, format, args...)
	}
}

func (r *run) logf(format string, args ...any) {
	if r.opts.Log != nil {
		stampf(r.opts.Log, format, args...)
	}
}

// stampf writes one timestamped line in a single call, since workers log
// concurrently. See cut.Stamp for the line format.
func stampf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s "+format+"\n", append([]any{cut.Stamp()}, args...)...)
}

// The cpu part of each architecture's multiarch triplet, plus gcc's spelling
// for i386, which it uses under /usr/lib/gcc.
var cpus = map[string][]string{
	"amd64":   {"x86_64"},
	"arm64":   {"aarch64"},
	"armhf":   {"arm"},
	"i386":    {"i386", "i686"},
	"ppc64el": {"powerpc64le"},
	"riscv64": {"riscv64"},
	"s390x":   {"s390x"},
}

var abi = regexp.MustCompile(`<arch>-linux-gnu[a-z0-9]*`)

// Normalise rewrites what names the architecture in path, so paths from
// different architectures compare equal: the multiarch triplet becomes
// <arch>-linux-<gnu> (the cpu part only where -linux- follows, so "arm" inside
// "apparmor" is left alone; the abi part wherever it is, in file names too),
// and the architecture's own name becomes <arch> where it stands as a word,
// as in java-21-openjdk-amd64 or README.Debian.armhf.gz.
func Normalise(path, arch string) string {
	if _, ok := cpus[arch]; !ok {
		return path
	}
	for _, cpu := range cpus[arch] {
		path = strings.ReplaceAll(path, cpu+"-linux-", "<arch>-linux-")
	}
	path = abi.ReplaceAllString(path, "<arch>-linux-<gnu>")
	return replaceWord(path, arch, "<arch>")
}

// replaceWord replaces word in s where neither neighbour is a letter or digit.
func replaceWord(s, word, with string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, word)
		if i < 0 {
			break
		}
		j := i + len(word)
		if (i == 0 || !alnum(s[i-1])) && (j == len(s) || !alnum(s[j])) {
			b.WriteString(s[:i])
			b.WriteString(with)
		} else {
			b.WriteString(s[:j])
		}
		s = s[j:]
	}
	b.WriteString(s)
	return b.String()
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// Group is a set of architectures with the same result. A group of several
// architectures holds the normalised paths, which stand for all of them; a
// group of one holds that architecture's real paths.
type Group struct {
	Arches []string
	Arch
}

// Groups folds a package's architectures into groups with identical results:
// the same version and availability, and the same uncovered, ignored and
// per-slice sets after Normalise. An arch: all package is one deb for every
// architecture, so its paths are compared as they are: a Go source tree
// names every architecture somewhere and would never fold otherwise. Failed
// cuts are never grouped.
func Groups(p Package) []Group {
	var groups []Group
	var raw []Arch
	index := map[string]int{}
	for _, a := range p.Arches {
		n := a
		if a.DebArch != "all" {
			n = normalised(a)
		}
		key := groupKey(n)
		if a.Err != nil {
			key = "err\x00" + a.Arch
		}
		if i, ok := index[key]; ok {
			groups[i].Arches = append(groups[i].Arches, a.Arch)
			continue
		}
		index[key] = len(groups)
		groups = append(groups, Group{Arches: []string{a.Arch}, Arch: n})
		raw = append(raw, a)
	}
	for i := range groups {
		if len(groups[i].Arches) == 1 {
			groups[i].Arch = raw[i]
		}
	}
	return groups
}

// Ungrouped is one group per architecture, real paths, nothing folded.
func Ungrouped(p Package) []Group {
	groups := make([]Group, 0, len(p.Arches))
	for _, a := range p.Arches {
		groups = append(groups, Group{Arches: []string{a.Arch}, Arch: a})
	}
	return groups
}

func normalised(a Arch) Arch {
	n := a
	n.Covered = normaliseAll(a.Covered, a.Arch)
	n.Uncovered = normaliseAll(a.Uncovered, a.Arch)
	n.Ignored = normaliseAll(a.Ignored, a.Arch)
	n.Slices = map[string][]string{}
	for name, paths := range a.Slices {
		n.Slices[name] = normaliseAll(paths, a.Arch)
	}
	return n
}

func normaliseAll(paths []string, arch string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, Normalise(p, arch))
	}
	sort.Strings(out)
	return out
}

func groupKey(a Arch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v\x00%s\x00%v\x00", a.Available, a.Version, a.DebArch == "all")
	for _, set := range [][]string{a.Uncovered, a.Ignored} {
		b.WriteString(strings.Join(set, "\x00"))
		b.WriteString("\x01")
	}
	names := make([]string, 0, len(a.Slices))
	for name := range a.Slices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(name)
		b.WriteString("\x02")
		b.WriteString(strings.Join(a.Slices[name], "\x00"))
		b.WriteString("\x01")
	}
	return b.String()
}
