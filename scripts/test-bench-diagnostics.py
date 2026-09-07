#!/usr/bin/env python3
import contextlib
import importlib.util
import io
import json
import os
import runpy
import subprocess
import sys
import tempfile
import traceback
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

SCRIPT = Path(__file__).with_name("bench.py")
spec = importlib.util.spec_from_file_location("bench", SCRIPT)
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)

FAKE_PASSWORD = "FAKE_BENCH_PASSWORD_NOT_A_CREDENTIAL"
FAKE_TOKEN = "FAKE_BENCH_TOKEN_NOT_A_CREDENTIAL"
ADMIN = f"postgres://fixture:{FAKE_PASSWORD}@invalid/postgres"
VALKEY = f"redis://fixture:{FAKE_PASSWORD}@invalid/15"
ENVIRONMENT = {
    "OLP_BENCH_DATABASE_ADMIN_URL": ADMIN,
    "OLP_BENCH_VALKEY_URL": VALKEY,
    "OLP_BENCH_VALKEY_CLI": "valkey-cli",
    "OLP_BENCH_BIN": "fixture-olp",
}


class BenchmarkDiagnostics(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.commands = []
        self.failed_program = None
        self.output = self.root / "result.json"
        self.run_dir = self.root / "run"
        self.run_dir.mkdir()
        self.enterContext(patch.dict(os.environ, ENVIRONMENT))
        self.enterContext(patch.object(sys, "argv", [str(SCRIPT), "--output", str(self.output)]))
        self.enterContext(patch("tempfile.mkdtemp", return_value=str(self.run_dir)))
        self.enterContext(patch("subprocess.run", side_effect=self.run_command))

    def run_command(self, command, **options):
        self.commands.append((command, options))
        if command[0] == self.failed_program:
            output = f"{FAKE_PASSWORD} {FAKE_TOKEN} {command}".encode()
            if options.get("check"):
                raise subprocess.CalledProcessError(23, command, output=output, stderr=output)
            return subprocess.CompletedProcess(command, 23, output, output)
        if command[:2] == ["git", "rev-parse"]:
            return subprocess.CompletedProcess(command, 0, "fixture-sha\n", "")
        output = "{}" if options.get("text") else b""
        return subprocess.CompletedProcess(command, 0, output, output)

    def assert_private(self, diagnostic):
        for forbidden in (FAKE_PASSWORD, FAKE_TOKEN, ADMIN, VALKEY, "--output-format",
                          "Authorization: Bearer", "ON_ERROR_STOP=1", "Traceback"):
            self.assertNotIn(forbidden, diagnostic)

    def assert_cleanup(self):
        commands = [command for command, _ in self.commands]
        self.assertEqual(commands[-2], ["valkey-cli", "-u", VALKEY, "FLUSHDB"])
        self.assertEqual(commands[-1][:3], ["psql", ADMIN, "-c"])
        self.assertIn("DROP DATABASE IF EXISTS", commands[-1][3])
        self.assertFalse(self.run_dir.exists())

    def test_entrypoint_reports_database_and_valkey_status_without_secrets(self):
        for program, action in (("psql", "PostgreSQL database creation"),
                                ("valkey-cli", "Valkey reset")):
            with self.subTest(program=program):
                self.run_dir.mkdir(exist_ok=True)
                self.failed_program = program
                stderr = io.StringIO()
                with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as raised:
                    runpy.run_path(str(SCRIPT), run_name="__main__")
                self.assertEqual(raised.exception.code, 1)
                self.assertEqual(stderr.getvalue(),
                                 f"benchmark failed: {action} failed with exit status 23\n")
                self.assert_private(stderr.getvalue())
                self.assert_cleanup()

    def test_entrypoint_suppresses_other_subprocess_arguments_and_output(self):
        self.failed_program = "git"
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as raised:
            runpy.run_path(str(SCRIPT), run_name="__main__")
        self.assertEqual(raised.exception.code, 1)
        self.assertEqual(stderr.getvalue(), "benchmark subprocess failed with exit status 23\n")
        self.assert_private(stderr.getvalue())

    def prepare_gateway(self):
        process = Mock()
        process.poll.return_value = None
        self.enterContext(patch.object(bench.subprocess, "Popen", return_value=process))
        self.enterContext(patch.object(bench, "await_live"))
        self.enterContext(patch.object(bench, "configure_gateway", return_value=FAKE_TOKEN))
        self.enterContext(patch.object(bench, "metrics_text", return_value="fixture metrics"))
        self.enterContext(patch.object(bench, "admission_rejections", return_value=0))
        self.enterContext(patch.object(bench, "machine_metadata", return_value={}))
        return process

    def test_load_driver_failure_keeps_status_and_cleans_up(self):
        process = self.prepare_gateway()
        self.failed_program = "oha"
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stderr), self.assertRaises(RuntimeError) as raised:
            bench.main()
        self.assertEqual(str(raised.exception), "oha load generation failed with exit status 23")
        diagnostic = "".join(traceback.format_exception(raised.exception))
        self.assert_private(diagnostic)
        self.assertTrue(raised.exception.__suppress_context__)
        process.send_signal.assert_called_once()
        process.wait.assert_called_once_with(timeout=10)
        self.assert_cleanup()

    def test_authenticated_load_driver_failure_omits_token(self):
        self.failed_program = "oha"
        with self.assertRaises(RuntimeError) as raised:
            bench.run_oha("http://invalid", 1, 2, {"model": "fixture"}, FAKE_TOKEN)
        self.assertEqual(str(raised.exception), "oha load generation failed with exit status 23")
        self.assert_private(str(raised.exception))
        self.assertIn(f"Authorization: Bearer {FAKE_TOKEN}", self.commands[-1][0])

    def test_migration_failure_is_captured_and_cleans_up(self):
        self.failed_program = "fixture-olp"
        with self.assertRaises(RuntimeError) as raised:
            bench.main()
        self.assertEqual(str(raised.exception), "database migration failed with exit status 23")
        self.assert_private(str(raised.exception))
        migration = next(options for command, options in self.commands
                         if command == ["fixture-olp", "migrate"])
        self.assertTrue(migration["capture_output"])
        self.assert_cleanup()

    def test_startup_exit_does_not_read_subprocess_log(self):
        process = Mock()
        process.poll.return_value = 23
        with self.assertRaises(RuntimeError) as raised:
            bench.await_live("http://invalid", process)
        self.assertEqual(str(raised.exception), "olp startup failed with exit status 23")
        self.assert_private(str(raised.exception))

    def test_successful_run_still_writes_results_and_cleans_up(self):
        process = self.prepare_gateway()
        self.enterContext(patch.object(bench, "benchmark_scenarios", return_value=[]))
        with contextlib.redirect_stdout(io.StringIO()):
            bench.main()
        self.assertTrue(json.loads(self.output.read_text())["valid"])
        process.send_signal.assert_called_once()
        self.assert_cleanup()
        self.assertEqual(bench.run_oha("http://invalid", 1, 2, token=FAKE_TOKEN), {})


if __name__ == "__main__":
    unittest.main()
