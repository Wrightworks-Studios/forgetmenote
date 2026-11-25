#!/usr/bin/env python3
"""Register only this companion in a personal OpenAI marketplace. No authentication."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import tempfile

FILES = {"plugin.json", "mcp.json", ".codex-plugin/plugin.json", ".mcp.json",
         "skills/workspace/SKILL.md", "instance.json", "README.md", "install.py", "assets/icon.png"}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON key: " + key)
        result[key] = value
    return result


def read_json(path):
    return json.loads(path.read_text(), object_pairs_hook=unique_object)


def safe_path(path):
    for part in (path, *path.parents):
        if part.is_symlink():
            raise ValueError("Refusing symlink: " + str(part))


def inventory(root, installed=False):
    safe_path(root)
    files = set()
    for path in root.rglob("*"):
        safe_path(path)
        if path.is_file():
            files.add(path.relative_to(root).as_posix())
        elif not path.is_dir():
            raise ValueError("Not a regular package file: " + str(path))
    # Current users may upgrade a pre-branding companion. New downloads always
    # include the icon; do not accept missing files in the source package.
    legacy = (installed and files == FILES - {"assets/icon.png"}
              and read_json(root / "instance.json").get("version") in ("0.2.0", "0.2.1"))
    if files != FILES and not legacy:
        raise ValueError("Package has missing or unexpected files; use a fresh download")


def atomic_json(path, value):
    fd, name = tempfile.mkstemp(prefix=".forgetmenote-", dir=path.parent)
    try:
        with os.fdopen(fd, "w") as out:
            json.dump(value, out, indent=2)
            out.write("\n")
            out.flush()
            os.fsync(out.fileno())
        os.replace(name, path)
    finally:
        if os.path.exists(name):
            os.unlink(name)


def install(source, home, uninstall=False):
    # Canonicalize the chosen roots (macOS /var is an OS alias). Links inside
    # either root are still refused before reading or changing package files.
    if source.is_symlink() or home.is_symlink():
        raise ValueError("Package and home directories must not be symlinks")
    source, home = source.resolve(), home.resolve()
    inventory(source)
    instance = read_json(source / "instance.json")
    identifier, origin = instance["id"], instance["origin"]
    if (not re.fullmatch(r"forgetmenote-[0-9a-f]{16}", identifier)
            or identifier != "forgetmenote-" + hashlib.sha256(origin.encode()).hexdigest()[:16]
            or instance["endpoint"] != origin + "/mcp"):
        raise ValueError("Invalid instance identity")
    if read_json(source / "plugin.json")["name"] != identifier:
        raise ValueError("Plugin identity does not match instance")
    target = home / ".codex" / "plugins" / identifier
    catalog = home / ".agents" / "plugins" / "marketplace.json"
    safe_path(target)
    safe_path(catalog)
    catalog.parent.mkdir(parents=True, exist_ok=True)
    lock = catalog.parent / ".forgetmenote-install.lock"
    fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    os.close(fd)
    stage = backup = None
    try:
        data = read_json(catalog) if catalog.exists() else {"name": "personal-forgetmenote", "plugins": []}
        if not isinstance(data, dict) or not isinstance(data.get("name"), str) or not data["name"] or not isinstance(data.get("plugins"), list):
            raise ValueError("Malformed personal marketplace; existing configuration was not changed")
        names = [p.get("name") if isinstance(p, dict) else None for p in data["plugins"]]
        if any(not isinstance(name, str) or not name for name in names) or len(names) != len(set(names)):
            raise ValueError("Malformed or duplicate marketplace entries")
        own_source = {"source": "local", "path": "./.codex/plugins/" + identifier}
        own = next((p for p in data["plugins"] if p["name"] == identifier), None)
        if own is not None and own.get("source") != own_source:
            raise ValueError("Conflicting registration for " + identifier)
        if uninstall:
            if own is not None:
                data["plugins"].remove(own)
                atomic_json(catalog, data)
            return "Registration removed. Package files and OAuth credentials retained. Restart the client."
        if target.exists():
            inventory(target, installed=True)
            installed = read_json(target / "instance.json")
            if installed.get("id") != identifier or installed.get("origin") != origin:
                raise ValueError("Conflicting installed package; existing files were not changed")
        if source.resolve() == target.resolve():
            raise ValueError("Install from a separate extracted download directory")
        target.parent.mkdir(parents=True, exist_ok=True)
        stage = Path(tempfile.mkdtemp(prefix=identifier + "-", dir=target.parent))
        for name in sorted(FILES):
            path = stage / name
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source / name, path)
        if target.exists():
            backup = Path(tempfile.mkdtemp(prefix=identifier + "-previous-", dir=target.parent))
            backup.rmdir()
            target.rename(backup)
        try:
            stage.rename(target)
            stage = None
            if own is None:
                data["plugins"].append({"name": identifier, "source": own_source,
                                        "policy": {"installation": "AVAILABLE", "authentication": "ON_INSTALL"},
                                        "category": "Productivity"})
            atomic_json(catalog, data)
        except Exception:
            if stage is None:
                shutil.rmtree(target)
            if backup is not None:
                backup.rename(target)
                backup = None
            raise
        if backup is not None:
            shutil.rmtree(backup)
        return "Registered " + identifier + ". Restart the desktop app, install from your personal marketplace, then sign in."
    finally:
        if stage is not None:
            shutil.rmtree(stage)
        lock.unlink()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("install", "uninstall"))
    parser.add_argument("--home", type=Path, default=Path.home(), help="Client home (default: your home directory)")
    args = parser.parse_args()
    try:
        print(install(Path(__file__).absolute().parent, args.home.absolute(), args.action == "uninstall"))
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.exit(1, "Cannot change companion registration: " + str(error) + "\n")


if __name__ == "__main__":
    main()
