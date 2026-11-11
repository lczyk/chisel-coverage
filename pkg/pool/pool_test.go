package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lczyk/assert"
	"github.com/lczyk/chisel-proxy/pkg/deb"

	"github.com/lczyk/chisel-coverage/pkg/cache"
	"github.com/lczyk/chisel-coverage/pkg/cut"
)

// fakeChisel writes $root/from/<release> and appends "<arch> <slice>
// <release>" to $FAKE_LOG. A slice listed in $FAKE_FAIL fails, one listed in
// $FAKE_NA fails the way chisel does for a package no archive has.
// $FAKE_SLEEP, when set, is slept first. With $FAKE_CACHE set it behaves like
// chisel's cache: it fails unless blob $FAKE_CACHE is in
// $XDG_CACHE_HOME/chisel/sha256/, then writes a blob named after the cut
// there and appends "<home> <arch> <slice>" to $FAKE_HOMES. With $FAKE_ARGS
// set it appends its arguments there, a cut a line.
const fakeChisel = `#!/bin/sh
[ -n "$FAKE_ARGS" ] && printf '%s\n' "$*" >> "$FAKE_ARGS"
slice=
while [ $# -gt 0 ]; do
  case "$1" in
    --release) release="$2"; shift 2;;
    --root) root="$2"; shift 2;;
    --arch) arch="$2"; shift 2;;
    --ignore) shift 2;;
    cut) shift;;
    *) [ -z "$slice" ] && slice="$1"; shift;;
  esac
done
pkg="${slice%%_*}"
printf '%s %s %s\n' "$arch" "$slice" "$release" >> "$FAKE_LOG"
[ -n "$FAKE_SLEEP" ] && sleep "$FAKE_SLEEP"
if [ -n "$FAKE_CACHE" ]; then
  blobs="$XDG_CACHE_HOME/chisel/sha256"
  [ -f "$blobs/$FAKE_CACHE" ] || { echo "seed blob missing from $blobs" >&2; exit 1; }
  printf '%s %s' "$arch" "$slice" > "$blobs/$(printf '%s %s' "$arch" "$slice" | cksum | cut -d' ' -f1)"
  printf '%s %s %s\n' "$XDG_CACHE_HOME" "$arch" "$slice" >> "$FAKE_HOMES"
fi
case " $FAKE_FAIL " in *" $slice "*) echo "boom" >&2; exit 1;; esac
case " $FAKE_NA " in *" $slice "*) echo "error: cannot find package \"$pkg\" in archive(s)" >&2; exit 1;; esac
echo "cutting $slice" >&2
mkdir -p "$root/from"
echo x > "$root/from/$release"
exit 0
`

type fixture struct {
	tmp    string // $TMPDIR for the run, so leftovers are visible
	log    string
	runner *cut.Runner
}

func setup(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	f := &fixture{tmp: filepath.Join(tmp, "tmpdir"), log: filepath.Join(tmp, "log")}
	bin := filepath.Join(tmp, "chisel")
	assert.NoError(t, os.WriteFile(bin, []byte(fakeChisel), 0755))
	f.runner = &cut.Runner{Bin: bin}
	assert.NoError(t, os.MkdirAll(f.tmp, 0755))
	t.Setenv("TMPDIR", f.tmp)
	t.Setenv("FAKE_LOG", f.log)
	t.Setenv("FAKE_SLEEP", "")
	t.Setenv("FAKE_FAIL", "")
	t.Setenv("FAKE_NA", "")
	t.Setenv("FAKE_CACHE", "")
	t.Setenv("FAKE_HOMES", filepath.Join(tmp, "homes"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "xdg"))
	return f
}

// cuts lists the cuts the fake ran so far, none when none ran.
func (f *fixture) cuts(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	assert.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// group is a group of slices of one package on one architecture, the first
// its lead.
func group(release, arch string, slices ...string) Group {
	g := Group{Lead: Unit{Release: release, Arch: arch, Slice: slices[0]}}
	for _, s := range slices[1:] {
		g.Rest = append(g.Rest, Unit{Release: release, Arch: arch, Slice: s})
	}
	return g
}

// locked is a writer two chisels can stream to at once, as stderr is.
type locked struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *locked) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// stamped matches the timestamp every line of the pool's log starts with.
var stamped = regexp.MustCompile(`^\d\d-\d\d-\d\dT\d\d:\d\d:\d\d `)

var took = regexp.MustCompile(` \d+\.\ds$`)

