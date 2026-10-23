# chisel-coverage

development tool for [chisel-releases](https://github.com/canonical/chisel-releases) work. for a package, it reports which paths of the `.deb` its slice definition file leaves out, per architecture; `--json` also lists what each slice does take.

## why

when writing or reviewing a slice definition you want to know what you left out, on every architecture, without cutting and diffing a rootfs by hand. the slice definition declares paths; the deb has paths; the difference is the question.

## use

```shell
cd ~/git/chisel-releases
chisel-coverage --ignore '/usr/share/man/**,/usr/share/doc/**' bash
```

```
bash 5.2.21-2ubuntu4 [amd64]
total 44 covered 16 ignored 19 uncovered 9
/usr/share/debianutils/
/usr/share/debianutils/shells.d/
/usr/share/debianutils/shells.d/bash
/usr/share/info/
/usr/share/lintian/
/usr/share/lintian/overrides/
/usr/share/lintian/overrides/bash
/usr/share/menu/
/usr/share/menu/bash
```

every entry of the deb counts, directories included; a directory is covered once anything under it is. `--arch all` checks every architecture, and architectures with the same version are folded on what their debs ship (after folding the multiarch triplet into `<arch>-linux-<gnu>`): one block for the paths on all of them, then one per set of architectures with paths of their own; `--no-group` gives one block per architecture with its real paths instead. several packages can be given at once. cuts run in parallel, one per cpu by default (`-j`), each chisel with a private view of the cache so they cannot trip over each other; `-j 1` runs them one after another. the `chisel` binary is found on `PATH`, or set `CHISEL=/path/to/chisel`.

```
chisel-coverage [flags] <package>...
      --release=<dir|name>     chisel-releases checkout, or a release name
                               like ubuntu-24.04 fetched from github and
                               cached (default: .)
      --arch=<a>[,<a>...]      architectures to check; "all" for every one
                               (default: host)
      --slice=<s>[,<s>...]     only these slices, bare or <package>_<slice>
                               (default: all)
      --ignore=<glob>[,...]    leave matching paths out of the uncovered set
                               (chisel glob syntax)
      --no-group               one block per architecture with its real paths,
                               instead of folding architectures with the same
                               result
      --json                   machine-readable output, one entry per
                               architecture
      --fail-uncovered         exit 1 when any architecture has uncovered paths
      --keep                   keep the temp releases and rootfs dirs
  -j, --jobs=<n>               cuts to run at once; -1 for one per cpu
                               (default: -1)
  -v, --verbose                one line per cut on stderr; twice also streams
                               chisel's output
      --version                print the version and exit
  -h, --help                   this text
```

`--ignore` only ever shrinks the uncovered set; a covered path that matches an ignore stays covered. the exit code is 0 unless something failed (2), `--fail-uncovered` is given and paths are left (1), or the run was interrupted (130), which makes it usable as a ci gate. the report is coloured a little when stdout is a terminal; set `NO_COLOR` to turn that off, and piped output is always plain.

## how it matches

chisel does the work. for each package the tool writes a throwaway release holding your `chisel.yaml` and a single slice definition: the package's own slices with everything a deb cannot answer stripped (essentials, mutate scripts, `until`, generated `text` / `symlink` / `make` / `generate` entries; `copy` entries are keyed by their source), plus one slice taking the whole deb (`/**`) and one per ignore glob. every slice is then `chisel cut` on its own and its rootfs walked. what the whole-deb cut has and no slice cut has is uncovered.

so archive selection, suites, priorities, pro archives, signatures, `arch:` lists and glob semantics are all whatever the chisel binary you run does, and the cache is chisel's own. two things follow:

- the result is "which deb paths does the slice definition declare", not "what does a real cut produce": essentials and mutation are out of the picture on purpose.
- a package not built for an architecture is reported as not available, not as an error.

## build

```shell
make build      # ./bin/chisel-coverage
make test       # unit tests; the real-chisel ones skip without `chisel`
make e2e        # real chisel against the real archive and a chisel-releases clone (needs network)
```
