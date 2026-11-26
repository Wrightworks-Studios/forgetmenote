#!/usr/bin/env python3
"""Actual headless image + CLI, isolated synthetic volume and independent restores."""
import json
import os
from pathlib import Path
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

from smoke_app import app_port, container_token, wait_app
from smoke_database import Database


def run(*args, input=None, check=True):
    result = subprocess.run(args, input=input, text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f'{args[0]} failed ({result.returncode}): {result.stderr}')
    return result


def main():
    image = os.environ.get('SMOKE_IMAGE', 'forgetmenote:poc')
    name = 'fmn-headless-' + uuid.uuid4().hex[:10]
    volume, restored_volume = name+'-data', name+'-restore'
    port = app_port()
    origin = f'http://localhost:{port}'
    run('docker', 'volume', 'create', volume)
    db = Database()

    def start(storage, database='forgetmenote'):
        run('docker','run','--init','-d','--name',name,'--read-only','--tmpfs','/tmp:size=256m',
            '--cap-drop','ALL','--security-opt','no-new-privileges:true','-p',f'127.0.0.1:{port}:8080',
            '-e','PUBLIC_URL=http://localhost:8080','-v',storage+':/data',*db.args(database),image,'serve','--headless')
        wait_app(origin,container_token(name),host='localhost:8080')
        with urllib.request.urlopen(urllib.request.Request(origin+'/readyz',headers={'Host':'localhost:8080'}),timeout=5) as response:
            assert response.status==200

    def cli(*args, input=None, check=True):
        # A deliberately unavailable DATA_DIR proves client dispatch never opens server storage.
        return run('docker','exec','-i','-e','DATA_DIR=/unavailable/server',
                   '-e','FORGETMENOTE_CONFIG_DIR=/data/test-client',name,
                   'forgetmenote',*args,input=input,check=check)

    try:
        db.migrate(image)
        boot=run('docker','run','--rm','-i','-v',volume+':/data',*db.args(),image,'admin','bootstrap',
                 '--username','smoke-admin','--password-stdin',input=secrets.token_urlsafe(24)+'\n')
        user=json.loads(boot.stdout)['user']
        run('docker','run','--rm','-v',volume+':/data',*db.args(),image,'admin','credential','--user',user,
            '--all-projects','--scopes','notes:read,notes:write,notes:manage','--output','/data/smoke.token')
        start(volume)
        token=run('docker','exec',name,'cat','/data/smoke.token').stdout
        cli('auth','login','--server','http://localhost:8080','--token-stdin',input=token)
        token=None
        with urllib.request.urlopen(urllib.request.Request(origin,headers={'Host':'localhost:8080'})) as response:
            assert json.load(response)['headless']
        try:
            urllib.request.urlopen(urllib.request.Request(origin+'/static/app.mjs',headers={'Host':'localhost:8080'}))
            raise AssertionError('headless served application assets')
        except urllib.error.HTTPError as error:
            assert error.code==404
        cli('notebook','create','central','--name','Synthetic central notebook')
        path="notes/CLI space ' quote.md"
        created=json.loads(cli('note','write','central',path,'--create','--file','-','--json',input='# Original\n').stdout)
        # Editor receives the file as a positional argument, including spaces/metacharacters.
        script='#!/bin/sh\nprintf "%s\\n" "# Edited centrally" > "$1"\n'
        run('docker','exec','-i',name,'sh','-c','cat > /tmp/test-editor',input=script)
        edited=json.loads(cli('note','edit','central',path,'--editor','sh /tmp/test-editor','--json').stdout)
        assert edited['revision']!=created['revision']
        noop=json.loads(cli('note','edit','central',path,'--editor','true','--json').stdout)
        assert noop['unchanged'] and noop['revision']==edited['revision']
        conflict=cli('note','write','central',path,'--expected-revision',created['revision'],'--file','-',input='stale',check=False)
        assert conflict.returncode==4 and 'conflict' in conflict.stderr
        assert '# Edited centrally' in cli('document','read','central',path).stdout
        outline=json.loads(cli('action','document_outline','--file','-',input=json.dumps({'project':'central','path':path})).stdout)
        assert outline['revision']==edited['revision'] and outline['stats']['words']==2
        assert outline['sections'][0]['title']=='Edited centrally'
        mentions=json.loads(cli('action','find_term_mentions','--file','-',input=json.dumps({'project':'central','terms':[{'name':'Edited'}]})).stdout)
        assert mentions['complete'] and len(mentions['matches'])==1
        assert mentions['matches'][0]['path']==path and mentions['matches'][0]['revision']==edited['revision']
        assert json.loads(cli('notebook','status','central','--json').stdout)['authority']['type']=='forgetmenote'
        assert 'central' in cli('search','Edited','--notebook','central','--mode','text').stdout
        cli('notebook','map','central')
        cli('notebook','export','central','--output','/data/notebook.bundle')
        with tempfile.TemporaryDirectory(prefix='fmn-headless-restore-') as tmp:
            bundle=Path(tmp)/'notebook.bundle'
            run('docker','cp',name+':/data/notebook.bundle',str(bundle))
            target=Path(tmp)/'notebook'
            run('git','clone',str(bundle),str(target))
            assert '# Edited centrally' in (target/path).read_text()
            assert '# Original' in run('git','-C',str(target),'show',created['revision']+':'+path).stdout
        run('docker','restart',name)
        # Wait through the same readiness gate after restart without recreating the container.
        for _ in range(100):
            check=cli('notebook','list','--json',check=False)
            if check.returncode==0:
                break
            time.sleep(.1)
        assert check.returncode==0 and 'central' in check.stdout
        assert '# Edited centrally' in cli('document','read','central',path).stdout
        run('docker','stop',name)
        run('docker','volume','create',restored_volume)
        run('docker','run','--rm','--user','0','--entrypoint','sh','-v',volume+':/source:ro',
            '-v',restored_volume+':/target',image,'-c','cp -a /source/. /target/')
        restored_db=db.restore_copy()
        db.migrate(image,restored_db)
        run('docker','rm',name)
        start(restored_volume,restored_db)
        assert '# Edited centrally' in cli('document','read','central',path).stdout
        cli('auth','logout')
        assert cli('notebook','list',check=False).returncode!=0
        print('PASS: headless bootstrap, scoped login, actual CLI/editor, no-op, conflict, search/map, writing outline and mentions, restart, Git bundle and full-instance restore, logout')
    finally:
        run('docker','rm','-f',name,check=False)
        run('docker','volume','rm',volume,restored_volume,check=False)
        db.close()


if __name__=='__main__':
    main()
