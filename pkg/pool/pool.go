// Package pool runs chisel cuts in parallel: workers that each cut one unit
// at a time, scheduled so that concurrent chisels never share a writable
// cache. A unit names the release dir it is cut against, so the pool knows
// nothing of packages or of where a release came from. Every finished cut is
// handed to the caller while its rootfs still exists.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/lczyk/chisel-proxy/pkg/deb"

	"github.com/lczyk/chisel-coverage/pkg/cache"
	"github.com/lczyk/chisel-coverage/pkg/cut"
)

// Options configures a pool.
type Options struct {
	Jobs   int    // cuts running at once; 0 or 1 is one after another, -1 one per cpu
	Cache  string // chisel's cache dir, for per-worker views; "" shares it as is
	Runner *cut.Runner
	Keep   bool // keep the rootfs dirs and the work dir
	// Name names the line a new cache view is logged with,
	// "<arch> <package>_<name>-cache-view <blobs>".
	Name string
	// Temp is the pattern of the work dir, as os.MkdirTemp takes it;
	// "chisel-coverage-*" when empty.
	Temp    string
	Log     io.Writer // notices (--keep, cache trouble); nil discards
	Verbose io.Writer // one line per cut; nil discards
}

// Unit is one cut to run.
type Unit struct {
	Release string // the release dir chisel cuts against
	Arch    string
	Slice   string // <package>_<slice>
	// Label is what the log lines call the unit, for telling apart two units
	// of one slice and architecture; Slice when empty.
	Label string
	// Dir is the unit's rootfs dir under the work dir, which two units must
	// not share; a number of its own when empty.
	Dir string
	// Cost is what the cut is expected to take, for sizing a pass; a second
	// when zero.
	Cost time.Duration
	Tag  any // the caller's own, handed back
}

// Group is a lead unit and the units that only run once it has, and only for
// as long as nothing has stopped the group.
type Group struct {
	Lead Unit
	Rest []Unit
}

// Cut is a finished unit.
type Cut struct {
	Unit
	Root   string // the rootfs, gone once the callback returns unless Options.Keep
	Result *cut.Result
	Err    error
	Start  time.Time // when the cut began
}

// Pool runs cuts. Make one with New and Close it when done.
type Pool struct {
	ctx      context.Context
	opts     Options
	work     string
	jobs     int           // cuts allowed to run at once
	seed     time.Duration // the last view's seeding time; 0 until one was
	viewCost time.Duration // of making and removing one view; 0 without a cache
	start    time.Time
	rm       sync.WaitGroup // rootfs dirs being removed in the background
	mu       sync.Mutex
	logMu    sync.Mutex    // the Log and Verbose writers
	seeding  sync.Mutex    // views are seeded one at a time, see worker
	views    []*cache.View // idle views, for the next worker
	dirs     int           // rootfs dirs numbered so far
	cuts     int           // the totals, for the summary line
	paths    int
	bytes    int64
}

// worker cuts one unit at a time with its own view of chisel's cache, merged
// back after every cut so the others gain the blobs.
type worker struct {
	p       *Pool
	runner  cut.Runner
	isolate bool // wants a view of the cache, made on its first unit
	view    *cache.View
}

// New makes a pool and its work dir. When ctx is done the cuts stop.
func New(ctx context.Context, opts Options) (*Pool, error) {
	p := &Pool{ctx: ctx, opts: opts, jobs: Jobs(opts.Jobs), start: time.Now()}
	if p.jobs > 1 && opts.Cache != "" {
		p.viewCost = cache.Cost(opts.Cache)
	}
	pattern := opts.Temp
	if pattern == "" {
		pattern = "chisel-coverage-*"
	}
	work, err := os.MkdirTemp("", pattern)
	if err != nil {
		return nil, err
	}
	p.work = work
	if opts.Keep {
		p.logf("rootfs dirs: %s", p.work)
	}
	return p, nil
}

