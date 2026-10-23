// Package cut runs one `chisel cut` and turns its outcome into data: the set of
// paths in the rootfs, the package version chisel logged, or a classified
// failure.
package cut

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Binary returns the chisel binary to run: $CHISEL, else "chisel" on PATH.
func Binary() string {
	if b := os.Getenv("CHISEL"); b != "" {
		return b
	}
	return "chisel"
}

// Runner runs chisel. Verbose, when set, receives every cut's output as it
// happens, each line as "<stamp> <arch> <slice> <text>" with chisel's own
// timestamp stripped. CacheHome, when set, is the XDG_CACHE_HOME chisel runs
// with, so a private view of the cache can stand in for the shared one.
type Runner struct {
	Bin       string
	Verbose   io.Writer
	CacheHome string
}

// Result is a successful cut.
type Result struct {
	Paths   []string // sorted; "/" excluded; directories end in "/"
	Bytes   int64    // of the regular files among them
	Version string   // from chisel's Fetching line, "" when not seen
	DebArch string   // "all" or the architecture, from the same line
}

// ErrNotAvailable means chisel found no such package for the architecture.
var ErrNotAvailable = errors.New("package not available for this architecture")

// ErrNoContent is the cause of an *Error when a path or glob of the slice
// matched nothing in the package, which chisel refuses.
var ErrNoContent = errors.New("a path of the slice matched nothing")

// Error is a cut that failed for any other reason. It carries what a user needs
// to re-run it by hand.
type Error struct {
	Args   []string
	Output string
	Err    error
}

func (e *Error) Error() string {
	out := strings.TrimRight(e.Output, "\n")
	if out != "" {
		out = "\n  " + strings.ReplaceAll(out, "\n", "\n  ")
	}
	return fmt.Sprintf("%s: %v%s", strings.Join(e.Args, " "), e.Err, out)
}

func (e *Error) Unwrap() error { return e.Err }

// The pool filename chisel logs when fetching a package. The archive drops the
// epoch from it, so the version here has none either.
var fetching = regexp.MustCompile(`Fetching (?:\S*/)?([^/_\s]+)_([^/_\s]+)_([^/_\s.]+)\.deb\.\.\.`)

// Cut runs "chisel cut" for slice (<package>_<name>) of release on arch into
// root, which it creates, and walks the result. Maintenance status is always
// ignored so an old or devel release still cuts. A missing package for the
// architecture is ErrNotAvailable; any other failure is an *Error, wrapping
// ErrNoContent when a path of the slice matched nothing. When ctx is done
// chisel is interrupted and the cut fails.
func (r *Runner) Cut(ctx context.Context, release, root, arch, slice string) (*Result, error) {
	if err := os.MkdirAll(root, 0755); err != nil {
		return nil, err
	}
	args := []string{"cut", "--release", release, "--root", root, "--arch", arch,
		"--ignore", "unmaintained", "--ignore", "unstable", slice}
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second
	if r.CacheHome != "" {
		cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+r.CacheHome)
	}
	var out bytes.Buffer
	var w io.Writer = &out
	if r.Verbose != nil {
		w = io.MultiWriter(&out, &prefixWriter{w: r.Verbose, prefix: arch + " " + slice + " "})
	}
	cmd.Stdout = w
	cmd.Stderr = w

	if err := cmd.Run(); err != nil {
		pkg, _, _ := strings.Cut(slice, "_")
		if strings.Contains(out.String(), fmt.Sprintf("cannot find package %q in archive", pkg)) {
			return nil, ErrNotAvailable
		}
		if strings.Contains(out.String(), fmt.Sprintf("cannot extract from package %q: no content at ", pkg)) {
			err = ErrNoContent
		}
		return nil, &Error{Args: append([]string{r.Bin}, args...), Output: out.String(), Err: err}
	}

	paths, bytes, err := Walk(root)
	if err != nil {
		return nil, err
	}
	res := &Result{Paths: paths, Bytes: bytes}
	pkg, _, _ := strings.Cut(slice, "_")
	for _, m := range fetching.FindAllStringSubmatch(out.String(), -1) {
		if m[1] == pkg {
			res.Version, res.DebArch = m[2], m[3]
			break
		}
	}
	return res, nil
}

// Walk lists every entry under root as an absolute path, sorted, with
// directories marked by a trailing slash, and adds up the regular files'
// sizes. root itself is not listed. Symlinks are entries, not followed.
func Walk(root string) ([]string, int64, error) {
	var paths []string
	var bytes int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		path := "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			path += "/"
		} else if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Strings(paths)
	return paths, bytes, nil
}

// Stamp is the timestamp every line the tool logs starts with. A cut's line
// is "<stamp> <arch> <slice> <text>", space-separated; a line about the run
// as a whole is "<stamp> <text>".
func Stamp() string {
	return time.Now().Format("06-01-02T15:04:05")
}

// The timestamp chisel's logger puts in front of every line.
var chiselStamp = regexp.MustCompile(`^\d{4}/\d\d/\d\d \d\d:\d\d:\d\d `)

// prefixWriter stamps and prefixes every line it is handed and drops chisel's
// timestamp. Workers write concurrently, so each line is written to the
// underlying writer in one call.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
	rest   []byte
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rest = append(p.rest, b...)
	for {
		i := bytes.IndexByte(p.rest, '\n')
		if i < 0 {
			return len(b), nil
		}
		line := chiselStamp.ReplaceAll(p.rest[:i+1], nil)
		if _, err := fmt.Fprintf(p.w, "%s %s%s", Stamp(), p.prefix, line); err != nil {
			return 0, err
		}
		p.rest = p.rest[i+1:]
	}
}
