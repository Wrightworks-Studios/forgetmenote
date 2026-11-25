import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtemp,cp,symlink,readFile,writeFile,rm,access} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {resolve,join} from 'node:path';
import {execFileSync} from 'node:child_process';
import {verifyFontAssets} from '../../../frontend/font-assets.mjs';

test('verified offline font assets reject changed bytes',async()=>{
 const dir=await mkdtemp(join(tmpdir(),'fmn-font-assets-'));
 try {
  await cp('internal/web/ui/fonts',dir,{recursive:true});
  await verifyFontAssets(dir);
  await writeFile(join(dir,'Onest.woff2'),'changed');
  await assert.rejects(verifyFontAssets(dir),/Font changed without verified regeneration/);
 }finally{await rm(dir,{recursive:true,force:true});}
});

test('clean frontend build creates deterministic hashes with absolute dependencies',async()=>{
 const dir=await mkdtemp(join(tmpdir(),'fmn-clean-assets-'));
 try {
  await cp('frontend',join(dir,'frontend'),{recursive:true});
  await cp('internal/web/ui',join(dir,'internal/web/ui'),{recursive:true,filter:path=>!path.includes('/dist')});
  await cp('package.json',join(dir,'package.json'));
  await symlink(resolve('node_modules'),join(dir,'node_modules'),'dir');
  execFileSync(process.execPath,['frontend/build.mjs'],{cwd:dir,stdio:'pipe'});
  const path=join(dir,'internal/web/ui/dist/manifest.json'),first=await readFile(path,'utf8'),manifest=JSON.parse(first);
  const entry=await readFile(join(dir,'internal/web/ui',manifest.aliases['/static/app.mjs'].slice('/static/'.length)),'utf8');
  assert.match(entry,/from"\/static\/dist\/chunks\//);
  assert.match(entry,/import\("\/static\/dist\/rendered-preview-/);
  const css=await readFile(join(dir,'internal/web/ui',manifest.aliases['/static/style.css'].slice('/static/'.length)),'utf8');
  const fontURLs=[...css.matchAll(/url\("([^"]+\.woff2)"\)/g)].map(match=>match[1]);
  assert.equal(fontURLs.length,4);
  assert.ok(fontURLs.every(url=>url.startsWith('/static/dist/assets/')));
  await writeFile(join(dir,'internal/web/ui/dist/obsolete.js'),'obsolete');
  execFileSync(process.execPath,['frontend/build.mjs'],{cwd:dir,stdio:'pipe'});
  assert.equal(await readFile(path,'utf8'),first);
  await assert.rejects(access(join(dir,'internal/web/ui/dist/obsolete.js')));
 }finally{await rm(dir,{recursive:true,force:true});}
});
