import importlib.util
import pathlib
import tempfile
import unittest
import yaml
spec=importlib.util.spec_from_file_location('operator_script',pathlib.Path(__file__).with_name('operator.py'))
op=importlib.util.module_from_spec(spec);spec.loader.exec_module(op)
class SetupTests(unittest.TestCase):
 def test_optional_provider_and_preserve_operator_decisions(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   settings={'domain':'notes.example.com','auth_domain':'auth.notes.example.com','admin':'alice','admin_email':'alice@example.com','smtp_address':'submission://smtp.example.com:587','smtp_username':'mailer','smtp_sender':'ForgetMeNote <notes@example.com>','uid':1000,'gid':1000}
   (root/"secrets").mkdir(exist_ok=True)
   (root/"secrets/smtp-password").write_text("disposable-smtp")
   op.render(root,settings)
   config=yaml.safe_load((root/'authelia/configuration.yml').read_text())
   self.assertEqual(config['identity_providers']['oidc']['clients'][0]['authorization_policy'],'one_factor')
   self.assertTrue(config['identity_providers']['oidc']['clients'][0]['require_pkce'])
   self.assertNotIn('redis',config['session'])
   self.assertIn('local',config['storage'])
   before={p.relative_to(root):p.read_bytes() for p in root.rglob('*') if p.is_file()}
   (root/'authelia/configuration.yml').write_text('# operator customization\n')
   op.render(root,settings)
   self.assertEqual((root/'authelia/configuration.yml').read_text(),'# operator customization\n')
   for p,raw in before.items():
    if str(p)!='authelia/configuration.yml':self.assertEqual((root/p).read_bytes(),raw)
 def test_bundle_serves_from_its_own_migrated_database(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   op.render(root,{'domain':'notes.example.com','auth_domain':'auth.notes.example.com','notification_mode':'operator','uid':1000,'gid':1000})
   services=yaml.safe_load((root/'compose.yaml').read_text())['services']
   password=(root/'secrets/database-password').read_text().strip()
   self.assertEqual((root/'secrets/database-url').read_text().strip(),'postgres://forgetmenote:'+password+'@postgres:5432/forgetmenote?sslmode=disable')
   self.assertEqual(services['postgres']['image'],op.POSTGRES)
   self.assertEqual(services['postgres']['environment']['POSTGRES_PASSWORD_FILE'],'/run/secrets/database-password')
   app=services['forgetmenote']
   self.assertEqual(app['depends_on'],{'postgres':{'condition':'service_healthy'}})
   self.assertIn('forgetmenote migrate &&',app['entrypoint'][2])
   self.assertIn('./secrets/database-url:/run/secrets/database-url:ro',app['volumes'])
   self.assertEqual(app['environment']['DATABASE_URL_FILE'],'/run/secrets/database-url')
   self.assertEqual((root/'postgres').stat().st_mode&0o777,0o700)
 def test_account_maintenance_preserves_disabled_state(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   op.atomic(root/'settings.json','{"uid":1000,"gid":1000}')
   hashed=op.PasswordHasher().hash('disposable-password')
   op.user_change(root,'add','alice','alice@example.com',hashed)
   with self.assertRaises(ValueError):op.user_change(root,'add','alice','alice@example.com',hashed)
   op.user_change(root,'disable','alice')
   op.user_change(root,'password','alice',hashed=hashed)
   op.user_change(root,'email','alice',email='new@example.com')
   self.assertTrue(yaml.safe_load((root/'authelia/users.yml').read_text())['users']['alice']['disabled'])
   op.atomic(root/'initial-account.created','alice')
   (root/'authelia/users.yml').unlink()
   with self.assertRaises(ValueError):op.user_change(root,'add','alice','alice@example.com',hashed)

class EmailFreeTests(unittest.TestCase):
 def test_no_email_bundle_is_explicit_and_preserves_customization(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   settings={'domain':'notes.example.com','auth_domain':'auth.notes.example.com','notification_mode':'operator','uid':1000,'gid':1000}
   op.render(root,settings)
   config=yaml.safe_load((root/'authelia/configuration.yml').read_text())
   self.assertNotIn('smtp',config['notifier'])
   self.assertTrue(config['authentication_backend']['password_reset']['disable'])
   self.assertEqual(config['identity_providers']['oidc']['clients'][0]['authorization_policy'],'one_factor')
   ui=(root/'authelia/forgetmenote-assets/locales/en/settings.json').read_text()
   self.assertIn('SMTP',ui)
   self.assertIn('No email was sent',ui)
   self.assertFalse((root/'secrets/smtp-password').exists())
 def test_enrollment_rejects_unknown_disabled_and_existing_qr(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   op.atomic(root/'settings.json','{"uid":1000,"gid":1000}')
   op.user_change(root,'add','alice','',hashed='test')
   with self.assertRaises(ValueError):op.enroll_totp(root,'bob')
   op.user_change(root,'disable','alice')
   with self.assertRaises(ValueError):op.enroll_totp(root,'alice')
   op.user_change(root,'enable','alice')
   op.atomic(root/'authelia/enrollment/alice.png','existing QR')
   with self.assertRaises(ValueError):op.enroll_totp(root,'alice')
   self.assertEqual((root/'authelia/enrollment/alice.png').read_text(),'existing QR')

class NotificationSetupTests(unittest.TestCase):
 def test_setup_defaults_to_no_email_without_smtp_prompts_and_reruns_preserve_user(self):
  from unittest.mock import patch
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   answers=['notes.example.com','','','alice','']
   with patch('builtins.input',side_effect=answers), patch.object(op,'password',return_value='test-hash'), patch.object(op,'compose'):
    op.setup(root)
   self.assertEqual(yaml.safe_load((root/'authelia/users.yml').read_text())['users']['alice']['email'],'')
   op.user_change(root,'disable','alice')
   before={p.relative_to(root):p.read_bytes() for p in root.rglob('*') if p.is_file()}
   with patch('builtins.input',side_effect=AssertionError('unexpected prompt')), patch.object(op,'compose'):
    op.setup(root)
   self.assertEqual(before,{p.relative_to(root):p.read_bytes() for p in root.rglob('*') if p.is_file()})
 def test_smtp_switch_removes_no_email_notices_without_rotating_identity_keys(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   settings={'domain':'notes.example.com','auth_domain':'auth.notes.example.com','notification_mode':'operator','uid':1000,'gid':1000}
   op.render(root,settings)
   config=yaml.safe_load((root/'authelia/configuration.yml').read_text())
   identity=config['identity_providers']
   settings.update(notification_mode='smtp',smtp_address='submission://smtp.example.com:587',smtp_username='test',smtp_sender='test@example.com')
   op.atomic(root/'secrets/smtp-password','test-only')
   op.configure_notifications(root,config,settings)
   self.assertIn('smtp',config['notifier'])
   self.assertNotIn('filesystem',config['notifier'])
   self.assertNotIn('asset_path',config['server'])
   self.assertFalse(config['authentication_backend']['password_reset']['disable'])
   self.assertEqual(config['identity_providers'],identity)

class RebrandCompatibilityTests(unittest.TestCase):
 def test_existing_deployment_service_and_assets_remain_manageable(self):
  with tempfile.TemporaryDirectory() as d:
   root=pathlib.Path(d)
   op.atomic(root/'compose.yaml','services: {gitnotes: {image: gitnotes:local}}')
   self.assertEqual(op.app_service(root),'gitnotes')
   op.atomic(root/'settings.json','{"uid":1000,"gid":1000}')
   config={'server':{'asset_path':'/config/gitnotes-assets'},'authentication_backend':{}}
   op.configure_notifications(root,config,{'notification_mode':'operator','uid':1000,'gid':1000})
   self.assertEqual(config['server']['asset_path'],'/config/forgetmenote-assets')

if __name__ == '__main__':
 unittest.main()
