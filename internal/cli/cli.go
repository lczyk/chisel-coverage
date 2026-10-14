// Package cli parses arguments, runs the coverage and renders the report.
package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lczyk/chisel-proxy/pkg/deb"
	ver "github.com/lczyk/version/go"

	chiselslicecoverage "github.com/lczyk/chisel-slice-coverage"
	"github.com/lczyk/chisel-slice-coverage/internal/coverage"
	"github.com/lczyk/chisel-slice-coverage/internal/cut"
)

// Main runs the CLI and returns a process exit code: 0 ran fine, 1 uncovered
// paths with --fail-uncovered, 2 a usage error or a failed cut.
func Main(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(ver.Read(fileVersion(chiselslicecoverage.VersionFile)))
		return 0
	case "help", "--help", "-h":
		usage(os.Stdout)
		return 0
	}

	f, pkgs, err := parseArgs(args)
	if err != nil {
		errf(err)
		return 2
	}
	if len(pkgs) == 0 {
		errf(fmt.Errorf("no packages given"))
		return 2
	}
	arches, err := resolveArches(f.arches)
	if err != nil {
		errf(err)
		return 2
	}

	runner := &cut.Runner{Bin: cut.Binary()}
	if f.verbose {
		runner.Verbose = os.Stderr
	}
	results, err := coverage.Run(coverage.Options{
		Release: f.release,
		Arches:  arches,
		Only:    f.slices,
		Ignores: f.ignores,
		Keep:    f.keep,
		Runner:  runner,
		Log:     os.Stderr,
	}, pkgs)
	if err != nil {
		errf(err)
		return 2
	}

	if f.json {
		if err := renderJSON(os.Stdout, results); err != nil {
			errf(err)
			return 2
		}
	} else {
		renderText(os.Stdout, results)
	}

	code := 0
	for _, p := range results {
		for _, a := range p.Arches {
			if a.Err != nil {
				return 2
			}
			if f.failUncovered && len(a.Uncovered) > 0 {
				code = 1
			}
		}
	}
	return code
}

type flags struct {
	release       string
	arches        []string
	slices        []string
	ignores       []string
	json          bool
	failUncovered bool
	keep          bool
	verbose       bool
}

// parseArgs pulls recognised flags and the package positionals out of args.
// List flags take comma-separated values and may repeat.
func parseArgs(args []string) (f flags, pkgs []string, err error) {
	f.release = "."
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-v" {
			f.verbose = true
			i++
			continue
		}
		if !strings.HasPrefix(a, "--") {
			pkgs = append(pkgs, a)
			i++
			continue
		}
		key := a[2:]
		val := ""
		hasVal := false
		if j := strings.IndexByte(key, '='); j >= 0 {
			val, key, hasVal = key[j+1:], key[:j], true
		}
		need := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag --%s needs a value", key)
			}
			i++
			return args[i], nil
		}
		list := func(dst *[]string) error {
			v, err := need()
			if err != nil {
				return err
			}
			for _, s := range strings.Split(v, ",") {
				if s = strings.TrimSpace(s); s != "" {
					*dst = append(*dst, s)
				}
			}
			return nil
		}
		switch key {
		case "release":
			if f.release, err = need(); err != nil {
				return f, nil, err
			}
		case "arch":
			err = list(&f.arches)
		case "slice":
			err = list(&f.slices)
		case "ignore":
			err = list(&f.ignores)
		case "json":
			f.json = true
		case "fail-uncovered":
			f.failUncovered = true
		case "keep":
			f.keep = true
		case "verbose":
			f.verbose = true
		default:
			return f, nil, fmt.Errorf("unknown flag --%s", key)
		}
		if err != nil {
			return f, nil, err
		}
		i++
	}
	return f, pkgs, nil
}

// resolveArches expands "all", defaults to the host and keeps the order given,
// first occurrence wins.
func resolveArches(given []string) ([]string, error) {
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
			for _, x := range coverage.AllArches {
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

// fileVersion returns the version line of a VERSION file: the first line that
// is neither blank nor a # comment.
func fileVersion(content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}

func errf(err error) {
	fmt.Fprintf(os.Stderr, "chisel-slice-coverage: %v\n", err)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `chisel-slice-coverage - which paths of a .deb do its slices leave out

usage:
  chisel-slice-coverage [flags] <package>...
  chisel-slice-coverage version

flags:
  --release <dir>        chisel-releases checkout (default ".")
  --arch <a>[,<a>...]    architectures to check; "all" for every one (default: host)
  --slice <s>[,<s>...]   only these slices, bare or <package>_<slice> (default: all)
  --ignore <glob>[,...]  leave matching paths out of the uncovered set (chisel glob syntax)
  --json                 machine-readable output, one entry per architecture
  --fail-uncovered       exit 1 when any architecture has uncovered paths
  --keep                 keep the temp releases and rootfs dirs
  -v                     stream chisel's output

for each package, every slice is cut on its own against a throwaway copy of
the release with only that package in it, and so is one slice taking the whole
.deb. what the whole-deb cut has and no slice cut has is uncovered. the
"chisel" binary is found on PATH, or set CHISEL=/path/to/chisel.
`)
}
