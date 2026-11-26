#!/usr/bin/env python3
"""Build, verify and publish a committed ForgetMeNote image to GHCR or a private ECR repository."""
import argparse
import fnmatch
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time

from verification import Verification, fingerprint, go_package_inputs, release_lock, source_inputs
from verification_plan import (verification_plan, selected_web_tests, affected_go_packages, retained_browser_failures,
                               path_class, TYPICAL_SECONDS)

# Git's well-known empty tree: a baseline with no files.
EMPTY_TREE = '4b825dc642cb6eb9a060e54bf8d69288fbee4904'

def run(command, *, cwd=None, capture=False, input=None):
    print("+ " + shlex.join(map(str, command)), flush=True)
    result = subprocess.run(command, cwd=cwd, text=True, input=input, check=True,
                            stdout=subprocess.PIPE if capture else None,
                            stderr=subprocess.PIPE if capture else None)
    return result.stdout.strip() if capture else None


def runtime_changes(root, previous, revision):
    """Changed image inputs: runtime paths (verification_plan.path_class) and embedded content.

    This decides whether to deploy, never whether a changed test needs to run.

    A full commit ID this history lacks, such as a live release built from the repository
    this one replaced, has no baseline here: every runtime input counts as changed.
    """
    if re.fullmatch(r'[0-9a-f]{40}', previous) and subprocess.run(
            ['git', 'cat-file', '-e', previous + '^{commit}'], cwd=root, capture_output=True).returncode:
        previous = EMPTY_TREE
    else:
        previous = subprocess.check_output(['git', 'rev-parse', '--verify', previous + '^{commit}'], cwd=root, text=True).strip()
    revision = subprocess.check_output(['git', 'rev-parse', '--verify', revision + '^{commit}'], cwd=root, text=True).strip()
    changed = subprocess.check_output(['git', 'diff', '--name-only', '--no-renames', '-z', previous, revision],
                                      cwd=root).decode().split('\0')
    changed = [path for path in changed if path]
    embedded = []
    for commit in (previous, revision):
        result = subprocess.run(['git', 'grep', '-n', '^[ \t]*//go:embed[ \t]', commit, '--', '*.go', ':!*_test.go'],
                                cwd=root, capture_output=True, text=True)
        if result.returncode not in (0, 1):
            raise ValueError('Cannot inspect committed embedding declarations')
        for line in result.stdout.splitlines():
            fields = line.split(':', 3)
            if len(fields) != 4 or not fields[2].isdigit():
                return changed
            directory = str(Path(fields[1]).parent)
            prefix = '' if directory == '.' else directory + '/'
            declaration = re.sub(r'^\s*//go:embed\s+', '', fields[3])
            # Unfamiliar escaping/globs conservatively include the whole package.
            patterns = ['*'] if any(c in declaration for c in '\\`[') else shlex.split(declaration)
            embedded.extend((prefix, pattern.removeprefix('all:')) for pattern in patterns)

    def is_embedded(path):
        for prefix, pattern in embedded:
            if path.startswith(prefix):
                parts = path[len(prefix):].split('/')
                if any(fnmatch.fnmatchcase('/'.join(parts[:n]), pattern) for n in range(1, len(parts) + 1)):
                    return True
        return False

    return [path for path in changed if path_class(path) == 'runtime' or is_embedded(path)]


GHCR = re.compile(r"ghcr\.io/[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9._-]*")
# A private ECR repository: ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME, which may hold namespaces.
ECR = re.compile(r"(\d{12})\.dkr\.ecr\.([a-z]{2}(?:-[a-z]+)+-\d+)\.amazonaws\.com/"
                 r"((?:[a-z0-9]+(?:[._-][a-z0-9]+)*/)*[a-z0-9]+(?:[._-][a-z0-9]+)*)")


def github(path):
    return json.loads(run(["gh", "api", "--hostname", "github.com", path], capture=True))


INDEX_TYPES = ("application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json")


def registry_descriptor(docker, reference):
    """The manifest descriptor a registry serves for reference, or None while it holds none.

    GHCR is read through the registry rather than GitHub's package API, which accepts only
    classic personal tokens: a workflow's own token signs docker in, and a public package
    needs no sign-in at all.
    """
    try:
        found = run([*docker, "buildx", "imagetools", "inspect", "--format", "{{json .Manifest}}", reference], capture=True)
    except subprocess.CalledProcessError as error:
        if re.search(r": not found\s*$", error.stderr or ""):
            return None
        raise ValueError("Cannot read " + reference + " from its registry: " + (error.stderr or "").strip()[-300:]) from None
    descriptor = json.loads(found)
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", descriptor.get("digest") or ""):
        raise ValueError("The registry returned an invalid image digest.")
    return descriptor


