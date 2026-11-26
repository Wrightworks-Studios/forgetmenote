"""Reviewed verification scopes for PR feedback and releases.

One selector serves both tiers: fast_check.py for pull requests, release_image.py
for routine releases. Unknown and shared changes use the complete suite."""
import json
from pathlib import Path
import fnmatch
import re
import subprocess


# Path classes, shared by PR feedback, releases and deployment. The Dockerfile copies
# only IMAGE_SOURCES and .dockerignore removes IMAGE_EXCLUDED from them; operator
# tests check both files, and publish.yml's paths-ignore, against these.
IMAGE_SOURCES = ('go.mod', 'go.sum', 'package.json', 'package-lock.json', 'frontend/', 'cmd/', 'internal/', 'plugins/',
                 'deploy/debian.sources', 'deploy/iwork/convert.cpp', 'deploy/git-backup/watcher.py')
IMAGE_EXCLUDED = ('**/*_test.go', '**/*.test.mjs', '**/testdata')
# In GitHub's path filter syntax: documents nothing builds or embeds.
DOCUMENTS = ('docs/**', '*.md', 'deploy/production/**/*.md')
# Release tooling and CI, which operators run from their current checkout.
OPERATOR = ('.github/', 'deploy/production/')
# Tests, fixtures, harnesses, smoke scripts and deployment templates outside the image.
TESTS = ('scripts/', 'deploy/standalone/', 'deploy/integrations/', 'deploy/git-backup/',
         'deploy/iwork/README.md', 'deploy/requirements.txt', 'playwright.config.mjs', 'Makefile',
         'compose.yaml', '.env.example', '.ignore', '.gitignore', '.gitattributes', '.prettierignore')
# Go test tools are main packages, which nothing can import.
GO_TOOLS = ('scripts/mcp-smoke/',)


def github_path_match(path, pattern):
    """GitHub Actions path filters: ** crosses directories, * does not."""
    expression, index = '', 0
    while index < len(pattern):
        if pattern.startswith('**/', index):
            expression, index = expression + '(?:.*/)?', index + 3
        elif pattern.startswith('**', index):
            expression, index = expression + '.*', index + 2
        elif pattern[index] == '*':
            expression, index = expression + '[^/]*', index + 1
        elif pattern[index] in '?+[]!':
            raise ValueError('Unsupported path filter syntax: ' + pattern)
        else:
            expression, index = expression + re.escape(pattern[index]), index + 1
    return re.fullmatch(expression, path) is not None


def image_excluded(path):
    """Docker excludes a path when it, or a directory above it, matches a pattern."""
    parts = path.split('/')
    return any(fnmatch.fnmatchcase(part, pattern.removeprefix('**/')) for pattern in IMAGE_EXCLUDED for part in parts)


def listed(path, entries):
    return any(path == entry or (entry.endswith('/') and path.startswith(entry)) for entry in entries)


def reviewed_class(path):
    """'runtime', 'docs', 'operator' or 'test', or None for a path no table names."""
    if path in ('Dockerfile', '.dockerignore'):
        return 'runtime'
    if path.endswith('.go') and not path.endswith('_test.go') and not path.startswith(GO_TOOLS):
        # Wherever it is, Go implementation could be compiled into the binary.
        return 'runtime'
    if any(github_path_match(path, pattern) for pattern in DOCUMENTS):
        return 'docs'
    if listed(path, IMAGE_SOURCES):
        return 'test' if image_excluded(path) else 'runtime'
    if path.startswith(OPERATOR):
        return 'operator'
    if listed(path, TESTS):
        return 'test'
    return None


def path_class(path):
    """An unreviewed path counts as a runtime input. Embedding is per commit: see
    release_image.runtime_changes."""
    return reviewed_class(path) or 'runtime'


# These are existing end-to-end cases, not weaker copies for the release gate.
# Playwright's grep includes the file name and full test title.
CRITICAL_BROWSER = (
    'groups, invitations and revocation work through real browser sessions',
    'Reader sidebar preserves access restrictions and touch menu dismissal',
    'visual edits preserve named anchors and inert HTML through toggles save reload and undo',
    'creation tabs retain inputs and create a named note in a searched folder',
    'search navigation focuses immediately and library Tags opens working filters',
    'actual private capture remains saved after reload without any AI connection',
    'desktop chat sends no editor text without explicit attachment and retains a lost acknowledgement',
    'revocation clears transcript, composer, attachments, and conversation titles',
    'headless setup retains downloads and functional session revocation',
)

