// Package cache gives each concurrently running chisel a private view of
// chisel's blob cache. chisel writes a missed blob to a fixed <digest>.tmp
// name with no locking, so two chisels missing the same blob at once corrupt
// each other (canonical/chisel#340). A view is a directory seeded with hard
// links to every blob of the shared cache; chisel runs with XDG_CACHE_HOME
// pointed at it, writes its misses there, and the new blobs are linked back
// into the shared cache afterwards. Blobs are content-addressed and chisel
// only ever renames a complete one into place, so sharing inodes is safe.
//
// A view may also be seeded with the blobs used of late alone, see
// NewRecent, since linking every blob of a big cache costs seconds a view
// whatever the run needs of it. A blob such a view lacks is one chisel
// fetches again, by its digest, so the cut is the same; the view tells of
// it, and Complete links the rest.
//
// A view is kept when its run lets it go, see Release, and the next run
// takes it and links in only what the shared cache gained since: seeding a
// view is the cost of a few cuts, bringing one up to date next to nothing.
// A lock on a file of the view says it is taken, held for as long as a run,
// or a chisel it left behind, works in it. How runs come by kept views and
// sweep the ones no run wants is modelled in kept.fizz.
package cache

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Dir is where the chisel binary keeps its cache, by chisel's own rule:
// $XDG_CACHE_HOME/chisel, else $HOME/.cache/chisel. It is "" when neither
// variable is set; chisel then uses a fresh temp dir per run and there is
// nothing to share.
func Dir() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return filepath.Join(d, "chisel")
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".cache", "chisel")
	}
	return ""
}

// View is a private copy of the shared cache for one chisel at a time.
type View struct {
	Home   string // the XDG_CACHE_HOME to run chisel with
	shared string
	dir    string          // Home/chisel
	copy   bool            // shared is on another filesystem: no links, merge by copying
	known  map[string]bool // <kind>/<name> seeded or merged already
	// rest are the blobs of shared the view was not seeded with, by
	// <kind>/<name>; refetched how many of them chisel fetched again.
	rest      map[string]bool
	refetched int
	lock      *os.File // held while the view is taken; nil where views are not kept
}

// Linking tens of thousands of blobs is syscall-bound; a few goroutines
// roughly halve the wall time on macOS and linux, more gain nothing.
const linkers = 4

// A blob's name is its digest in hex; anything else in a digest directory is
// a temp file or a lock.
var blob = regexp.MustCompile(`^[0-9a-f]+$`)

// The names of the dirs under <parent of shared>/chisel-coverage: a kept
// view, one being made, and one being removed. A view-* dir is a view of a
// release that kept none, swept as that release swept them.
const (
	keptPrefix  = "kept-"
	newPrefix   = "new-"
	trashPrefix = "trash-"
	lockName    = "lock"
)

// A kept view no run has taken for this long is removed by the next run.
const keepFor = 24 * time.Hour

// New gives a view under <parent of shared>/chisel-coverage, so on the
// same filesystem, with every blob of shared: a kept one no run has, brought
// up to date, else one made new. Views no run has taken for a day are
// removed on the way.
func New(shared string) (*View, error) { return NewRecent(shared, 0) }

// NewRecent is New, but a view made new is seeded only with the blobs of
// shared used within the window, and a kept one gains only those: chisel
// touches a blob whenever it takes it from its cache. A window of zero is
// every blob.
func NewRecent(shared string, window time.Duration) (*View, error) {
	base := filepath.Join(filepath.Dir(shared), "chisel-coverage")
	// Next to shared, not to Dir(): shared may be a test's own dir rather
	// than chisel's.
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, err
	}
	sweep(base)
	home, lock, err := take(base)
	if err != nil {
		return nil, err
	}
	v := &View{Home: home, shared: shared, dir: filepath.Join(home, "chisel"), known: map[string]bool{}, rest: map[string]bool{}, lock: lock}
	if err := v.seed(window); err != nil {
		_ = v.Remove()
		return nil, err
	}
	return v, nil
}

// take is the home of a view no other run has, and the lock that says so:
// a kept one, else one made new. Where views are not kept the lock is nil.
func take(base string) (home string, lock *os.File, err error) {
	if !keeps {
		home, err = os.MkdirTemp(base, "view-*")
		return home, nil, err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), keptPrefix) {
			continue
		}
		home := filepath.Join(base, e.Name())
		if lock := lockKept(home); lock != nil {
			now := time.Now()
			_ = os.Chtimes(home, now, now)
			return home, lock, nil
		}
	}
	// Made under a name no run lists, locked there, then renamed: a kept
	// view is never seen without its lock held or free to take.
	tmp, err := os.MkdirTemp(base, newPrefix+"*")
	if err != nil {
		return "", nil, err
	}
	lock, err = os.OpenFile(filepath.Join(tmp, lockName), os.O_RDWR|os.O_CREATE, 0644)
	if err == nil {
		if !tryLock(lock) {
			err = errors.New("cannot lock a new cache view")
		}
	}
	home = filepath.Join(base, keptPrefix+strings.TrimPrefix(filepath.Base(tmp), newPrefix))
	if err == nil {
		err = os.Rename(tmp, home)
	}
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		_ = os.RemoveAll(tmp)
		return "", nil, err
	}
	return home, lock, nil
}

