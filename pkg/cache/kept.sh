#!/bin/sh
# Model-checks kept.fizz with fizzbee: the two configurations that must pass
# and the three designs that must fail, each as its header says.
set -eu
cd "$(dirname "$0")"

three='W = 3
KEPT = 1
AGES = 1
CRASHES = 0'

# check <what the output must hold> <name> [fizz arguments]
check() {
    want=$1
    name=$2
    shift 2
    got=$(timeout -k 10 600 fizz "$@" kept.fizz 2>&1 | grep -E '^(PASSED|FAILED|DEADLOCK|Invariant)' | tr '\n' ' ' || true)
    echo "$name: $got"
    case "$got" in
    *"$want"*) ;;
    *)
        echo "$name: expected $want" >&2
        exit 1
        ;;
    esac
}

check PASSED pair
check PASSED three --preinit-hook "$three"
check AViewInUseStays 'pair, DESIGN noverify' --preinit-hook 'DESIGN = "noverify"'
check OnlyStaleAndFreeIsSwept 'pair, DESIGN nolock' --preinit-hook 'DESIGN = "nolock"'
check OneRunAView 'pair, INHERIT False' --preinit-hook 'INHERIT = False'