# A routine release can change one of these features. All affected Go package
# race tests still run (including importers); caller, browser and container matrices
# narrow. Shared access/storage/runtime inputs are deliberately absent here.
FEATURES = {
    'appearance': (['internal/web/ui/style.css', 'internal/web/ui/styles/', 'internal/web/ui/brand.html', 'internal/web/ui/fonts/'],
                   ['settings-layout', 'reading-room', 'visual-polish', 'notebook-creation']),
    'chat': (['internal/chat/', 'internal/web/chat*.go', 'internal/web/ui/chat.mjs'],
             ['chat', 'chat-controls', 'chat-readable', 'chat-corpus', 'chat-corpus-settings', 'website-capture']),
    'decisions': (['internal/decision/', 'internal/jev/', 'internal/web/jev*.go', 'internal/web/ui/jev.mjs'],
                  ['jev', 'jev-integration']),
    'sources': (['internal/sources/', 'internal/web/sources*.go', 'internal/web/ui/sources.mjs'],
                ['editor-creation', 'editor-notebooks', 'notebook-documents']),
    'repositories': (['internal/reposource/', 'internal/web/repository*.go', 'internal/web/ui/repositories.mjs'],
                     ['notebook-documents', 'authority', 'containers']),
    # The connector web adapters (data_connections.go, personal_connections.go) enforce
    # grants, so their changes keep the full gate with the other shared access inputs.
    'connectors': (['internal/connectors/', 'internal/web/ui/integrations.mjs'], ['integrations']),
    'writing': (['internal/web/ui/writing.mjs', 'internal/web/ui/writing-terms.mjs'], ['writing-tools']),
}

# Inputs that always take the full gate, whatever else changed: the notes store and
# its Git objects and exports, and the notebook workspace on them, test helpers
# (internal/workspace/workspacetest) included.
FULL_GATE = {
    'storage': ('internal/gitobj/', 'internal/notestore/', 'internal/gitexport/',
                'internal/workspace/', 'internal/blob/', 'internal/pg/', 'internal/durable/'),
}


# Share baseline authorization/storage callers with PR verification. Feature
# modules run their complete unit suites; these are their HTTP/MCP consumers.
CRITICAL_WEB = (
    'TestPrivateRoutesAndSaveConflict',
    'TestBrowserLoginAndCrossOriginWrites',
    'TestPreviewSanitizesMarkup',
    'TestMCPUsesSameStorageAndPermissions',
    'TestSharedActionDispatcherAuthorizationAndSchema',
    'TestMCPActionParityInventory',
    'TestUnclassifiedProjectRouteFailsClosed',
    'TestRegisteredActionsHaveExplicitChatClassification',
    'TestUnclassifiedRegisteredActionCannotEnterChat',
    'TestOAuthCodeBoundToPKCEAndSingleUse',
    'TestOAuthRefreshRotationPersistenceAndUserRevocation',
    'TestVerifiedProxyLoginRegistersDirectoryWithoutGrants',
    'TestUnverifiedProxyRequestsDoNotRegisterAccounts',
    'TestAPICredentialRechecksMembershipAccountAndExpiry',
    'TestAPICredentialNotebookScopesRevocationAndAudience',
    'TestAPIUsesSharedSaveAndConflicts',
    'TestCredentialSnapshotsCannotChangeNotebookRestrictions',
    'TestLiveNotePublicBoundary',
    'TestLiveLongPollStopsWhenNotebookAccessIsRevoked',
    'TestDataQueuedWriteRechecksRevocation',
)
# Globs in internal/web; the operator tests require each to select a current case.
FEATURE_WEB_TESTS = {
    'appearance': ['asset*_test.go','branding*_test.go'],
    # Hosted image and source processing runs through the chat provider configuration.
    'chat': ['chat_test.go','chat_runner_test.go','chat_security_review_test.go',
             'chat_context_controls_test.go','chat_research*_test.go','automation_*_test.go',
             'source_hosted_test.go'],
    'decisions': ['jev*_test.go','review*_test.go','organizer*_test.go'],
    'sources': ['source*_test.go','import*_test.go','notebooks_test.go'],
    'repositories': ['repository*_test.go','source*_test.go','authority*_test.go'],
    # The Integrations page: Slack, Google and the other data connections, and the
    # automations that deliver through them.
    'connectors': ['slack*_test.go','data*_test.go','automation_*slack_test.go',
                   'automation_drive_test.go','automation_*boundary_test.go'],
    'writing': ['writing*_test.go'],
}
FEATURE_CONTAINERS = {
    'appearance': set(), 'chat': {'chat'}, 'decisions': {'jev'},
    'sources': {'source'}, 'repositories': {'source','resource'},
    'connectors': {'chat'}, 'writing': set(),
}