func TestPassCutsLeadsThenTheRest(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_NA", "gone_lead")
	t.Setenv("FAKE_FAIL", "bad_two")
	var out strings.Builder
	p, err := New(context.Background(), Options{Runner: f.runner, Verbose: &out})
	assert.NoError(t, err)
	var seen []string
	err = p.Pass([]Group{
		group("rel", "amd64", "ok_lead", "ok_one", "ok_two"),
		group("rel", "amd64", "gone_lead", "gone_one"),
		group("rel", "amd64", "bad_lead", "bad_one", "bad_two", "bad_three"),
	}, func(c Cut) bool {
		// The rootfs is still there for a cut that worked.
		if c.Err == nil {
			_, err := os.Stat(filepath.Join(c.Root, "from", "rel"))
			assert.NoError(t, err)
			assert.EqualArrays(t, c.Result.Paths, []string{"/from/", "/from/rel"})
		}
		seen = append(seen, c.Slice)
		return c.Err == nil
	})
	assert.NoError(t, err)
	// One worker: every lead, then the rest of each group in order. A lead
	// with no package stops its group, and so does a failure half way.
	want := []string{"ok_lead", "gone_lead", "bad_lead", "ok_one", "ok_two", "bad_one", "bad_two"}
	assert.EqualArrays(t, seen, want)
	for i, c := range f.cuts(t) {
		assert.Equal(t, c, "amd64 "+want[i]+" rel")
	}

	p.Summary()
	p.Close()
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		assert.That(t, stamped.MatchString(l), l)
		lines = append(lines, stamped.ReplaceAllString(l, ""))
	}
	last := len(lines) - 1
	assert.That(t, took.MatchString(lines[last]), lines[last])
	lines[last] = took.ReplaceAllString(lines[last], "")
	assert.EqualArrays(t, lines, []string{
		"amd64 ok_lead 2",
		"amd64 gone_lead n/a",
		"amd64 bad_lead 2",
		"amd64 ok_one 2",
		"amd64 ok_two 2",
		"amd64 bad_one 2",
		"amd64 bad_two error",
		"7 cuts 10 paths 0.0mb",
	})
	// Everything temporary is gone.
	left, err := os.ReadDir(f.tmp)
	assert.NoError(t, err)
	assert.Len(t, left, 0)
}

func TestPassKeepsUnitsOfOneSliceApart(t *testing.T) {
	f := setup(t)
	var out strings.Builder
	var verbose locked
	runner := *f.runner
	runner.Verbose = &verbose
	p, err := New(context.Background(), Options{Runner: &runner, Jobs: 2, Verbose: &out})
	assert.NoError(t, err)
	defer p.Close()
	var mu sync.Mutex
	roots := map[string]string{}
	err = p.Pass([]Group{
		{Lead: Unit{Release: "base", Arch: "amd64", Slice: "bash_bins", Label: "bash_bins@base", Tag: "b"}},
		{Lead: Unit{Release: "head", Arch: "amd64", Slice: "bash_bins", Label: "bash_bins@head", Tag: "h"}},
	}, func(c Cut) bool {
		assert.NoError(t, c.Err)
		// Each side got a rootfs of its own, cut from its own release.
		_, err := os.Stat(filepath.Join(c.Root, "from", c.Release))
		assert.NoError(t, err)
		mu.Lock()
		roots[c.Tag.(string)] = c.Root
		mu.Unlock()
		return true
	})
	assert.NoError(t, err)
	assert.Len(t, roots, 2)
	assert.That(t, roots["b"] != roots["h"])
	// The label is what both kinds of line carry.
	assert.ContainsString(t, out.String(), " amd64 bash_bins@base 2\n")
	assert.ContainsString(t, out.String(), " amd64 bash_bins@head 2\n")
	assert.ContainsString(t, verbose.b.String(), " amd64 bash_bins@base cutting bash_bins\n")
	assert.ContainsString(t, verbose.b.String(), " amd64 bash_bins@head cutting bash_bins\n")
}

