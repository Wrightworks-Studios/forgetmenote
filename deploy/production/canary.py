#!/usr/bin/env python3
"""Read-only acceptance cases against an authorized parallel pilot.

Print status and latency only; tokens, query text and returned data stay private.
No automatic deployment, notebook updates or Slack delivery.
"""
import argparse
import json
from pathlib import Path
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

READS = {"list_projects", "notebook_map", "search_notebooks", "read_document",
         "find_recipes", "list_data_connections", "list_data_operations"}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Client:
    def __init__(self, server, token):
        url = urllib.parse.urlsplit(server)
        if (url.username or url.password or url.query or url.fragment or
                url.path not in ("", "/") or not url.hostname or
                url.scheme != "https" and not (url.scheme == "http" and url.hostname in ("127.0.0.1", "localhost", "::1"))):
            raise ValueError("use an HTTPS server origin (loopback HTTP is allowed)")
        if not token or "\n" in token or "\r" in token:
            raise ValueError("invalid credential file")
        self.server, self.token = server.rstrip("/"), token
        self.opener = urllib.request.build_opener(NoRedirect(), urllib.request.ProxyHandler({}))

    def action(self, name, args):
        if name not in READS | {"call_data_operation"}:
            raise ValueError("canary refuses a write or unknown action")
        request = urllib.request.Request(self.server + "/api/v1/actions/" + name,
            json.dumps(args).encode(), {"Authorization": "Bearer " + self.token, "Content-Type": "application/json"})
        try:
            response = self.opener.open(request, timeout=120)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            body = response.read(8 * 1024 * 1024 + 1)
            if len(body) > 8 * 1024 * 1024:
                raise ValueError("canary result exceeds inspection capacity")
            return response.status, json.loads(body)


def pointer(value, path):
    if path and not path.startswith("/"):
        raise ValueError("invalid assertion pointer")
    for part in path.split("/")[1:]:
        part = part.replace("~1", "/").replace("~0", "~")
        value = value[int(part)] if isinstance(value, list) else value[part]
    return value


def check_case(client, case):
    name, args = case["action"], dict(case.get("arguments", {}))
    expected = case.get("status", 200)
    if name not in READS | {"call_data_operation"}:
        raise ValueError("canary refuses a write or unknown action")
    if name == "call_data_operation":
        if args.get("profilePath") or args.get("profileRevision"):
            raise ValueError("canary must not trigger recipe maintenance")
        status, tools = client.action("list_data_operations", {"connection": args["connection"]})
        allowed = next((op for op in tools if op.get("name") == args["operation"] and op.get("readOnly")), None) if status == 200 else None
        if not allowed:
            raise ValueError("data operation is unavailable or not read-only")
        args["schemaHash"] = allowed["schemaHash"]
    started = time.monotonic()
    status, value = client.action(name, args)
    if status != expected:
        raise ValueError("unexpected HTTP status")
    if time.monotonic() - started > case.get("maxSeconds", 120):
        raise ValueError("latency exceeded acceptance bound")
    for assertion in case.get("assertions", []):
        actual = pointer(value, assertion["pointer"])
        if "equals" in assertion and actual != assertion["equals"]:
            raise ValueError("value did not match")
        if "contains" in assertion and assertion["contains"] not in actual:
            raise ValueError("expected evidence missing")
        if assertion.get("notEmpty") and not actual:
            raise ValueError("expected nonempty result")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--server", required=True)
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--cases", type=Path, required=True)
    args = parser.parse_args()
    cases = json.loads(args.cases.read_text())
    if not isinstance(cases, list) or not 1 <= len(cases) <= 100:
        raise ValueError("provide 1–100 acceptance cases")
    client = Client(args.server, args.token_file.read_text().strip())
    passed = 0
    for index, case in enumerate(cases, 1):
        started = time.monotonic()
        try:
            check_case(client, case)
            passed += 1
            print(f"Case {index}: PASS ({time.monotonic() - started:.2f}s)")
        except Exception:
            # A remote exception could contain credentials or source data.
            print(f"Case {index}: FAIL ({time.monotonic() - started:.2f}s)")
    print(f"{passed}/{len(cases)} acceptance cases passed")
    return 0 if passed == len(cases) else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception:
        print("Canary setup failed; verify the private credential, case file and server.", file=sys.stderr)
        sys.exit(1)
