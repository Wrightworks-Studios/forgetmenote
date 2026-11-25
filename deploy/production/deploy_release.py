#!/usr/bin/env python3
"""Deploy one verified release with one command, from any signed-in host.

Preflights the owner and the gateways together, deploys the owner, rolls the
gateways, then waits for the upgrade backup: the order deploying.md requires.
Release receipts and deployment records live in the recovery bucket, so a host
keeps nothing between runs and needs only its sign-ins, Docker and a full checkout.
"""
import argparse
import contextlib
import datetime as dt
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

import cutover
import deploy_instance as deploy


RECEIPTS = ('publication.json', 'verification.json')
RECORDS = ('deploy-*/*', 'gateways-*/*', 'preflight-*/*', 'release-*/*')
TOOLS = Path(__file__).resolve().parent
ROOT = TOOLS.parents[1]
HELP = """
  deploy_release.py store --owner-config CONFIG --release-dir DIR
      after publishing: keep the release's receipts in the recovery bucket
  deploy_release.py preflight --owner-config CONFIG --gateway-config CONFIG --image REF ...
      read-only: every check for both roles, all problems reported together
  deploy_release.py deploy --owner-config CONFIG --gateway-config CONFIG --image REF ...
      preflight, owner, gateways, then wait for the upgrade backup
  deploy_release.py resume --owner-config CONFIG --gateway-config CONFIG --image REF
      finish a stopped release: the owner, the gateways, then the upgrade backup
  deploy_release.py rollback-gateways --owner-config CONFIG --gateway-config CONFIG --image REF
      return this release's gateways to their previous image; the owner stays
  deploy_release.py storage-cutover --owner-config CONFIG --gateway-config CONFIG --image REF [--preflight]
      move an owner and gateways onto the first storage v2 release (storage-cutover.md);
      --resume continues a stopped one, --rollback [--discard-v2-writes] returns to the previous release
  deploy_release.py deployed --owner-config CONFIG
      JSON: the live release's revision and image, and the runtime files this checkout changed since
  deploy_release.py prepared --owner-config CONFIG --repository REPOSITORY
      the pinned image published for this checkout's runtime, alone on stdout

REF is the pinned `Deploy this image` reference release_image.py printed. A run that
stops leaves its records in the bucket. Read them first, then resume, roll the gateways
back, or start over with deploy --fresh-attempt. Do not rerun blindly.
"""


def stamp():
    return dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d-%H%M%S')


def run_tool(name, args):
    print('== ' + name, flush=True)
    return subprocess.run([sys.executable, str(TOOLS / name), *args]).returncode


def receipts_prefix(digest):
    return 'releases/' + digest.replace(':', '-') + '/'


def store_release(cloud, directory):
    release = deploy.release_record(directory)
    for name in RECEIPTS:
        cloud.store(receipts_prefix(release['registry_digest']) + name, directory / name)
    print('Stored the receipts of ' + release['reference'])


def fetch_release(cloud, image, directory):
    """The stored verified receipts of image, refused unless they describe exactly it."""
    prefix = receipts_prefix(image.rsplit('@', 1)[1])
    for name in RECEIPTS:
        found = cloud.command(cloud.aws + ['s3api', 'get-object', '--bucket', cloud.config['bucket'],
                                           '--key', prefix + name, str(directory / name)], allow_missing=True)
        if found is None:
            raise ValueError('No stored receipts for ' + image + '; run `deploy_release.py store` after publishing')
    if deploy.release_record(directory)['reference'] != image:
        raise ValueError('Stored receipts do not describe ' + image)


def records_uri(config):
    return 's3://' + config['bucket'] + '/deployments/'


def pull_records(cloud, root):
    cloud.command(cloud.aws + ['s3', 'sync', records_uri(cloud.config), str(root), '--only-show-errors'], timeout=600)


def push_records(cloud, root):
    """Add this host's records to the bucket. Sync never deletes, so failures stay evidence."""
    includes = [flag for pattern in RECORDS for flag in ('--include', pattern)]
    cloud.command(cloud.aws + ['s3', 'sync', str(root), records_uri(cloud.config), '--exclude', '*', *includes,
                               '--exclude', '*.tar.gz', '--exclude', '*.partial', '--sse', 'aws:kms',
                               '--sse-kms-key-id', cloud.config['kms_key'], '--only-show-errors'], timeout=600)


def owner_flags(args, release):
    flags = ['--config', args.owner_config, '--release-dir', str(release), '--compatibility', args.compatibility,
             '--timeout', str(args.timeout)]
    if args.compatibility_note:
        flags += ['--compatibility-note', args.compatibility_note]
    for job in args.accept_failed_backup:
        flags += ['--accept-failed-backup', job]
    if args.accepted_failure_note:
        flags += ['--accepted-failure-note', args.accepted_failure_note]
    return flags


