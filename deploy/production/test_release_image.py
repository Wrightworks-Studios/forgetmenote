"""Exercise the release CLI with real Git and simulated GitHub/Docker boundaries."""
import os
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("release_image.py")
REGISTRY = "ghcr.io/example-owner/forgetmenote"
ECR = "123456789012.dkr.ecr.us-east-1.amazonaws.com/forgetmenote"
DIGEST = "sha256:" + "a" * 64
IMAGE_ID = "sha256:" + "b" * 64

TOOLS = r'''
import json, os, pathlib, sys
args = sys.argv[1:]
tool = pathlib.Path(sys.argv[0]).name
root = pathlib.Path(os.environ["RELEASE_FIXTURE"])
record = {"tool": tool, "args": args}
record['verification_env'] = {key: os.environ[key] for key in ('SMOKE_PROFILE', 'FMN_BROWSER_GREP') if key in os.environ}
if tool == "docker" and "login" in args:
    record["password"] = sys.stdin.read().strip()
    config = pathlib.Path(args[args.index("--config") + 1])
    record["context"] = json.loads((config / "config.json").read_text())["currentContext"]
    record["contexts_available"] = (config / "contexts/meta/fixture").is_file()
    (config / "config.json").write_text('{"auths":{"synthetic":"credential"}}')
if tool == "docker" and "build" in args:
    context = pathlib.Path(args[-1])
    record["source"] = (context / "note.txt").read_text()
    record["ignored_file_present"] = (context / "private.env").exists()
    (root / "source/note.txt").write_text("edited while building\n")
with (root / "calls.jsonl").open("a") as f:
    f.write(json.dumps(record) + "\n")
fail = os.environ.get("FAIL_COMMAND", "")
if fail and fail in " ".join([tool] + args):
    print("synthetic command failure", file=sys.stderr)
    sys.exit(7)
if tool == "gh":
    if args[:2] == ["auth", "token"]:
        print("synthetic-oauth-token")
    elif args[0] == "api":
        path = args[3]
        if path == "user":
            print('{"login":"example-owner"}')
        else:
            sys.exit("unexpected GitHub operation: " + path)
    else:
        sys.exit("unexpected gh command")
elif tool == "docker":
    if "build" in args:
        pathlib.Path(args[args.index("--iidfile") + 1]).write_text("sha256:" + "b" * 64)
    elif "imagetools" in args:
        # The registry: anonymous reads only when the package allows them.
        reference = args[-1]
        config = pathlib.Path(args[args.index("--config") + 1]) if "--config" in args else None
        signed_in = bool(config) and "auths" in json.loads((config / "config.json").read_text())
        if os.environ.get("ANONYMOUS_DENIED") and not signed_in:
            print("ERROR: failed to authorize: 401 Unauthorized", file=sys.stderr)
            sys.exit(1)
        if "--raw" in args:
            print(json.dumps({"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
                              "config": {"digest": os.environ.get("REGISTRY_IMAGE_ID", "sha256:" + "b" * 64)}}))
            sys.exit(0)
        tag = reference.rpartition(":")[2]
        current = tag == "main-" + os.environ["FIXTURE_REVISION"] and (
            (root / "pushed").exists() or os.environ.get("EXISTING_IMAGE"))
        if not (current or tag in os.environ.get("PUBLISHED_TAGS", "").split(",")):
            print("ERROR: " + reference + ": not found", file=sys.stderr)
            sys.exit(1)
        print(json.dumps({"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + "a" * 64, "size": 1}))
    elif "inspect" in args:
        if os.environ.get("NO_LOCAL_IMAGE"):
            print("Error: No such image", file=sys.stderr)
            sys.exit(1)
        digest = os.environ.get("PUSHED_DIGEST", "sha256:" + "a" * 64)
        print(json.dumps([{"Os": "linux", "Architecture": os.environ.get("IMAGE_ARCH", "amd64"),
                           "RepoDigests": [os.environ.get("PUSHED_REPOSITORY", "ghcr.io/example-owner/forgetmenote") + "@" + digest],
                           "Config": {"Labels": {
                               "org.opencontainers.image.revision": os.environ["FIXTURE_REVISION"],
                               "org.opencontainers.image.source": "https://github.com/example-owner/forgetmenote"}}}]))
    elif args == ["context", "show"]:
        print("desktop-linux")
    elif "push" in args:
        (root / "pushed").touch()
elif tool == "aws":
    if args[-2:] == ["ecr", "get-login-password"]:
        print("synthetic-ecr-password")
    elif "describe-images" in args:
        if os.environ.get("ECR_MISSING_REPOSITORY"):
            print("An error occurred (RepositoryNotFoundException) when calling the DescribeImages operation", file=sys.stderr)
            sys.exit(254)
        if not (root / "pushed").exists() and not os.environ.get("EXISTING_IMAGE"):
            print("An error occurred (ImageNotFoundException) when calling the DescribeImages operation", file=sys.stderr)
            sys.exit(254)
        print(json.dumps({"imageDetails": [{"imageDigest": "sha256:" + "a" * 64}]}))
    else:
        sys.exit("unexpected aws command")
elif tool == "go":
    if args[:2] == ["env", "-json"]:
        print(json.dumps({name: os.environ.get(name, "") for name in args[2:]}))
    elif args == ["list", "-race", "./..."]:
        print("forgetmenote/internal/web\nforgetmenote/internal/store\nforgetmenote/internal/sources")
    elif args == ["list", "-race", "-deps", "-test", "-json", "./..."]:
        for name in ("store", "sources", "web"):
            print(json.dumps({"ImportPath": "forgetmenote/internal/" + name,
                "Dir": str(pathlib.Path.cwd() / "internal" / name),
                "Module": {"Main": True}, "GoFiles": ["main.go"],
                "TestGoFiles": ["main_test.go"],
                "Imports": ["forgetmenote/internal/store"] if name in ("web", "sources") else [],
                "EmbedFiles": ["ui/index.html"] if name == "web" else []}))
    elif args[:4] == ["test", "-race", "-list", "."]:
        print("TestFirst\nTestSecond\nExamplePublic\nFuzzInput\nok forgetmenote/internal/web 0.0s")
    elif args[:3] == ["test", "-race", "-json"] and os.environ.get("CASE_SECONDS"):
        # Report a measured time for each case this process ran.
        seconds = json.loads(os.environ["CASE_SECONDS"]).get(args[-1], 1)
        names = (args[args.index("-run") + 1].removeprefix("^(").removesuffix(")$").split("|") if "-run" in args
                 else ["TestFirst", "TestSecond", "ExamplePublic", "FuzzInput"])
        for name in names:
            print(json.dumps({"Action": "pass", "Package": args[-1], "Test": name, "Elapsed": seconds}))
        print(json.dumps({"Action": "pass", "Package": args[-1], "Elapsed": seconds * len(names)}))
elif tool == "npm" and args == ["ci"]:
    pathlib.Path("node_modules").mkdir(exist_ok=True)
elif tool == "npm" and args == ["run", "build"]:
    pathlib.Path("internal/web/ui/dist").mkdir(parents=True, exist_ok=True)
elif tool == "make" and "smoke-totp" in args:
    (root / "checked").touch()
'''


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="forgetmenote-release-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "source"
        self.repo.mkdir()
        self.script = self.repo / "deploy/production/release_image.py"
        self.script.parent.mkdir(parents=True)
        shutil.copyfile(SCRIPT, self.script)
        shutil.copyfile(SCRIPT.with_name("verification.py"), self.script.with_name("verification.py"))
        shutil.copyfile(SCRIPT.with_name("verification_plan.py"), self.script.with_name("verification_plan.py"))
        (self.repo / "go.mod").write_text("module forgetmenote\n")
        for name in ("store", "sources", "web"):
            directory = self.repo / "internal" / name
            directory.mkdir(parents=True)
            (directory / "main.go").write_text("package " + name + "\n")
            (directory / "main_test.go").write_text("package " + name + "\n")
        (self.repo / "internal/web/ui").mkdir()
        (self.repo / "internal/web/ui/index.html").write_text("<main>Notes</main>\n")
        (self.repo / "note.txt").write_text("committed source\n")
        (self.repo / ".gitignore").write_text("private.env\n__pycache__/\n")
        (self.repo / "private.env").write_text("excluded local configuration\n")
        self.git("init", "-b", "main")
        self.git("config", "user.name", "Release fixture")
        self.git("config", "user.email", "fixture@localhost")
        self.git("remote", "add", "origin", "git@github.com:example-owner/forgetmenote.git")
        self.git("add", ".")
        self.git("commit", "-m", "Synthetic release")
        self.revision = self.git("rev-parse", "HEAD").stdout.strip()
        tools = self.root / "tools"
        tools.mkdir()
        for tool in ["gh", "aws", "docker", "make", "npm", "npx", "go", "gofmt", "openssl"]:
            executable = tools / tool
            executable.write_text(f"#!{sys.executable}\n" + TOOLS)
            executable.chmod(0o755)
        docker_config = self.root / "docker-config"
        (docker_config / "contexts/meta").mkdir(parents=True)
        (docker_config / "contexts/meta/fixture").touch()
        self.original_config = '{"currentContext":"desktop-linux","credsStore":"desktop"}'
        (docker_config / "config.json").write_text(self.original_config)
        self.env = {**os.environ, "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                    "RELEASE_FIXTURE": str(self.root), "FIXTURE_REVISION": self.revision,
                    "XDG_CACHE_HOME": str(self.root / "cache"),
                    "DOCKER_CONFIG": str(docker_config)}
        # The gate itself may run under these (GitHub Actions, the Check workflow);
        # each test sets what it needs.
        for inherited in ("GITHUB_ACTOR", "FMN_IMAGE_CACHE"):
            self.env.pop(inherited, None)

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args],
                              check=True, capture_output=True, text=True)

    def release(self, *args):
        return subprocess.run([sys.executable, str(self.script), *args],
                              cwd=self.root, capture_output=True, text=True, env=self.env)

    def calls(self):
        log = self.root / "calls.jsonl"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []

    def next_revision(self, path, content):
        self.git("checkout", "--", "note.txt")
        (self.repo / path).write_text(content)
        self.git("add", ".")
        self.git("commit", "-m", "Change " + path)
        self.revision = self.git("rev-parse", "HEAD").stdout.strip()
        self.env["FIXTURE_REVISION"] = self.revision

    def test_ui_edit_reuses_unrelated_go_package_checks(self):
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("internal/web/ui/index.html", "<main>Changed notes</main>\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        store = [c for c in self.calls() if c["tool"] == "go" and "-race" in c["args"]
                 and "forgetmenote/internal/store" in c["args"]]
        self.assertEqual(len(store), 1, "UI changes must not invalidate storage race coverage")
        self.assertEqual(sum(c["tool"] == "make" and "check-browser" in c["args"] for c in self.calls()), 2)

    def test_ui_only_release_selects_routine_browser_and_compact_smokes(self):
        first = self.release('--verify-only')
        self.assertEqual(first.returncode, 0, first.stderr)
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        start = len(self.calls())
        result = self.release('--verify-only', '--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads((self.root / 'cache/forgetmenote/releases' / self.revision / 'verification.json').read_text())
        self.assertEqual(record['plan'].get('mode'), 'routine')
        calls = self.calls()[start:]
        browser = next(c for c in calls if c['tool'] == 'make' and 'check-browser' in c['args'])
        self.assertRegex('settings-layout.spec.mjs', browser['verification_env']['FMN_BROWSER_GREP'])
        self.assertEqual([n for n in record['expected_checks'] if n.startswith('smoke-')], ['smoke-core'])
        for name in ('smoke-oidc','smoke-totp','backup'):
            self.assertNotIn(name,record['expected_checks'])

    def test_dry_run_describes_only_the_checks_its_mode_runs(self):
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        routine = self.release('--dry-run', '--since', previous)
        self.assertEqual(routine.returncode, 0, routine.stderr)
        self.assertIn('"mode": "routine"', routine.stdout)
        self.assertIn('affected Go race tests', routine.stdout)
        self.assertNotIn('backup', routine.stdout.split('Would build', 1)[1])
        full = self.release('--dry-run', '--since', previous, '--mode', 'full')
        self.assertEqual(full.returncode, 0, full.stderr)
        self.assertIn('every Go race test', full.stdout)
        self.assertIn('backup worker checks', full.stdout)
        self.assertEqual(self.calls(), [])

    def test_routine_cannot_bypass_shared_security_or_unknown_changes(self):
        previous = self.revision
        self.next_revision('internal/store/main.go', 'package store\n// shared storage\n')
        result = self.release('--verify-only', '--since', previous, '--mode', 'routine')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Routine verification is not allowed', result.stderr)
        self.assertFalse(any(c['tool'] == 'docker' for c in self.calls()))
        result = self.release('--dry-run', '--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('"mode": "full"', result.stdout)

    def test_explicit_full_keeps_complete_browser_and_container_coverage(self):
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        result = self.release('--verify-only', '--since', previous, '--mode', 'full')
        self.assertEqual(result.returncode, 0, result.stderr)
        browser = next(c for c in self.calls() if c['tool'] == 'make' and 'check-browser' in c['args'])
        self.assertEqual(browser['verification_env']['FMN_BROWSER_GREP'], '')
        for target in ('smoke-source', 'smoke-jev'):
            smoke = next(c for c in self.calls() if c['tool'] == 'make' and target in c['args'])
            self.assertEqual(smoke['verification_env']['SMOKE_PROFILE'], 'full')

    def test_expanding_routine_gate_runs_omitted_matrix_without_repeating_go(self):
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        result = self.release('--verify-only', '--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.git('checkout', '--', 'note.txt')
        start = len(self.calls())
        result = self.release('--verify-only', '--since', previous, '--mode', 'full')
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()[start:]
        self.assertFalse(any(c['tool'] == 'go' and '-json' in c['args'] and '-run' in c['args'] for c in calls))
        for target in ('check-browser', 'smoke-source', 'smoke-jev'):
            self.assertTrue(any(c['tool'] == 'make' and target in c['args'] for c in calls), target)

    def test_smoke_and_browser_selection_edits_do_not_require_a_new_application_image(self):
        previous = self.revision
        (self.repo / 'scripts').mkdir()
        self.next_revision('scripts/source_smoke.py', '# changed operator smoke\n')
        self.next_revision('playwright.config.mjs', '// changed test selection\n')
        result = self.release('--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('No runtime changes', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_documents_fixtures_and_tests_do_not_require_a_new_application_image(self):
        previous = self.revision
        for path in ('CHANGELOG.md', 'SECURITY.md', 'internal/store/testdata/case.json',
                     'internal/web/ui/state.test.mjs', 'deploy/git-backup/test_watcher.py'):
            (self.repo / path).parent.mkdir(parents=True, exist_ok=True)
            self.next_revision(path, 'changed\n')
        result = self.release('--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('No runtime changes', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_agent_tooling_edits_do_not_require_a_new_application_image(self):
        previous = self.revision
        (self.repo / 'scripts').mkdir()
        self.next_revision('scripts/browser-summary.mjs', '// changed summary\n')
        self.next_revision('scripts/ui-capture.mjs', '// changed capture\n')
        self.next_revision('.ignore', 'docs/archive/\n')
        result = self.release('--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('No runtime changes', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_preflight_failure_stops_before_browser_and_container_matrix(self):
        self.env['FAIL_COMMAND'] = 'make vet'
        result = self.release('--verify-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('vet failed', result.stderr)
        self.assertFalse(any(c['tool'] == 'make' and ('check-browser' in c['args'] or
                             any(arg.startswith('smoke-') for arg in c['args'])) for c in self.calls()))

    def test_failed_full_gate_cannot_be_hidden_by_switching_to_routine(self):
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        self.env['FAIL_COMMAND'] = 'make check-browser'
        result = self.release('--verify-only', '--since', previous, '--mode', 'full')
        self.assertNotEqual(result.returncode, 0)
        self.git('checkout', '--', 'note.txt')
        del self.env['FAIL_COMMAND']
        result = self.release('--verify-only', '--since', previous)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Cannot narrow a failed full gate', result.stderr)

    def test_blocked_resume_preserves_failure_against_later_narrowing(self):
        previous = self.revision
        self.next_revision('internal/web/ui/style.css', 'button { gap: .5rem; }\n')
        self.env['FAIL_COMMAND'] = 'forgetmenote/internal/store'
        first = self.release('--verify-only', '--since', previous, '--mode', 'full')
        self.assertNotEqual(first.returncode, 0)
        self.git('checkout', '--', 'note.txt')
        del self.env['FAIL_COMMAND']
        second = self.release('--verify-only', '--since', previous, '--mode', 'full')
        self.assertNotEqual(second.returncode, 0)
        third = self.release('--verify-only', '--since', previous, '--mode', 'auto')
        self.assertNotEqual(third.returncode, 0)
        self.assertIn('Cannot narrow', third.stderr)

    def test_companion_asset_edit_invalidates_browser_and_format(self):
        (self.repo / "plugins").mkdir()
        self.next_revision("plugins/companion.md", "Original embedded skill\n")
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("plugins/companion.md", "Changed embedded skill\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        for target in ("check-browser", "test-fixture", "format", "vet"):
            self.assertEqual(sum(c["tool"] == "make" and target in c["args"] for c in self.calls()), 2, target)

    def test_python_receipt_binds_the_repository_files_its_tests_read(self):
        runs = lambda: sum(c["tool"] == "make" and "test-python-host" in c["args"] for c in self.calls())
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        expected = 1
        for path in ("scripts/browser/journey.spec.mjs", "internal/web/main_test.go", "internal/instanceconfig/config.go",
                     ".env.example", "compose.yaml", "Dockerfile", ".dockerignore",
                     ".github/workflows/publish.yml", "internal/sources/testdata/scan.txt",
                     "internal/web/renamed_test.go"):
            with self.subTest(path=path):
                (self.repo / path).parent.mkdir(parents=True, exist_ok=True)
                self.next_revision(path, "changed " + path + "\n")
                result = self.release("--verify-only")
                self.assertEqual(result.returncode, 0, result.stderr)
                expected += 1
                self.assertEqual(runs(), expected)
        # The mapping tests read which paths exist, not their contents: a new file reruns them.
        self.next_revision("assets.txt", "new committed path\n")
        self.assertEqual(self.release("--verify-only").returncode, 0)
        self.assertEqual(runs(), expected + 1)
        self.next_revision("assets.txt", "same path, other content\n")
        self.assertEqual(self.release("--verify-only").returncode, 0)
        self.assertEqual(runs(), expected + 1)

    def test_vet_and_format_receipts_bind_the_go_test_tools(self):
        (self.repo / "scripts/mcp-smoke").mkdir(parents=True)
        self.next_revision("scripts/mcp-smoke/main.go", "package main\n")
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("scripts/mcp-smoke/main.go", "package main\n\nfunc main() {}\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        for target in ("vet", "format"):
            self.assertEqual(sum(c["tool"] == "make" and target in c["args"] for c in self.calls()), 2, target)
        self.assertEqual(sum(c["tool"] == "make" and "check-browser" in c["args"] for c in self.calls()), 1)

    def test_go_test_edit_reuses_application_browser_and_frontend_results(self):
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("internal/store/main_test.go", "package store\n// another regression\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        for target in ("check-browser", "test-frontend", "test-fixture"):
            self.assertEqual(sum(c["tool"] == "make" and target in c["args"] for c in self.calls()), 1, target)
        store = [c for c in self.calls() if c["tool"] == "go" and "-race" in c["args"]
                 and "forgetmenote/internal/store" in c["args"]]
        self.assertEqual(len(store), 2, "the modified package still needs its race tests")

    def test_native_source_test_edit_invalidates_container_matrix(self):
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("internal/sources/main_test.go", "package sources\n// changed native matrix\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        calls = self.calls()
        self.assertEqual(sum(c['tool'] == 'make' and 'smoke-source' in c['args'] for c in calls), 2)
        self.assertEqual(sum(c['tool'] == 'make' and 'smoke-core' in c['args'] for c in calls), 1)
        self.assertEqual(sum(c['tool'] == 'make' and 'check-browser' in c['args'] for c in calls), 1)

    def test_shared_go_implementation_invalidates_dependents_and_browser(self):
        first = self.release("--verify-only")
        self.assertEqual(first.returncode, 0, first.stderr)
        self.next_revision("internal/store/main.go", "package store\n// changed storage behavior\n")
        second = self.release("--verify-only")
        self.assertEqual(second.returncode, 0, second.stderr)
        race = [c for c in self.calls() if c["tool"] == "go" and c['args'][0] == 'test' and '-json' in c['args'] and '-race' in c['args']]
        self.assertEqual(len(race), 12, 'both packages and every web shard must run after a shared implementation change')
        for target in ('check-browser', 'test-fixture'):
            self.assertEqual(sum(c['tool'] == 'make' and target in c['args'] for c in self.calls()), 2)
        self.assertEqual(sum(c['tool'] == 'make' and 'test-frontend' in c['args'] for c in self.calls()), 1)

    def test_untracked_go_overlays_cannot_reuse_release_evidence(self):
        self.env['GOFLAGS'] = '-overlay=/tmp/other-inputs.json'
        result = self.release('--verify-only')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('committed Go inputs', result.stderr)
        self.assertFalse(any(c['tool'] == 'docker' and 'build' in c['args'] for c in self.calls()))

    def test_dirty_source_is_rejected(self):
        (self.repo / "note.txt").write_text("unsaved edit\n")
        result = self.release()
        self.assertNotEqual(result.returncode, 0, "publishing must reject uncommitted source")
        self.assertIn("clean", result.stderr)
        self.assertEqual(self.calls(), [])

    def test_tooling_only_change_does_not_build_publish_or_contact_registry(self):
        previous = self.revision
        self.next_revision('deploy/production/README.md', 'Updated release instructions\n')
        result = self.release('--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('No runtime changes', result.stdout)
        self.assertEqual(self.calls(), [], 'operator-only releases need no image or cloud access')

    def test_operator_only_head_can_reuse_a_committed_runtime_candidate(self):
        runtime=self.revision
        self.next_revision('deploy/production/README.md','Operator selection update\n')
        self.env['FIXTURE_REVISION']=runtime
        result=self.release('--revision',runtime,'--verify-only')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('Verified source: '+runtime,result.stdout)
        receipt=json.loads((self.root/'cache/forgetmenote/releases'/runtime/'verification.json').read_text())
        self.assertEqual(receipt['source_revision'],runtime)
        self.assertEqual(receipt['plan']['operator_revision'],self.revision)

    def test_prior_candidate_cannot_skip_newer_application_regressions(self):
        runtime=self.revision
        self.next_revision('internal/store/main_test.go','package store\n// New regression\n')
        self.env['FIXTURE_REVISION']=runtime
        result=self.release('--revision',runtime,'--verify-only')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('excludes newer verification inputs',result.stderr)
        self.assertEqual(self.calls(),[])

    def test_prior_candidate_cannot_bypass_new_runtime_changes(self):
        runtime=self.revision
        self.next_revision('internal/store/main.go','package store\n// New runtime\n')
        result=self.release('--revision',runtime,'--verify-only')
        self.assertNotEqual(result.returncode,0)
        self.assertIn('differs from current runtime inputs',result.stderr)
        self.assertEqual(self.calls(),[])

    def test_runtime_change_still_runs_the_complete_image_gate(self):
        previous = self.revision
        self.next_revision('internal/store/main.go', 'package store\n// runtime change\n')
        smokes = lambda: sorted(a for c in self.calls() if c['tool'] == 'make' for a in c['args'] if a.startswith('smoke-'))
        result = self.release('--since', previous, '--verify-only')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(any(c['tool'] == 'docker' and 'build' in c['args'] for c in self.calls()))
        # A storage change runs every host test but only the quick containers.
        self.assertEqual(smokes(), ['smoke-chat', 'smoke-cluster', 'smoke-core', 'smoke-headless', 'smoke-resource'])
        self.assertTrue(any(c['tool'] == 'make' and 'check-browser' in c['args'] for c in self.calls()))
        self.git('checkout', '--', 'note.txt')
        explicit = self.release('--since', previous, '--verify-only', '--mode', 'full')
        self.assertEqual(explicit.returncode, 0, explicit.stderr)
        self.assertEqual(len(set(smokes())), 9)

    def test_go_test_only_change_skips_image_but_reports_focused_checks_required(self):
        previous = self.revision
        self.next_revision('internal/store/main_test.go', 'package store\n// changed test\n')
        result = self.release('--since', previous)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('focused checks', result.stdout)
        self.assertEqual(self.calls(), [])

    def test_embedded_test_file_is_still_a_runtime_change(self):
        self.next_revision('internal/store/main.go', 'package store\n\t//go:embed main_test.go\n')
        previous = self.revision
        self.next_revision('internal/store/main_test.go', 'package store\n// embedded content changed\n')
        result = self.release('--since', previous, '--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)
        self.assertIn('main-' + self.revision, result.stdout)
        self.assertIn('"mode": "full"', result.stdout)

    def test_connector_host_fixture_skips_image_but_embedded_fixture_deploys(self):
        paths = ('internal/connectors/testdata/automation-http.json',)
        for path in paths:
            with self.subTest(path=path):
                (self.repo / path).parent.mkdir(parents=True, exist_ok=True)
                previous = self.revision
                self.next_revision(path, '{}\n')
                result = self.release('--since', previous, '--dry-run')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('No runtime changes', result.stdout)
        self.assertEqual(self.calls(), [])
        self.next_revision('internal/connectors/main.go',
                           'package connectors\n//go:embed testdata/automation-http.json\n')
        previous = self.revision
        self.next_revision(paths[0], '{"embedded":true}\n')
        result = self.release('--since', previous, '--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)

    def test_go_implementation_under_operator_directory_still_requires_a_release(self):
        previous = self.revision
        self.next_revision('deploy/production/helper.go', 'package helper\n')
        result = self.release('--since', previous, '--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)
        self.assertIn('"mode": "full"', result.stdout)

    def test_runtime_removal_and_unknown_files_cannot_be_skipped(self):
        previous = self.revision
        self.git('rm', 'internal/store/main.go')
        self.git('commit', '-m', 'Remove implementation')
        result = self.release('--since', previous, '--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)
        previous = self.git('rev-parse', 'HEAD').stdout.strip()
        self.next_revision('new-runtime-input.txt', 'Unknown inputs require release review\n')
        result = self.release('--since', previous, '--dry-run')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)

    def test_unknown_deployed_revision_fails_without_building(self):
        result = self.release('--since', 'not-a-revision')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.calls(), [])

    def test_deployed_commit_from_a_replaced_history_runs_the_complete_gate(self):
        # Production can still run a commit of the repository this history replaced.
        foreign = hashlib.sha1(b'commit of a replaced repository').hexdigest()
        result = self.release('--since', foreign, '--verify-only')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn('No runtime changes', result.stdout)
        self.assertTrue(any(c['tool'] == 'docker' and 'build' in c['args'] for c in self.calls()))
        self.assertTrue(any(c['tool'] == 'make' and 'check-browser' in c['args'] for c in self.calls()))

    def test_ghcr_preview_needs_no_aws_account(self):
        result = self.release("--dry-run")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(REGISTRY + ":main-" + self.revision, result.stdout)
        self.assertEqual(self.calls(), [])

    def test_github_check_shares_image_layers_through_the_actions_cache_only_when_asked(self):
        result = self.release("--verify-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        build = next(c for c in self.calls() if c["tool"] == "docker" and "build" in c["args"])
        self.assertFalse(any(arg.startswith("--cache") for arg in build["args"]))
        self.git("checkout", "--", "note.txt")
        shutil.rmtree(self.root / "cache")
        (self.root / "calls.jsonl").unlink()
        self.env["FMN_IMAGE_CACHE"] = "gha"
        result = self.release("--verify-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        build = next(c for c in self.calls() if c["tool"] == "docker" and "build" in c["args"])
        cache = build["args"][build["args"].index("--cache-from"):][:4]
        self.assertEqual(cache, ["--cache-from", "type=gha,scope=forgetmenote-image",
                                 "--cache-to", "type=gha,scope=forgetmenote-image,mode=max"])
        self.git("checkout", "--", "note.txt")
        self.env["FMN_IMAGE_CACHE"] = "registry"
        result = self.release("--verify-only")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("FMN_IMAGE_CACHE", result.stderr)

    def test_ghcr_lookups_read_the_registry_signed_in_not_the_package_api(self):
        # GitHub's package API refuses a workflow's own token; the registry accepts it.
        result = self.release()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls()
        logins = [c["args"][1] for c in calls if c["tool"] == "docker" and "login" in c["args"]]
        # Only the sign-in's user name comes from the API, once per login.
        self.assertEqual([c["args"] for c in calls if c["tool"] == "gh" and c["args"][0] == "api"],
                         [["api", "--hostname", "github.com", "user"]] * len(logins))
        lookups = [c["args"] for c in calls if c["tool"] == "docker" and "imagetools" in c["args"]]
        self.assertEqual(len(lookups), 3, "before building, before pushing and after pushing")
        for lookup in lookups:
            self.assertEqual(lookup[:2], ["--config", lookup[1]])
            self.assertIn(lookup[1], logins)
            self.assertEqual(lookup[-1], REGISTRY + ":main-" + self.revision)
        self.assertFalse(any(Path(config).exists() for config in logins), "temporary Docker credentials must be removed")

    def test_release_publishes_verified_snapshot_with_isolated_credentials(self):
        result = self.release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(REGISTRY + "@" + DIGEST, result.stdout)
        calls = self.calls()
        build = next(c for c in calls if c["tool"] == "docker" and "build" in c["args"])
        self.assertEqual(build["source"], "committed source\n")
        self.assertFalse(build["ignored_file_present"])
        self.assertIn("linux/amd64", build["args"])
        self.assertIn("org.opencontainers.image.source=https://github.com/example-owner/forgetmenote", build["args"])
        self.assertTrue(any(c["tool"] == "make" and "check-browser" in c["args"] for c in calls))
        self.assertEqual(sum(c["tool"] == "go" and "-json" in c["args"] and "-race" in c["args"] and c['args'][0] == 'test' for c in calls), 6)
        smoke = next(c for c in calls if c["tool"] == "make" and "smoke-totp" in c["args"])
        self.assertIn("SMOKE_IMAGE=" + IMAGE_ID, smoke["args"])
        push = next(c for c in calls if c["tool"] == "docker" and "push" in c["args"])
        self.assertGreater(calls.index(push), calls.index(smoke))
        self.assertIn(REGISTRY + ":main-" + self.revision, push["args"])
        tag = next(c for c in calls if c["tool"] == "docker" and "tag" in c["args"])
        self.assertIn(IMAGE_ID, tag["args"])
        login = next(c for c in calls if c["tool"] == "docker" and "login" in c["args"])
        self.assertEqual(login["password"], "synthetic-oauth-token")
        self.assertEqual(login["context"], "desktop-linux")
        self.assertTrue(login["contexts_available"])
        self.assertNotIn("synthetic-oauth-token", result.stdout + result.stderr)
        self.assertFalse(Path(login["args"][1]).exists(), "temporary Docker credentials must be removed")
        self.assertEqual((Path(self.env["DOCKER_CONFIG"]) / "config.json").read_text(), self.original_config)

    def test_ecr_release_signs_in_through_aws_and_records_the_pushed_digest(self):
        self.env["PUSHED_REPOSITORY"] = ECR
        result = self.release("--image", ECR)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Deploy this image: " + ECR + "@" + DIGEST, result.stdout)
        calls = self.calls()
        self.assertFalse(any(c["tool"] == "gh" for c in calls), "an ECR release needs no GitHub access")
        password = next(c for c in calls if c["tool"] == "aws" and "get-login-password" in c["args"])
        self.assertIn("us-east-1", password["args"])
        login = next(c for c in calls if c["tool"] == "docker" and "login" in c["args"])
        self.assertEqual(login["password"], "synthetic-ecr-password")
        self.assertEqual(login["args"][-4:], ["--username", "AWS", "--password-stdin", ECR.split("/")[0]])
        self.assertNotIn("synthetic-ecr-password", result.stdout + result.stderr)
        self.assertFalse(Path(login["args"][1]).exists(), "temporary Docker credentials must be removed")
        lookup = next(c["args"] for c in calls if c["tool"] == "aws" and "describe-images" in c["args"])
        self.assertEqual(lookup[-6:], ["--registry-id", "123456789012", "--repository-name", "forgetmenote",
                                       "--image-ids", "imageTag=main-" + self.revision])
        push = next(c for c in calls if c["tool"] == "docker" and "push" in c["args"])
        self.assertEqual(push["args"][-1], ECR + ":main-" + self.revision)
        record = self.root / "cache/forgetmenote/releases" / self.revision / "publication.json"
        publication = json.loads(record.read_text())
        self.assertEqual((publication["image"], publication["registry_digest"]), (ECR + ":main-" + self.revision, DIGEST))

    def test_ecr_digest_must_match_the_pushed_image(self):
        self.env.update(PUSHED_REPOSITORY=ECR, PUSHED_DIGEST="sha256:" + "c" * 64)
        result = self.release("--image", ECR)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match", result.stderr)

    def test_missing_ecr_repository_stops_before_building(self):
        self.env["ECR_MISSING_REPOSITORY"] = "1"
        result = self.release("--image", ECR)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not exist", result.stderr)
        self.assertFalse(any(c["tool"] in ("docker", "make") for c in self.calls()))

    def test_other_registries_and_tagged_references_are_refused(self):
        for image in ("docker.io/example/forgetmenote", ECR + ":latest", ECR.replace("forgetmenote", "Notes"),
                      "12345.dkr.ecr.us-east-1.amazonaws.com/forgetmenote"):
            result = self.release("--dry-run", "--image", image)
            self.assertNotEqual(result.returncode, 0, image)
            self.assertIn("--image must be", result.stderr)
        self.assertIn(ECR + ":main-" + self.revision, self.release("--dry-run", "--image", ECR).stdout)
        self.assertEqual(self.calls(), [])

    def test_failures_never_reach_push(self):
        for failure in ["gh api", "auth token", "docker --config", "build --load", "make test-frontend", "make smoke-core"]:
            with self.subTest(failure=failure):
                self.git("checkout", "--", "note.txt")
                (self.root / "calls.jsonl").unlink(missing_ok=True)
                shutil.rmtree(self.root / "cache", ignore_errors=True)
                self.env["FAIL_COMMAND"] = failure
                result = self.release()
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse((self.root / "pushed").exists())

    def test_existing_tag_returns_digest_without_rebuilding(self):
        self.env["EXISTING_IMAGE"] = "1"
        result = self.release()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(REGISTRY + "@" + DIGEST, result.stdout)
        self.assertFalse(any(c["tool"] == "make" or c["tool"] == "docker" and ("build" in c["args"] or "push" in c["args"])
                             for c in self.calls()))

    def test_wrong_architecture_is_rejected(self):
        self.env["IMAGE_ARCH"] = "arm64"
        result = self.release()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("linux/amd64", result.stderr)
        self.assertFalse((self.root / "pushed").exists())

    def test_postpublication_check_rejects_a_mismatched_digest(self):
        self.env["PUSHED_DIGEST"] = "sha256:" + "c" * 64
        result = self.release()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not match", result.stderr)
        self.assertTrue((self.root / "pushed").exists())

    def test_push_failure_cleans_temporary_login(self):
        self.env["FAIL_COMMAND"] = "push"
        result = self.release()
        self.assertNotEqual(result.returncode, 0)
        login = next(c for c in self.calls() if c["tool"] == "docker" and "login" in c["args"])
        self.assertFalse(Path(login["args"][1]).exists())
        self.assertEqual((Path(self.env["DOCKER_CONFIG"]) / "config.json").read_text(), self.original_config)

    def test_failed_check_requires_explicit_retry_and_reuses_successful_work(self):
        self.env["FAIL_COMMAND"] = "make smoke"
        failed = self.release()
        self.assertNotEqual(failed.returncode, 0)
        self.assertFalse((self.root / "pushed").exists())
        self.git("checkout", "--", "note.txt")
        self.env.pop("FAIL_COMMAND")
        blocked = self.release()
        self.assertNotEqual(blocked.returncode, 0, "a prior failure must not silently become an automatic retry")
        self.assertIn("retry-failed", blocked.stderr)
        resumed = self.release("--retry-failed")
        self.assertEqual(resumed.returncode, 0, resumed.stderr)
        builds = [c for c in self.calls() if c["tool"] == "docker" and "build" in c["args"]]
        self.assertEqual(len(builds), 1, "resumption must preserve the exact verified image")
        for target in ["check-browser", "test-frontend", "test-python-host", "vet", "format"]:
            self.assertEqual(sum(c["tool"] == "make" and target in c["args"] for c in self.calls()), 1)
        receipts = list((self.root / "cache/forgetmenote/releases" / self.revision).glob("smoke-*.json"))
        self.assertTrue(any(json.loads(p.read_text())["status"] == "failed" for p in receipts))
        record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
        self.assertTrue(record["complete"])
        self.assertEqual(len(record["expected_checks"]), 23)
        self.assertIn("build", record["expected_checks"])
        self.assertEqual(record["source_revision"], self.revision)
        self.assertEqual(record["image_id"], IMAGE_ID)
        self.assertTrue(all(c["status"] == "passed" for c in record["checks"].values()))

    def test_measured_long_package_runs_in_process_batches(self):
        earlier = self.root / "cache/forgetmenote/releases/earlier"
        earlier.mkdir(parents=True)
        log = earlier / "go-store.log"
        package = "forgetmenote/internal/store"
        log.write_text("".join(json.dumps(event) + "\n" for event in [
            {"Action": "pass", "Package": package, "Test": name, "Elapsed": seconds}
            for name, seconds in [("TestFirst", 150), ("TestSecond", 150), ("ExamplePublic", 50), ("FuzzInput", 50)]
        ] + [{"Action": "pass", "Package": package, "Elapsed": 400}]))
        (earlier / "verification.json").write_text(json.dumps({"checks": {"go-forgetmenote-internal-store": {
            "status": "passed", "exit_code": 0, "started_at": 1, "elapsed_seconds": 400, "log": str(log)}}}))
        result = self.release("--verify-only", "--mode", "full")
        self.assertEqual(result.returncode, 0, result.stderr)
        runs = [c["args"][c["args"].index("-run") + 1] for c in self.calls() if c["tool"] == "go"
                and c["args"][:3] == ["test", "-race", "-json"] and c["args"][-1] == package]
        self.assertGreater(len(runs), 1)
        selected = [name for pattern in runs for name in pattern.removeprefix("^(").removesuffix(")$").split("|")]
        self.assertCountEqual(selected, ["TestFirst", "TestSecond", "ExamplePublic", "FuzzInput"])
        record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
        self.assertIn("go-forgetmenote-internal-store-1", record["expected_checks"])
        self.assertNotIn("go-forgetmenote-internal-store", record["expected_checks"])
        self.assertTrue(record["complete"])

    def test_batched_long_package_stays_batched(self):
        earlier = self.root / "cache/forgetmenote/releases/earlier"
        earlier.mkdir(parents=True)
        package = "forgetmenote/internal/store"
        checks = {}
        for index, names in enumerate([["TestFirst", "ExamplePublic"], ["TestSecond", "FuzzInput"]], 1):
            log = earlier / f"go-store-{index}.log"
            log.write_text("".join(json.dumps(event) + "\n" for event in [
                {"Action": "pass", "Package": package, "Test": name, "Elapsed": 70} for name in names
            ] + [{"Action": "pass", "Package": package, "Elapsed": 140}]))
            checks[f"go-forgetmenote-internal-store-{index}"] = {
                "status": "passed", "exit_code": 0, "started_at": index, "elapsed_seconds": 140, "log": str(log)}
        (earlier / "verification.json").write_text(json.dumps({"checks": checks}))
        result = self.release("--verify-only", "--mode", "full")
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
        self.assertIn("go-forgetmenote-internal-store-1", record["expected_checks"])
        self.assertNotIn("go-forgetmenote-internal-store", record["expected_checks"])

    def test_publish_reuses_the_go_checks_its_verify_run_planned(self):
        earlier = self.root / "cache/forgetmenote/releases/earlier"
        earlier.mkdir(parents=True)
        log = earlier / "go-store.log"
        store = "forgetmenote/internal/store"
        log.write_text("".join(json.dumps(event) + "\n" for event in [
            {"Action": "pass", "Package": store, "Test": name, "Elapsed": 100}
            for name in ["TestFirst", "TestSecond", "ExamplePublic", "FuzzInput"]
        ] + [{"Action": "pass", "Package": store, "Elapsed": 400}]))
        (earlier / "verification.json").write_text(json.dumps({"checks": {"go-forgetmenote-internal-store": {
            "status": "passed", "exit_code": 0, "started_at": 1, "elapsed_seconds": 400, "log": str(log)}}}))
        # The candidate's own runs measure store as short and sources as long.
        self.env["CASE_SECONDS"] = json.dumps({store: 30, "forgetmenote/internal/sources": 60})
        verified = self.release("--verify-only", "--mode", "full")
        self.assertEqual(verified.returncode, 0, verified.stderr)
        record = self.root / "cache/forgetmenote/releases" / self.revision / "verification.json"
        planned = json.loads(record.read_text())["expected_checks"]
        self.assertIn("go-forgetmenote-internal-store-1", planned)
        self.assertIn("go-forgetmenote-internal-sources", planned)
        self.git("checkout", "--", "note.txt")
        start = len(self.calls())
        published = self.release("--mode", "full")
        self.assertEqual(published.returncode, 0, published.stderr)
        self.assertCountEqual(json.loads(record.read_text())["expected_checks"], planned)
        self.assertEqual([c["args"] for c in self.calls()[start:] if c["tool"] == "go" and c["args"][:3] == ["test", "-race", "-json"]], [])

    def test_browser_workers_take_a_third_of_the_host_slots(self):
        self.assertIn("between 1 and 12", self.release("--verify-only", "--jobs", "13").stderr)
        result = self.release("--verify-only", "--jobs", "12")
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
        browser = record["checks"]["browser"]
        self.assertEqual(browser["inputs"]["execution"]["env"]["FMN_BROWSER_WORKERS"], "4")

    def test_host_without_timings_starts_the_browser_before_go_tests(self):
        # A GitHub runner keeps no history. Its browser suite took 25 of the
        # gate's 48 minutes and started last, leaving the other slot idle.
        result = self.release("--verify-only", "--jobs", "1")
        self.assertEqual(result.returncode, 0, result.stderr)
        started = [c["args"] for c in self.calls() if c["tool"] == "make" and "check-browser" in c["args"]
                   or c["tool"] == "go" and c["args"][:3] == ["test", "-race", "-json"]]
        self.assertIn("check-browser", started[0])

    def test_gate_installs_every_browser_engine_the_suite_uses(self):
        # A spec in an engine the gate did not install fails to launch:
        # "Executable doesn't exist".
        specs = SCRIPT.parents[2].glob("scripts/browser/*.spec.mjs")
        engines = {"chromium"} | {name for spec in specs for name in re.findall(r"browserName:\s*'(\w+)'", spec.read_text())}
        result = self.release("--verify-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        installs = [c["args"] for c in self.calls() if c["tool"] == "npx" and c["args"][1:3] == ["playwright", "install"]]
        self.assertEqual(len(installs), 1, installs)
        self.assertEqual(set(installs[0][3:]), engines)

    def verified_record(self):
        """A copy of a complete verify-only record, as the publishing job downloads it."""
        result = self.release("--verify-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.git("checkout", "--", "note.txt")
        artifact = self.root / "artifact"
        artifact.mkdir()
        shutil.copyfile(self.root / "cache/forgetmenote/releases" / self.revision / "verification.json",
                        artifact / "verification.json")
        return artifact

    def test_publish_verified_pushes_the_checked_image_without_checking_again(self):
        artifact = self.verified_record()
        start = len(self.calls())
        self.env["GITHUB_ACTOR"] = "example-bot"
        result = self.release("--publish-verified", str(artifact))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Deploy this image: " + REGISTRY + "@" + DIGEST, result.stdout)
        calls = self.calls()[start:]
        self.assertFalse(any(c["tool"] in ("make", "go", "npm", "npx") or c["tool"] == "docker" and "build" in c["args"]
                             for c in calls), "publishing runs no check and builds nothing")
        push = next(c for c in calls if c["tool"] == "docker" and "push" in c["args"])
        self.assertEqual(push["args"][-1], REGISTRY + ":main-" + self.revision)
        tag = next(c for c in calls if c["tool"] == "docker" and "tag" in c["args"])
        self.assertIn(IMAGE_ID, tag["args"])
        login = next(c for c in calls if c["tool"] == "docker" and "login" in c["args"])
        self.assertEqual(login["args"][-4:], ["--username", "example-bot", "--password-stdin", "ghcr.io"])
        self.assertEqual(login["password"], "synthetic-oauth-token")
        self.assertFalse(Path(login["args"][1]).exists(), "temporary Docker credentials must be removed")
        self.assertNotIn("synthetic-oauth-token", result.stdout + result.stderr)
        publication = json.loads((artifact / "publication.json").read_text())
        self.assertEqual(publication, {"source_revision": self.revision, "image_id": IMAGE_ID, "registry_digest": DIGEST,
                                       "image": REGISTRY + ":main-" + self.revision})
        raw = (artifact / "verification.json").read_bytes()
        record = json.loads(raw)
        summary = json.loads((artifact / "verification-summary.json").read_text())
        self.assertEqual(summary["record_sha256"], hashlib.sha256(raw).hexdigest())
        self.assertEqual((summary["source_revision"], summary["image_id"], summary["complete"]), (self.revision, IMAGE_ID, True))
        self.assertEqual(summary["expected_checks"], record["expected_checks"])
        self.assertEqual(set(summary["checks"]), set(record["expected_checks"]))
        self.assertEqual({(c["status"], c["exit_code"]) for c in summary["checks"].values()}, {("passed", 0)})
        self.assertEqual(summary["plan"]["mode"], record["plan"]["mode"])
        # The summary is published: commands, inputs and host paths stay in the record.
        self.assertIn(str(self.root), raw.decode())
        self.assertNotIn(str(self.root), json.dumps(summary))

    def test_publish_verified_refuses_what_the_record_does_not_prove(self):
        artifact = self.verified_record()
        original = (artifact / "verification.json").read_text()
        record = json.loads(original)
        first = record["expected_checks"][0]
        failed = {**record, "checks": {**record["checks"], first: {**record["checks"][first], "status": "failed", "exit_code": 1}}}
        for name, value in [("incomplete", {**record, "complete": False}), ("failed check", failed),
                            ("another revision", {**record, "source_revision": "c" * 40}),
                            ("no expected checks", {**record, "expected_checks": []})]:
            with self.subTest(name):
                (artifact / "verification.json").write_text(json.dumps(value))
                result = self.release("--publish-verified", str(artifact))
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("complete verification record", result.stderr)
        (artifact / "verification.json").write_text(original)
        self.env["NO_LOCAL_IMAGE"] = "1"
        result = self.release("--publish-verified", str(artifact))
        self.assertNotEqual(result.returncode, 0, "the checked image must be loaded")
        self.assertFalse((self.root / "pushed").exists())
        self.assertFalse((artifact / "publication.json").exists())
        for extra in (["--verify-only"], ["--since", self.revision], ["--since-published"], ["--dry-run"]):
            result = self.release("--publish-verified", str(artifact), *extra)
            self.assertNotEqual(result.returncode, 0, extra)
            self.assertIn("--publish-verified", result.stderr)

    def test_publish_verified_rerun_reuses_its_own_image_and_refuses_another(self):
        artifact = self.verified_record()
        self.env["EXISTING_IMAGE"] = "1"
        result = self.release("--publish-verified", str(artifact))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Already published: " + REGISTRY + "@" + DIGEST, result.stdout)
        self.assertFalse((self.root / "pushed").exists())
        self.assertEqual(json.loads((artifact / "publication.json").read_text())["registry_digest"], DIGEST)
        (artifact / "publication.json").unlink()
        self.env["REGISTRY_IMAGE_ID"] = "sha256:" + "c" * 64
        result = self.release("--publish-verified", str(artifact))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("different image", result.stderr)
        self.assertFalse((self.root / "pushed").exists())
        self.assertFalse((artifact / "publication.json").exists())

    def test_since_published_verifies_against_the_newest_published_main_commit(self):
        published = self.revision
        self.next_revision("README.md", "Documentation that published no image\n")
        self.next_revision("internal/web/ui/style.css", "button { gap: .5rem; }\n")
        self.env["PUBLISHED_TAGS"] = "main-" + published
        result = self.release("--verify-only", "--since-published")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Published baseline: " + published, result.stdout)
        record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
        self.assertEqual((record["plan"]["mode"], record["plan"]["baseline"]), ("routine", published))
        lookups = [c["args"][-1] for c in self.calls() if c["tool"] == "docker" and "imagetools" in c["args"]]
        self.assertEqual(lookups, [REGISTRY + ":main-" + self.git("rev-parse", "HEAD^").stdout.strip(),
                                   REGISTRY + ":main-" + published])
        self.assertFalse(any(c["tool"] == "gh" or "login" in c["args"] for c in self.calls()), "the gate never signs in")

    def test_since_published_without_a_readable_baseline_verifies_completely(self):
        self.assertIn("exclusive", self.release("--verify-only", "--since-published", "--since", self.revision).stderr)
        self.next_revision("README.md", "Documentation that published no image\n")
        self.next_revision("internal/web/ui/style.css", "button { gap: .5rem; }\n")
        # A refused read stops the search; an unpublished history is searched to its start.
        for setting, lookups in (({"ANONYMOUS_DENIED": "1"}, 1), ({}, 2)):
            with self.subTest(setting):
                self.git("checkout", "--", "note.txt")
                shutil.rmtree(self.root / "cache", ignore_errors=True)
                (self.root / "calls.jsonl").unlink(missing_ok=True)
                self.env.update(setting)
                result = self.release("--verify-only", "--since-published")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("verifying completely", result.stdout)
                self.assertEqual(sum(c["tool"] == "docker" and "imagetools" in c["args"] for c in self.calls()), lookups)
                record = json.loads((self.root / "cache/forgetmenote/releases" / self.revision / "verification.json").read_text())
                self.assertEqual(record["plan"]["mode"], "full")
                self.env.pop("ANONYMOUS_DENIED", None)

    def test_verify_only_runs_without_github_or_credentials(self):
        result = self.release("--verify-only")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(any(c["tool"] == "gh" or "push" in c["args"] or "login" in c["args"] for c in self.calls()))
        self.assertIn("Verified image: " + IMAGE_ID, result.stdout)


if __name__ == "__main__":
    unittest.main()
