#!/usr/bin/env python3
"""Validate the local operational documentation contract."""

from __future__ import annotations

import re
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
tracked_markdown = subprocess.run(
    ["git", "ls-files", "-z", "--", "*.md"],
    cwd=ROOT,
    check=True,
    capture_output=True,
).stdout.split(b"\0")
MARKDOWN = [ROOT / path.decode() for path in tracked_markdown if path]
qualification_document = ROOT / "docs/qualification.md"
if qualification_document not in MARKDOWN:
    MARKDOWN.append(qualification_document)
LINK = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")
IDS = [f"QL-{number:02d}" for number in range(1, 21)]


def anchor(text: str) -> str:
    text = text.strip().lower()
    text = re.sub(r"[^\w\s-]", "", text)
    return re.sub(r"[\s-]+", "-", text).strip("-")


def fail(message: str) -> None:
    print(message, file=sys.stderr)
    raise SystemExit(1)


for document in MARKDOWN:
    if not document.is_file():
        fail(f"missing documentation file: {document.relative_to(ROOT)}")
    content = document.read_text(encoding="utf-8")
    own_anchors = {anchor(line.lstrip("#")) for line in content.splitlines() if line.startswith("#")}
    for raw_target in LINK.findall(content):
        target = raw_target.strip().strip("<>")
        if re.match(r"^(?:https?://|mailto:)", target):
            continue
        path_text, _, fragment = target.partition("#")
        linked = document if not path_text else (document.parent / path_text).resolve()
        try:
            linked.relative_to(ROOT)
        except ValueError:
            fail(f"{document.relative_to(ROOT)} links outside the repository: {target}")
        if not linked.exists():
            fail(f"{document.relative_to(ROOT)} has a missing local link: {target}")
        if fragment:
            linked_content = content if linked == document else linked.read_text(encoding="utf-8")
            anchors = own_anchors if linked == document else {
                anchor(line.lstrip("#")) for line in linked_content.splitlines() if line.startswith("#")
            }
            if fragment not in anchors:
                fail(f"{document.relative_to(ROOT)} has a missing anchor: {target}")

matrix = (ROOT / "docs/qualification.md").read_text(encoding="utf-8")
for criterion in IDS:
    if len(re.findall(rf"\b{re.escape(criterion)}\b", matrix)) != 1:
        fail(f"docs/qualification.md must contain {criterion} exactly once")

version_match = re.search(
    r'(?ms)^\[workspace\.package\].*?^version = "([^"]+)"$', (ROOT / "Cargo.toml").read_text()
)
if not version_match or version_match.group(1) not in matrix:
    fail("qualification documentation does not contain the current workspace version")
documented_release_versions = set(re.findall(r"\b[0-9]+\.[0-9]+\.[0-9]+\b", matrix))
if documented_release_versions != {version_match.group(1)}:
    fail(
        "qualification release examples must use only the current workspace version: "
        + ", ".join(sorted(documented_release_versions))
    )

help_scripts = [
    "tests/qualification/run.sh",
    "tests/qualification/compose-clean-install.sh",
    "tests/qualification/helm-clean-install.sh",
    "tests/qualification/backup-restore.sh",
    "tests/qualification/n-minus-one.sh",
    "tests/qualification/performance.sh",
    "tests/qualification/canary.sh",
    "scripts/release-metadata-next.sh",
]
for relative in help_scripts:
    result = subprocess.run(
        [str(ROOT / relative), "--help"], cwd=ROOT, text=True, capture_output=True, timeout=10
    )
    output = result.stdout + result.stderr
    if result.returncode != 0 or "usage:" not in output.lower():
        fail(f"{relative} --help is not a successful usage contract")

print(f"documentation contract passed: {len(MARKDOWN)} files, 20 criteria, {len(help_scripts)} help commands")
