import json
import os
from pathlib import Path
import tempfile
import unittest
import github_app_manifest as app


class ManifestTest(unittest.TestCase):
    def test_distinct_permissions_and_callback_origins(self):
        source=app.manifest('sources','https://notes.example','Sources')
        backup=app.manifest('backups','https://notes.example','Backups',True)
        self.assertEqual(source['default_permissions'],{'metadata':'read','contents':'read'})
        self.assertEqual(backup['default_permissions']['administration'],'write')
        self.assertNotEqual(source['callback_urls'],backup['callback_urls'])
        for origin in ['http://notes.example','https://notes.example/path','https://user@notes.example']:
            with self.assertRaises(ValueError):app.manifest('sources',origin,'App')
        with self.assertRaises(ValueError):app.manifest('sources','https://notes.example','App',True)

    def test_only_hosted_sources_register_a_webhook(self):
        for kind, origin in [('backups','https://notes.example'), ('backups','http://localhost:18083'), ('sources','http://localhost:18083')]:
            with self.subTest(kind=kind, origin=origin):
                value=app.manifest(kind,origin,'Pilot')
                self.assertNotIn('hook_attributes',value)
                self.assertEqual(value['default_events'],[])
        source=app.manifest('sources','https://notes.example','Sources')
        self.assertEqual(source['hook_attributes'],{'url':'https://notes.example/repository/github/webhook','active':True})

    def test_registration_without_a_webhook_still_has_private_local_secret(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory=Path(tmp)
            response=dict(pem='fixture-key',client_secret='fixture-client',webhook_secret=None,id=7,slug='backups',client_id='fixture-id')
            app.save_credentials(directory,response,'backups',False)
            self.assertGreaterEqual(len((directory/'webhook-secret').read_text().strip()),32)

    def test_private_files_never_overwrite_prior_registration(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory=Path(tmp)
            response=dict(pem='fixture-key',client_secret='fixture-client',webhook_secret='fixture-hook',id=7,slug='backups',client_id='fixture-id')
            app.save_credentials(directory,response,'backups',False)
            config=json.loads((directory/'app.json').read_text())
            self.assertFalse(config['allowCreate'])
            self.assertEqual(config['app']['appId'],'7')
            for path in directory.iterdir():self.assertEqual(path.stat().st_mode&0o777,0o600)
            with self.assertRaises(FileExistsError):app.save_credentials(directory,response,'backups',False)
