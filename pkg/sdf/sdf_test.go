package sdf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lczyk/assert"
)

const bashYAML = `package: bash

essential:
  - bash_copyright

slices:
  config:
    contents:
      /etc/bash.bashrc:
      /etc/skel/.bashrc: {mutable: true}
      /etc/skel/.profile: {until: mutate}
  bins:
    essential:
      - bash_config
      - libc6_libs
    contents:
      /usr/bin/bash:
      /usr/bin/sh: {symlink: /usr/bin/bash}
      /usr/lib/*/bash/loadables.so: {arch: [amd64, arm64]}
      /usr/lib/bash/one: {arch: s390x}
      /etc/motd: {text: "hi"}
      /var/lib/bash/: {make: true, mode: 0755}
      /usr/share/bash/cfg.default: {copy: /usr/share/bash/cfg}
      /usr/share/bash/cfg: {arch: [amd64]}
  copyright:
    contents:
      /usr/share/doc/bash/copyright:
  scripts:
    essential:
      - bash_bins
    mutate: |
      content.write("/etc/skel/.profile", "")
  chisel:
    contents:
      /var/lib/chisel/**: {generate: manifest}
`

func TestReadStrips(t *testing.T) {
	rel := t.TempDir()
	assert.NoError(t, os.MkdirAll(filepath.Join(rel, "slices"), 0755))
	assert.NoError(t, os.WriteFile(filepath.Join(rel, "slices", "bash.yaml"), []byte(bashYAML), 0644))

	d, err := Read(rel, "bash")
	assert.NoError(t, err)
	assert.Equal(t, d.Package, "bash")
	assert.Equal(t, d.Archive, "")
	assert.Len(t, d.Slices, 5)

	names := []string{}
	for _, s := range d.Slices {
		names = append(names, s.Name)
	}
	assert.EqualArrays(t, names, []string{"config", "bins", "copyright", "scripts", "chisel"})

	config := d.Slice("config")
	assert.EqualArrays(t, paths(config), []string{"/etc/bash.bashrc", "/etc/skel/.bashrc", "/etc/skel/.profile"})
	for _, e := range config.Entries {
		assert.Nil(t, e.Arch)
	}

	bins := d.Slice("bins")
	assert.EqualArrays(t, paths(bins), []string{"/usr/bin/bash", "/usr/lib/*/bash/loadables.so", "/usr/lib/bash/one", "/usr/share/bash/cfg"})
	assert.EqualArrays(t, bins.Entries[1].Arch, []string{"amd64", "arm64"})
	assert.EqualArrays(t, bins.Entries[2].Arch, []string{"s390x"})
	// The copy re-keyed onto a path the slice also lists: as wide as the wider one.
	assert.Nil(t, bins.Entries[3].Arch)

	assert.Len(t, d.Slice("scripts").Entries, 0)
	assert.Len(t, d.Slice("chisel").Entries, 0)
	assert.Nil(t, d.Slice("nope"))
}

func TestReadErrors(t *testing.T) {
	rel := t.TempDir()
	assert.NoError(t, os.MkdirAll(filepath.Join(rel, "slices"), 0755))
	_, err := Read(rel, "bash")
	assert.Error(t, err, "no such file")

	assert.NoError(t, os.WriteFile(filepath.Join(rel, "slices", "bash.yaml"), []byte("package: zsh\n"), 0644))
	_, err = Read(rel, "bash")
	assert.Error(t, err, `package is "zsh"`)

	assert.NoError(t, os.WriteFile(filepath.Join(rel, "slices", "bash.yaml"), []byte("slices: {}\n"), 0644))
	_, err = Read(rel, "bash")
	assert.Error(t, err, "no package name")

	assert.NoError(t, os.WriteFile(filepath.Join(rel, "slices", "bash.yaml"), []byte("package: bash\nslices: [a]\n"), 0644))
	_, err = Read(rel, "bash")
	assert.Error(t, err, "not a mapping")
}

func TestSynthesise(t *testing.T) {
	d := &Definition{Package: "bash", Archive: "ubuntu", Slices: []Slice{
		{Name: "bins", Entries: []Entry{{Path: "/usr/bin/bash"}, {Path: "/usr/lib/*/x.so", Arch: []string{"amd64"}}}},
		{Name: "scripts"},
		{Name: "copyright", Entries: []Entry{{Path: "/usr/share/doc/bash/copyright"}}},
	}}

	file, cuts, err := Synthesise(d, nil, []string{"/usr/share/man/**", "/usr/share/doc/**"})
	assert.NoError(t, err)
	assert.EqualArrays(t, cuts, []string{"cov-all", "bins", "copyright", "cov-ignore-0", "cov-ignore-1"})
	assert.Equal(t, string(file), `package: bash
archive: ubuntu
slices:
  cov-all:
    contents:
      /**:
  cov-probe:
    contents: {}
  bins:
    contents:
      /usr/bin/bash:
      /usr/lib/*/x.so: {arch: [amd64]}
  copyright:
    contents:
      /usr/share/doc/bash/copyright:
  cov-ignore-0:
    contents:
      /usr/share/man/**:
  cov-ignore-1:
    contents:
      /usr/share/doc/**:
`)

	file, cuts, err = Synthesise(d, []string{"copyright"}, nil)
	assert.NoError(t, err)
	assert.EqualArrays(t, cuts, []string{"cov-all", "copyright"})
	assert.That(t, !strings.Contains(string(file), "bins"))
	assert.That(t, !strings.Contains(string(file), "cov-ignore"))

	_, cuts, err = Synthesise(d, []string{}, nil)
	assert.NoError(t, err)
	assert.EqualArrays(t, cuts, []string{"cov-all"})

	_, _, err = Synthesise(d, []string{"nope"}, nil)
	assert.Error(t, err, `no slice "nope"`)

	d.Slices = append(d.Slices, Slice{Name: AllSlice, Entries: []Entry{{Path: "/x"}}})
	_, _, err = Synthesise(d, nil, nil)
	assert.Error(t, err, "reserved")
	d.Slices[len(d.Slices)-1].Name = "cov-ignore-7"
	_, _, err = Synthesise(d, nil, nil)
	assert.Error(t, err, "reserved")
}

func paths(s *Slice) []string {
	out := []string{}
	for _, e := range s.Entries {
		out = append(out, e.Path)
	}
	return out
}