def registry_image_id(docker, uri, descriptor):
    """The image ID (configuration digest) of the linux/amd64 image descriptor names in uri."""
    digest = descriptor["digest"]
    if descriptor.get("mediaType") in INDEX_TYPES:
        platforms = [entry["digest"] for entry in descriptor.get("manifests", [])
                     if (entry.get("platform") or {}).get("os") == "linux"
                     and (entry.get("platform") or {}).get("architecture") == "amd64"]
        if len(platforms) != 1:
            raise ValueError(uri + "@" + digest + " does not hold exactly one linux/amd64 image.")
        digest = platforms[0]
    manifest = json.loads(run([*docker, "buildx", "imagetools", "inspect", "--raw", uri + "@" + digest], capture=True))
    return (manifest.get("config") or {}).get("digest")


def ghcr_digest(uri, tag):
    # Each lookup signs in for itself, so credentials never outlive it.
    with tempfile.TemporaryDirectory(prefix="forgetmenote-registry-") as directory:
        descriptor = registry_descriptor(registry_login(Path(directory), uri), uri + ":" + tag)
    return descriptor["digest"] if descriptor else None


LONG_PACKAGE_SECONDS = 150


def image_cache_options():
    """FMN_IMAGE_CACHE=gha shares layers between GitHub Check runs through the Actions
    cache, which needs a docker-container builder and the runtime token exposed to run
    steps (check.yml). Cached layers come from identical instructions and inputs."""
    cache = os.environ.get("FMN_IMAGE_CACHE", "")
    if not cache:
        return []
    if cache != "gha":
        raise ValueError("FMN_IMAGE_CACHE must be empty or gha.")
    scope = "type=gha,scope=forgetmenote-image"
    return ["--cache-from", scope, "--cache-to", scope + ",mode=max"]


def check_image(image_id, revision, source_url):
    if not re.fullmatch(r"sha256:[a-f0-9]{64}", image_id or ""):
        raise ValueError("Docker did not return an immutable image ID.")
    image = json.loads(run(["docker", "image", "inspect", image_id], capture=True))[0]
    labels = image["Config"].get("Labels") or {}
    if (image["Os"], image["Architecture"], labels.get("org.opencontainers.image.revision"),
            labels.get("org.opencontainers.image.source")) != ("linux", "amd64", revision, source_url):
        raise ValueError("Built image must be linux/amd64 and match the source repository and revision.")
    return image


