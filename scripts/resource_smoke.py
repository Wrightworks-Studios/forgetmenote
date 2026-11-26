#!/usr/bin/env python3
"""Bounded source concurrency and disk-full recovery using disposable Linux data."""
import base64
from concurrent.futures import ThreadPoolExecutor
import http.client
import io
import json
import os
from pathlib import Path
import subprocess
import time
import uuid
import zipfile
from source_smoke import pdf_bytes
from smoke_app import app_port, container_token, wait_app
from smoke_database import Database

def run(*args):
    return subprocess.check_output(args, text=True, stderr=subprocess.STDOUT).strip()

def send(port,token,path,body=None):
    """Make one API call, reading a reply that arrives before the upload ends.

    At capacity the server refuses a large upload unread and closes, so the
    write can fail with the 429 already waiting. urllib raises there without
    reading it. With no reply, getresponse raises and the smoke still fails.
    """
    raw=None if body is None else json.dumps(body).encode()
    connection=http.client.HTTPConnection('localhost',port,timeout=45)
    try:
        try:
            connection.request('GET' if raw is None else 'POST',path,raw,{'Authorization':'Bearer '+token,'Content-Type':'application/json'})
        except (BrokenPipeError,ConnectionResetError):
            pass
        response=connection.getresponse()
        return response.status,response.read()
    finally:
        connection.close()

def main():
    image=os.environ.get('SMOKE_IMAGE','forgetmenote:poc')
    name='forgetmenote-resource-'+uuid.uuid4().hex[:10]
    port=app_port()
    origin=f'http://localhost:{port}'
    db=Database()
    try:
        db.migrate(image)
        # /data itself is size-limited disposable RAM storage, making ENOSPC safe to test.
        run('docker','run','--init','-d','--name',name,'--memory','512m','--memory-swap','512m','--read-only','--tmpfs','/data:size=128m,uid=1000,gid=1000,mode=700','--tmpfs','/tmp:size=64m,mode=1777','-p',f'127.0.0.1:{port}:8080','-e',f'PUBLIC_URL={origin}',*db.args(),image)
        wait_app(origin,container_token(name))
        token=run('docker','exec',name,'forgetmenote','token')
        def request(path,body=None):
            return send(port,token,path,body)
        limit=16<<20
        html=b'<html><body><p>Budget proof</p>'+b'<!--'+b'x'*1000+b'-->'
        comment=b'<!--'+b'x'*1000+b'-->'
        html=(html+comment*((limit-len(html)-14)//len(comment)))+b'</body></html>'
        pdf=pdf_bytes('Budget proof')
        pdf+=b'\n%'+b'x'*(limit-len(pdf)-2)
        package=io.BytesIO()
        with zipfile.ZipFile(package,'w',zipfile.ZIP_STORED) as archive:
            archive.writestr('word/document.xml','<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>Budget proof</w:t></w:r></w:p></w:body></w:document>')
            archive.writestr('word/media/padding.bin',b'x'*(limit-2048))
        files=[('budget.html',html),('budget.pdf',pdf),('budget.docx',package.getvalue())]
        outcomes=[]
        for filename,data in files:
            payload={'filename':filename,'data':base64.b64encode(data).decode()}
            with ThreadPoolExecutor(max_workers=2) as pool:
                futures=[pool.submit(request,'/p/default/api/sources/preview',payload) for _ in range(2)]
                start=time.monotonic();status,_=request('/p/default/api/notes');elapsed=time.monotonic()-start
                assert status==200 and elapsed<5, (status,elapsed)
                responses=[future.result() for future in futures]
                results=[code for code,_ in responses]
            assert 200 in results and all(code in (200,429) for code in results), (filename,results)
            if filename.endswith('.pdf'):
                for code, body in responses:
                    if code == 200:
                        record=json.loads(body)['capture']['record']
                        assert record['extraction']=='extracted' and 'Tesseract' in record['extractor'],record
                        assert 'Verify against the retained original' in record['warning'],record
                        assert 'Budget proof' in json.loads(body)['preview']
            outcomes.append((filename,len(data),results))
        # A successful save remains readable after a subsequent ENOSPC failure.
        status,body=request('/p/default/api/note',{'path':'proof.md','content':'# Before full disk','revision':''})
        assert status==200,body
        note=json.loads(body)
        fill=subprocess.run(['docker','exec',name,'sh','-c','dd if=/dev/zero of=/data/fill bs=1M 2>/dev/null'],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        assert fill.returncode!=0
        time.sleep(5.1)
        assert request('/readyz')[0]==503
        # Text of 8 KiB or more is kept as content blocks in the blob store on
        # /data (a shorter note is a Postgres row, which this disk doesn't hold).
        long_text='# Must not replace committed text\n\n'+'A paragraph that needs the data disk.\n\n'*1024
        assert request('/p/default/api/note',{'path':'proof.md','content':long_text,'revision':note['revision']})[0]>=400
        run('docker','exec',name,'rm','/data/fill')
        status,body=request('/p/default/api/note?path=proof.md')
        assert status==200 and json.loads(body)['content']=='# Before full disk'
        status,body=request('/p/default/api/status')
        assert json.loads(body)['revision']==note['revision'] and not json.loads(body)['dirty']
        status,body=request('/p/default/api/note',{'path':'proof.md','content':long_text,'revision':note['revision']})
        assert status==200,body
        assert json.loads(request('/p/default/api/note?path=proof.md')[1])['content']==long_text
        assert run('docker','inspect','--format','{{.State.OOMKilled}}',name)=='false'
        print('PASS: near-16MiB HTML/PDF/DOCX concurrency in 512MiB container, read responsiveness, ENOSPC refusal and recovery',outcomes)
    finally:
        subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
        db.close()
if __name__=='__main__':main()
