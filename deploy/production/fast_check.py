#!/usr/bin/env python3
"""Affected race tests for PR feedback; never a production release receipt.

Selection is verification_plan's, which routine releases share; this tier adds
committed, staged, unstaged and untracked inputs and produces no receipt."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
import re
from pathlib import Path
import subprocess

from verification import go_package_inputs, web_shards
from verification_plan import affected_go_packages, feature_of, selected_web_tests


def output(root, *args):
    return subprocess.check_output(args, cwd=root, text=True)


def plan(root, base=None):
    root = Path(root).resolve()
    packages = output(root, 'go', 'list', '-race', './...').splitlines()
    complete = {'mode': 'full', 'packages': packages, 'changed_paths': [], 'features': []}
    if not base:
        return {**complete, 'reason': 'No baseline: all race tests'}
    try:
        base = output(root, 'git', 'merge-base', '--', 'HEAD', base).strip()
        changed = set(output(root, 'git', 'diff', '--name-only', '--no-renames', '-z', base).split('\0'))
        changed.update(output(root, 'git', 'ls-files', '--others', '--exclude-standard', '-z').split('\0'))
        changed.discard('')
        inventory = output(root, 'go', 'list', '-race', '-deps', '-test', '-json', './...')
        inputs = go_package_inputs(root, packages, inventory)
    except (subprocess.CalledProcessError, ValueError, KeyError):
        return {**complete, 'reason': 'Unverified baseline or inputs: all race tests'}
    selection = {'mode': 'routine', 'changed_paths': sorted(changed),
                 'features': sorted({feature for feature in map(feature_of, changed) if feature})}
    selected, reason = affected_go_packages(selection, inputs)
    return {**selection, 'packages': sorted(selected), 'reason': reason}


def commands(root, selection):
    """One process per package; the web package runs its selected cases in isolated batches."""
    result = []
    for package in selection['packages']:
        if not package.endswith('/internal/web'):
            result.append(['go', 'test', '-race', '-count=1', '-timeout', '45m', package])
            continue
        listed = output(root, 'go', 'test', '-race', package, '-list', '^(Test|Example|Fuzz)')
        available = [name for name in listed.splitlines() if re.fullmatch(r'(Test|Example|Fuzz)\w*', name)]
        names, reason = selected_web_tests(root, selection, available)
        selection['web'] = {'reason': reason, 'tests': names, 'omitted': len(available) - len(names)}
        for batch in web_shards(names, 4 if len(names) < 100 else 16):
            result.append(['go', 'test', '-race', '-count=1', '-timeout', '45m', package,
                           '-run', '^(?:' + '|'.join(batch) + ')$'])
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base', default=os.environ.get('BASE') or None)
    parser.add_argument('--jobs', type=int, default=2, choices=range(1, 9))
    parser.add_argument('--plan', action='store_true')
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    selection = plan(root, args.base)
    planned = commands(root, selection)
    print(json.dumps(selection), flush=True)
    if args.plan:
        return 0
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        results = list(pool.map(lambda command: subprocess.run(command, cwd=root).returncode, planned))
    return 1 if any(results) else 0


if __name__ == '__main__':
    raise SystemExit(main())
