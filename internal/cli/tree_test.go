package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/lczyk/assert"

	"github.com/lczyk/chisel-coverage/internal/coverage"
)

// boxes spells the tree glyphs in ascii in a test's expected text: "T- ",
// "L- " and "|  " for the three the tree draws.
func boxes(s string) string {
	return strings.NewReplacer("T- ", "\u251c\u2500 ", "L- ", "\u2514\u2500 ", "|  ", "\u2502  ").Replace(s)
}

func TestRenderTree(t *testing.T) {
	var out bytes.Buffer
	renderText(&out, sample(), true, true, style{})
	assert.Equal(t, out.String(), boxes(`bash 5.2-1 [amd64, arm64]
total 6 covered 3 ignored 1 uncovered 2
/usr/lib/
L- <arch>-linux-<gnu>/
bash [i386]: not available
bash [s390x]: error
  chisel cut ... exited 1
    boom
tzdata 2026a-1 (all) [amd64]
total 1 covered 1 ignored 0 uncovered 0
empty [amd64]
total 0 covered 0 ignored 0 uncovered 0
`))

	// The glyphs are dim, the rest as the flat report colours it.
	out.Reset()
	renderText(&out, sample()[:1], true, true, colours)
	assert.ContainsString(t, out.String(), "\x1b[31muncovered 2\x1b[0m\n/usr/lib/\n"+
		boxes("\x1b[2mL- \x1b[0m<arch>-linux-<gnu>/\n"))
}

// Directories with nothing uncovered of their own are drawn to hold what
// is, a run of them as one line, and an uncovered directory is a line of
// its own over its content: /usr/share/menu/ here.
func TestRenderTreeHolders(t *testing.T) {
	var out bytes.Buffer
	renderGroup(&out, coverage.Package{Name: "bash"}, coverage.Group{
		Arches: []string{"amd64"}, Available: true, Version: "5.2-1", DebArch: "amd64", Total: 29,
		Covered: make([]string, 23), Ignored: []string{},
		Uncovered: []string{
			"/usr/share/doc/bash/NEWS.gz",
			"/usr/share/doc/bash/README.gz",
			"/usr/share/man/man1/bash.1.gz",
			"/usr/share/man/man1/rbash.1.gz",
			"/usr/share/menu/",
			"/usr/share/menu/bash",
		},
	}, true, style{})
	assert.Equal(t, out.String(), boxes(`bash 5.2-1 [amd64]
total 29 covered 23 ignored 0 uncovered 6
/usr/share/
T- doc/bash/
|  T- NEWS.gz
|  L- README.gz
T- man/man1/
|  T- bash.1.gz
|  L- rbash.1.gz
L- menu/
   L- bash
`))
}

// Architectures fold first and each block is a tree of its own paths. What
// only amd64 has is drawn under a directory to hold it, and a path alone in
// its block is one line, as in the flat report.
func TestRenderTreeFolded(t *testing.T) {
	pkgs := sample()[:1]
	pkgs[0].Arches = []coverage.Arch{pkgs[0].Arches[0], pkgs[0].Arches[2]}
	amd64 := &pkgs[0].Arches[0]
	amd64.Uncovered = append(amd64.Uncovered, "/usr/lib/x86_64-linux-gnu/bash/a", "/usr/lib/x86_64-linux-gnu/bash/b")
	amd64.Total = 8
	var out bytes.Buffer
	renderText(&out, pkgs, true, true, style{})
	assert.Equal(t, out.String(), boxes(`bash 5.2-1 [amd64, arm64]
total 6 covered 3 ignored 1 uncovered 2
/usr/lib/
L- <arch>-linux-<gnu>/
bash 5.2-1 [amd64]
total 2 covered 0 ignored 0 uncovered 2
/usr/lib/x86_64-linux-gnu/bash/
T- a
L- b
`))

	amd64.Uncovered = amd64.Uncovered[:3]
	amd64.Total = 7
	out.Reset()
	renderText(&out, pkgs, true, true, style{})
	assert.ContainsString(t, out.String(), "bash 5.2-1 [amd64]\ntotal 1 covered 0 ignored 0 uncovered 1\n/usr/lib/x86_64-linux-gnu/bash/a\n")
}

// Two uncovered paths that fold to one shown path are two lines, with what
// is under them drawn under the last.
func TestRenderTreeTwice(t *testing.T) {
	var out bytes.Buffer
	renderGroup(&out, coverage.Package{Name: "libc6"}, coverage.Group{
		Arches: []string{"amd64", "i386"}, Available: true, Version: "2.39-1", DebArch: "amd64", Total: 3,
		Covered: []string{}, Ignored: []string{},
		Uncovered: []string{
			"/usr/lib/<arch>-linux-<gnu>/",
			"/usr/lib/<arch>-linux-<gnu>/",
			"/usr/lib/<arch>-linux-<gnu>/x",
		},
	}, true, style{})
	assert.Equal(t, out.String(), boxes(`libc6 2.39-1 [amd64, i386]
total 3 covered 0 ignored 0 uncovered 3
/usr/lib/<arch>-linux-<gnu>/
/usr/lib/<arch>-linux-<gnu>/
L- x
`))
}

func TestParseArgsTree(t *testing.T) {
	f, _, err := parseArgs([]string{"bash"}, io.Discard)
	assert.NoError(t, err)
	assert.That(t, !f.Tree)
	for _, flag := range []string{"-T", "--tree"} {
		f, _, err = parseArgs([]string{flag, "bash"}, io.Discard)
		assert.NoError(t, err)
		assert.That(t, f.Tree, flag)
	}
	_, _, err = parseArgs([]string{"--tree", "--json", "bash"}, io.Discard)
	assert.Error(t, err, "--tree draws the text report, so it does not go with --json")
}
