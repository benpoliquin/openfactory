#!/usr/bin/env python3
"""Local, deterministic counterexamples; results are measured, never prefilled.

The operator profile fixes the interpreter and this script. A frozen contract
chooses one allowed mode and predicates over observations.json. Arrival-order
handling should be tested first, not silently replaced with the corrected mode.
"""
import json
import pathlib
import sys


def consume(events, versioned):
    state = {}
    applied = 0
    for event in events:
        old = state.get(event["key"])
        if versioned and old is not None and event["version"] <= old["version"]:
            continue
        state[event["key"]] = dict(event)
        applied += 1
    return state, applied


def ordering(versioned):
    first = {"key": "invoice-1", "version": 1, "value": "pending"}
    second = {"key": "invoice-1", "version": 2, "value": "paid"}
    canonical, _ = consume([first, second], versioned)
    duplicate, duplicate_applied = consume([first, second, dict(second)], versioned)
    reordered, _ = consume([second, first], versioned)
    return {
        "duplicate_same_state": duplicate == canonical,
        "duplicate_applied_once": duplicate_applied == 2,
        "reorder_same_state": reordered == canonical,
        "canonical_version": canonical["invoice-1"]["version"],
        "reordered_version": reordered["invoice-1"]["version"],
    }


class FencedStore:
    def __init__(self):
        self.fence = 0
        self.value = None

    def acquire(self):
        self.fence += 1
        return self.fence

    def write(self, token, value):
        if token != self.fence:
            return False
        self.value = value
        return True


def fencing():
    store = FencedStore()
    stale = store.acquire()
    current = store.acquire()
    current_accepted = store.write(current, "current-writer")
    stale_accepted = store.write(stale, "stale-writer")
    return {
        "current_writer_accepted": current_accepted,
        "stale_writer_rejected": not stale_accepted,
        "current_value_preserved": store.value == "current-writer",
        "current_fence": store.fence,
    }


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in {"arrival", "versioned", "fenced"}:
        raise SystemExit("expected exactly one mode: arrival, versioned, fenced")
    mode = sys.argv[1]
    observations = fencing() if mode == "fenced" else ordering(mode == "versioned")
    # A completed experiment may falsify its hypothesis. This script records
    # observations; the frozen typed predicates and independent evaluator decide.
    encoded = json.dumps(observations, sort_keys=True)
    pathlib.Path("observations.json").write_text(encoded + "\n", encoding="utf-8")
    print(encoded)


if __name__ == "__main__":
    main()
