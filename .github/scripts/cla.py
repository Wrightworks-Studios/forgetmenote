#!/usr/bin/env python3
"""Contributor License Agreement check for pull requests (.github/workflows/cla.yml).

Every author and committer of a pull request's commits agrees to CLA.md by
commenting SENTENCE on a pull request, or is named in a corporate agreement
(CLA-corporate.md) that a maintainer recorded. Bots and repository admins are
exempt. Agreements live in signatures.json on the cla-signatures branch, which
shares no history with the code. The result is the `cla` commit status on the pull
request's head, which the merge queue requires; a merge queue entry passes, since
each of its pull requests passed before it could join.

The workflow runs the base branch's copy of this file with its own token and never
runs pull request code. Standard library only.

signatures.json:
  {"individual": [{"login", "id", "signedAt", "pullRequest", "comment",
                   "agreement", "agreementSha"}],
   "corporate": [{"entity", "signedAt", "agreement",
                  "contributors": [{"login", "id"?}]}]}
Corporate entries are added by hand once a signed agreement arrives; a
contributor matches by account id when the entry has one, else by login.
"""
import base64
import json
import os
import re
import sys
import time
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen

CONTEXT = 'cla'
BRANCH = 'cla-signatures'
FILE = 'signatures.json'
AGREEMENT = 'CLA.md'
CORPORATE = 'CLA-corporate.md'
SENTENCE = 'I have read the ForgetMeNote Contributor License Agreement and I agree to it.'
MARKER = '<!-- forgetmenote-cla -->'
COMMENTER = 'github-actions[bot]'
MAX_COMMITS = 250  # the most the pull request commits endpoint lists


class GitHubError(Exception):
    def __init__(self, code, method, path, detail):
        super().__init__('%s %s: HTTP %d %s' % (method, path, code, detail))
        self.code = code


class GitHub:
    def __init__(self, api, repository, token):
        self.api = api.rstrip('/')
        self.repo = '/repos/' + repository
        self.token = token

    def request(self, method, path, body=None, missing_ok=False):
        data = None if body is None else json.dumps(body).encode()
        request = Request(self.api + path, data=data, method=method, headers={
            'Authorization': 'Bearer ' + self.token,
            'Accept': 'application/vnd.github+json',
            'X-GitHub-Api-Version': '2022-11-28',
            'User-Agent': 'forgetmenote-cla-check',
        })
        if data is not None:
            request.add_header('Content-Type', 'application/json')
        try:
            with urlopen(request, timeout=30) as response:
                text = response.read()
        except HTTPError as error:
            with error:
                detail = error.read()[:300].decode(errors='replace')
            if missing_ok and error.code == 404:
                return None
            raise GitHubError(error.code, method, path, detail) from None
        return json.loads(text) if text else None

    def pages(self, path):
        result, page = [], 1
        while True:
            batch = self.request('GET', '%s?per_page=100&page=%d' % (path, page))
            result.extend(batch)
            if len(batch) < 100:
                return result
            page += 1


def normalized(text):
    lines = [re.sub(r'^\s*>', '', line) for line in (text or '').splitlines()]
    return ' '.join(' '.join(lines).split()).lower().rstrip('.')


def agrees(text):
    return normalized(text) == normalized(SENTENCE)


def people(commits):
    """Accounts that authored or committed the commits, and commits whose author or
    committer email belongs to no account (so no one can be checked for them)."""
    found, unlinked = {}, []
    for entry in commits:
        for role in ('author', 'committer'):
            account = entry.get(role)
            if not account or not account.get('login'):
                if entry['sha'][:7] not in unlinked:
                    unlinked.append(entry['sha'][:7])
            elif not (role == 'committer' and account['login'] == 'web-flow'):
                found.setdefault(account['id'], account)
    return list(found.values()), unlinked


def covered(signatures, account):
    login = account['login'].lower()
    for entry in signatures.get('individual', []):
        if entry.get('id') == account['id']:
            return True
    for agreement in signatures.get('corporate', []):
        for person in agreement.get('contributors', []):
            if 'id' in person and person['id'] == account['id']:
                return True
            if 'id' not in person and person.get('login', '').lower() == login:
                return True
    return False


def exempt(github, account, permissions):
    if account.get('type') == 'Bot' or account['login'].endswith('[bot]'):
        return True
    login = account['login']
    if login not in permissions:
        answer = github.request('GET', '%s/collaborators/%s/permission' % (github.repo, quote(login)), missing_ok=True)
        permissions[login] = (answer or {}).get('permission')
    return permissions[login] == 'admin'


def load(github):
    found = github.request('GET', '%s/contents/%s?ref=%s' % (github.repo, FILE, BRANCH), missing_ok=True)
    if found is None:
        return {'individual': [], 'corporate': []}, None
    return json.loads(base64.b64decode(found['content'])), found['sha']


def save(github, signatures, sha, message):
    text = json.dumps(signatures, indent=2) + '\n'
    if sha is None:
        try:
            tree = github.request('POST', github.repo + '/git/trees', {
                'tree': [{'path': FILE, 'mode': '100644', 'type': 'blob', 'content': text}]})
            commit = github.request('POST', github.repo + '/git/commits', {
                'message': message, 'tree': tree['sha'], 'parents': []})
            github.request('POST', github.repo + '/git/refs', {'ref': 'refs/heads/' + BRANCH, 'sha': commit['sha']})
            return
        except GitHubError as error:
            if error.code != 422:  # the branch exists, without the file
                raise
    body = {'message': message, 'branch': BRANCH, 'content': base64.b64encode(text.encode()).decode()}
    if sha:
        body['sha'] = sha
    github.request('PUT', '%s/contents/%s' % (github.repo, FILE), body)


