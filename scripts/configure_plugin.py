#!/usr/bin/env python3
"""Compatibility wrapper for the application's shared companion package generator."""
import argparse
from pathlib import Path
import shutil
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("origin", help="Canonical HTTPS instance origin")
parser.add_argument("--output", default="plugin-local/forgetmenote")
parser.add_argument("--name", default="ForgetMeNote")
parser.add_argument("--client", choices=("codex", "claude"), default="codex")
parser.add_argument("--zip", action="store_true")
parser.add_argument("--binary", help="Use a built ForgetMeNote binary instead of go run")
args = parser.parse_args()
root = Path(__file__).resolve().parent.parent
command = [args.binary] if args.binary else [shutil.which("go") or "go", "run", "./cmd/forgetmenote"]
command += ["plugin", "package", "--origin", args.origin, "--name", args.name,
            "--client", args.client, "--output", str(Path(args.output).absolute())]
if args.zip:
    command.append("--zip")
raise SystemExit(subprocess.run(command, cwd=root).returncode)