def build_and_verify(root, revision, verification, source_url, jobs):
    candidate = verification.candidate
    archive = candidate / "source.tar"
    source = candidate / "source"
    # Host checks run in source while the image builds; the build context is a
    # separate copy that only the archive writes.
    context = candidate / "image-source"
    if not archive.exists():
        temporary_archive = archive.with_suffix(".tmp")
        with temporary_archive.open("wb") as output:
            subprocess.run(["git", "archive", "--format=tar", revision], cwd=root, stdout=output, check=True)
        temporary_archive.replace(archive)
    for directory in (source, context):
        directory.mkdir(exist_ok=True)
        # Restore committed inputs on resume; only generated dependencies/assets remain.
        with tarfile.open(archive) as bundle:
            bundle.extractall(directory, filter="data")
    policy = source_inputs(source, ["deploy/production/release_image.py", "deploy/production/verification.py",
                                    "deploy/production/verification_plan.py", "Makefile"])
    iidfile = candidate / "image-id"
    build_inputs = {"revision": revision, "source_url": source_url, "policy": policy}
    build_command = ["docker", "buildx", "build", "--load", "--platform", "linux/amd64", "--iidfile", str(iidfile),
                     *image_cache_options(),
                     "--label", "org.opencontainers.image.revision=" + revision,
                     "--label", "org.opencontainers.image.source=" + source_url, str(context)]
    exists = iidfile.exists() and subprocess.run(["docker", "image", "inspect", iidfile.read_text().strip()],
                                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode == 0
    dependencies = source_inputs(source, ["package*.json"])
    # Dependency directories belong to this snapshot. A receipt alone is not enough
    # if an operator has removed its installed artifacts.
    verification.check("npm", ["npm", "ci"], {"revision": revision, **dependencies}, cwd=source,
                       force=not (source / "node_modules").exists())
    # Browser installation is idempotent and checks whether its external cache exists.
    # The suite runs in Chromium, and the caret spec also in WebKit, Safari's engine;
    # their system libraries come from make setup-browser (docs/verification.md).
    run(["npx", "--no-install", "playwright", "install", "chromium", "webkit"], cwd=source)
    verification.check("frontend-build", ["npm", "run", "build"],
                       {"revision": revision, **source_inputs(source, ["frontend/**", "package*.json", "internal/web/ui/*.mjs"])},
                       cwd=source, force=not (source / "internal/web/ui/dist").exists())

    go_inputs = source_inputs(source, ["go.*", "cmd/**/*", "internal/**/*", "plugins/**", "frontend/**", "package*.json"])
    # go vet ./... and gofmt also cover the Go test tools under scripts/.
    go_tool_inputs = {**go_inputs, **source_inputs(source, ["scripts/**/*.go"])}
    # Operator tests read browser titles, web test names, configuration templates, the
    # image and deployment definitions, source fixtures, and which committed paths exist.
    with tarfile.open(archive) as bundle:
        committed = sorted(member.name for member in bundle.getmembers() if member.isfile())
    python_inputs = {"committed_paths": fingerprint(committed), **source_inputs(source, [
        "deploy/**", "scripts/*.py", "scripts/browser/*.spec.mjs", "internal/web/*_test.go",
        "internal/instanceconfig/config.go", "internal/sources/**", ".env.example", "compose.yaml",
        "Dockerfile", ".dockerignore", ".github/workflows/*.yml"])}
    # Test-only edits cannot change the binary exercised by browser fixtures.
    application_inputs = {path: digest for path, digest in go_inputs.items()
                          if not path.endswith("_test.go")}
    browser_inputs = {**application_inputs, **source_inputs(source, ["scripts/browser/**", "playwright.config.mjs"])}
    frontend_inputs = source_inputs(source, ["frontend/**", "internal/web/ui/**", "package*.json"])
    fixture_inputs = {**application_inputs, **source_inputs(source, ["scripts/browser/app-fixture.mjs", "scripts/browser/postgres.mjs",
                                                                      "scripts/browser/no-netlink.*", "scripts/browser-fixture.test.mjs"])}
    common_tools = ("python", "platform", "machine")
    go_tools = (*common_tools, "go", "git", "postgres", "python3", "go_environment")
    browser_tools = (*go_tools, "node", "npm", "browser_environment")
    checks = []
    python = "PYTHON=" + shlex.quote(sys.executable)

    def define(name, command, inputs, **options):
        execution = {"command": [str(arg).replace(str(source), "$SOURCE") for arg in command], "env": options.get('env', {})}
        # Selection/scheduling changes do not change the test being executed.
        # Bind the archived check policy and actual command/env; Make targets bind Makefile.
        # New attempts separately record the current operator revision.
        check_policy = source_inputs(source, ['deploy/production/verification.py'] + (['Makefile'] if command[0] == 'make' else []))
        return dict(name=name, command=command, inputs={"policy": check_policy, "execution": execution, **inputs}, cwd=source, **options)

    def add(name, command, inputs, **options):
        checks.append(define(name, command, inputs, **options))

    def image_checks():
        image_id = iidfile.read_text().strip()
        check_image(image_id, revision, source_url)
        verification.image_id = image_id
        later = []
        # Docker scenarios share a memory budget and OIDC's HTTPS port. Never overlap
        # them, even while independent host tests are running.
        # apt once waited three hours on a closed connection, and a failure here
        # stops the gate, so a stalled download gets a limit and another attempt.
        install = "apt-get update -qq && apt-get install -y -qq --no-install-recommends inotify-tools"
        backup_test = ["docker", "run", "--rm", "--init", "--user", "0", "--entrypoint", "sh",
                       "--mount", f"type=bind,source={source / 'deploy/git-backup'},target=/tests,readonly",
                       image_id, "-ec", f"for attempt in 1 2 3; do timeout 120 sh -c '{install}' && break; "
                       "[ $attempt -lt 3 ]; done; python3 -B -m unittest discover -s /tests"]
        if verification.plan['mode']=='full':
            later.append(define("backup", backup_test, {"image": image_id, **source_inputs(source, ["deploy/git-backup/**"])},
                                docker=True, limit=600))
        for name in verification.plan['containers']:
            profile = verification.plan.get(name + '_profile', 'full')
            inputs = {"image": image_id, **source_inputs(source, ["scripts/*.py", "deploy/**/*", "Makefile"])}
            tool_keys = None
            if name == "source":
                # This scenario cross-compiles the native Go source matrix on the
                # host. Its tests and runtime fixtures are not application image
                # inputs, so bind their actual closure and the compiler explicitly.
                inputs["native_source_matrix"] = package_inputs["forgetmenote/internal/sources"]
                tool_keys = (*go_tools, "docker", "image_overrides", "source_smoke_url")
            later.append(define("smoke-" + name, ["make", "smoke-" + name, python, "SMOKE_IMAGE=" + image_id],
                                inputs, docker=True, tool_keys=tool_keys, limit=1200,
                                env={**verification.tools.get("image_overrides", {}), 'SMOKE_PROFILE': profile}))
        return later

    # The build holds the Docker lane from the start, beside the quick preflight
    # and host checks; containers of the image queue behind it.
    checks.append(dict(name="build", command=build_command, inputs=build_inputs, cwd=context,
                       force=not exists, docker=True, eager=True, then=image_checks))
    # Each browser worker drives its own fixture server and Chromium: one slot apiece.
    workers = min(4, max(1, jobs // 3))
    # Without a measured run, as on a fresh GitHub runner, the browser suite is
    # still the longest check. Started last there, it ran alone for 20 minutes.
    add("browser", ["make", "check-browser", "-o", "frontend"], browser_inputs, slots=workers,
        env={"FMN_BROWSER_WORKERS": str(workers), "FMN_BROWSER_GREP": verification.plan['browser_grep']}, tool_keys=browser_tools,
        estimate=verification.history.get("browser", {}).get("elapsed_seconds", math.inf))
    packages = run(["go", "list", "-race", "./..."], cwd=source, capture=True).splitlines()
    inventory = run(["go", "list", "-race", "-deps", "-test", "-json", "./..."], cwd=source, capture=True)
    package_inputs = go_package_inputs(source, packages, inventory)
    package_times, case_times = verification.go_timings()
    web = next((package for package in packages if package.endswith('/internal/web')), None)
    if not web:
        raise ValueError("Go package inventory is missing internal/web.")
    web_times = case_times.get(web, {})

    def test_names(package):
        return [n for n in run(["go", "test", "-race", "-list", ".", package], cwd=source, capture=True).splitlines()
                if re.fullmatch(r"(?:Test|Example|Fuzz)\w*", n)]
    names = test_names(web)
    if 'TestMCPActionParityInventory' in names:
        add('mcp-parity', ['go', 'test', '-race', '-json', '-count=1', '-timeout=45m',
            '-run', '^TestMCPActionParityInventory$', web], {'source': package_inputs[web]},
            env={'GOMAXPROCS': '2'}, tool_keys=go_tools, preflight=True)

    chosen, reason = selected_web_tests(source, verification.plan, names)
    verification.plan['web_selection']={'selected':chosen,'omitted':len(names)-len(chosen),'reason':reason}
    chosen=[n for n in chosen if n!='TestMCPActionParityInventory']
    selected_packages, _ = affected_go_packages(verification.plan,package_inputs)
    for index, shard in enumerate(verification.partition_web(chosen, web_times)):
        add("go-web-" + str(index + 1), ["go", "test", "-race", "-json", "-count=1", "-timeout=45m",
            "-run", "^(" + "|".join(shard) + ")$", web], {"source": package_inputs[web], "tests": shard},
            env={"GOMAXPROCS": "2"}, tool_keys=go_tools, estimate=sum(web_times.get(test, 1) for test in shard))
    for package in packages:
        if package != web and package in selected_packages:
            # Opt-in checks against mutable external services cannot reuse a
            # source-only pass. Never store their connection strings in receipts.
            live_setting = {'forgetmenote/internal/reposource': 'FMN_SOURCE_EVAL_CONFIG',
                            'forgetmenote/internal/knowledge': 'FMN_LIVE_ORGANIZER_EVAL',
                            'forgetmenote/internal/connectors': 'FMN_TEST_POSTGRES'}.get(package, '')
            name = "go-" + package.replace('/', '-')
            command = ["go", "test", "-race", "-json", "-count=1", "-p", "2", "-timeout=45m"]
            options = dict(env={"GOMAXPROCS": "2"}, tool_keys=go_tools, force=bool(os.environ.get(live_setting)))
            if not verification.package_batched(package, package_times.get(package, 0) > LONG_PACKAGE_SECONDS):
                add(name, command + [package], {"source": package_inputs[package], "package": package},
                    estimate=package_times.get(package, TYPICAL_SECONDS.get(package, 1)), **options)
                continue
            # Like the web package, a measured long suite runs in separate process
            # batches so it does not hold one slot for the whole matrix.
            times = case_times.get(package, {})
            for index, shard in enumerate(verification.partition_package(package, test_names(package), times)):
                add(name + "-" + str(index + 1), command + ["-run", "^(" + "|".join(shard) + ")$", package],
                    {"source": package_inputs[package], "package": package, "tests": shard},
                    estimate=sum(times.get(test, 1) for test in shard), **options)
    for name, target, inputs, tool_keys in [
        ("frontend", "test-frontend", frontend_inputs, (*common_tools, "node", "npm")),
        ("fixture", "test-fixture", fixture_inputs, browser_tools),
        ("python", "test-python-host", python_inputs, (*common_tools, "python_dependencies", "git", "go")),
        ("vet", "vet", go_tool_inputs, go_tools), ("format", "format", go_tool_inputs, go_tools)]:
        # The operator tests take about three minutes in one process and cannot change
        # the image, so they run beside the matrix instead of holding it back.
        add(name, ["make", target, "-o", "frontend", python], inputs, tool_keys=tool_keys,
            preflight=name in ('format', 'vet', 'frontend'),
            estimate=verification.history.get(name, {}).get("elapsed_seconds", TYPICAL_SECONDS.get(name, 0)))
    verification.run(checks, jobs=jobs)
    print("Verification record: " + str(candidate / "verification.json"), flush=True)
    return verification.image_id


def docker_auth_config(temporary):
    # Keep registry credentials out of the build context and the operator's Docker
    # credential store, while preserving Docker Desktop's selected context and the
    # plugins, such as buildx, it installs beside them.
    config = temporary / "docker-auth"
    config.mkdir(mode=0o700)
    original = Path(os.environ.get("DOCKER_CONFIG", str(Path.home() / ".docker"))).resolve()
    for name in ("contexts", "cli-plugins"):
        if (original / name).is_dir():
            (config / name).symlink_to(original / name, target_is_directory=True)
    context = run(["docker", "context", "show"], capture=True)
    (config / "config.json").write_text(json.dumps({"currentContext": context}))
    return config


def registry_login(temporary, uri):
    config = docker_auth_config(temporary)
    ecr = ECR.fullmatch(uri)
    if ecr:
        # The host's own AWS sign-in: a profile, or a runner's session variables.
        password = run(["aws", "--region", ecr[2], "--no-cli-pager", "ecr", "get-login-password"], capture=True)
        username, host = "AWS", uri.split("/", 1)[0]
    else:
        username = os.environ.get("GITHUB_ACTOR") or github("user")["login"]
        password = run(["gh", "auth", "token", "--hostname", "github.com"], capture=True)
        host = "ghcr.io"
    if not password:
        raise ValueError("The registry sign-in returned an empty password; sign in to " + ("AWS" if ecr else "GitHub (gh auth login)") + " first.")
    docker = ["docker", "--config", str(config)]
    run([*docker, "login", "--username", username, "--password-stdin", host], input=password)
    return docker


def publish(docker, image_id, image):
    run(["docker", "tag", image_id, image])
    run([*docker, "push", image])


def push_image(docker, image_id, uri, tag, lookup):
    """Push the checked image_id as uri:tag; the digest the registry then serves for it."""
    publish(docker, image_id, uri + ":" + tag)
    pushed = json.loads(run(["docker", "image", "inspect", image_id], capture=True))[0]
    for attempt in range(4):
        digest = lookup()
        if digest:
            break
        time.sleep(2 ** attempt)
    if not digest or uri + "@" + digest not in pushed.get("RepoDigests", []):
        raise ValueError("Push completed but the registry digest does not match the checked image; inspect the registry before retrying.")
    return digest


def repository(root, image):
    """The source repository URL and the image repository, from origin and --image."""
    remote = run(["git", "remote", "get-url", "origin"], cwd=root, capture=True)
    match = re.fullmatch(r"(?:git@github\.com:|https://github\.com/)([\w.-]+/[\w.-]+?)(?:\.git)?", remote)
    if not match:
        raise ValueError("origin must be a GitHub repository URL without embedded credentials.")
    uri = image or "ghcr.io/" + match[1].lower()
    if not (GHCR.fullmatch(uri) or ECR.fullmatch(uri)):
        raise ValueError("--image must be ghcr.io/OWNER/NAME or ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME, "
                         "lowercase, without a tag or digest.")
    return "https://github.com/" + match[1], uri


def published_baseline(root, uri, revision, limit=100):
    """The newest earlier first-parent commit whose main-COMMIT image uri serves without a sign-in.

    A publishing workflow verifies against the last release it published. None means the
    gate runs completely: nothing published yet, or a package that refuses anonymous
    reads, such as a private one.
    """
    if subprocess.run(["git", "rev-parse", "--verify", "--quiet", revision + "^"], cwd=root, capture_output=True).returncode:
        return None
    commits = run(["git", "rev-list", "--first-parent", "--max-count=" + str(limit), revision + "^"], cwd=root, capture=True).split()
    with tempfile.TemporaryDirectory(prefix="forgetmenote-registry-") as directory:
        docker = ["docker", "--config", str(docker_auth_config(Path(directory)))]
        for commit in commits:
            try:
                if registry_descriptor(docker, uri + ":main-" + commit):
                    print("Published baseline: " + commit, flush=True)
                    return commit
            except ValueError as error:
                print(str(error) + "\nNo readable published baseline; verifying completely.", flush=True)
                return None
    print("No published image among the last " + str(len(commits)) + " main commits; verifying completely.", flush=True)
    return None


def write_json(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def verification_summary(record, raw):
    """What an image's verification attestation states: each expected check's verdict and the
    plan. Commands, inputs and host paths stay in the record, which record_sha256 names."""
    return {"source_revision": record["source_revision"], "image_id": record["image_id"], "complete": True,
            "expected_checks": record["expected_checks"],
            "checks": {name: {key: record["checks"][name].get(key) for key in ("status", "exit_code", "elapsed_seconds", "reused")}
                       for name in record["expected_checks"]},
            "plan": {key: (record.get("plan") or {}).get(key) for key in ("mode", "reason", "baseline")},
            "record_sha256": hashlib.sha256(raw).hexdigest()}


def publish_verified(args, root):
    """Publish the image a complete verification record in args.publish_verified describes.

    The gate and the publication can then run in separate jobs, so no check ever holds
    registry or signing credentials. The image must already be loaded under the record's
    image ID (docker load). Nothing is rebuilt or checked again.
    """
    if run(["git", "branch", "--show-current"], cwd=root, capture=True) != "main":
        raise ValueError("Release from the main branch.")
    if run(["git", "status", "--porcelain"], cwd=root, capture=True):
        raise ValueError("Release requires a clean checkout; commit or stash your changes first.")
    revision = run(["git", "rev-parse", "HEAD"], cwd=root, capture=True)
    directory = Path(args.publish_verified).expanduser().resolve()
    raw = (directory / "verification.json").read_bytes()
    record = json.loads(raw)
    checks, results = record.get("expected_checks") or [], record.get("checks") or {}
    if (record.get("source_revision") != revision or record.get("complete") is not True or not checks or
            any(results.get(name, {}).get("status") != "passed" or results.get(name, {}).get("exit_code") != 0 for name in checks)):
        raise ValueError("Publish only a complete verification record of " + revision + " whose expected checks all passed.")
    source_url, uri = repository(root, args.image)
    image_id = record.get("image_id")
    check_image(image_id, revision, source_url)
    tag = "main-" + revision
    with tempfile.TemporaryDirectory(prefix="forgetmenote-registry-") as temporary:
        docker = registry_login(Path(temporary), uri)
        existing = registry_descriptor(docker, uri + ":" + tag)
        if existing:
            # A rerun may follow a push that succeeded; any other image keeps its tag.
            if image_id not in (existing["digest"], registry_image_id(docker, uri, existing)):
                raise ValueError(uri + ":" + tag + " already holds a different image; inspect the registry before retrying.")
            digest = existing["digest"]
            print("Already published: " + uri + "@" + digest)
        else:
            digest = push_image(docker, image_id, uri, tag,
                                lambda: (registry_descriptor(docker, uri + ":" + tag) or {}).get("digest"))
    write_json(directory / "publication.json", {
        "source_revision": revision, "image_id": image_id, "registry_digest": digest, "image": uri + ":" + tag})
    write_json(directory / "verification-summary.json", verification_summary(record, raw))
    print("\nPublished: " + uri + ":" + tag)
    print("Deploy this image: " + uri + "@" + digest)


def release(args, root):
    if not args.verify_only and run(["git", "branch", "--show-current"], cwd=root, capture=True) != "main":
        raise ValueError("Release from the main branch.")
    if run(["git", "status", "--porcelain"], cwd=root, capture=True):
        raise ValueError("Release requires a clean checkout; commit or stash your changes first.")
    operator_revision = run(["git", "rev-parse", "HEAD"], cwd=root, capture=True)
    revision=run(["git","rev-parse","--verify","--end-of-options",args.revision+"^{commit}"],cwd=root,capture=True) if args.revision else operator_revision
    if revision!=operator_revision:
        run(["git","merge-base","--is-ancestor",revision,operator_revision],cwd=root)
        if runtime_changes(root,revision,operator_revision):raise ValueError('Selected candidate differs from current runtime inputs; use its original checkout.')
        newer=run(["git","diff","--name-only","--no-renames","-z",revision,operator_revision],cwd=root,capture=True).split('\0')
        if any(path and path_class(path) not in ('docs', 'operator') for path in newer):
            raise ValueError('Selected candidate excludes newer verification inputs; use its original checkout and verify newer tests separately.')
    source_url, uri = repository(root, args.image)
    if args.since_published:
        args.since = published_baseline(root, uri, revision)
    runtime_paths = runtime_changes(root, args.since, revision) if args.since else None
    if runtime_paths == []:
        print('No runtime changes since ' + args.since + '; no image build, publication or deployment required. '
              'Run the focused checks for changed tests and operator tools separately.')
        return
    tag = "main-" + revision
    image = uri + ":" + tag
    print("Image: " + image, flush=True)
    plan = verification_plan(root, args.since, revision, args.mode, runtime_paths=runtime_paths)
    print('Verification: ' + plan['mode'] + ' — ' + plan['reason'], flush=True)
    if args.dry_run:
        print(json.dumps(plan, indent=2))
        full = plan['mode'] == 'full'
        print("Would build linux/amd64 from committed source; run " + ("every Go race test" if full else "affected Go race tests")
              + ", mandatory security, frontend, fixture and Python checks" + (", backup worker checks" if full else "")
              + ", the selected browser and container coverage; then publish and verify the image digest.")
        return
    lookup = None
    if not args.verify_only:
        lookup, digest = locate_package(uri, tag)
        if digest:
            print("Already published (not rebuilt): " + uri + "@" + digest)
            return
    for tool in ("docker", "make", "go", "gofmt", "node", "npm", "npx", "openssl", "initdb", "postgres", "psql"):
        if shutil.which(tool) is None:
            raise ValueError("Required command is missing: " + tool)
    tools = {"python": sys.version, "platform": sys.platform, "machine": os.uname().machine}
    for name, command in {"go": ["go", "version"], "git": ["git", "--version"], "postgres": ["postgres", "--version"], "node": ["node", "--version"], "python3": ["python3", "--version"],
                          "npm": ["npm", "--version"], "docker": ["docker", "version", "--format", "{{.Server.Version}}"],
                          "buildx": ["docker", "buildx", "version"], "compose": ["docker", "compose", "version"]}.items():
        tools[name] = run(command, capture=True)
    tools["environment"] = {name: os.environ.get(name, "") for name in (
        "GOFLAGS", "GOOS", "GOARCH", "CGO_ENABLED", "GOTOOLCHAIN", "FMN_LARGE_EXPORT_TEST",
        "PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH", "SMOKE_RESTORE_IMAGE", "SMOKE_PROVIDER_IMAGE",
        "SMOKE_POSTGRES_IMAGE", "SOURCE_SMOKE_KEEP", "KEEP_OIDC_SMOKE")}
    # Include effective defaults (including go env -w), not just exported values.
    tools['go_environment'] = json.loads(run(['go', 'env', '-json',
        'GOFLAGS', 'GOOS', 'GOARCH', 'GOAMD64', 'GOARM64', 'GOEXPERIMENT', 'CGO_ENABLED',
        'GOTOOLCHAIN', 'GOWORK', 'CC', 'CXX', 'CGO_CFLAGS', 'CGO_CPPFLAGS', 'CGO_CXXFLAGS', 'CGO_LDFLAGS'],
        cwd=root, capture=True))
    tools['go_environment']['FMN_LARGE_EXPORT_TEST'] = os.environ.get('FMN_LARGE_EXPORT_TEST', '')
    for name in ('FMN_SOURCE_EVAL_CONFIG', 'FMN_LIVE_ORGANIZER_EVAL', 'FMN_TEST_POSTGRES', 'FMN_TEST_DATABASE_URL'):
        tools['go_environment'][name] = fingerprint(os.environ.get(name, ''))
    if any(flag.split('=')[0] in ('-overlay', '-modfile') for flag in shlex.split(tools['go_environment'].get('GOFLAGS', ''))):
        raise ValueError('Release verification requires committed Go inputs; remove GOFLAGS -overlay/-modfile.')
    tools['browser_environment'] = {name: os.environ.get(name, '') for name in ('PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH',)}
    run(["docker", "info"], capture=True)
    tools["python_dependencies"] = run([sys.executable, "-c",
        "import yaml, argon2; from importlib.metadata import version; print(version('PyYAML'), version('argon2-cffi'))"], capture=True)
    tools["source_smoke_url"] = fingerprint(os.environ.get("SOURCE_SMOKE_URL", ""))
    tools["image_overrides"] = {name: run(["docker", "image", "inspect", os.environ[name], "--format", "{{.Id}}"], capture=True)
                                for name in ("SMOKE_RESTORE_IMAGE", "SMOKE_PROVIDER_IMAGE", "SMOKE_POSTGRES_IMAGE") if os.environ.get(name)}
    state = Path(args.state_dir).expanduser().resolve()
    with release_lock(state) as lock_fd:
        verification = Verification(state, revision, tools, retry_failed=args.retry_failed, lock_fd=lock_fd)
        previous_record = verification.candidate / 'verification.json'
        if previous_record.exists() and plan['mode'] == 'routine':
            previous = json.loads(previous_record.read_text())
            retained=retained_browser_failures(previous,plan,args.retry_failed)
            if retained:plan['retained_failed_browser_tests']=retained
        plan['operator_revision']=operator_revision
        verification.plan.update(plan)
        image_id = build_and_verify(root, revision, verification, source_url, args.jobs)
        if args.verify_only:
            print("Verified source: " + revision + "\nVerified image: " + image_id)
            return
        # Credentials exist only during publication, outside persistent receipts.
        with tempfile.TemporaryDirectory(prefix="forgetmenote-registry-") as directory:
            docker = registry_login(Path(directory), uri)
            digest = lookup()
            if digest:
                print("Already published (not replaced): " + uri + "@" + digest)
                return
            digest = push_image(docker, image_id, uri, tag, lookup)
        verification.write(verification.candidate / "publication.json", {
            "source_revision": revision, "image_id": image_id, "registry_digest": digest, "image": image})
    print("\nPublished: " + image)
    print("Deploy this image: " + uri + "@" + digest)


def ecr_digest(account, region, name, tag, profile=None):
    aws = ["aws", *(["--profile", profile] if profile else []), "--region", region, "--output", "json", "--no-cli-pager"]
    try:
        found = json.loads(run([*aws, "ecr", "describe-images", "--registry-id", account, "--repository-name", name,
                                "--image-ids", "imageTag=" + tag], capture=True))
    except subprocess.CalledProcessError as error:
        if "ImageNotFoundException" in (error.stderr or ""):
            return None
        if "RepositoryNotFoundException" in (error.stderr or ""):
            raise ValueError("ECR repository " + name + " does not exist in account " + account +
                             "; create it, private with immutable tags, before publishing.") from None
        raise
    digests = {image.get("imageDigest") for image in found.get("imageDetails", [])}
    if len(digests) != 1 or not re.fullmatch(r"sha256:[a-f0-9]{64}", next(iter(digests)) or ""):
        raise ValueError("ECR returned an invalid image digest.")
    return digests.pop()


def locate_package(uri, tag, profile=None):
    """A lookup of the digest tag names in repository uri, and its answer now (None while unpublished).

    profile names the AWS CLI profile for ECR; without it the host's own sign-in answers.
    GHCR lookups sign in with the GitHub CLI's token (GH_TOKEN in a workflow).
    """
    ecr = ECR.fullmatch(uri)
    lookup = (lambda: ecr_digest(*ecr.groups(), tag, profile)) if ecr else (lambda: ghcr_digest(uri, tag))
    return lookup, lookup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", help="ghcr.io/OWNER/NAME or ACCOUNT.dkr.ecr.REGION.amazonaws.com/NAME; "
                        "defaults to the origin repository's GHCR package")
    parser.add_argument("--dry-run", action="store_true", help="show the release plan without network calls, builds or pushes")
    parser.add_argument("--verify-only", action="store_true", help="verify without publishing or registry authentication")
    parser.add_argument('--mode', choices=('auto', 'routine', 'full'), default='auto',
                        help='auto selects affected modules and caller/browser/container boundaries against --since; full retains every regression')
    parser.add_argument('--since', help='deployed source revision; skip the image release when runtime inputs are unchanged')
    parser.add_argument('--since-published', action='store_true',
                        help='as --since, the newest earlier main commit whose image --image serves without a sign-in; '
                             'without one the gate runs completely')
    parser.add_argument('--publish-verified', metavar='DIR',
                        help='publish the loaded image the complete verification.json in DIR describes, without rebuilding '
                             'or checking again, and write publication.json and verification-summary.json beside it')
    parser.add_argument('--revision', help='previous committed runtime candidate, allowed only when current HEAD has identical runtime inputs')
    parser.add_argument("--retry-failed", action="store_true", help="explicitly retry investigated failures; keep their original evidence")
    parser.add_argument("--jobs", type=int, default=min(10, max(1, (os.cpu_count() or 2) - 4)),
                        help="host concurrency slots (1–12); browser workers use a third, Docker remains sequential")
    parser.add_argument("--state-dir", default=str(Path(os.environ.get("XDG_CACHE_HOME", str(Path.home() / ".cache"))) / "forgetmenote/releases"),
                        help="private persistent receipts, committed snapshots and failure artifacts")
    args = parser.parse_args()
    if not 1 <= args.jobs <= 12:
        parser.error("--jobs must be between 1 and 12")
    if args.since and args.since_published:
        parser.error("--since and --since-published are exclusive")
    if args.since_published and args.dry_run:
        parser.error("--since-published reads the registry; preview with --since instead")
    if args.publish_verified and (args.verify_only or args.dry_run or args.since or args.since_published
                                  or args.revision or args.retry_failed):
        parser.error("--publish-verified publishes an existing record; it takes only --image")
    os.umask(0o077)
    if sys.version_info < (3, 12):
        parser.error("Python 3.12 or newer is required")
    root = Path(__file__).resolve().parents[2]
    try:
        (publish_verified if args.publish_verified else release)(args, root)
    except subprocess.CalledProcessError as error:
        parser.exit(1, f"Release stopped: {shlex.join(error.cmd)} exited {error.returncode}.\n{error.stderr or ''}")
    except (OSError, ValueError, tarfile.TarError) as error:
        parser.exit(1, f"Release stopped: {error}\n")
    except KeyboardInterrupt:
        parser.exit(130, "Release interrupted.\n")


if __name__ == "__main__":
    main()