def gateway_flags(args, release):
    return ['--config', args.gateway_config, '--release-dir', str(release), '--timeout', str(args.timeout)]


def preflight(args, release):
    # Both always run, so one pass shows everything blocking the release.
    return max(run_tool('deploy_instance.py', [*owner_flags(args, release), '--preflight']),
               run_tool('roll_gateways.py', [*gateway_flags(args, release), '--preflight']))


def roll_out(args, release):
    # A stopped attempt's receipt refuses a rerun until someone has read it, so starting over is explicit.
    attempt = ['--state-dir', str(args.root / ('deploy-' + stamp()))] if args.fresh_attempt else []
    # Gateways may call owner methods that older owners lack, so owners go first.
    return (preflight(args, release) or
            run_tool('deploy_instance.py', [*owner_flags(args, release), *attempt]) or
            run_tool('roll_gateways.py', gateway_flags(args, release)) or
            run_tool('deploy_instance.py', ['--config', args.owner_config, '--status', '--wait', '--timeout', str(args.timeout)]))


def newest_rollout(root):
    """The newest gateway rollout by its own clock: pulled records have no local history."""
    found = [(path.parent, json.loads(path.read_text())) for path in root.glob('gateways-*/rollout.json')]
    return max(found, key=lambda item: item[1]['started_at'], default=(None, {}))


def finish(args, release):
    """Finish a stopped release in deploy order; each tool refuses what it cannot safely continue."""
    owner = next((directory for _, directory, receipt in deploy.deployments(args.root) if receipt.get('image') == args.image), None)
    if owner is None:
        raise ValueError('No deployment of ' + args.image + ' to resume; a release that never started needs `deploy`')
    state = ['--config', args.owner_config, '--state-dir', str(owner), '--timeout', str(args.timeout)]
    directory, rollout = newest_rollout(args.root)
    # Without a rollout of this release, the newest one is the last release's: resuming it would change nothing.
    gateways = (['--config', args.gateway_config, '--state-dir', str(directory), '--resume', '--timeout', str(args.timeout)]
                if rollout.get('image') == args.image else gateway_flags(args, release))
    return (run_tool('deploy_instance.py', [*state, '--resume']) or
            run_tool('roll_gateways.py', gateways) or
            run_tool('deploy_instance.py', [*state, '--status', '--wait']))


def roll_back_gateways(args):
    directory, rollout = newest_rollout(args.root)
    if rollout.get('image') != args.image:
        raise ValueError('The newest gateway rollout is not ' + args.image + '; rolling back would skip a release')
    return run_tool('roll_gateways.py', ['--config', args.gateway_config, '--state-dir', str(directory), '--rollback',
                                         '--timeout', str(args.timeout)])


def storage_cutover(args, release):
    flags = ['--owner-config', args.owner_config, '--gateway-config', args.gateway_config, '--release-dir', str(release),
             '--timeout', str(args.timeout)]
    for name in ('preflight', 'resume', 'rollback', 'discard_v2_writes'):
        if getattr(args, name):
            flags.append('--' + name.replace('_', '-'))
    return run_tool('storage_cutover.py', flags)


def deployed(root):
    """The live release, and the runtime files the checkout changed since it: whether a release is due."""
    live = deploy.release_record(deploy.latest_ready_release(root))
    return {'revision': live['source_revision'], 'image': live['reference'],
            'runtime_changes': deploy.runtime_changes(ROOT, live['source_revision'], 'HEAD')}


def git(*args):
    return subprocess.check_output(['git', '-C', str(ROOT), *args], text=True).strip()


def prepared(repository, profile, limit=200):
    """The pinned image published for HEAD's runtime, by HEAD or the newest earlier commit with identical runtime inputs.

    Commits that change only tests, documents or operator tools publish nothing, and a
    release run may still be verifying HEAD; an image of older runtime is never offered.
    """
    if not (deploy.GHCR.fullmatch(repository) or deploy.ECR.fullmatch(repository)):
        raise ValueError('--repository must be ghcr.io/OWNER/NAME or ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME')
    head = git('rev-parse', 'HEAD')
    for commit in git('rev-list', '--first-parent', '--max-count=' + str(limit), 'HEAD').split():
        if commit != head and deploy.runtime_changes(ROOT, commit, head):
            break
        _, digest = deploy.locate_package(repository, 'main-' + commit, profile)
        if digest:
            return repository + '@' + digest
    raise ValueError('No image in ' + repository + ' has the runtime of ' + head + ' yet; wait for its release run')


