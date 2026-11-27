"""Slither on the contracts, against the findings reviewed one by one.

Runs in the Slither image (infra/images/slither) from contracts/. Every finding is
either fixed in the code or recorded in slither.db.json with the reason in
docs/review/vault.md. The check fails on a finding that is not recorded, and on a
recorded one that no longer occurs, so the record says exactly what Slither reports.
Finding ids are Slither's own: a hash of the finding with its source lines, so a
changed line is a new finding. With --record, writes what Slither reports now to
slither.db.json: run it only once each new finding is reviewed in the document.
"""

import collections
import json
import os
import subprocess
import sys
import tempfile

REVIEWED = "slither.db.json"


def main() -> int:
    # A new path each run: Slither does not overwrite an existing report.
    report_path = os.path.join(tempfile.mkdtemp(), "slither.json")
    run = subprocess.run(
        ["slither", ".", "--show-ignored-findings", "--json", report_path],
        capture_output=True,
        text=True,
        check=False,
    )
    try:
        with open(report_path, encoding="utf-8") as f:
            report = json.load(f)
    except (OSError, ValueError):
        print(run.stdout[-3000:] + run.stderr[-3000:])
        print("slither: no report")
        return 2
    if not report.get("success"):
        print("slither:", report.get("error"))
        return 2

    found = {r["id"]: r for r in report["results"].get("detectors", [])}

    if sys.argv[1:] == ["--record"]:
        keep = ("check", "impact", "confidence", "id", "description")
        record = [{k: r[k] for k in keep} for r in sorted(found.values(), key=lambda r: (r["check"], r["description"]))]
        with open(REVIEWED, "w", encoding="utf-8") as f:
            f.write(json.dumps(record, indent=2) + "\n")
        print(f"slither          recorded {len(record)} findings in {REVIEWED}")
        return 0

    with open(REVIEWED, encoding="utf-8") as f:
        reviewed = {r["id"]: r for r in json.load(f)}
    new = [r for i, r in found.items() if i not in reviewed]
    gone = [r for i, r in reviewed.items() if i not in found]
    for r in new:
        print(f"NEW   {r['impact']:13s} {r['check']}: {r['description'].strip()}")
    for r in gone:
        print(f"GONE  {r['impact']:13s} {r['check']}: {r['description'].strip().splitlines()[0]}")

    ours = sum(1 for r in found.values() if any(
        e.get("source_mapping", {}).get("filename_relative", "").startswith("src/") for e in r["elements"]))
    by_impact = collections.Counter(r["impact"] for r in found.values() if r["id"] in reviewed)
    summary = ", ".join(f"{n} {k}" for k, n in sorted(by_impact.items()))
    print(f"slither          {len(found)} findings, {ours} touching src/; reviewed: {summary or 'none'}")
    if new or gone:
        print(f"slither: {len(new)} not reviewed, {len(gone)} reviewed but gone; see docs/review/vault.md")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
