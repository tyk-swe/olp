#!/usr/bin/env python3
import argparse
import base64
import hashlib
import json
import os
import platform
import re
import secrets
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

MODEL = "bench-model"
CHAT_ROUTE = "bench-chat"
EMBEDDING_ROUTE = "bench-embeddings"
UPSTREAM_KEY = "sk-bench-upstream"


class MockHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path.rstrip("/") == "/v1/models":
            self.send_json({"object": "list", "data": [{"id": MODEL, "object": "model"}]})
        else:
            self.send_error(404)

    def do_POST(self):
        length = int(self.headers.get("content-length", "0"))
        body = json.loads(self.rfile.read(length) or b"{}")
        if self.path.rstrip("/") == "/v1/chat/completions":
            if body.get("stream"):
                self.send_chat_stream()
            else:
                time.sleep(0.2)
                self.send_json(chat_response())
        elif self.path.rstrip("/") == "/v1/responses":
            if body.get("stream"):
                self.send_responses_stream()
            else:
                self.send_json(responses_response())
        elif self.path.rstrip("/") == "/v1/embeddings":
            time.sleep(0.2)
            self.send_json(embedding_response(body))
        else:
            self.send_error(404)

    def send_chat_stream(self):
        frames = [chat_chunk({"role": "assistant"}, None)]
        frames.extend(chat_chunk({"content": f"token-{index} "}, None) for index in range(50))
        frames.append(chat_chunk({}, "stop"))
        frames.append({"id": "chatcmpl-bench", "object": "chat.completion.chunk", "created": 0,
                       "model": MODEL, "choices": [],
                       "usage": {"prompt_tokens": 8, "completion_tokens": 50, "total_tokens": 58}})
        payload = b"".join(f"data: {json.dumps(frame, separators=(',', ':'))}\n\n".encode() for frame in frames)
        payload += b"data: [DONE]\n\n"
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def send_responses_stream(self):
        frames = [
            ("response.created", {"type": "response.created", "response": {
                "id": "resp_bench", "model": MODEL}}),
            ("response.output_text.delta", {"type": "response.output_text.delta",
                "output_index": 0, "delta": "OK"}),
            ("response.completed", {"type": "response.completed", "response": {
                "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}}}),
        ]
        payload = b"".join(
            f"event: {event}\ndata: {json.dumps(data, separators=(',', ':'))}\n\n".encode()
            for event, data in frames
        )
        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def send_json(self, body):
        payload = json.dumps(body, separators=(",", ":")).encode()
        self.send_response(200)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, _format, *_args):
        return


class MockServer(ThreadingHTTPServer):
    request_queue_size = socket.SOMAXCONN

    def handle_error(self, request, client_address):
        if isinstance(sys.exception(), (BrokenPipeError, ConnectionResetError)):
            return
        super().handle_error(request, client_address)


def chat_chunk(delta, finish_reason):
    return {"id": "chatcmpl-bench", "object": "chat.completion.chunk", "created": 0,
            "model": MODEL, "choices": [{"index": 0, "delta": delta,
                                           "finish_reason": finish_reason}]}


def chat_response():
    text = " ".join(f"token-{index}" for index in range(50))
    return {"id": "chatcmpl-bench", "object": "chat.completion", "created": 0,
            "model": MODEL, "choices": [{"index": 0, "message": {"role": "assistant",
            "content": text}, "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 8, "completion_tokens": 50, "total_tokens": 58}}


def responses_response():
    return {"id": "resp_bench", "object": "response", "created_at": 0,
            "status": "completed", "model": MODEL,
            "output": [{"id": "msg_bench", "type": "message", "role": "assistant",
                        "status": "completed", "content": [{"type": "output_text", "text": "OK",
                                                              "annotations": []}]}],
            "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}}


def embedding_response(request):
    inputs = request.get("input", [""])
    if not isinstance(inputs, list):
        inputs = [inputs]
    data = [{"object": "embedding", "index": index, "embedding": [0.125, -0.25, 0.5, 0.75]}
            for index, _ in enumerate(inputs)]
    return {"object": "list", "model": MODEL, "data": data,
            "usage": {"prompt_tokens": len(inputs), "total_tokens": len(inputs)}}


class Management:
    def __init__(self, origin):
        self.origin = origin
        self.cookie = ""
        self.csrf = ""
        self.sequence = 0

    def setup(self, token):
        response = request(self.origin + "/api/v1/setup", "POST", {
            "email": "bench@example.invalid", "password": "Correct-Horse-Battery-Staple-42!",
            "display_name": "Benchmark Owner", "installation_name": "Benchmark",
        }, {"origin": self.origin, "x-olp-setup-token": token})
        expect(response, 201, "setup")
        self.cookie = "; ".join(value.split(";", 1)[0] for value in response[1].get_all("set-cookie"))
        self.csrf = response[2]["csrf_token"]

    def send(self, method, path, body=None, etag=None, status=200):
        self.sequence += 1
        headers = {"cookie": self.cookie, "idempotency-key": f"benchmark-{self.sequence:04}"}
        if method != "GET":
            headers.update({"origin": self.origin, "x-csrf-token": self.csrf})
        if etag:
            headers["if-match"] = etag
        response = request(self.origin + path, method, body, headers)
        expect(response, status, f"{method} {path}")
        return response


def request(url, method="GET", body=None, headers=None, timeout=30):
    data = None if body is None else json.dumps(body).encode()
    all_headers = {"accept": "application/json", **(headers or {})}
    if data is not None:
        all_headers["content-type"] = "application/json"
    call = urllib.request.Request(url, data=data, headers=all_headers, method=method)
    try:
        with urllib.request.urlopen(call, timeout=timeout) as response:
            raw = response.read()
            parsed = json.loads(raw) if raw else None
            return response.status, response.headers, parsed
    except urllib.error.HTTPError as error:
        raw = error.read()
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            parsed = raw.decode(errors="replace")
        return error.code, error.headers, parsed


def expect(response, status, action):
    if response[0] != status:
        raise RuntimeError(f"{action} returned {response[0]}: {response[2]}")


def probe(management, provider_id, etag):
    response = management.send("POST", f"/api/v1/providers/{provider_id}/probe", etag=etag)
    return response[1].get("etag", etag)


def configure_provider(management, mock_origin):
    created = management.send("POST", "/api/v1/providers", {
        "name": "benchmark", "kind": "openai_compatible", "endpoint": mock_origin + "/v1/",
        "auth_mode": "api_key", "credential": UPSTREAM_KEY,
    }, status=201)
    provider_id = created[2]["id"]
    etag = created[1]["etag"]
    etag = probe(management, provider_id, etag)
    discovered = management.send("POST", f"/api/v1/providers/{provider_id}/discovery",
                                 {"mode": "live"}, etag)
    etag = discovered[1]["etag"]
    models = management.send("GET", f"/api/v1/providers/{provider_id}/models?limit=100")[2]["items"]
    model_row = next(row["id"] for row in models if row["upstream_model"] == MODEL)
    capabilities = [
        {"operation": "generation", "surface": "openai", "mode": "unary"},
        {"operation": "generation", "surface": "openai", "mode": "streaming"},
        {"operation": "embeddings", "surface": "openai", "mode": "unary"},
    ]
    reviewed = management.send("PATCH", f"/api/v1/providers/{provider_id}/models/{model_row}",
                               {"enabled": True, "capabilities": capabilities}, etag)
    etag = probe(management, provider_id, reviewed[1]["etag"])
    certified = management.send("POST", f"/api/v1/providers/{provider_id}/models/{model_row}/certify",
                                etag=etag)
    etag = probe(management, provider_id, certified[1]["etag"])
    management.send("POST", f"/api/v1/providers/{provider_id}/activate", etag=etag)
    return provider_id


def configure_route(management, slug, operation, provider_id):
    created = management.send("POST", "/api/v1/route-drafts", {
        "slug": slug, "operations": [operation], "overall_timeout_ms": 30000, "max_attempts": 1,
        "targets": [{"provider_id": provider_id, "provider_model": MODEL, "priority": 0,
                     "weight": 1, "timeout_ms": 30000}],
    }, status=201)
    draft_id = created[2]["id"]
    validated = management.send("POST", f"/api/v1/route-drafts/{draft_id}/validate",
                                etag=created[1]["etag"])
    management.send("POST", f"/api/v1/route-drafts/{draft_id}/activate",
                    etag=validated[1].get("etag", created[1]["etag"]))


def configure_gateway(origin, setup_token, mock_origin):
    management = Management(origin)
    management.setup(setup_token)
    provider_id = configure_provider(management, mock_origin)
    configure_route(management, CHAT_ROUTE, "generation", provider_id)
    configure_route(management, EMBEDDING_ROUTE, "embeddings", provider_id)
    key = management.send("POST", "/api/v1/api-keys", {
        "name": "benchmark", "scopes": ["inference", "models_read"],
        "allowed_routes": [CHAT_ROUTE, EMBEDDING_ROUTE],
    }, status=201)[2]["secret"]
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if request(origin + "/openai/v1/models", headers={"authorization": f"Bearer {key}"})[0] == 200:
            return key
        time.sleep(0.25)
    raise RuntimeError("gateway did not publish the benchmark API key")


def run_oha(url, duration, concurrency, body=None, token=None):
    command = ["oha", "-z", f"{duration}s", "-c", str(concurrency), "--no-tui",
               "--output-format", "json"]
    if token:
        command.extend(["-H", f"Authorization: Bearer {token}"])
    if body is not None:
        command.extend(["-m", "POST", "-T", "application/json", "-d",
                        json.dumps(body, separators=(",", ":"))])
    command.append(url)
    environment = os.environ.copy()
    environment.pop("NO_COLOR", None)
    completed = subprocess.run(command, check=True, capture_output=True, text=True, env=environment)
    return json.loads(completed.stdout)


def summarize(raw):
    summary = raw["summary"]
    percentiles = raw["latencyPercentiles"]
    return {
        "latency_ms": {name: round(percentiles[name] * 1000, 3) for name in ("p50", "p95", "p99")},
        "throughput_rps": round(summary["requestsPerSec"], 3),
        "error_rate": round(1 - summary["successRate"], 8),
        "status_codes": raw.get("statusCodeDistribution", {}),
        "errors": raw.get("errorDistribution", {}),
        "raw": raw,
    }


def benchmark_scenarios(origin, mock_origin, key, duration):
    chat = {"model": CHAT_ROUTE, "messages": [{"role": "user", "content": "hello"}]}
    stream = {**chat, "stream": True, "stream_options": {"include_usage": True}}
    embedding = {"model": EMBEDDING_ROUTE, "input": ["benchmark input"]}
    direct_chat = {**chat, "model": MODEL}
    direct_stream = {**stream, "model": MODEL}
    direct_embedding = {**embedding, "model": MODEL}
    definitions = [
        ("chat_unary_c16", 16, "/openai/v1/chat/completions", chat, "/v1/chat/completions", direct_chat),
        ("chat_unary_c64", 64, "/openai/v1/chat/completions", chat, "/v1/chat/completions", direct_chat),
        ("chat_unary_c256", 256, "/openai/v1/chat/completions", chat, "/v1/chat/completions", direct_chat),
        ("chat_stream_c64", 64, "/openai/v1/chat/completions", stream, "/v1/chat/completions", direct_stream),
        ("models_c256", 256, "/openai/v1/models", None, None, None),
        ("embeddings_c64", 64, "/openai/v1/embeddings", embedding, "/v1/embeddings", direct_embedding),
    ]
    results = []
    for name, concurrency, gateway_path, body, mock_path, mock_body in definitions:
        phases = f"{duration}s mock + {duration}s gateway" if mock_path else f"{duration}s gateway"
        print(f"benchmarking {name} ({phases})", flush=True)
        mock = (summarize(run_oha(mock_origin + mock_path, duration, concurrency, mock_body))
                if mock_path else None)
        gateway = summarize(run_oha(origin + gateway_path, duration, concurrency, body, key))
        result = {"name": name, "duration_seconds": duration, "concurrency": concurrency,
                  "gateway": gateway}
        if mock:
            result["mock"] = mock
            result["added_latency_ms"] = {
                percentile: round(gateway["latency_ms"][percentile]
                                  - mock["latency_ms"][percentile], 3)
                for percentile in ("p50", "p95", "p99")
            }
        results.append(result)
    return results


def scenario_errors(scenarios):
    failures = []
    for scenario in scenarios:
        for side in ("mock", "gateway"):
            result = scenario.get(side)
            if result is None:
                continue
            status_codes = result["status_codes"]
            has_unsuccessful_status = not status_codes or any(
                not 200 <= int(status) < 300 for status in status_codes
            )
            if result["error_rate"] != 0 or has_unsuccessful_status:
                failures.append(
                    f'{scenario["name"]} {side}: error_rate={result["error_rate"]}, '
                    f'status_codes={status_codes}, errors={result["errors"]}'
                )
    return failures


def reserve_port():
    listener = socket.socket()
    try:
        listener.bind(("127.0.0.1", 0))
    except Exception:
        listener.close()
        raise
    return listener


def database_url(admin_url, database):
    parsed = urllib.parse.urlsplit(admin_url)
    return urllib.parse.urlunsplit(parsed._replace(path=f"/{database}"))


def write_secret(path, size=32):
    path.write_text(base64.b64encode(secrets.token_bytes(size)).decode() + "\n")
    path.chmod(0o600)


def process_environment(run_dir, database, valkey, origin, observability):
    environment = {name: value for name, value in os.environ.items()
                   if not name.startswith("OLP_")}
    environment.update({
        "OLP_DATABASE_URL": database, "OLP_VALKEY_URL": valkey,
        "OLP_LISTEN_ADDR": origin.removeprefix("http://"),
        "OLP_OBSERVABILITY_LISTEN_ADDR": observability.removeprefix("http://"),
        "OLP_PUBLIC_ORIGIN": origin, "OLP_CONSOLE_DIR": str(run_dir / "console"),
        "OLP_MEDIA_SPOOL_DIR": str(run_dir / "spool"),
        "OLP_MASTER_KEY_FILE": str(run_dir / "master-key"),
        "OLP_AUTH_HMAC_KEY_FILE": str(run_dir / "auth-hmac-key"),
        "OLP_BOOTSTRAP_TOKEN_FILE": str(run_dir / "bootstrap-token"),
        "OLP_PROVIDER_EGRESS_ALLOW_CIDRS": "127.0.0.0/8,::1/128",
        "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1,localhost", "RUST_LOG": "olp=warn",
    })
    return environment


def await_live(observability, process, log_path):
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError(f"olp exited during startup:\n{log_path.read_text(errors='replace')}")
        try:
            if request(observability + "/health/live", timeout=1)[0] == 200:
                return
        except (OSError, urllib.error.URLError):
            pass
        time.sleep(0.2)
    raise RuntimeError("olp did not become live within 30 seconds")


def metrics_text(observability):
    with urllib.request.urlopen(observability + "/metrics", timeout=10) as response:
        return response.read().decode()


def admission_rejections(metrics):
    matches = re.findall(r'^olp_http_admission_rejections_total\{surface="[^"]+"\} (\d+)$',
                         metrics, re.MULTILINE)
    if len(matches) != 2:
        raise RuntimeError("admission rejection metrics were not present")
    return sum(int(value) for value in matches)


def machine_metadata():
    cpu = "unknown"
    cpuinfo = Path("/proc/cpuinfo")
    if cpuinfo.exists():
        match = re.search(r"^model name\s*:\s*(.+)$", cpuinfo.read_text(), re.MULTILINE)
        if match:
            cpu = match.group(1)
    return {"platform": platform.platform(), "cpu": cpu, "logical_cpus": os.cpu_count(),
            "rustc": subprocess.run(["rustc", "--version"], capture_output=True, text=True,
                                     check=True).stdout.strip(), "oha": "1.12.0"}


def source_metadata(repo):
    excluded_paths = [":(exclude)bench/results/**", ":(exclude)bench/baseline/**",
                      ":(exclude)bench/comment.md"]
    sha = subprocess.run(["git", "rev-parse", "HEAD"], cwd=repo, check=True,
                         capture_output=True, text=True).stdout.strip()
    status = subprocess.run(
        ["git", "status", "--porcelain", "--untracked-files=all", "--", ".", *excluded_paths],
        cwd=repo, check=True, capture_output=True,
    ).stdout
    dirty = bool(status)
    if not dirty:
        return sha, False, sha
    digest = hashlib.sha256(sha.encode())
    digest.update(subprocess.run(
        ["git", "diff", "--binary", "HEAD", "--", ".", *excluded_paths], cwd=repo,
        check=True, capture_output=True,
    ).stdout)
    untracked = subprocess.run(
        ["git", "ls-files", "--others", "--exclude-standard", "-z", "--", ".", *excluded_paths],
        cwd=repo, check=True, capture_output=True,
    ).stdout.split(b"\0")
    for relative in sorted(path for path in untracked if path):
        digest.update(relative)
        digest.update(b"\0")
        digest.update((repo / os.fsdecode(relative)).read_bytes())
    return sha, True, digest.hexdigest()


def terminate(process):
    if process and process.poll() is None:
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()


def parse_args():
    parser = argparse.ArgumentParser(description="Run OpenLLMProxy performance scenarios")
    duration = os.getenv("BENCH_DURATION", "60").removesuffix("s")
    parser.add_argument("--duration", type=int, default=int(duration))
    parser.add_argument("--output", type=Path, default=None)
    parser.add_argument("--print-output-path", action="store_true")
    return parser.parse_args()


def main():
    args = parse_args()
    repo = Path(__file__).resolve().parent.parent
    sha, source_dirty, source_fingerprint = source_metadata(repo)
    output_name = f"{sha}-dirty-{source_fingerprint[:12]}.json" if source_dirty else f"{sha}.json"
    output = args.output or repo / "bench" / "results" / output_name
    if args.print_output_path:
        print(output)
        return
    admin = os.getenv("OLP_BENCH_DATABASE_ADMIN_URL",
                      "postgres://olp_test:olp_test@localhost:5433/postgres")
    valkey = os.getenv("OLP_BENCH_VALKEY_URL", "redis://localhost:6379/15")
    database_name = f"olp_bench_{os.getpid()}_{secrets.token_hex(4)}"
    valkey_cli = os.environ.get("OLP_BENCH_VALKEY_CLI", "valkey-cli")
    run_dir = Path(tempfile.mkdtemp(prefix="olp-bench-"))
    process = None
    port_reservations = []
    mock = MockServer(("127.0.0.1", 0), MockHandler)
    threading.Thread(target=mock.serve_forever, daemon=True).start()
    try:
        subprocess.run(["psql", admin, "-v", "ON_ERROR_STOP=1", "-c",
                        f'CREATE DATABASE "{database_name}"'], check=True, capture_output=True)
        subprocess.run([valkey_cli, "-u", valkey, "FLUSHDB"], check=True, capture_output=True)
        for directory in (run_dir / "console", run_dir / "spool"):
            directory.mkdir()
        write_secret(run_dir / "master-key")
        write_secret(run_dir / "auth-hmac-key")
        write_secret(run_dir / "bootstrap-token")
        setup_token = (run_dir / "bootstrap-token").read_text().strip()
        port_reservations.append(reserve_port())
        port_reservations.append(reserve_port())
        origin = f"http://127.0.0.1:{port_reservations[0].getsockname()[1]}"
        observability = f"http://127.0.0.1:{port_reservations[1].getsockname()[1]}"
        mock_origin = f"http://127.0.0.1:{mock.server_port}"
        database = database_url(admin, database_name)
        environment = process_environment(run_dir, database, valkey, origin, observability)
        binary = os.environ["OLP_BENCH_BIN"]
        subprocess.run([binary, "migrate"], check=True, env=environment)
        log_path = run_dir / "olp.log"
        log = log_path.open("w")
        for reservation in port_reservations:
            reservation.close()
        process = subprocess.Popen([binary, "all"], env=environment, stdout=log, stderr=subprocess.STDOUT)
        await_live(observability, process, log_path)
        key = configure_gateway(origin, setup_token, mock_origin)
        before = metrics_text(observability)
        scenarios = benchmark_scenarios(origin, mock_origin, key, args.duration)
        after = metrics_text(observability)
        rejections = admission_rejections(after)
        failures = scenario_errors(scenarios)
        invalid_reasons = failures.copy()
        if rejections != 0:
            invalid_reasons.insert(0, f"observed {rejections} admission rejections")
        result = {"schema_version": 2, "git_sha": sha, "source_dirty": source_dirty,
                  "source_fingerprint": source_fingerprint, "duration_seconds": args.duration,
                  "build_profile": os.getenv("OLP_BENCH_BUILD_PROFILE", "external"),
                  "machine": machine_metadata(), "mock": {"unary_delay_ms": 200,
                  "stream_tokens": 50}, "admission_rejections": rejections,
                  "valid": not invalid_reasons, "invalid_reasons": invalid_reasons,
                  "scenarios": scenarios, "metrics": {"before": before, "after": after}}
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text(json.dumps(result, indent=2) + "\n")
        print(output)
        if invalid_reasons:
            raise RuntimeError("benchmark invalid: " + "; ".join(invalid_reasons))
    finally:
        for reservation in port_reservations:
            reservation.close()
        terminate(process)
        mock.shutdown()
        subprocess.run([valkey_cli, "-u", valkey, "FLUSHDB"], capture_output=True)
        subprocess.run(["psql", admin, "-c", f'DROP DATABASE IF EXISTS "{database_name}" WITH (FORCE)'],
                       capture_output=True)
        shutil.rmtree(run_dir, ignore_errors=True)


if __name__ == "__main__":
    try:
        main()
    except (KeyError, RuntimeError, subprocess.CalledProcessError) as error:
        print(f"benchmark failed: {error}", file=sys.stderr)
        raise SystemExit(1)
