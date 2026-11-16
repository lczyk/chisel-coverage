#!/bin/sh
# Model-checks views.fizz with fizzbee: the five configurations that must
# pass and the three designs that must fail, each as its header says.
set -eu
cd "$(dirname "$0")"

stops='GROUPS = [2, 0]
MAX_STOPS = 1
KINDS = ["ok"]'
later='GROUPS = [2, 1]
MAX_STOPS = 1
KINDS = ["ok"]
NEEDS = ["recent"]'
kept='KINDS = ["ok"]
SEEDS = ["thin", "whole"]'
kept4='W = 4
GROUPS = [0, 0, 0]
KINDS = ["ok"]
NEEDS = ["recent"]
SEEDS = ["thin", "whole"]'

# check <what the output must hold> <name> [fizz arguments]
check() {
    want=$1
    name=$2
    shift 2
    got=$(timeout -k 10 600 fizz "$@" views.fizz 2>&1 | grep -E '^(PASSED|FAILED|DEADLOCK|Invariant)' | tr '\n' ' ' || true)
    echo "$name: $got"
    case "$got" in
    *"$want"*) ;;
    *)
        echo "$name: expected $want" >&2
        exit 1
        ;;
    esac
}

check PASSED views
check PASSED stops --preinit-hook "$stops"
check PASSED later --preinit-hook "$later"
check PASSED kept --preinit-hook "$kept"
check PASSED kept4 --preinit-hook "$kept4"
check DEADLOCK 'stops, FIX none' --preinit-hook "$stops
FIX = \"none\""
check OneThinViewUntilProven 'stops, FIX release' --preinit-hook "$stops
FIX = \"release\""
check OneThinViewUntilProven 'kept4, ORDER decide_first' --preinit-hook "$kept4
ORDER = \"decide_first\""