// lockKept takes the lock of the kept view at home, nil when another run
// has it or the view is gone. The lock is got on the file as it was when
// opened, and the view may have been swept between the two: only a lock on
// the file still at its place is a lock on the view.
func lockKept(home string) *os.File {
	p := filepath.Join(home, lockName)
	lock, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	if tryLock(lock) {
		held, err1 := lock.Stat()
		there, err2 := os.Stat(p)
		if err1 == nil && err2 == nil && os.SameFile(held, there) {
			return lock
		}
	}
	_ = lock.Close()
	return nil
}

// seed brings the view up to date with the shared cache: every blob of it
// hard-linked in, or with a window those used within it, on top of what a
// kept view has. What the view has and the shared cache does not goes: the
// shared cache is what holds, and a blob taken out of it is not put back
// by a view. On another filesystem the first link fails and a view made
// new starts empty.
func (v *View) seed(window time.Duration) error {
	has := map[string]bool{}
	err := eachFile(v.dir, func(kind, name string) error {
		if blob.MatchString(name) {
			has[filepath.Join(kind, name)] = true
			return nil
		}
		// What a chisel that died left half written.
		return os.Remove(filepath.Join(v.dir, kind, name))
	})
	if err != nil {
		return err
	}
	var names []string
	kinds := map[string]bool{}
	since := time.Now().Add(-window)
	err = eachBlob(v.shared, func(kind, name string) error {
		if !kinds[kind] {
			kinds[kind] = true
			if err := os.MkdirAll(filepath.Join(v.dir, kind), 0755); err != nil {
				return err
			}
		}
		if has[filepath.Join(kind, name)] {
			delete(has, filepath.Join(kind, name))
			v.known[filepath.Join(kind, name)] = true
			return nil
		}
		if window > 0 {
			// A blob gone since it was listed is one to do without.
			info, err := os.Lstat(filepath.Join(v.shared, kind, name))
			if err != nil || info.ModTime().Before(since) {
				v.rest[filepath.Join(kind, name)] = true
				return nil
			}
		}
		names = append(names, filepath.Join(kind, name))
		return nil
	})
	if err != nil {
		return err
	}
	for name := range has {
		if err := os.Remove(filepath.Join(v.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return v.link(names)
}

// link hard-links the named blobs of the shared cache into the view. One
// the view has already, fetched by chisel since, is left as it is.
func (v *View) link(names []string) error {
	if len(names) > 0 {
		err := os.Link(filepath.Join(v.shared, names[0]), filepath.Join(v.dir, names[0]))
		if errors.Is(err, syscall.EXDEV) {
			v.copy = true
			return nil
		}
		if err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		v.known[names[0]] = true
		names = names[1:]
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	next := make(chan string)
	for i := 0; i < linkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for name := range next {
				// One chisel has fetched since is there already, and one
				// chisel has expired from the shared cache is not to be had.
				if err := os.Link(filepath.Join(v.shared, name), filepath.Join(v.dir, name)); err != nil && !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist) {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		}()
	}
	for _, name := range names {
		next <- name
	}
	close(next)
	wg.Wait()
	for _, name := range names {
		v.known[name] = true
	}
	return first
}

// Blobs is how many blobs the view holds, seeded or merged.
func (v *View) Blobs() int { return len(v.known) }

// Partial reports whether the shared cache has blobs the view was not
// seeded with.
func (v *View) Partial() bool { return len(v.rest) > 0 }

// Refetched is how many blobs chisel fetched into the view that the shared
// cache had all along, as the merges so far found: what seeding the view in
// part has cost.
func (v *View) Refetched() int { return v.refetched }

// Complete links in every blob the view was not seeded with, so it is as
// one New made. Not while a chisel runs in the view.
func (v *View) Complete() error {
	if v.copy || len(v.rest) == 0 {
		v.rest = map[string]bool{}
		return nil
	}
	names := make([]string, 0, len(v.rest))
	for name := range v.rest {
		names = append(names, name)
	}
	v.rest = map[string]bool{}
	return v.link(names)
}

// Merge publishes every blob the view gained since it was seeded or last
// merged into the shared cache. A blob already there is left alone, which is
// also how two views merging the same blob at once settle it.
func (v *View) Merge() error {
	return eachBlob(v.dir, func(kind, name string) error {
		if v.known[filepath.Join(kind, name)] {
			return nil
		}
		v.known[filepath.Join(kind, name)] = true
		if v.rest[filepath.Join(kind, name)] {
			delete(v.rest, filepath.Join(kind, name))
			v.refetched++
		}
		dst := filepath.Join(v.shared, kind, name)
		if _, err := os.Lstat(dst); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		src := filepath.Join(v.dir, kind, name)
		if v.copy {
			return copyFile(src, dst)
		}
		err := os.Link(src, dst)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		if errors.Is(err, syscall.EXDEV) {
			// A view seeded with nothing never tried a link before.
			v.copy = true
			return copyFile(src, dst)
		}
		return err
	})
}

// A hard link costs about this long on apfs and ext4, which is what a view
// made new costs a blob. A kept view is listed and compared, far less a
// blob, and gains the few the shared cache did.
const (
	perBlob     = 400 * time.Microsecond
	perKeptBlob = 10 * time.Microsecond
)

// Cost estimates how long New takes for a view of shared, so a caller can
// tell whether a worker has enough work to be worth one.
func Cost(shared string) time.Duration { return CostRecent(shared, 0) }

// CostRecent is Cost for a view NewRecent gives with the window. With a
// kept view to be had it is the cost of taking one, though the views kept
// may be fewer than the caller comes to ask for.
func CostRecent(shared string, window time.Duration) time.Duration {
	n, all := 0, 0
	since := time.Now().Add(-window)
	_ = eachBlob(shared, func(kind, name string) error {
		all++
		if window > 0 {
			if info, err := os.Lstat(filepath.Join(shared, kind, name)); err != nil || info.ModTime().Before(since) {
				return nil
			}
		}
		n++
		return nil
	})
	if kept(filepath.Join(filepath.Dir(shared), "chisel-coverage")) {
		return max(time.Millisecond, time.Duration(all)*perKeptBlob)
	}
	return max(time.Millisecond, time.Duration(n)*perBlob)
}

// kept reports whether base holds a kept view, taken or not.
func kept(base string) bool {
	entries, _ := os.ReadDir(base)
	for _, e := range entries {
		if keeps && e.IsDir() && strings.HasPrefix(e.Name(), keptPrefix) {
			return true
		}
	}
	return false
}

// Remove deletes the view. Blobs not merged are lost.
func (v *View) Remove() error {
	err := os.RemoveAll(v.Home)
	v.unlock()
	return err
}

// Release lets the view go for another run to take, as it is: blobs not
// merged are lost to the shared cache until a run fetches them again. Not
// while a chisel runs in the view. Where views are not kept it is Remove.
func (v *View) Release() error {
	if v.lock == nil {
		return v.Remove()
	}
	now := time.Now()
	_ = os.Chtimes(v.Home, now, now)
	v.unlock()
	return nil
}

// Lock is the open file whose lock says the view is taken, nil where views
// are not kept. A chisel given it to inherit holds the view for as long as
// it runs, should the run that started it die first.
func (v *View) Lock() *os.File { return v.lock }

func (v *View) unlock() {
	if v.lock != nil {
		_ = v.lock.Close()
		v.lock = nil
	}
}

// eachBlob calls fn for every blob under root, as <digest kind dir>/<name>.
// A missing root is an empty cache.
func eachBlob(root string, fn func(kind, name string) error) error {
	return eachFile(root, func(kind, name string) error {
		if !blob.MatchString(name) {
			return nil
		}
		return fn(kind, name)
	})
}

// eachFile calls fn for every regular file of a digest kind dir under root.
func eachFile(root string, fn func(kind, name string) error) error {
	kinds, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, k := range kinds {
		if !k.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, k.Name()))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			if err := fn(k.Name(), e.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

// copyFile writes src to dst through a temp file in dst's directory, so dst
// is never seen incomplete.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".view.*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// sweep removes the views under base that no run has taken for a day, and
// what a run that died left of making or removing one.
func sweep(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	stale := func(p string) bool {
		info, err := os.Lstat(p)
		return err == nil && info.ModTime().Before(time.Now().Add(-keepFor))
	}
	for _, e := range entries {
		p := filepath.Join(base, e.Name())
		if !e.IsDir() || !stale(p) {
			continue
		}
		switch {
		case strings.HasPrefix(e.Name(), "view-"), strings.HasPrefix(e.Name(), newPrefix), strings.HasPrefix(e.Name(), trashPrefix):
			_ = os.RemoveAll(p)
		case strings.HasPrefix(e.Name(), keptPrefix):
			// Only with its lock held, so no run has it, and still
			// stale then, so none took and let go of it meanwhile. It
			// leaves its name in one step: a run that opened its lock
			// before then finds the file no longer at its place.
			lock := lockKept(p)
			if lock == nil {
				continue
			}
			trash := filepath.Join(base, trashPrefix+strings.TrimPrefix(e.Name(), keptPrefix))
			if stale(p) && os.Rename(p, trash) == nil {
				_ = os.RemoveAll(trash)
			}
			_ = lock.Close()
		}
	}
}
