from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path

metadata = json.loads(Path(os.environ["OLP_SDK_SMOKE_METADATA"]).read_text())
assert metadata["origin"].startswith("http://127.0.0.1:")
assert metadata["api_key"] and metadata["route_slug"]
if "--check-metadata" in sys.argv:
    raise SystemExit(0)

binary = os.environ.get("OLP_TEST_BINARY")
assert binary, "OLP_TEST_BINARY is required for generated configuration qualification"
with tempfile.TemporaryDirectory() as directory:
    key_file = Path(directory) / "mounted ' key"
    key_file.write_text(metadata["api_key"] + "\n")
    key_file.chmod(0o600)
    for surface in ("openai", "anthropic", "gemini"):
        source = subprocess.check_output(
            [
                binary, "client-env", surface, "--format", "python",
                "--url", metadata["origin"], "--model", metadata["route_slug"],
                "--key-file", str(key_file),
            ],
            text=True,
        )
        assert metadata["api_key"] not in source
        configuration: dict = {}
        exec(compile(source, "generated_client.py", "exec"), configuration)
        client, model = configuration["client"], configuration["model_name"]
        if surface == "openai":
            result = client.chat.completions.create(
                model=model,
                messages=[{"role": "user", "content": "Say hello."}],
                max_completion_tokens=64,
            )
            assert result.choices[0].message.content
        elif surface == "anthropic":
            result = client.messages.create(
                model=model,
                messages=[{"role": "user", "content": "Say hello."}],
                max_tokens=64,
            )
            assert result.content[0].text
        else:
            result = client.models.generate_content(
                model=model,
                contents="Say hello.",
                config={"max_output_tokens": 64},
            )
            assert result.text
        client.close()
        print(f"generated {surface} Python client passed")