def release_command(commands, name, description):
    command = commands.add_parser(name, help=description)
    command.add_argument('--owner-config', required=True, help='private deployment configuration of the owner')
    command.add_argument('--gateway-config', required=True, help='private configuration with the gateway service')
    command.add_argument('--image', required=True, help='the pinned reference release_image.py printed')
    command.add_argument('--timeout', type=int, default=900, help='maximum seconds for each readiness or snapshot wait')
    return command


def parse(argv):
    parser = argparse.ArgumentParser(description=__doc__, epilog=HELP, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command', required=True)
    store = commands.add_parser('store', help="keep a published release's receipts in the recovery bucket")
    store.add_argument('--owner-config', required=True, help='private deployment configuration of the owner')
    store.add_argument('--release-dir', required=True, type=Path, help='publication/verification directory of the release')
    for name, description in (('preflight', 'run every read-only check and change nothing'),
                              ('deploy', 'preflight, then deploy the owner and the gateways')):
        command = release_command(commands, name, description)
        command.add_argument('--compatibility', choices=('unknown', 'compatible', 'migration'), default='unknown')
        command.add_argument('--compatibility-note', default='', help='review evidence for the compatible fast path')
        command.add_argument('--accept-failed-backup', action='append', default=[], metavar='JOB',
                             help='reviewed failed backup job to ignore, once a later backup completed; repeatable')
        command.add_argument('--accepted-failure-note', default='', help='review evidence for --accept-failed-backup')
        if name == 'deploy':
            command.add_argument('--fresh-attempt', action='store_true',
                                 help='start over after reading a stopped attempt that resume refuses')
    release_command(commands, 'resume', "finish a stopped release from where it stopped")
    release_command(commands, 'rollback-gateways', "return this release's gateways to their previous image")
    move = release_command(commands, 'storage-cutover', 'move an owner and gateways onto the first storage v2 release')
    mode = move.add_mutually_exclusive_group()
    mode.add_argument('--preflight', action='store_true', help='run every read-only check and change nothing')
    mode.add_argument('--resume', action='store_true', help='continue the newest storage cutover from its recorded phase')
    mode.add_argument('--rollback', action='store_true', help='return both services to the previous release')
    move.add_argument('--discard-v2-writes', action='store_true', help='with --rollback after the new release served')
    live = commands.add_parser('deployed', help='print the live release and the runtime changes since it, as JSON')
    live.add_argument('--owner-config', required=True, help='private deployment configuration of the owner')
    image = commands.add_parser('prepared', help="print the pinned image published for this checkout's runtime")
    image.add_argument('--owner-config', required=True, help='private deployment configuration of the owner')
    image.add_argument('--repository', required=True, help='ghcr.io/OWNER/NAME or ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME')
    return parser.parse_args(argv)


def main(argv=None):
    args = parse(argv)
    os.umask(0o077)
    cloud = None
    code = 1
    try:
        config = deploy.configuration(args.owner_config)
        root = args.root = deploy.state_root(config, args.owner_config)
        if args.command in ('deployed', 'prepared'):
            # Read-only queries for a pipeline: the answer alone on stdout, and no records written back.
            with contextlib.redirect_stdout(sys.stderr), tempfile.TemporaryDirectory() as temporary:
                if args.command == 'deployed':
                    pull_records(deploy.Runtime(config, Path(temporary)), root)
                    answer = json.dumps(deployed(root))
                else:
                    answer = prepared(args.repository, config.get('aws_profile'))
            print(answer)
            return 0
        directory = root / ('release-' + stamp())
        if args.command != 'store':
            if not re.fullmatch(r'[^@\s]+@sha256:[0-9a-f]{64}', args.image):
                raise ValueError('--image must be the pinned reference release_image.py printed')
            if deploy.state_root(cutover.configuration(args.gateway_config), args.gateway_config) != root:
                raise ValueError('The owner and gateway configurations must share one state_root')
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
        cloud = deploy.Runtime(config, directory)
        if args.command == 'store':
            store_release(cloud, args.release_dir)
            return 0
        pull_records(cloud, root)
        if args.command == 'rollback-gateways':
            code = roll_back_gateways(args)
        else:
            with tempfile.TemporaryDirectory() as temporary:
                fetch_release(cloud, args.image, Path(temporary))
                code = {'preflight': preflight, 'deploy': roll_out, 'resume': finish,
                        'storage-cutover': storage_cutover}[args.command](args, Path(temporary))
    except Exception as error:
        print('Release stopped: ' + str(error), file=sys.stderr)
    if cloud is not None and args.command != 'store':
        try:
            push_records(cloud, root)
        except Exception as error:
            print('Records stay in ' + str(root) + '; returning them to the bucket failed: ' + str(error), file=sys.stderr)
            code = code or 1
    return code


if __name__ == '__main__':
    sys.exit(main())
