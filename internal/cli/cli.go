// Package cli parses arguments, runs the coverage and renders the report.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	goflags "github.com/jessevdk/go-flags"
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
		_, _, _ = parseArgs([]string{"--help"}, os.Stderr)
		return 2
	}
	switch args[0] {
	case "version":
		args = []string{"--version"}
	case "help":
		args = []string{"--help"}
	}

	f, pkgs, err := parseArgs(args, os.Stdout)
	if err != nil {
		errf(err)
		return 2
	}
	if f.help {
		return 0
	}
	if f.Version {
		fmt.Println(ver.Read(fileVersion(chiselslicecoverage.VersionFile)))
		return 0
	}
	if len(pkgs) == 0 {
		errf(fmt.Errorf("no packages given"))
		return 2
	}
	arches, err := resolveArches(f.Arches)
	if err != nil {
		errf(err)
		return 2
	}

	runner := &cut.Runner{Bin: cut.Binary()}
	opts := coverage.Options{
		Release: f.Release,
		Arches:  arches,
		Only:    f.Slices,
		Ignores: f.Ignores,
		Keep:    f.Keep,
		Runner:  runner,
		Log:     os.Stderr,
	}
	if len(f.Verbose) >= 1 {
		opts.Verbose = os.Stderr
	}
	if len(f.Verbose) >= 2 {
		runner.Verbose = os.Stderr
	}
	results, err := coverage.Run(opts, pkgs)
	if err != nil {
		errf(err)
		return 2
	}

	if f.JSON {
		if err := renderJSON(os.Stdout, results); err != nil {
			errf(err)
			return 2
		}
	} else {
		renderText(os.Stdout, results, !f.NoGroup, styleFor(os.Stdout))
	}

	code := 0
	for _, p := range results {
		for _, a := range p.Arches {
			if a.Err != nil {
				return 2
			}
			if f.FailUncovered && len(a.Uncovered) > 0 {
				code = 1
			}
		}
	}
	return code
}

// flags is the command line; go-flags fills it and renders the help from it.
type flags struct {
	Release       string   `long:"release" value-name:"<dir>" default:"." description:"chisel-releases checkout"`
	Arches        []string `long:"arch" value-name:"<a>[,<a>...]" description:"architectures to check; \"all\" for every one (default: host)"`
	Slices        []string `long:"slice" value-name:"<s>[,<s>...]" description:"only these slices, bare or <package>_<slice> (default: all)"`
	Ignores       []string `long:"ignore" value-name:"<glob>[,...]" description:"leave matching paths out of the uncovered set (chisel glob syntax)"`
	NoGroup       bool     `long:"no-group" description:"one block per architecture with its real paths, instead of folding architectures with the same result"`
	JSON          bool     `long:"json" description:"machine-readable output, one entry per architecture"`
	FailUncovered bool     `long:"fail-uncovered" description:"exit 1 when any architecture has uncovered paths"`
	Keep          bool     `long:"keep" description:"keep the temp releases and rootfs dirs"`
	Verbose       []bool   `short:"v" long:"verbose" description:"one line per cut on stderr; twice also streams chisel's output"`
	Version       bool     `long:"version" description:"print the version and exit"`

	help bool // -h or --help was given
}

func newParser(f *flags) *goflags.Parser {
	p := goflags.NewNamedParser("chisel-slice-coverage", goflags.HelpFlag|goflags.PassDoubleDash)
	p.Usage = "[flags] <package>..."
	p.LongDescription = strings.TrimSpace(`
for each package, every slice is cut on its own against a throwaway copy of
the release with only that package in it, and so is one slice taking the whole
.deb. what the whole-deb cut has and no slice cut has is uncovered. the
"chisel" binary is found on PATH, or set CHISEL=/path/to/chisel. "version"
and "help" as the first argument are --version and --help.`)
	if _, err := p.AddGroup("flags", "", f); err != nil {
		panic(err)
	}
	return p
}

// parseArgs parses the command line. List flags take comma-separated values
// and may repeat; -h, --help and --version work wherever they land on the
// line. Help is not an error: the usage goes to help, f.help is set and the
// caller stops. The usage is written by the parser that ran, since go-flags
// only renders defaults once it has parsed.
func parseArgs(args []string, help io.Writer) (f flags, pkgs []string, err error) {
	p := newParser(&f)
	pkgs, err = p.ParseArgs(args)
	if err != nil {
		var fe *goflags.Error
		if errors.As(err, &fe) && fe.Type == goflags.ErrHelp {
			p.WriteHelp(help)
			f.help = true
			return f, nil, nil
		}
		return f, nil, err
	}
	for _, l := range []*[]string{&f.Arches, &f.Slices, &f.Ignores} {
		*l = splitLists(*l)
	}
	return f, pkgs, nil
}

// splitLists splits every value of a repeatable list flag on commas and drops
// the blanks, so --arch amd64,arm64 and --arch amd64 --arch arm64 are the same.
func splitLists(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
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
