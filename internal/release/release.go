// Package release resolves the --release argument: a chisel-releases
// checkout is used as it is, and a release name such as ubuntu-24.04 is
// fetched the way chisel fetches it, as the tarball of the branch of that
// name on github, and cached.
package release

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The branch tarball of a release, as chisel downloads it.
var baseURL = "https://codeload.github.com/canonical/chisel-releases/tar.gz/refs/heads/"

var client = &http.Client{Timeout: 5 * time.Minute}

// A release name is <label>-<version>, as in ubuntu-24.04.
var name = regexp.MustCompile(`^[a-z]+-[0-9]+\.[0-9]+$`)

// Resolve returns the directory holding the release arg names. An existing
// directory is returned as it is. A release name is fetched into
// <cacheDir>/releases/<name>, refreshed when github has a newer branch, and
// the cached copy serves when github cannot be reached; cacheDir "" means
// the system temp dir. Notices about fetching go to log when it is not nil.
func Resolve(ctx context.Context, arg, cacheDir string, log func(format string, args ...any)) (string, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return arg, nil
	}
	if !name.MatchString(arg) {
		return "", fmt.Errorf("%s is not a directory or a release name like ubuntu-24.04", arg)
	}
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "chisel-slice-coverage")
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	return fetch(ctx, arg, filepath.Join(cacheDir, "releases"), log)
}

// fetch brings <releases>/<name> up to date with github. The releases dir is
// locked meanwhile, since two runs may want the same release at once.
func fetch(ctx context.Context, name, releases string, log func(string, ...any)) (string, error) {
	if err := os.MkdirAll(releases, 0755); err != nil {
		return "", err
	}
	unlock, err := lock(filepath.Join(releases, ".lock"))
	if err != nil {
		return "", err
	}
	defer unlock()

	dir := filepath.Join(releases, name)
	cached := false
	if _, err := os.Stat(filepath.Join(dir, "chisel.yaml")); err == nil {
		cached = true
	}
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+name, nil)
	if err != nil {
		return "", err
	}
	if cached {
		if tag, err := os.ReadFile(filepath.Join(dir, ".etag")); err == nil {
			req.Header.Set("If-None-Match", string(tag))
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		if cached && ctx.Err() == nil {
			log("release %s: cannot reach github (%v); using the cached copy", name, err)
			return dir, nil
		}
		return "", fmt.Errorf("cannot fetch release %s: %w", name, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return dir, nil
	case http.StatusNotFound, http.StatusUnauthorized:
		return "", fmt.Errorf("no such release %s", name)
	default:
		return "", fmt.Errorf("cannot fetch release %s: %s", name, resp.Status)
	}

	log("release %s: fetching from github", name)
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := extract(resp.Body, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("cannot fetch release %s: %w", name, err)
	}
	if tag := resp.Header.Get("ETag"); tag != "" {
		if err := os.WriteFile(filepath.Join(tmp, ".etag"), []byte(tag), 0644); err != nil {
			return "", err
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// extract unpacks a github branch tarball into dir, dropping the top-level
// directory the archive wraps everything in. Only directories and regular
// files are taken, which is all a release holds.
func extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		_, rest, ok := strings.Cut(h.Name, "/")
		if !ok || rest == "" {
			continue
		}
		rel := filepath.Clean(rest)
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
			return fmt.Errorf("tarball entry %q escapes the release", h.Name)
		}
		path := filepath.Join(dir, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		}
	}
}

// lock takes an exclusive flock on path and returns the release of it.
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