def record(github, entry):
    """Append one agreement, rereading after a concurrent write."""
    for attempt in range(5):
        signatures, sha = load(github)
        if covered(signatures, entry):
            return
        signatures.setdefault('individual', []).append(entry)
        try:
            save(github, signatures, sha, 'Record the CLA agreement of %s (#%d)' % (entry['login'], entry['pullRequest']))
            return
        except GitHubError as error:
            if error.code not in (409, 422) or attempt == 4:
                raise
            time.sleep(attempt)


def report(github, number, body, passing):
    """Keep one status comment: post it when the check fails, and edit it after."""
    mine = [c for c in github.pages('%s/issues/%d/comments' % (github.repo, number))
            if c['user']['login'] == COMMENTER and c['body'].startswith(MARKER)]
    if mine:
        if mine[0]['body'] != body:
            github.request('PATCH', '%s/issues/comments/%d' % (github.repo, mine[0]['id']), {'body': body})
    elif not passing:
        github.request('POST', '%s/issues/%d/comments' % (github.repo, number), {'body': body})


def status(github, sha, state, description, target):
    github.request('POST', '%s/statuses/%s' % (github.repo, sha), {
        'state': state, 'context': CONTEXT, 'description': description[:140], 'target_url': target})


def message(links, missing, unlinked, too_many):
    agreement, corporate = links
    lines = [MARKER, '**Contributor License Agreement**', '']
    if not (missing or unlinked or too_many):
        lines.append('Every author of this pull request has agreed to the [ForgetMeNote Contributor '
                     'License Agreement](%s). Thank you.' % agreement)
        return '\n'.join(lines) + '\n'
    lines.append('Before this pull request can merge, each author and committer of its commits needs to '
                 'agree to the [ForgetMeNote Contributor License Agreement](%s).' % agreement)
    if too_many:
        lines += ['', 'It has more than %d commits, more than GitHub lists, so the check can’t see them all. '
                  'Split it into smaller pull requests.' % MAX_COMMITS]
    if missing:
        lines += ['', 'Still to agree: ' + ', '.join('@' + login for login in missing), '',
                  'To agree, comment on this pull request with exactly:', '', '> ' + SENTENCE, '',
                  'You agree once, and it covers your later contributions too. If your employer owns your '
                  'work, it signs the [corporate agreement](%s) instead, and a maintainer records it.' % corporate]
    if unlinked:
        lines += ['', 'These commits have an author or committer email that isn’t linked to a GitHub '
                  'account, so the check can’t tell who wrote them: ' + ', '.join('`%s`' % sha for sha in unlinked)
                  + '. Add the email to your GitHub account, or amend the commits with your GitHub noreply '
                  'address, then push.']
    return '\n'.join(lines) + '\n'


def run(event_name, event, github):
    repository = event['repository']
    base = '%s/blob/%s/' % (repository['html_url'], repository['default_branch'])
    links = (base + AGREEMENT, base + CORPORATE)
    if event_name == 'merge_group':
        status(github, event['merge_group']['head_sha'], 'success',
               'Each pull request is checked before it joins the queue', links[0])
        return 0
    comment = None
    if event_name == 'pull_request_target':
        pull = event['pull_request']
    elif event_name == 'issue_comment':
        if 'pull_request' not in event['issue']:
            return 0
        pull = github.request('GET', '%s/pulls/%d' % (github.repo, event['issue']['number']))
        comment = event['comment']
    else:
        raise SystemExit('the CLA check runs for pull requests, comments and the merge queue, not ' + event_name)
    number, head = pull['number'], pull['head']['sha']

    too_many = pull['commits'] > MAX_COMMITS
    accounts, unlinked = ([], []) if too_many else people(github.pages('%s/pulls/%d/commits' % (github.repo, number)))
    signatures, _ = load(github)
    permissions = {}
    missing = [a for a in accounts if not covered(signatures, a) and not exempt(github, a, permissions)]

    if comment and agrees(comment.get('body')):
        signer = next((a for a in missing if a['id'] == comment['user']['id']), None)
        if signer:
            agreement = github.request('GET', '%s/contents/%s?ref=%s' % (
                github.repo, AGREEMENT, repository['default_branch']))
            record(github, {
                'login': signer['login'], 'id': signer['id'], 'signedAt': comment['created_at'],
                'pullRequest': number, 'comment': comment['html_url'],
                'agreement': AGREEMENT, 'agreementSha': agreement['sha']})
            missing.remove(signer)

    logins = [a['login'] for a in missing]
    report(github, number, message(links, logins, unlinked, too_many), not (too_many or missing or unlinked))
    if too_many:
        status(github, head, 'failure', 'More than %d commits: too many to check' % MAX_COMMITS, links[0])
    elif missing or unlinked:
        count = len(missing) + len(unlinked)
        status(github, head, 'failure', '%d %s still to agree to the CLA' % (
            count, 'author' if count == 1 else 'authors'), links[0])
    else:
        status(github, head, 'success', 'Every author agreed to the CLA', links[0])
    return 0


def main():
    with open(os.environ['GITHUB_EVENT_PATH'], encoding='utf-8') as source:
        event = json.load(source)
    github = GitHub(os.environ.get('GITHUB_API_URL', 'https://api.github.com'),
                    os.environ['GITHUB_REPOSITORY'], os.environ['GITHUB_TOKEN'])
    return run(os.environ['GITHUB_EVENT_NAME'], event, github)


if __name__ == '__main__':
    sys.exit(main())
