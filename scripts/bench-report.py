#!/usr/bin/env python3
import argparse
import json
import os
import sys
from pathlib import Path


def result_file(path: Path) -> Path | None:
    if path.is_file():
        return path
    files = sorted(path.rglob("*.json")) if path.exists() else []
    return files[-1] if files else None


def scenario_map(result: dict) -> dict:
    return {scenario["name"]: scenario for scenario in result["scenarios"]}


def milliseconds(value: float) -> str:
    return f"{value:.2f} ms"


def result_is_valid(result: dict) -> bool:
    if result.get("valid") is not True or result.get("invalid_reasons"):
        return False
    return result.get("admission_rejections", 0) == 0


def main() -> int:
    parser = argparse.ArgumentParser(description="Render a benchmark comparison report")
    parser.add_argument("current", type=Path)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    arguments = parser.parse_args()

    current_path = result_file(arguments.current)
    if current_path is None:
        print("current benchmark JSON was not found", file=sys.stderr)
        return 1
    current = json.loads(current_path.read_text())
    current_valid = result_is_valid(current)
    current_scenarios = scenario_map(current)
    baseline_path = result_file(arguments.baseline) if arguments.baseline else None
    baseline = json.loads(baseline_path.read_text()) if baseline_path else None
    baseline_ignored = baseline is not None and not result_is_valid(baseline)
    if baseline_ignored:
        baseline = None
    baseline_scenarios = scenario_map(baseline) if baseline and current_valid else {}

    rows = []
    warnings = [f"invalid benchmark: {reason}" for reason in current.get("invalid_reasons", [])]
    if not current_valid and not warnings:
        warnings.append("invalid benchmark")
    if baseline_ignored:
        warnings.append("invalid previous benchmark was ignored")
    for name, scenario in current_scenarios.items():
        added = scenario.get("added_latency_ms")
        previous = baseline_scenarios.get(name)
        previous_added = previous.get("added_latency_ms") if previous else None
        use_added = added is not None and added["p95"] > 0 and (
            previous is None or (previous_added is not None and previous_added["p95"] > 0)
        )
        if use_added:
            latency = added
            basis = "added"
            previous_p95 = previous_added["p95"] if previous_added else None
        else:
            latency = scenario["gateway"]["latency_ms"]
            basis = "gateway"
            previous_p95 = previous["gateway"]["latency_ms"]["p95"] if previous else None
        current_p95 = latency["p95"]
        changes = []
        if previous_p95 is not None and previous_p95 > 0:
            latency_change = (current_p95 - previous_p95) / previous_p95 * 100
            changes.append(f"p95 {latency_change:+.1f}%")
            if latency_change > 25:
                warnings.append(
                    f"{name}: p95 {basis} latency increased {latency_change:.1f}%"
                )
        if basis == "gateway" and previous:
            previous_throughput = previous["gateway"]["throughput_rps"]
            current_throughput = scenario["gateway"]["throughput_rps"]
            if previous_throughput > 0:
                throughput_change = (
                    (current_throughput - previous_throughput) / previous_throughput * 100
                )
                changes.append(f"throughput {throughput_change:+.1f}%")
                if throughput_change < -25:
                    warnings.append(
                        f"{name}: gateway throughput decreased {-throughput_change:.1f}%"
                    )
        change_label = ", ".join(changes) if changes else "—"
        rows.append(
            "| "
            + " | ".join(
                [
                    name,
                    basis,
                    milliseconds(current_p95),
                    milliseconds(latency["p99"]),
                    milliseconds(previous_p95) if previous_p95 is not None else "—",
                    change_label,
                ]
            )
            + " |"
        )

    source = current["git_sha"][:12]
    if current.get("source_dirty"):
        fingerprint = current.get("source_fingerprint")
        source += f"-dirty-{fingerprint[:12]}" if fingerprint else " (dirty tree)"
    report = [
        f"Performance results for `{source}`",
        "",
        "| Scenario | Latency basis | p95 | p99 | Previous p95 | Change |",
        "|---|---|---:|---:|---:|---:|",
        *rows,
        "",
        f"Machine: {current['machine']['cpu']} · oha {current['machine']['oha']}",
    ]
    if warnings:
        report.extend(["", "Warnings:", *[f"- {message}" for message in warnings]])
        for message in warnings:
            print(f"::warning::{message}")
    elif baseline:
        report.extend(["", "No performance regression exceeded 25%."])
    else:
        report.extend(["", "No previous main-branch benchmark artifact was available."])

    arguments.output.parent.mkdir(parents=True, exist_ok=True)
    arguments.output.write_text("\n".join(report) + "\n")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write("\n".join(report) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