// Pass cuts the lead of every group and, as each lead finishes, the rest of
// its group. Workers call done concurrently, once per unit cut, while the
// unit's rootfs exists. When done returns false the group is stopped: its
// units not yet started are skipped, the ones running finish and are still
// handed to done. Workers feed the queue themselves, so no worker waits for
// the others between a lead and the rest. Every worker but the first cuts in
// a private view of the cache. Pass returns once everything is cut or
// skipped, with ctx's error when ctx is done. One Pass or Serial at a time.
func (p *Pool) Pass(groups []Group, done func(Cut) bool) error {
	var work time.Duration
	total := 0
	for i := range groups {
		total += 1 + len(groups[i].Rest)
		work += cost(groups[i].Lead)
		for _, u := range groups[i].Rest {
			work += cost(u)
		}
	}
	n := p.workers(work, total)
	// ui indexes a group's units, 0 the lead.
	type item struct{ gi, ui int }
	queue := make(chan item, total)
	var pm sync.Mutex
	stopped := make([]bool, len(groups))
	pending := len(groups)
	for gi := range groups {
		queue <- item{gi, 0}
	}
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := p.worker(i > 0)
			defer w.close()
			for it := range queue {
				g := &groups[it.gi]
				u := g.Lead
				if it.ui > 0 {
					u = g.Rest[it.ui-1]
				}
				w.prepare(u)
				pm.Lock()
				skip := stopped[it.gi]
				pm.Unlock()
				if !skip {
					c := w.cut(u)
					more := done(c)
					p.discard(c.Root)
					if !more {
						pm.Lock()
						stopped[it.gi] = true
						pm.Unlock()
					}
				}
				w.merge()
				pm.Lock()
				rest := 0
				if it.ui == 0 && !stopped[it.gi] && p.ctx.Err() == nil {
					rest = len(g.Rest)
					for ui := 1; ui <= rest; ui++ {
						queue <- item{it.gi, ui}
					}
				}
				pending += rest - 1
				if pending == 0 {
					close(queue)
				}
				pm.Unlock()
			}
		}()
	}
	wg.Wait()
	return p.ctx.Err()
}

// Serial cuts each list on a worker of its own, one unit after another, in
// chisel's cache as it is: no views. Workers call done concurrently, once per
// unit, while the unit's rootfs exists. It is safe only when no two lists can
// miss the same blob, as with one architecture per list once a Pass has
// cached what the architectures share: the only blob two chisels can then
// both miss is an architecture's Packages index. No new list is started once
// ctx is done, and its error is returned.
func (p *Pool) Serial(lists [][]Unit, done func(Cut)) error {
	n := min(p.jobs, len(lists))
	next := make(chan []Unit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := p.worker(false)
			for list := range next {
				for _, u := range list {
					c := w.cut(u)
					done(c)
					p.discard(c.Root)
				}
			}
		}()
	}
lists:
	for _, list := range lists {
		select {
		case next <- list:
		case <-p.ctx.Done():
			break lists
		}
	}
	close(next)
	wg.Wait()
	return p.ctx.Err()
}

// Summary removes the idle views and writes the totals so far on the Verbose
// writer, as "<cuts> cuts <paths> paths <megabytes>mb <seconds>s".
func (p *Pool) Summary() {
	p.removeViews()
	p.verbosef("%d cuts %d paths %.1fmb %.1fs", p.cuts, p.paths, float64(p.bytes)/1e6, time.Since(p.start).Seconds())
}

