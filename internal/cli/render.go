package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lczyk/chisel-slice-coverage/internal/coverage"
)

// style colours the text report. The zero value colours nothing.
type style struct {
	name, dim, red, green, yellow, reset string
}

// colours is the style for a terminal: a little, not a lot.
var colours = style{name: "\x1b[1;34m", dim: "\x1b[2m", red: "\x1b[31m", green: "\x1b[32m", yellow: "\x1b[33m", reset: "\x1b[0m"}

// styleFor picks colours when w is a terminal and NO_COLOR is unset or empty
// (https://no-color.org), else nothing.
func styleFor(w io.Writer) style {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return style{}
	}
	f, ok := w.(*os.File)
	if !ok {
		return style{}
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return style{}
	}
	return colours
}

func (s style) wrap(code, text string) string {
	if code == "" {
		return text
	}
	return code + text + s.reset
}

// count renders "<label> <n>" green when n is what one hopes for (non-zero
// for covered, zero for ignored and uncovered), else warn (yellow for
// covered and ignored, red for uncovered).
func (s style) count(label string, n int, wantZero bool, warn string) string {
	text := fmt.Sprintf("%s %d", label, n)
	if (n == 0) == wantZero {
		return s.wrap(s.green, text)
	}
	return s.wrap(warn, text)
}

// renderText prints one block per group of architectures with the same
// result (or per architecture, when group is false), per package: a header
// line, a counts line, then the uncovered paths one per line. Nothing is
// indented and no line is blank, so the output greps and pipes. What each
// slice takes is only in the JSON.
func renderText(w io.Writer, pkgs []coverage.Package, group bool, s style) {
	for _, p := range pkgs {
		groups := coverage.Ungrouped(p)
		if group {
			groups = coverage.Groups(p)
		}
		for _, g := range groups {
			renderGroup(w, p, g, s)
		}
	}
}

func renderGroup(w io.Writer, p coverage.Package, g coverage.Group, s style) {
	name := s.wrap(s.name, p.Name)
	arches := s.wrap(s.dim, "["+strings.Join(g.Arches, ", ")+"]")
	switch {
	case g.Err != nil:
		fmt.Fprintf(w, "%s %s: %s\n  %s\n", name, arches, s.wrap(s.red, "error"), strings.ReplaceAll(g.Err.Error(), "\n", "\n  "))
		return
	case !g.Available:
		fmt.Fprintf(w, "%s %s: %s\n", name, arches, s.wrap(s.yellow, "not available"))
		return
	}
	head := name
	if g.Version != "" {
		head += " " + g.Version
		if g.DebArch == "all" {
			head += " (all)"
		}
	}
	fmt.Fprintf(w, "%s %s\n", head, arches)
	fmt.Fprintf(w, "total %d  %s  %s  %s\n", g.Total,
		s.count("covered", len(g.Covered), false, s.yellow),
		s.count("ignored", len(g.Ignored), true, s.yellow),
		s.count("uncovered", len(g.Uncovered), true, s.red))
	for _, path := range g.Uncovered {
		fmt.Fprintln(w, path)
	}
}

type jsonOut struct {
	Packages []jsonPackage `json:"packages"`
}

type jsonPackage struct {
	Name   string     `json:"name"`
	Arches []jsonArch `json:"arches"`
}

type jsonArch struct {
	Arch      string              `json:"arch"`
	Available bool                `json:"available"`
	Version   *string             `json:"version"`
	DebArch   *string             `json:"deb-arch"`
	Error     *string             `json:"error"`
	Total     int                 `json:"total"`
	Covered   []string            `json:"covered"`
	Uncovered []string            `json:"uncovered"`
	Ignored   []string            `json:"ignored"`
	Slices    map[string][]string `json:"slices"`
}

// renderJSON prints every architecture as-is: raw paths, no grouping.
func renderJSON(w io.Writer, pkgs []coverage.Package) error {
	out := jsonOut{Packages: []jsonPackage{}}
	for _, p := range pkgs {
		jp := jsonPackage{Name: p.Name, Arches: []jsonArch{}}
		for _, a := range p.Arches {
			ja := jsonArch{
				Arch:      a.Arch,
				Available: a.Available,
				Version:   optional(a.Version),
				DebArch:   optional(a.DebArch),
				Total:     a.Total,
				Covered:   orEmpty(a.Covered),
				Uncovered: orEmpty(a.Uncovered),
				Ignored:   orEmpty(a.Ignored),
				Slices:    map[string][]string{},
			}
			if a.Err != nil {
				ja.Error = optional(a.Err.Error())
			}
			for name, paths := range a.Slices {
				ja.Slices[name] = orEmpty(paths)
			}
			jp.Arches = append(jp.Arches, ja)
		}
		out.Packages = append(out.Packages, jp)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