ALL_CONTAINERS = ['core', 'headless', 'source', 'resource', 'chat', 'jev', 'oidc', 'totp', 'cluster']
# Each takes seconds, so every automatic release runs them.
QUICK_CONTAINERS = {'core', 'headless', 'resource', 'chat', 'cluster'}
# These four are about 7 of the 8.5 container minutes on a runner, and the Docker
# lane runs one container at a time. An automatic release runs one only when what
# it exercises changed; an explicit full run keeps all of them.
# Each pattern must match a real path (operator tests); any match selects the scenario.
# TOTP belongs to the identity provider; oidc_smoke.py drives both scenarios.
IDENTITY = (r'^internal/auth/', r'^internal/catalog/', r'^internal/backups/', r'^internal/durable/',
            r'^internal/instanceconfig/', r'^scripts/oidc_smoke\.py$',
            'auth', 'oidc', 'login', 'session', 'password', 'proxy')
SLOW_CONTAINERS = {
    'source': (r'^internal/sources/', r'^internal/reposource/', r'^internal/sourcehttp/', r'^scripts/source_smoke\.py$'),
    'jev': (r'^internal/decision/', r'^internal/jev/', r'^internal/web/jev', r'^scripts/jev_smoke\.py$'),
    'oidc': IDENTITY,
    'totp': IDENTITY,
}
# Smoke scripts are operator inputs, but changing one reruns its feature's scenario.
SMOKE_FEATURES = {'scripts/source_smoke.py': 'sources', 'scripts/jev_smoke.py': 'decisions'}
# The image itself and its dependencies reach every scenario.
IMAGE_INPUTS = r'^(Dockerfile|\.dockerignore|go\.(mod|sum)|package(-lock)?\.json)$|^cmd/|^deploy/(debian\.sources$|iwork/|git-backup/)'


def escalated_containers(paths):
    if not paths or any(re.search(IMAGE_INPUTS, path) for path in paths):
        return list(ALL_CONTAINERS)
    return [name for name in ALL_CONTAINERS if name in QUICK_CONTAINERS or
            any(re.search(pattern, path, re.I) for pattern in SLOW_CONTAINERS[name] for path in paths)]


def path_matches(path, pattern):
    """A trailing slash names a directory tree; anything else is an fnmatch glob."""
    return path.startswith(pattern) if pattern.endswith('/') else fnmatch.fnmatchcase(path, pattern)


def browser_grep(suites):
    return '|'.join([re.escape(title) for title in CRITICAL_BROWSER] +
                    [r'\b' + re.escape(name) + r'\.spec\.mjs\b' for name in sorted(suites)])


def shared_browser_input(path):
    return path == 'playwright.config.mjs' or (path.startswith('scripts/browser/') and not path.endswith('.spec.mjs'))


def feature_of(path):
    return next((name for name, (patterns, _) in FEATURES.items()
                 if any(path_matches(path, pattern) for pattern in patterns)), None)


