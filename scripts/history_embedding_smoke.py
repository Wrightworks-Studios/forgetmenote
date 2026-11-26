#!/usr/bin/env python3
"""Exercise historical search with a real Ollama model in a disposable notebook."""
import argparse
import json
import subprocess
import time
import urllib.request
import uuid

from smoke_app import free_port, wait_ready
from smoke_database import Database


def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="forgetmenote:local")
    parser.add_argument("--model", required=True)
    parser.add_argument("--dimensions", type=int, default=768)
    parser.add_argument("--network", default="forgetmenote_embeddings")
    parser.add_argument("--endpoint", default="http://forgetmenote-ollama:11434/api/embed")
    args = parser.parse_args()
    name = "forgetmenote-history-check-" + uuid.uuid4().hex[:10]
    volume = name + "-data"
    port = free_port()
    origin = f"http://localhost:{port}"
    run("docker", "volume", "create", volume)
    db = Database()
    try:
        db.migrate(args.image)
        # The app starts on its database's network and then joins the embedding network.
        run("docker", "run", "-d", "--name", name, *db.args(),
            "--read-only", "--tmpfs", "/tmp:size=64m", "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges:true", "-p", f"127.0.0.1:{port}:8080",
            "-e", f"PUBLIC_URL={origin}", "-e", f"EMBEDDING_URL={args.endpoint}",
            "-v", f"{volume}:/data", args.image)

        def ready():
            wait_ready(origin, name, 15)

        ready()
        run("docker", "network", "connect", args.network, name)
        token = run("docker", "exec", name, "forgetmenote", "token")

        def request(path, body=None):
            data = None if body is None else json.dumps(body).encode()
            req = urllib.request.Request(origin + "/p/default" + path, data,
                {"Authorization": "Bearer " + token, "Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=45) as response:
                return json.load(response)

        def eventually(check, description, timeout=150):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if check():
                    print("PASS " + description, flush=True)
                    return
                time.sleep(2)
            raise AssertionError(description + " did not complete")

        def search(text, mode="text", scope="current"):
            return request("/api/knowledge/hybrid-search",
                           {"text": text, "mode": mode, "scope": scope, "limit": 20})["matches"]

        original = "# Orchard\nMoonpears grow in a secret orchard beneath the northern lights.\n\n## Lighthouse\nA lighthouse guides fishing boats safely to the harbor.\n"
        replacement = "# Orchard\nThe garden now grows ordinary carrots and potatoes.\n\n## Lighthouse\nA lighthouse guides fishing boats safely to the harbor.\n"
        first = request("/api/note", {"path": "ideas.md", "content": original, "revision": ""})
        request("/api/knowledge/config", {"config": {"enabled": True, "model": args.model,
            "dimensions": args.dimensions, "passageRunes": 1400,
            "splitter": "markdown-heading-v1", "endpoint": True}, "rebuild": False})
        eventually(lambda: request("/api/knowledge/status")["state"] == "ready",
                   "initial current embeddings are ready")
        assert search("secret orchard beneath northern lights", "meaning")
        changed = request("/api/note", {"path": "ideas.md", "content": replacement,
                                        "revision": first["revision"]})
        assert not search("Moonpears"), "current search exposed removed content"
        eventually(lambda: bool(search("Moonpears", scope="history")),
                   "historical text search finds the removed idea")
        eventually(lambda: any("Moonpears" in m.get("text", "")
                               for m in search("secret orchard beneath northern lights", "meaning", "history")),
                   "real-model historical meaning search finds the removed idea")
        head = request("/api/status")["revision"]
        assert head == changed["revision"], "search changed notebook history"
        run("docker", "restart", name)
        ready()
        eventually(lambda: bool(search("Moonpears", scope="history")),
                   "historical text index survives an application restart")
        eventually(lambda: any("Moonpears" in m.get("text", "")
                               for m in search("secret orchard beneath northern lights", "meaning", "history")),
                   "historical vectors remain searchable after restart")
        assert not search("Moonpears"), "restart mixed historical content into current search"
        assert request("/api/status")["revision"] == head
        print("PASS current search and Git history remain unchanged", flush=True)
    finally:
        # These unique resources were created above; never target production data.
        subprocess.run(["docker", "rm", "-f", name], check=False, capture_output=True)
        db.close()
        subprocess.run(["docker", "volume", "rm", volume], check=True, capture_output=True)


if __name__ == "__main__":
    main()