func TestPassUsesCacheViews(t *testing.T) {
	f := setup(t)
	shared := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "chisel")
	assert.NoError(t, os.MkdirAll(filepath.Join(shared, "sha256"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(shared, "sha256", "abc123"), []byte("x"), 0644))
	t.Setenv("FAKE_CACHE", "abc123")
	var log, out strings.Builder
	p, err := New(context.Background(), Options{Runner: f.runner, Jobs: 2, Cache: shared, Name: "diff", Log: &log, Verbose: &out})
	assert.NoError(t, err)
	err = p.Pass([]Group{
		group("rel", "amd64", "bash_a", "bash_b", "bash_c"),
		group("rel", "amd64", "dash_a", "dash_b", "dash_c"),
	}, func(c Cut) bool {
		assert.NoError(t, c.Err)
		return true
	})
	assert.NoError(t, err)
	p.Close()
	assert.Equal(t, log.String(), "")
	// The first worker cuts in the shared cache, the second in a view.
	homes, err := os.ReadFile(os.Getenv("FAKE_HOMES"))
	assert.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(homes)), "\n")
	assert.Len(t, lines, 6)
	base := filepath.Join(filepath.Dir(shared), "chisel-coverage")
	inView := 0
	for _, l := range lines {
		h := strings.Fields(l)[0]
		if h == os.Getenv("XDG_CACHE_HOME") {
			continue
		}
		assert.That(t, strings.HasPrefix(h, base+string(filepath.Separator)+"view-"), h)
		inView++
	}
	assert.That(t, inView > 0 && inView < len(lines), lines)
	assert.That(t, regexp.MustCompile(`amd64 (bash|dash)_diff-cache-view \d+\n`).MatchString(out.String()), out.String())
	// Each cut's blob was merged back, and no view is left behind.
	blobs, err := os.ReadDir(filepath.Join(shared, "sha256"))
	assert.NoError(t, err)
	assert.Len(t, blobs, 1+6)
	views, err := os.ReadDir(base)
	assert.NoError(t, err)
	assert.Len(t, views, 0)
}

func TestSerialCutsEachListInOrderWithoutViews(t *testing.T) {
	f := setup(t)
	shared := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "chisel")
	assert.NoError(t, os.MkdirAll(filepath.Join(shared, "sha256"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(shared, "sha256", "abc123"), []byte("x"), 0644))
	t.Setenv("FAKE_CACHE", "abc123")
	t.Setenv("FAKE_NA", "dash_probe")
	p, err := New(context.Background(), Options{Runner: f.runner, Jobs: 3, Cache: shared})
	assert.NoError(t, err)
	defer p.Close()
	var lists [][]Unit
	for _, arch := range []string{"arm64", "riscv64"} {
		var list []Unit
		for _, s := range []string{"bash_probe", "dash_probe", "zsh_probe"} {
			list = append(list, Unit{Release: "rel", Arch: arch, Slice: s})
		}
		lists = append(lists, list)
	}
	var mu sync.Mutex
	seen := map[string][]string{}
	err = p.Serial(lists, func(c Cut) {
		mu.Lock()
		defer mu.Unlock()
		// A failure does not stop the list.
		assert.Equal(t, errors.Is(c.Err, cut.ErrNotAvailable), c.Slice == "dash_probe")
		seen[c.Arch] = append(seen[c.Arch], c.Slice)
	})
	assert.NoError(t, err)
	for _, arch := range []string{"arm64", "riscv64"} {
		assert.EqualArrays(t, seen[arch], []string{"bash_probe", "dash_probe", "zsh_probe"})
	}
	homes, err := os.ReadFile(os.Getenv("FAKE_HOMES"))
	assert.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(homes)), "\n")
	assert.Len(t, lines, 6)
	for _, l := range lines {
		assert.Equal(t, strings.Fields(l)[0], os.Getenv("XDG_CACHE_HOME"))
	}
}

func TestWith(t *testing.T) {
	f := setup(t)
	args := filepath.Join(f.tmp, "..", "args")
	t.Setenv("FAKE_ARGS", args)
	p, err := New(context.Background(), Options{Runner: f.runner})
	assert.NoError(t, err)
	err = p.Pass([]Group{{Lead: Unit{Release: "rel", Arch: "amd64", Slice: "bash_bins", With: []string{"bash_extra"}}}}, func(Cut) bool { return true })
	assert.NoError(t, err)
	p.Close()
	b, err := os.ReadFile(args)
	assert.NoError(t, err)
	assert.That(t, strings.HasSuffix(string(b), " bash_bins bash_extra\n"), string(b))
	// The cut is the unit's own slice's, not the one alongside.
	assert.EqualArrays(t, f.cuts(t), []string{"amd64 bash_bins rel"})
}