def selected_web_tests(root, plan, available):
    """The internal/web cases to run, and why: critical boundaries, the reviewed callers of
    each affected feature and every current case in a changed web test file. A full plan,
    or an inventory missing a critical case, keeps every case."""
    available = set(available)
    if plan['mode'] == 'full' or not set(CRITICAL_WEB) <= available:
        return sorted(available), 'Complete web inventory: full plan or a critical case is missing'
    web = Path(root) / 'internal/web'
    paths = {web / Path(name).name for name in plan['changed_paths']
             if Path(name).parent == Path('internal/web') and name.endswith('_test.go')}
    for feature in plan.get('features', []):
        for pattern in FEATURE_WEB_TESTS[feature]:
            paths.update(web.glob(pattern))
    selected = set(CRITICAL_WEB)
    for path in paths:
        if path.is_file():
            selected.update(re.findall(r'^func\s+((?:Test|Example|Fuzz)\w*)\s*\(', path.read_text(), re.M))
    return sorted(selected & available), 'Critical boundaries, affected feature callers and changed web test files'


def go_package_owners(path, package_inputs):
    return {name for name,files in package_inputs.items() if path in files or any(
        Path(path).parent==Path(f).parent if path.endswith('.go') else
        str(Path(f).parent)=='.' or path.startswith(str(Path(f).parent)+'/')
        for f in files if f.endswith('.go'))}


def bundle_input(path):
    """A source npm run build bundles into internal/web/ui/dist. frontend/ and the
    package manifests are no Go package's inputs; only the built bundle is embedded."""
    return path.startswith(('frontend/','internal/web/ui/')) or path in ('package.json','package-lock.json')


def bundle_packages(package_inputs):
    """internal/web, which embeds the bundle, and its production and race-test importers."""
    return {name for name,files in package_inputs.items()
            if any(str(Path(f).parent)=='internal/web' for f in files if f.endswith('.go'))}


def affected_go_packages(plan, package_inputs):
    """The race-test packages to run, and why: every package for a full plan, a dependency
    change or a runtime input no package owns; otherwise each changed input's owners and
    their production and race-test importers, plus internal/web's for bundle sources."""
    everything = set(package_inputs)
    if plan['mode'] == 'full':
        return everything, 'Complete regression: all race tests'
    known = set().union(*(set(files) for files in package_inputs.values()))
    # Deleted Go files still affect the package and every production/test importer.
    directories = {str(Path(name).parent) for files in package_inputs.values() for name in files if name.endswith('.go')}
    selected = set()
    for path in plan['changed_paths']:
        if path in ('go.mod', 'go.sum', 'go.work', 'go.work.sum') or path.startswith('vendor/'):
            return everything, 'Dependency change: all packages'
        if path.endswith('.go') and path not in known and str(Path(path).parent) not in directories:
            return everything, 'Unknown Go input: all packages'
        owners = go_package_owners(path, package_inputs)
        # Deleted embeds and fixtures keep their owners. Test, document and operator
        # inputs that no package reads select nothing; any other runtime input could
        # reach a package unseen, so it selects them all.
        if path not in known and not owners and not bundle_input(path) and path_class(path) == 'runtime':
            return everything, 'Unknown input: all packages'
        selected |= owners
    if any(bundle_input(path) for path in plan['changed_paths']):
        # Bundle sources need not themselves appear in the Go embed inventory.
        selected |= bundle_packages(package_inputs)
    return selected, 'Affected production and race-test import closures'


def browser_cases(report):
    cases={}
    def visit(suite):
        for spec in suite.get('specs',[]):
            cases[Path(spec['file']).name+' '+spec['title']]=spec.get('ok',False)
        for child in suite.get('suites',[]):visit(child)
    for suite in report.get('suites',[]):visit(suite)
    return cases


