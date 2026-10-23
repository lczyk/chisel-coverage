// Package cache gives each concurrently running chisel a private view of
// chisel's blob cache. chisel writes a missed blob to a fixed <digest>.tmp
// name with no locking, so two chisels missing the same blob at once corrupt
// each other (canonical/chisel#340). A view is a directory seeded with hard
// links to every blob of the shared cache; chisel runs with XDG_CACHE_HOME
// pointed at it, writes its misses there, and the new blobs are linked back
// into the shared cache afterwards. Blobs are content-addressed and chisel
// only ever renames a complete one into place, so sharing inodes is safe.
package cache

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
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
}

// Linking tens of thousands of blobs is syscall-bound; a few goroutines
// roughly halve the wall time on macOS and linux, more gain nothing.
const linkers = 4

// A blob's name is its digest in hex; anything else in a digest directory is
// a temp file or a lock.
var blob = regexp.MustCompile(`^[0-9a-f]+$`)

// New makes a view under <parent of shared>/chisel-slice-coverage, so on the
// same filesystem, seeded with every blob of shared. Views a crashed run left
// behind for more than a day are removed on the way.
func New(shared string) (*View, error) {
	base := filepath.Join(filepath.Dir(shared), "chisel-slice-coverage")
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, err
	}
	sweep(base)
	home, err := os.MkdirTemp(base, "view-*")
	if err != nil {
		return nil, err
	}
	v := &View{Home: home, shared: shared, dir: filepath.Join(home, "chisel"), known: map[string]bool{}}
	if err := v.seed(); err != nil {
		_ = os.RemoveAll(home)
		return nil, err
	}
	return v, nil
}

// seed hard-links every blob of the shared cache into the view. On another
// filesystem the first link fails and the view starts empty.
func (v *View) seed() error {
	var names []string
	kinds := map[string]bool{}
	err := eachBlob(v.shared, func(kind, name string) error {
		if !kinds[kind] {
			kinds[kind] = true
			if err := os.MkdirAll(filepath.Join(v.dir, kind), 0755); err != nil {
				return err
			}
		}
		names = append(names, filepath.Join(kind, name))
		return nil
	})
	if err != nil {
		return err
	}
	if len(names) > 0 {
		err := os.Link(filepath.Join(v.shared, names[0]), filepath.Join(v.dir, names[0]))
		if errors.Is(err, syscall.EXDEV) {
			v.copy = true
			return nil
		}
		if err != nil {
			return err
		}
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
				if err := os.Link(filepath.Join(v.shared, name), filepath.Join(v.dir, name)); err != nil {
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

// Merge publishes every blob the view gained since it was seeded or last
// merged into the shared cache. A blob already there is left alone, which is
// also how two views merging the same blob at once settle it.
func (v *View) Merge() error {
	return eachBlob(v.dir, func(kind, name string) error {
		if v.known[filepath.Join(kind, name)] {
			return nil
		}
		v.known[filepath.Join(kind, name)] = true
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
		return err
	})
}

// A hard link costs about this long on apfs and ext4, and a view is linked
// once and removed once, which is what Cost adds up.
const perBlob = 400 * time.Microsecond

// Cost estimates how long New and Remove take for a view of shared, so a
// caller can tell whether a worker has enough work to be worth one.
func Cost(shared string) time.Duration {
	n := 0
	_ = eachBlob(shared, func(_, _ string) error { n++; return nil })
	return max(time.Millisecond, time.Duration(n)*perBlob)
}

// Remove deletes the view. Blobs not merged are lost.
func (v *View) Remove() error {
	return os.RemoveAll(v.Home)
}

// eachBlob calls fn for every blob under root, as <digest kind dir>/<name>.
// A missing root is an empty cache.
func eachBlob(root string, fn func(kind, name string) error) error {
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
			if !e.Type().IsRegular() || !blob.MatchString(e.Name()) {
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

// sweep removes views under base that nothing has touched for a day.
func sweep(base string) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	stale := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !e.IsDir() || !info.ModTime().Before(stale) {
			continue
		}
		if ok, _ := filepath.Match("view-*", e.Name()); ok {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
}
