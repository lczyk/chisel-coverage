// Package release resolves the --release argument: a chisel-releases
// checkout is used as it is, and a release name such as ubuntu-24.04 is
// fetched by chisel itself and read from chisel's own cache.
package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// A release name as chisel parses it: <label>-<version>, as in ubuntu-24.04.
var name = regexp.MustCompile(`^[a-z](?:-?[a-z0-9]){2,}-[0-9]+(?:\.?[0-9])+$`)

// Resolve returns the directory holding the release arg names. An existing
// directory is returned as it is. A release name is handed to fetch, which
// has chisel fetch or refresh it, and the copy chisel keeps under
// <chiselCache>/releases/<name> is returned. When chisel cannot fetch it and
// that copy exists, the copy serves and log says so.
func Resolve(ctx context.Context, arg, chiselCache string, fetch func(ctx context.Context, name string) error, log func(format string, args ...any)) (string, error) {
	if info, err := os.Stat(arg); err == nil && info.IsDir() {
		return arg, nil
	}
	if !name.MatchString(arg) {
		return "", fmt.Errorf("%s is not a directory or a release name like ubuntu-24.04", arg)
	}
	if chiselCache == "" {
		return "", fmt.Errorf("cannot find chisel's cache: neither XDG_CACHE_HOME nor HOME is set")
	}
	if log == nil {
		log = func(string, ...any) {}
	}
	dir := filepath.Join(chiselCache, "releases", arg)
	if err := fetch(ctx, arg); err != nil {
		if cached(dir) && ctx.Err() == nil {
			log("release %s: chisel cannot refresh it; using its cached copy\n  %v", arg, err)
			return dir, nil
		}
		return "", err
	}
	if !cached(dir) {
		return "", fmt.Errorf("chisel fetched release %s but left nothing at %s", arg, dir)
	}
	return dir, nil
}

func cached(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "chisel.yaml"))
	return err == nil
}