def retained_browser_failures(previous, plan, retry_failed):
    failures={name:check.get('unresolved_failure',check) for name,check in previous.get('checks',{}).items() if check.get('status')=='failed' or check.get('unresolved_failure')}
    original=previous.get('plan',{}).get('retained_failed_browser_tests',[])
    artifact=previous.get('plan',{}).get('original_failed_browser_report')
    if (previous.get('plan',{}).get('mode')=='full' or original) and any(name!='browser' for name in failures):
        raise ValueError('Cannot narrow while a non-browser check remains failed.')
    if original:
        # A started/interrupted scoped retry is not a pass. Keep its original
        # report even after the current browser receipt gets a new artifact path.
        current=previous.get('checks',{}).get('browser',{})
        passed=set()
        current_report=current.get('artifacts',{}).get('FMN_BROWSER_REPORT')
        if current.get('status')=='passed' and current_report and Path(current_report).is_file():
            report=json.loads(Path(current_report).read_text())
            if not report.get('errors'):passed={name for name,ok in browser_cases(report).items() if ok}
        failed=[name for name in original if name not in passed]
        if not failed:
            plan['retained_failed_browser_tests']=[]
            return []
    else:
        if previous.get('plan',{}).get('mode')!='full' or not failures:return []
        artifact=failures.get('browser',{}).get('artifacts',{}).get('FMN_BROWSER_REPORT')
        failed=[]
    if not retry_failed or any(name!='browser' for name in failures):
        raise ValueError('Cannot narrow a failed full gate without investigating and explicitly retrying every retained failed browser test.')
    if not artifact:raise ValueError('Failed browser report is unavailable; retain full verification.')
    report=json.loads(Path(artifact).read_text())
    if report.get('errors'):raise ValueError('Browser infrastructure failure requires full verification.')
    if not original:failed=[name for name,ok in browser_cases(report).items() if not ok]
    if not failed or any(not re.search(plan['browser_grep'],title) for title in failed):
        raise ValueError('Scoped verification omits an original failed browser test; retain full verification.')
    plan['original_failed_browser_report']=artifact
    plan['retained_failed_browser_tests']=failed
    return failed


def verification_plan(root, previous, revision, requested='auto', *, runtime_paths=None):
    def full(reason, changed=None):
        if requested == 'routine':
            raise ValueError('Routine verification is not allowed: ' + reason)
        # Every Go, web and browser test runs. Unknown changes keep every container.
        explicit = requested == 'full'
        return {'mode': 'full', 'baseline': previous, 'reason': reason,
                'changed_paths': changed or [], 'browser_grep': '',
                'source_profile': 'full', 'jev_profile': 'full',
                'containers': list(ALL_CONTAINERS) if explicit else escalated_containers(changed)}

    if requested == 'full':
        return full('explicit complete regression')
    if not previous:
        return full('no deployed baseline supplied')
    if subprocess.run(['git', 'merge-base', '--is-ancestor', previous, revision], cwd=root).returncode:
        return full('baseline is not an ancestor of this candidate')
    if runtime_paths is None:
        return full('runtime input classification is unavailable')
    paths = subprocess.check_output(['git', 'diff', '--name-only', '--no-renames', '-z', previous, revision], cwd=root).decode().split('\0')
    paths = [path for path in paths if path]
    features, browser = set(), set()
    for path in paths:
        if shared_browser_input(path):
            return full('shared browser fixture or configuration: ' + path, paths)
        # Use the same committed embedding-aware classification as deployment.
        # An embedded test/doc is production content, regardless of its suffix.
        if path not in runtime_paths:
            if path.startswith('scripts/browser/') and path.endswith('.spec.mjs'):
                browser.add(path.rsplit('/', 1)[-1].removesuffix('.spec.mjs'))
            if path in SMOKE_FEATURES:
                features.add(SMOKE_FEATURES[path])
            continue
        if path_class(path) != 'runtime':
            return full('production content in a test/operator path: ' + path, paths)
        storage = next((name for name, patterns in FULL_GATE.items()
                        if any(path_matches(path, pattern) for pattern in patterns)), None)
        if storage:
            return full(storage + ' input: ' + path, paths)
        # Even within a feature, identity/authorization changes escalate.
        if re.search(r'(auth|oauth|permission|credential|secret|session|grant|policy|store|admission|config|authority|catalog|persist|backup)', path, re.I):
            return full('security-sensitive input: ' + path, paths)
        feature = feature_of(path)
        if not feature:
            return full('shared or unclassified input: ' + path, paths)
        features.add(feature)
    for feature in features:
        browser.update(FEATURES[feature][1])
    grep = browser_grep(browser)
    return {'mode': 'routine', 'baseline': previous,
            'reason': 'critical journeys plus affected feature coverage', 'changed_paths': paths,
            'features': sorted(features), 'containers': sorted({'core'} | set().union(*(FEATURE_CONTAINERS[f] for f in features))), 'browser_suites': sorted(browser), 'browser_grep': grep,
            'source_profile': 'full' if 'sources' in features else 'routine',
            'jev_profile': 'full' if 'decisions' in features else 'routine'}
