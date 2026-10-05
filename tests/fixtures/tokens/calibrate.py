#!/usr/bin/env python3
"""Measures token-estimation factors for model families without a public tokenizer.

Run from the repository root with vendor keys in the environment:

    ANTHROPIC_API_KEY=... GEMINI_API_KEY=... \\
      uv run --project tests/fixtures/tokens --frozen python tests/fixtures/tokens/calibrate.py

For each family it counts the oracle corpus in o200k_base.json with the vendor's
own free token-count endpoint, compares the total with the estimator's
heuristic of four characters per token, and writes calibration.json next to
this script: per family, the vendor and heuristic totals, their ratio rounded up
to two decimals, the model and endpoint measured, and the observation time.

A maintainer copies each ratio into the reference catalog's estimation list,
with the endpoint's documentation as its source, and re-signs the catalog:

    make catalog-sign

Only families measured here ever get a factor; a family without a key is
skipped and keeps the plain heuristic.
"""
import datetime
import json
import math
import os
import pathlib
import urllib.request

HERE = pathlib.Path(__file__).resolve().parent
CHARS_PER_TOKEN = 4

FAMILIES = {
    "anthropic": {
        "key": "ANTHROPIC_API_KEY",
        "model": os.environ.get("OLP_CALIBRATE_ANTHROPIC_MODEL", "claude-sonnet-4-5"),
        "endpoint": "https://api.anthropic.com/v1/messages/count_tokens",
        "documentation": "https://docs.claude.com/en/docs/build-with-claude/token-counting",
    },
    "gemini": {
        "key": "GEMINI_API_KEY",
        "model": os.environ.get("OLP_CALIBRATE_GEMINI_MODEL", "gemini-2.5-flash"),
        "endpoint": "https://generativelanguage.googleapis.com/v1beta/models/{model}:countTokens",
        "documentation": "https://ai.google.dev/gemini-api/docs/tokens",
    },
}


def heuristic(text):
    return math.ceil(len(text) / CHARS_PER_TOKEN)


def post(url, headers, body):
    request = urllib.request.Request(url, data=json.dumps(body).encode(), headers={"Content-Type": "application/json", **headers})
    with urllib.request.urlopen(request, timeout=60) as response:
        return json.load(response)


def anthropic_tokens(spec, key, text):
    reply = post(spec["endpoint"], {"x-api-key": key, "anthropic-version": "2023-06-01"},
                 {"model": spec["model"], "messages": [{"role": "user", "content": text}]})
    return reply["input_tokens"]


def gemini_tokens(spec, key, text):
    reply = post(spec["endpoint"].format(model=spec["model"]), {"x-goog-api-key": key},
                 {"contents": [{"role": "user", "parts": [{"text": text}]}]})
    return reply["totalTokens"]


COUNTERS = {"anthropic": anthropic_tokens, "gemini": gemini_tokens}


def main():
    corpus = [entry["text"] for entry in json.loads((HERE / "o200k_base.json").read_text()) if entry["text"].strip()]
    results = {}
    for family, spec in FAMILIES.items():
        key = os.environ.get(spec["key"])
        if not key:
            print(f"{family}: {spec['key']} is not set; skipped")
            continue
        vendor = sum(COUNTERS[family](spec, key, text) for text in corpus)
        estimated = sum(heuristic(text) for text in corpus)
        results[family] = {
            "model": spec["model"],
            "endpoint": spec["endpoint"],
            "documentation": spec["documentation"],
            "vendor_tokens": vendor,
            "heuristic_tokens": estimated,
            "factor": f"{math.ceil(vendor / estimated * 100) / 100:.2f}".rstrip("0").rstrip("."),
            "observed_at": datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z"),
            "texts": len(corpus),
        }
        print(f"{family}: {vendor} vendor tokens for {estimated} heuristic tokens, factor {results[family]['factor']}")
    (HERE / "calibration.json").write_text(json.dumps(results, indent=2, sort_keys=True) + "\n")


if __name__ == "__main__":
    main()
