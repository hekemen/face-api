#!/usr/bin/env python3
"""Analyze labeled face-match samples to choose a match threshold.

Reads the samples CSV written by scripts/collect-samples.sh, where each row
records who was actually in front of the camera (the label) and what the
system scored. The labels turn the similarity scores into ground truth, so
the threshold can be chosen from measurements instead of a guess.

Usage:
  analyze_samples.py [samples.csv]

A label of "empty" (or a blank label) counts as a non-match; every other
label counts as a genuine match for that user.

The threshold sweep is the point of the tool: it shows, for each candidate
threshold, how many genuine samples would be caught and how many empty-scene
samples would be wrongly accepted. The usable threshold is the gap between
those two numbers.
"""
import csv
import os
import sys
from collections import defaultdict

PATH = sys.argv[1] if len(sys.argv) > 1 else "samples.csv"
NON_MATCH_LABELS = {"", "empty", "none", "nobody", "-", "unknown"}


def pct(xs, p):
    if not xs:
        return 0.0
    xs = sorted(xs)
    i = min(len(xs) - 1, int(round((p / 100.0) * (len(xs) - 1))))
    return xs[i]


def main():
    if not os.path.exists(PATH):
        print("no samples file at %s" % PATH)
        return 1

    with open(PATH, newline="") as f:
        rows = list(csv.DictReader(f))
    if not rows:
        print("no samples yet")
        return 1

    genuine = []   # (label, name, similarity, matched)
    nonmatch = []  # (label, name, similarity, matched, status)
    for r in rows:
        label = (r.get("label") or "").strip()
        name = (r.get("name") or "").strip()
        try:
            sim = float(r.get("similarity") or 0)
        except ValueError:
            sim = 0.0
        matched = (r.get("matched") or "").strip().lower() == "true"
        status = (r.get("audit_status") or "").strip()
        if label.lower() in NON_MATCH_LABELS:
            nonmatch.append((label, name, sim, matched, status))
        else:
            genuine.append((label, name, sim, matched))

    print("samples: %d   genuine: %d   non-match: %d"
          % (len(rows), len(genuine), len(nonmatch)))

    print("")
    print("--- per person ---")
    by_label = defaultdict(list)
    for label, name, sim, matched in genuine:
        by_label[label].append((name, sim, matched))
    for label in sorted(by_label):
        items = by_label[label]
        sims = [s for _, s, _ in items]
        correct = sum(1 for n, _, m in items if m and n == label)
        wrong = sum(1 for n, _, m in items if m and n != label)
        missed = sum(1 for _, _, m in items if not m)
        print("  %-12s n=%-3d min=%.3f median=%.3f max=%.3f   correct=%d wrong-name=%d missed=%d"
              % (label, len(items), min(sims), pct(sims, 50), max(sims), correct, wrong, missed))
        for n, s, m in items:
            verdict = "OK" if (m and n == label) else ("WRONG-NAME" if m else "MISSED")
            print("      sim=%.4f  system=%-10s %s" % (s, n or "-", verdict))

    if nonmatch:
        sims = [s for _, _, s, _, _ in nonmatch]
        print("")
        print("--- empty scene (the false-positive floor) ---")
        print("  n=%d  min=%.3f median=%.3f max=%.3f"
              % (len(sims), min(sims), pct(sims, 50), max(sims)))
        for label, name, s, m, st in nonmatch:
            if st != "no_face":
                print("      sim=%.4f system=%-10s %s" % (s, name or "-", st))

    if not genuine:
        print("")
        print("No labelled genuine samples yet. Walk up to the camera with a")
        print("person, then:  collect-samples.sh <that person's name>")
        return 0

    print("")
    print("--- threshold sweep ---")
    print("threshold  genuine caught   empty wrongly accepted")
    best = None
    for t in [x / 100.0 for x in range(20, 91, 5)]:
        caught = sum(1 for _, _, s, _ in genuine if s >= t)
        wrong = sum(1 for _, _, s, _, _ in nonmatch if s >= t)
        flag = ""
        if caught == len(genuine) and wrong == 0:
            flag = "   <- catches every genuine sample, accepts no empty scene"
            best = t
        print("  %.2f      %3d/%-3d          %3d/%-3d%s"
              % (t, caught, len(genuine), wrong, len(nonmatch), flag))
    if best is not None:
        print("")
        print("threshold %.2f satisfies both constraints on this data" % best)
    else:
        print("")
        print("No threshold in the sweep satisfies both constraints on this data:")
        print("collect more samples, or note that the empty-scene samples must")
        print("include faces that are present but not enrolled (a real stranger).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