// Close waits for the rootfs removals and removes the views and, without
// Options.Keep, the work dir.
func (p *Pool) Close() {
	p.rm.Wait()
	p.removeViews()
	if !p.opts.Keep {
		_ = os.RemoveAll(p.work)
	}
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

func cost(u Unit) time.Duration {
	if u.Cost == 0 {
		return time.Second
	}
	return u.Cost
}

func label(u Unit) string {
	if u.Label == "" {
		return u.Slice
	}
	return u.Label
}

// workers is how many workers a pass of units gets for work seconds of
// cuts: as many as cuts may run at once, but no more than pay off. The first
// worker and every pooled view are free; each further worker waits for its
// view, and views are seeded one at a time, so the k-th new one starts k
// seeds in. A worker is added only while it brings the end of the pass
// nearer.
func (p *Pool) workers(work time.Duration, units int) int {
	most := min(p.jobs, units)
	if most <= 1 || p.viewCost == 0 {
		return most
	}
	p.mu.Lock()
	free, seed := 1+len(p.views), p.seed
	p.mu.Unlock()
	if seed == 0 {
		seed = p.viewCost
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

// worker makes a worker, isolated in a view of the cache when asked to and
// there is a cache dir.
func (p *Pool) worker(isolate bool) *worker {
	return &worker{p: p, runner: *p.opts.Runner, isolate: isolate && p.opts.Cache != ""}
}

// prepare gives an isolated worker its view before its first unit: one an
// earlier worker left when there is one, else a new one, logged like a cut
// under the unit's architecture and package. When no view can be made the
// worker shares chisel's cache as is.
func (w *worker) prepare(u Unit) {
	if !w.isolate {
		return
	}
	w.isolate = false
	p := w.p
	p.mu.Lock()
	if n := len(p.views); n > 0 {
		w.view, p.views = p.views[n-1], p.views[:n-1]
	}
	p.mu.Unlock()
	if w.view == nil {
		pkg, _, _ := strings.Cut(u.Slice, "_")
		// Linking into one directory serialises in the kernel anyway, so
		// seeding views one at a time costs nothing overall and gets the
		// first one out sooner.
		p.seeding.Lock()
		start := time.Now()
		view, err := cache.New(p.opts.Cache)
		p.seeding.Unlock()
		if err != nil {
			p.logf("%s %s_%s-cache-view error: %v; sharing chisel's cache instead", u.Arch, pkg, p.opts.Name, err)
			return
		}
		p.mu.Lock()
		p.seed = time.Since(start)
		p.mu.Unlock()
		p.verbosef("%s %s_%s-cache-view %d", u.Arch, pkg, p.opts.Name, view.Blobs())
		w.view = view
	}
	w.runner.CacheHome = w.view.Home
}

// cut cuts one unit into a rootfs dir of its own and logs it.
func (w *worker) cut(u Unit) Cut {
	p := w.p
	c := Cut{Unit: u, Root: p.root(u), Start: time.Now()}
	c.Result, c.Err = w.runner.CutAs(p.ctx, u.Release, c.Root, u.Arch, u.Slice, label(u))
	p.cutf(u.Arch, label(u), c.Result, c.Err)
	return c
}

// merge publishes what the worker's view fetched into the shared cache.
func (w *worker) merge() {
	if w.view == nil {
		return
	}
	if err := w.view.Merge(); err != nil {
		w.p.logf("cache view: %v", err)
	}
}

// close hands the view back for the next worker.
func (w *worker) close() {
	w.merge()
	if w.view != nil {
		w.p.mu.Lock()
		w.p.views = append(w.p.views, w.view)
		w.p.mu.Unlock()
	}
}

// root is where a unit's rootfs goes.
func (p *Pool) root(u Unit) string {
	if u.Dir != "" {
		return filepath.Join(p.work, u.Dir)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirs++
	return filepath.Join(p.work, fmt.Sprintf("%06d", p.dirs))
}

// removeViews deletes the idle views, all at once since each is thousands of
// unlinks.
func (p *Pool) removeViews() {
	var wg sync.WaitGroup
	for _, v := range p.views {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = v.Remove()
		}()
	}
	wg.Wait()
	p.views = nil
}

// discard removes a cut's rootfs in the background, since a big one is
// thousands of unlinks the worker need not wait for. Kept with Options.Keep.
func (p *Pool) discard(root string) {
	if p.opts.Keep {
		return
	}
	p.rm.Add(1)
	go func() {
		defer p.rm.Done()
		_ = os.RemoveAll(root)
	}()
}

// cutf counts one finished cut and reports it on the Verbose writer as
// "<arch> <label> <paths>"; a package missing for the architecture is
// "n/a", a path matching nothing "0", a failure "error".
func (p *Pool) cutf(arch, label string, res *cut.Result, err error) {
	p.mu.Lock()
	p.cuts++
	if res != nil {
		p.paths += len(res.Paths)
		p.bytes += res.Bytes
	}
	p.mu.Unlock()
	if p.opts.Verbose == nil {
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
	p.verbosef("%s %s %s", arch, label, what)
}

// verbosef writes a line on the Verbose writer.
func (p *Pool) verbosef(format string, args ...any) {
	if p.opts.Verbose != nil {
		p.stampf(p.opts.Verbose, format, args...)
	}
}

func (p *Pool) logf(format string, args ...any) {
	if p.opts.Log != nil {
		p.stampf(p.opts.Log, format, args...)
	}
}

// stampf writes one timestamped line in a single call under the pool's log
// lock, since workers log concurrently and the writer need not be safe for
// that. See cut.Stamp for the line format.
func (p *Pool) stampf(w io.Writer, format string, args ...any) {
	p.logMu.Lock()
	defer p.logMu.Unlock()
	fmt.Fprintf(w, "%s "+format+"\n", append([]any{cut.Stamp()}, args...)...)
}
