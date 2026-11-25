import test from 'node:test';
import assert from 'node:assert/strict';
import {readdir,readFile} from 'node:fs/promises';
import {join} from 'node:path';

// The package a bare import specifier names ('@scope/name/sub' → '@scope/name').
const packageOf=specifier=>specifier.split('/').slice(0,specifier.startsWith('@')?2:1).join('/');
// Static imports and re-exports (which may span lines), side-effect imports
// and dynamic imports of a literal.
const imports=text=>[
 ...text.matchAll(/^[ \t]*(?:import|export)\b[^;'"`]*?\bfrom[ \t]*['"]([^'"]+)['"]/gm),
 ...text.matchAll(/^[ \t]*import[ \t]*['"]([^'"]+)['"]/gm),
 ...text.matchAll(/\bimport\(\s*['"]([^'"]+)['"]\s*\)/g),
].map(match=>match[1]);
async function sources(dir){
 const found=[];
 for(const entry of await readdir(dir,{withFileTypes:true})){
  if(['node_modules','dist'].includes(entry.name))continue;
  const path=join(dir,entry.name);
  if(entry.isDirectory())found.push(...await sources(path));
  else if(/\.m?js$/.test(entry.name))found.push(path);
 }
 return found;
}

test('import statements are read, including ones that span lines',()=>{
 // Split, so this file's own imports do not include the sample's.
 assert.deepEqual(imports("import {a,\n  b} from '@scope/pkg/sub';\nimport 'side';\nexport {c} from \"./local.mjs\";\nconst x=await im"+"port('lazy');\nconst s='text from \\'quoted\\'';"),
  ['@scope/pkg/sub','./local.mjs','side','lazy']);
 assert.equal(packageOf('@scope/pkg/sub'),'@scope/pkg');assert.equal(packageOf('pkg/sub'),'pkg');
});

// A package that is only some declared package's dependency sits at the top
// of node_modules by chance: the build must not rely on that.
test('every package the app and its tests import is declared in package.json',async()=>{
 const manifest=JSON.parse(await readFile('package.json','utf8'));
 const declared=new Set(Object.keys({...manifest.dependencies,...manifest.devDependencies}));
 const missing={};
 for(const path of [...await sources('frontend'),...await sources('internal/web/ui')]){
  for(const specifier of imports(await readFile(path,'utf8'))){
   if(/^(\.|\/|node:|https?:)/.test(specifier))continue;
   const name=packageOf(specifier);
   if(!declared.has(name))(missing[name] ??= []).push(path);
  }
 }
 assert.deepEqual(missing,{});
});
