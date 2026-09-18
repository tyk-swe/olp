#!/usr/bin/env python3
"""Inventory the pinned prebuilt GLIDE FFI without invoking a Rust toolchain.

The upstream FFI lock includes build/dev/optional dependencies; scanning this
superset is intentional. It is not a claim that each crate is reachable.
"""
import hashlib
import json
from pathlib import Path
import subprocess
import sys
import tomllib

ROOT = Path(__file__).resolve().parent.parent
MODULE = "github.com/valkey-io/valkey-glide/go/v2"
VERSION = "v2.5.2"
COMMIT = "a2165cc9d752818a9912c4aafe1cd7235ed2d5bc"
lock_path = ROOT / "deploy/native/valkey-glide.Cargo.lock"
lock = tomllib.loads(lock_path.read_text())
module = json.loads(subprocess.check_output(["go", "list", "-m", "-json", MODULE], cwd=ROOT))
if module["Version"] != VERSION:
    sys.exit("GLIDE version changed: refresh its upstream FFI lock and native inventory")
module_dir = Path(module["Dir"])
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

packages = []
for package in lock["package"]:
    name, version = package["name"], package["version"]
    item = {
        "SPDXID": f"SPDXRef-{name}-{version}", "name": name, "versionInfo": version,
        "downloadLocation": (f"https://crates.io/api/v1/crates/{name}/{version}/download"
                             if "checksum" in package else f"git+https://github.com/valkey-io/valkey-glide@{COMMIT}"),
        "filesAnalyzed": False, "licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION",
        "copyrightText": "NOASSERTION",
        "externalRefs": [{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
                          "referenceLocator": f"pkg:cargo/{name}@{version}"}],
    }
    if "checksum" in package:
        item["checksums"] = [{"algorithm": "SHA256", "checksumValue": package["checksum"]}]
    packages.append(item)
spdx = {
    "spdxVersion": "SPDX-2.3", "dataLicense": "CC0-1.0", "SPDXID": "SPDXRef-DOCUMENT",
    "name": f"valkey-glide-go-{VERSION}-ffi-lock-inventory",
    "documentNamespace": f"https://github.com/tyk-swe/olp/sbom/glide/{COMMIT}",
    "creationInfo": {"creators": ["Tool: scripts/native-sbom.py"], "created": "2026-09-18T00:00:00Z"},
    "packages": packages,
    "relationships": [{"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": p["SPDXID"]} for p in packages],
}
inventory = {
    "module": MODULE, "version": VERSION, "upstreamCommit": COMMIT,
    "source": f"https://github.com/valkey-io/valkey-glide/blob/{COMMIT}/ffi/Cargo.lock",
    "lockSHA256": digest(lock_path), "lockedPackages": len(packages),
    "scope": "Conservative upstream FFI lock inventory, including optional/build/dev crates",
    "artifacts": {str(p.relative_to(module_dir)): digest(p) for p in sorted(module_dir.rglob("*.a"))},
    "notices": {name: digest(module_dir / name) for name in ["LICENSE", "THIRD_PARTY_LICENSES_GO"]},
}
for name, value in [("valkey-glide.spdx.json", spdx), ("inventory.json", inventory)]:
    path = ROOT / "deploy/native" / name
    text = json.dumps(value, indent=2) + "\n"
    if "--check" in sys.argv:
        if not path.exists() or path.read_text() != text:
            sys.exit(f"stale native inventory: {path.relative_to(ROOT)}")
    else:
        path.write_text(text)
print(f"Verified GLIDE {VERSION}: {len(packages)} locked crates, {len(inventory['artifacts'])} prebuilt archives")
