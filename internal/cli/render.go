package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/lczyk/chisel-slice-coverage/internal/coverage"
)

// renderText prints one block per group of architectures with the same
// result, per package.
func renderText(w io.Writer, pkgs []coverage.Package) {
	first := true
	for _, p := range pkgs {
		for _, g := range coverage.Groups(p) {
			if !first {
				fmt.Fprintln(w)
			}
			first = false
			renderGroup(w, p, g)
		}
	}
}

func renderGroup(w io.Writer, p coverage.Package, g coverage.Group) {
	arches := "[" + strings.Join(g.Arches, ", ") + "]"
	switch {
	case g.Err != nil:
		fmt.Fprintf(w, "%s %s: error\n  %s\n", p.Name, arches, strings.ReplaceAll(g.Err.Error(), "\n", "\n  "))
		return
	case !g.Available:
		fmt.Fprintf(w, "%s %s: not available\n", p.Name, arches)
		return
	}
	head := p.Name
	if g.Version != "" {
		head += " " + g.Version
		if g.DebArch == "all" {
			head += " (all)"
		}
	}
	fmt.Fprintf(w, "%s %s\n", head, arches)
	fmt.Fprintf(w, "  total %d  covered %d  ignored %d  uncovered %d\n",
		g.Total, len(g.Covered), len(g.Ignored), len(g.Uncovered))
	if len(g.Uncovered) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  uncovered:")
		for _, path := range g.Uncovered {
			fmt.Fprintf(w, "    %s\n", path)
		}
	}
	if len(p.Slices) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  slices:")
		for _, name := range p.Slices {
			paths := g.Slices[name]
			fmt.Fprintf(w, "    %s (%d):\n", name, len(paths))
			for _, path := range paths {
				fmt.Fprintf(w, "      %s\n", path)
			}
		}
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