func TestKeepAndDir(t *testing.T) {
	f := setup(t)
	var log strings.Builder
	p, err := New(context.Background(), Options{Runner: f.runner, Keep: true, Temp: "pool-test-*", Log: &log})
	assert.NoError(t, err)
	err = p.Pass([]Group{
		{Lead: Unit{Release: "rel", Arch: "amd64", Slice: "bash_bins", Dir: filepath.Join("bash", "amd64", "bins")}},
		{Lead: Unit{Release: "rel", Arch: "amd64", Slice: "dash_bins"}},
	}, func(Cut) bool { return true })
	assert.NoError(t, err)
	p.Close()
	line := strings.TrimSpace(log.String())
	assert.That(t, stamped.MatchString(line), line)
	work := strings.TrimPrefix(stamped.ReplaceAllString(line, ""), "rootfs dirs: ")
	defer os.RemoveAll(work)
	assert.That(t, strings.HasPrefix(filepath.Base(work), "pool-test-"), work)
	_, err = os.Stat(filepath.Join(work, "bash", "amd64", "bins", "from", "rel"))
	assert.NoError(t, err)
	// A unit that names no dir gets a numbered one.
	_, err = os.Stat(filepath.Join(work, "000001", "from", "rel"))
	assert.NoError(t, err)
}

func TestPassStopsWhenCancelled(t *testing.T) {
	f := setup(t)
	t.Setenv("FAKE_SLEEP", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	p, err := New(ctx, Options{Runner: f.runner, Jobs: 2})
	assert.NoError(t, err)
	defer p.Close()
	start := time.Now()
	var mu sync.Mutex
	failed := 0
	err = p.Pass([]Group{
		group("rel", "amd64", "bash_a", "bash_b", "bash_c"),
		group("rel", "amd64", "dash_a", "dash_b", "dash_c"),
	}, func(c Cut) bool {
		mu.Lock()
		defer mu.Unlock()
		if c.Err != nil {
			failed++
		}
		return c.Err == nil
	})
	assert.That(t, errors.Is(err, context.DeadlineExceeded))
	assert.That(t, time.Since(start) < 4*time.Second)
	// The cuts in flight were interrupted and nothing ran after them.
	assert.Equal(t, failed, 2)
	assert.That(t, len(f.cuts(t)) <= 2)
}

func TestFinishAndWorkers(t *testing.T) {
	s := time.Second
	// One free worker, the rest one seed apart: 16s of cuts over 4 workers
	// end at (16 + 2.5 + 5 + 7.5) / 4.
	assert.Equal(t, finish(16*s, 4, 1, 2500*time.Millisecond), 7750*time.Millisecond)
	// A worker that would start after the end does not count.
	assert.Equal(t, finish(2*s, 3, 1, 5*s), 2*s)
	// Pooled views are free.
	assert.Equal(t, finish(7*s, 4, 4, 2500*time.Millisecond), 1750*time.Millisecond)

	p := &Pool{jobs: 8, viewCost: 6 * s, seed: 2500 * time.Millisecond}
	assert.Equal(t, p.workers(16*s, 66), 4)
	assert.Equal(t, p.workers(2*s, 66), 1)
	p.views = make([]*cache.View, 3)
	assert.Equal(t, p.workers(7*s, 18), 4)
	p.viewCost = 0
	assert.Equal(t, p.workers(7*s, 18), 8)
	assert.Equal(t, p.workers(7*s, 3), 3)
}

func TestJobs(t *testing.T) {
	assert.Equal(t, Jobs(0), 1)
	assert.Equal(t, Jobs(1), 1)
	assert.Equal(t, Jobs(5), 5)
	assert.That(t, Jobs(-1) >= 1)
}

func TestResolveArches(t *testing.T) {
	host, err := deb.InferArch()
	assert.NoError(t, err)
	got, err := ResolveArches(nil)
	assert.NoError(t, err)
	assert.EqualArrays(t, got, []string{host})

	got, err = ResolveArches([]string{"arm64", "amd64", "arm64"})
	assert.NoError(t, err)
	assert.EqualArrays(t, got, []string{"arm64", "amd64"})

	got, err = ResolveArches([]string{"s390x", "all"})
	assert.NoError(t, err)
	assert.Equal(t, got[0], "s390x")
	all := slices.Clone(deb.Arches())
	sort.Strings(all)
	sort.Strings(got)
	assert.EqualArrays(t, got, all)

	_, err = ResolveArches([]string{"vax"})
	assert.Error(t, err, `invalid architecture "vax"`)
}
