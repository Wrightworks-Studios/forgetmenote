"""Run trusted administrator-installed Python. This is not a sandbox."""
import importlib.util
import os
import pathlib
import signal
import sys
import threading

sys.path.insert(0, str(pathlib.Path(__file__).parent))
from fmn_host import Host

# The Go parent holds the write end; EOF also detects an abrupt server crash.
# The server gives each run its own process group. Packages must not daemonize.
def parent_lifetime():
    try:
        os.read(3, 1)
    finally:
        os.killpg(os.getpgrp(), signal.SIGKILL)

threading.Thread(target=parent_lifetime, daemon=True).start()
# Ordinary print output is diagnostic, never protocol traffic.
sys.stdout = sys.stderr

def main():
    root = pathlib.Path(sys.argv[1]).resolve(strict=True)
    source = (root / sys.argv[2]).resolve(strict=True)
    source.relative_to(root)
    sys.path.insert(0, str(root))
    sys.path.insert(0, str(source.parent))
    spec = importlib.util.spec_from_file_location('automation_package', source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    host = Host()
    result = module.main(host)
    host.call('complete', 'run.complete', result=result)

if __name__ == '__main__':
    main()
