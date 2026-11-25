"""The Git export a storage v2 gateway keeps of each notebook.

A notebook lives in Postgres and the blob store; its Git form is the bare export
DATA/exports/NOTEBOOK.git, which the gateway brings up to each new revision in
the background. The smokes prove a notebook's history through it, as they once
did through its working tree.
"""
import subprocess
import time


def wait_exported(container, notebook, revision, timeout=60):
    """Waits until the container's export of notebook has revision, the
    notebook's head, on main, then proves the export whole with git fsck."""
    git = ['docker', 'exec', container, 'git', '--git-dir', '/data/exports/' + notebook + '.git']
    deadline = time.monotonic() + timeout
    while True:
        result = subprocess.run([*git, 'rev-parse', '--verify', '--quiet', 'refs/heads/main'], text=True, capture_output=True)
        if result.stdout.strip() == revision:
            break
        if time.monotonic() > deadline:
            raise AssertionError(f'the export of {notebook} in {container} has {result.stdout.strip() or "no main"}, not {revision}: '
                                 + result.stderr.strip())
        time.sleep(0.2)
    subprocess.check_output([*git, 'fsck', '--full', '--strict', '--no-dangling'], text=True, stderr=subprocess.STDOUT)
