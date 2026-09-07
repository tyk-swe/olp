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


def metadata_issues(current, previous, fields, prefix="", unknown=("unknown",)) -> list[str]:
    current = current if isinstance(current, dict) else {}
    previous = previous if isinstance(previous, dict) else {}
    issues = []
    for field in fields:
        name = f"{prefix}{field}"
        missing = [
            side for side, values in (("current", current), ("previous", previous))
            if values.get(field) in (None, "", *unknown)
        ]
        if missing:
            issues.append(f"missing {name} metadata in {' and '.join(missing)}")
        elif current[field] != previous[field]:
            issues.append(f"{name} differs")
    return issues


def run_comparison_issues(current: dict, previous: dict) -> list[str]:
    # bench.py records "external" when the build profile was not supplied.
    return metadata_issues(current, previous, ("build_profile", "duration_seconds"),
                           unknown=("unknown", "external")) + metadata_issues(
        current.get("machine"), previous.get("machine"),
        ("platform", "cpu", "logical_cpus", "rustc", "oha"), "machine."
    )


def scenario_comparison_issues(current, previous, scenario, prior) -> list[str]:
    issues = metadata_issues(scenario, prior, ("duration_seconds", "concurrency"))
    has_mock = scenario.get("mock") is not None
    had_mock = prior.get("mock") is not None
    if (
        scenario["name"] != "models_c256" and (not has_mock or not had_mock)
    ) or (scenario.get("added_latency_ms") is not None and not has_mock) or (
        prior.get("added_latency_ms") is not None and not had_mock
    ):
        issues.append("missing direct-mock phase metadata")
    elif has_mock != had_mock:
        issues.append("direct-mock phase differs")
    elif has_mock:
        issues.extend(metadata_issues(
            current.get("mock"), previous.get("mock"),
            ("unary_delay_ms", "stream_tokens"), "mock."
        ))
    return issues


def scenario_row(name, scenario, previous, warnings) -> str:
    added = scenario.get("added_latency_ms")
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
    if previous:
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
    return (
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


def comparison_rows(current: dict, baseline: dict | None, warnings: list[str]) -> list[str]:
    run_issues = run_comparison_issues(current, baseline) if baseline else []
    warnings.extend(f"comparison unavailable: {issue}" for issue in run_issues)
    baseline_scenarios = scenario_map(baseline) if baseline and not run_issues else {}
    rows = []
    for name, scenario in scenario_map(current).items():
        previous = baseline_scenarios.get(name)
        if previous:
            issues = scenario_comparison_issues(current, baseline, scenario, previous)
            warnings.extend(f"{name}: comparison unavailable: {issue}" for issue in issues)
            if issues:
                previous = None
        elif baseline and not run_issues:
            warnings.append(f"{name}: comparison unavailable: no previous scenario")
        rows.append(scenario_row(name, scenario, previous, warnings))
    return rows


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
    baseline_path = result_file(arguments.baseline) if arguments.baseline else None
    baseline = json.loads(baseline_path.read_text()) if baseline_path else None
    baseline_ignored = baseline is not None and not result_is_valid(baseline)
    if baseline_ignored:
        baseline = None

    warnings = [f"invalid benchmark: {reason}" for reason in current.get("invalid_reasons", [])]
    if not current_valid and not warnings:
        warnings.append("invalid benchmark")
    if baseline_ignored:
        warnings.append("invalid previous benchmark was ignored")
    rows = comparison_rows(current, baseline if current_valid else None, warnings)

    source = current["git_sha"][:12]
    if current.get("source_dirty"):
        fingerprint = current.get("source_fingerprint")
        source += f"-dirty-{fingerprint[:12]}" if fingerprint else " (dirty tree)"
    machine = current.get("machine")
    machine = machine if isinstance(machine, dict) else {}
    report = [
        f"Performance results for `{source}`",
        "",
        "| Scenario | Latency basis | p95 | p99 | Previous p95 | Change |",
        "|---|---|---:|---:|---:|---:|",
        *rows,
        "",
        f"Machine: {machine.get('cpu') or 'unknown'} · oha {machine.get('oha') or 'unknown'}",
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
