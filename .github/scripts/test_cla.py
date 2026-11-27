"""Behaviour of the CLA check against a fake GitHub API served over HTTP."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import threading
import unittest
from urllib.parse import parse_qs, urlparse

import cla

REPO = 'example/notebooks'
HEAD = 'a' * 40
SENTENCE = 'I have read the ForgetMeNote Contributor License Agreement and I agree to it.'


def user(login, uid, kind='User'):
    return {'login': login, 'id': uid, 'type': kind}


def commit(sha, author, committer=None):
    return {'sha': sha, 'author': author, 'committer': committer if committer is not None else author}


class FakeGitHub:
    """The endpoints the check uses, with just enough state to observe it."""

    def __init__(self):
        self.pull = {'number': 7, 'head': {'sha': HEAD}, 'commits': 1}
        self.commits = []
        self.permissions = {}
        self.branches = {}  # branch -> {path: text}
        self.comments = []
        self.statuses = []
        self.requests = []
        self.conflicts = 0
        fake = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                fake.handle(self, 'GET')

            def do_POST(self):
                fake.handle(self, 'POST')

            def do_PUT(self):
                fake.handle(self, 'PUT')

            def do_PATCH(self):
                fake.handle(self, 'PATCH')

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.api = 'http://127.0.0.1:%d' % self.server.server_port

    def close(self):
        self.server.shutdown()
        self.server.server_close()

    def file(self, branch, path):
        return self.branches.get(branch, {}).get(path)

    def blob(self, text):
        return 'blob-%d' % (hash(text) & 0xffffffff)

    def handle(self, request, method):
        url = urlparse(request.path)
        query = parse_qs(url.query)
        length = int(request.headers.get('Content-Length') or 0)
        body = json.loads(request.rfile.read(length)) if length else None
        self.requests.append((method, url.path))
        prefix = '/repos/' + REPO
        path = url.path[len(prefix):] if url.path.startswith(prefix) else None
        status, reply = 404, {'message': 'Not Found'}
        page = int(query.get('page', ['1'])[0])
        if path == '/pulls/7' and method == 'GET':
            status, reply = 200, self.pull
        elif path == '/pulls/7/commits' and method == 'GET':
            status, reply = 200, self.commits[(page - 1) * 100:page * 100]
        elif path and path.startswith('/collaborators/') and method == 'GET':
            login = path.split('/')[2]
            status, reply = 200, {'permission': self.permissions.get(login, 'read')}
        elif path and path.startswith('/contents/') and method == 'GET':
            name = path[len('/contents/'):]
            text = self.file(query['ref'][0], name)
            if text is not None:
                status, reply = 200, {'sha': self.blob(text), 'content': base64.b64encode(text.encode()).decode()}
        elif path and path.startswith('/contents/') and method == 'PUT':
            name = path[len('/contents/'):]
            current = self.file(body['branch'], name)
            if self.conflicts:
                self.conflicts -= 1
                status, reply = 409, {'message': 'conflict'}
            elif body['branch'] not in self.branches:
                status, reply = 404, {'message': 'Branch not found'}
            elif current is not None and body.get('sha') != self.blob(current):
                status, reply = 409, {'message': 'sha does not match'}
            else:
                self.branches[body['branch']][name] = base64.b64decode(body['content']).decode()
                status, reply = 200, {}
        elif path == '/git/trees' and method == 'POST':
            status, reply = 201, {'sha': 'tree-1', 'tree': body['tree']}
            self.tree = body['tree']
        elif path == '/git/commits' and method == 'POST':
            self.assert_orphan = body['parents'] == []
            status, reply = 201, {'sha': 'commit-1'}
        elif path == '/git/refs' and method == 'POST':
            branch = body['ref'][len('refs/heads/'):]
            if branch in self.branches:
                status, reply = 422, {'message': 'Reference already exists'}
            else:
                self.branches[branch] = {entry['path']: entry['content'] for entry in self.tree}
                status, reply = 201, {}
        elif path == '/issues/7/comments' and method == 'GET':
            status, reply = 200, self.comments[(page - 1) * 100:page * 100]
        elif path == '/issues/7/comments' and method == 'POST':
            comment = {'id': len(self.comments) + 1, 'body': body['body'], 'user': user('github-actions[bot]', 41898282, 'Bot')}
            self.comments.append(comment)
            status, reply = 201, comment
        elif path and path.startswith('/issues/comments/') and method == 'PATCH':
            comment = next(c for c in self.comments if c['id'] == int(path.split('/')[-1]))
            comment['body'] = body['body']
            status, reply = 200, comment
        elif path == '/statuses/' + HEAD and method == 'POST' or path == '/statuses/' + 'b' * 40 and method == 'POST':
            self.statuses.append({'sha': path.split('/')[-1], **body})
            status, reply = 201, {}
        data = json.dumps(reply).encode()
        request.send_response(status)
        request.send_header('Content-Type', 'application/json')
        request.send_header('Content-Length', str(len(data)))
        request.end_headers()
        request.wfile.write(data)


class CLACheckTest(unittest.TestCase):
    def setUp(self):
        self.github = FakeGitHub()
        self.addCleanup(self.github.close)
        self.github.branches['next'] = {'CLA.md': '# Agreement\n'}

    def repository(self):
        return {'full_name': REPO, 'default_branch': 'next', 'html_url': 'https://github.example/' + REPO}

    def run_event(self, name, event):
        event = {'repository': self.repository(), **event}
        return cla.run(name, event, cla.GitHub(self.github.api, REPO, 'test-token'))

    def opened(self):
        return self.run_event('pull_request_target', {'pull_request': self.github.pull})

    def commented(self, author, body):
        comment = {'id': 99, 'body': body, 'user': author, 'created_at': '2026-11-27T18:00:00Z',
                   'html_url': 'https://github.example/%s/pull/7#issuecomment-99' % REPO}
        return self.run_event('issue_comment', {'issue': {'number': 7, 'pull_request': {}}, 'comment': comment})

    def last_status(self):
        self.assertTrue(self.github.statuses, 'no status was set')
        status = self.github.statuses[-1]
        self.assertEqual(status['context'], 'cla')
        return status

    def signatures(self):
        text = self.github.file('cla-signatures', 'signatures.json')
        return json.loads(text) if text else None

    def test_an_unsigned_author_blocks_the_pull_request_and_is_told_how_to_agree(self):
        self.github.commits = [commit('1' * 40, user('alice', 101))]
        self.opened()
        status = self.last_status()
        self.assertEqual(status['state'], 'failure')
        self.assertEqual(status['sha'], HEAD)
        self.assertEqual(status['target_url'], 'https://github.example/%s/blob/next/CLA.md' % REPO)
        self.assertEqual(len(self.github.comments), 1)
        body = self.github.comments[0]['body']
        self.assertIn('@alice', body)
        self.assertIn(SENTENCE, body)
        self.assertIn('CLA-corporate.md', body)

    def test_the_authors_comment_records_the_agreement_and_passes_the_check(self):
        self.github.commits = [commit('1' * 40, user('alice', 101))]
        self.opened()
        self.commented(user('alice', 101), SENTENCE)
        self.assertEqual(self.last_status()['state'], 'success')
        self.assertTrue(self.github.assert_orphan, 'the signatures branch should not share history with the code')
        entry, = self.signatures()['individual']
        self.assertEqual(entry['login'], 'alice')
        self.assertEqual(entry['id'], 101)
        self.assertEqual(entry['pullRequest'], 7)
        self.assertEqual(entry['comment'], 'https://github.example/%s/pull/7#issuecomment-99' % REPO)
        self.assertEqual(entry['signedAt'], '2026-11-27T18:00:00Z')
        self.assertEqual(entry['agreement'], 'CLA.md')
        self.assertEqual(entry['agreementSha'], self.github.blob('# Agreement\n'))
        self.assertEqual(len(self.github.comments), 1, 'the status comment is edited, not repeated')
        self.assertNotIn('@alice', self.github.comments[0]['body'])

    def test_an_agreement_counts_by_account_id_after_a_rename(self):
        self.github.branches['cla-signatures'] = {'signatures.json': json.dumps(
            {'individual': [{'login': 'alice', 'id': 101}], 'corporate': []})}
        self.github.commits = [commit('1' * 40, user('alice-renamed', 101))]
        self.opened()
        self.assertEqual(self.last_status()['state'], 'success')
        self.assertEqual(self.github.comments, [])

    def test_a_quoted_or_reformatted_sentence_still_counts(self):
        self.github.commits = [commit('1' * 40, user('alice', 101))]
        self.commented(user('alice', 101), '>  i have read the ForgetMeNote contributor license agreement\nand I agree to it')
        self.assertEqual(self.last_status()['state'], 'success')

    def test_other_comments_and_other_people_record_nothing(self):
        self.github.commits = [commit('1' * 40, user('alice', 101))]
        self.commented(user('mallory', 666), SENTENCE)
        self.commented(user('alice', 101), 'I agree to it, mostly. ' + SENTENCE)
        self.assertIsNone(self.signatures())
        self.assertEqual(self.last_status()['state'], 'failure')

    def test_every_author_and_committer_must_agree(self):
        self.github.commits = [commit('1' * 40, user('alice', 101), user('bob', 102)),
                               commit('2' * 40, user('carol', 103), user('web-flow', 19864447))]
        self.commented(user('alice', 101), SENTENCE)
        status = self.last_status()
        self.assertEqual(status['state'], 'failure')
        body = self.github.comments[-1]['body']
        self.assertIn('@bob', body)
        self.assertIn('@carol', body)
        self.assertNotIn('@alice', body)
        self.assertNotIn('web-flow', body)
        self.commented(user('bob', 102), SENTENCE)
        self.commented(user('carol', 103), SENTENCE)
        self.assertEqual(self.last_status()['state'], 'success')
        self.assertEqual([entry['login'] for entry in self.signatures()['individual']], ['alice', 'bob', 'carol'])

    def test_a_corporate_agreement_covers_the_people_it_names(self):
        self.github.branches['cla-signatures'] = {'signatures.json': json.dumps({'individual': [], 'corporate': [
            {'entity': 'Example Corp', 'agreement': 'CLA-corporate.md',
             'contributors': [{'login': 'Dana'}, {'login': 'erin', 'id': 105}]}]})}
        self.github.commits = [commit('1' * 40, user('dana', 104)), commit('2' * 40, user('erin2', 105))]
        self.opened()
        self.assertEqual(self.last_status()['state'], 'success')

    def test_bots_and_repository_admins_need_no_agreement(self):
        self.github.permissions['owner'] = 'admin'
        self.github.permissions['writer'] = 'write'
        self.github.commits = [commit('1' * 40, user('dependabot[bot]', 49699333, 'Bot')),
                               commit('2' * 40, user('owner', 1))]
        self.opened()
        self.assertEqual(self.last_status()['state'], 'success')
        self.assertEqual(self.github.comments, [], 'a passing check posts nothing')
        self.github.commits.append(commit('3' * 40, user('writer', 2)))
        self.opened()
        self.assertEqual(self.last_status()['state'], 'failure', 'write access alone is not an agreement')

    def test_a_commit_without_a_linked_account_cannot_pass(self):
        self.github.permissions['owner'] = 'admin'
        self.github.commits = [commit('1234567' + '0' * 33, None, user('owner', 1))]
        self.opened()
        self.assertEqual(self.last_status()['state'], 'failure')
        self.assertIn('1234567', self.github.comments[-1]['body'])

    def test_more_commits_than_the_api_lists_fail_closed(self):
        self.github.pull['commits'] = 251
        self.opened()
        status = self.last_status()
        self.assertEqual(status['state'], 'failure')
        self.assertIn('250', status['description'])

    def test_a_second_agreement_is_appended_after_a_concurrent_write(self):
        self.github.branches['cla-signatures'] = {'signatures.json': json.dumps(
            {'individual': [{'login': 'alice', 'id': 101}], 'corporate': []})}
        self.github.commits = [commit('1' * 40, user('bob', 102))]
        self.github.conflicts = 1
        self.commented(user('bob', 102), SENTENCE)
        self.assertEqual([entry['login'] for entry in self.signatures()['individual']], ['alice', 'bob'])
        self.assertEqual(self.last_status()['state'], 'success')

    def test_a_merge_queue_entry_passes_on_its_own_commit(self):
        self.run_event('merge_group', {'merge_group': {'head_sha': 'b' * 40}})
        status = self.last_status()
        self.assertEqual((status['sha'], status['state']), ('b' * 40, 'success'))

    def test_comments_on_issues_are_ignored(self):
        self.run_event('issue_comment', {'issue': {'number': 7}, 'comment': {'body': SENTENCE, 'user': user('alice', 101)}})
        self.assertEqual(self.github.requests, [])


if __name__ == '__main__':
    unittest.main()
