import {build} from 'esbuild';
import {readFile, writeFile, readdir, rm, mkdir} from 'node:fs/promises';
import {basename, resolve} from 'node:path';
import {verifyFontAssets} from './font-assets.mjs';

const outdir = 'internal/web/ui/dist';
await verifyFontAssets();
await mkdir(outdir, {recursive:true});
// Obsolete hashes must not enter a new image or its immutable allowlist.
for (const name of await readdir(outdir)) {
  if (name !== 'BUILD_REQUIRED.txt') await rm(`${outdir}/${name}`, {recursive:true});
}
const options = {
  bundle:true, format:'esm', minify:true, legalComments:'linked', metafile:true,
  outdir, publicPath:'/static/dist', entryNames:'[name]-[hash]', chunkNames:'chunks/[name]-[hash]',
  assetNames:'assets/[name]-[hash]', logLevel:'info',
};
// Workers have a different environment and must never share DOM chunks.
const worker = await build({...options,
  entryPoints:['frontend/writing-worker.mjs'], conditions:['worker'],
});
const workerFile = Object.entries(worker.metafile.outputs).find(([, info]) => info.entryPoint)?.[0];
const main = await build({...options, splitting:true,
  entryPoints:[
    'internal/web/ui/app.mjs', 'internal/web/ui/assistant.mjs',
    'internal/web/ui/invitation.mjs', 'internal/web/ui/connect-ai.mjs',
    // Embedding pages import the chat panel by its stable /static/chat.mjs URL.
    'internal/web/ui/chat.mjs',
    'internal/web/ui/style.css',
    'frontend/collaboration.mjs', 'frontend/source-view.mjs', 'frontend/source-editor.mjs',
    'frontend/rendered-preview.mjs',
  ],
  loader:{'.ttf':'file', '.woff2':'file'},
  plugins:[{name:'lazy-editor-sources', setup(builder) {
    builder.onResolve({filter:/^\.\/dist\/(collaboration|source-view|source-editor|rendered-preview)\.js$/}, args => ({path:resolve('frontend/'+basename(args.path, '.js')+'.mjs')}));
  }}],
  define:{FMN_WRITING_WORKER_URL:JSON.stringify('/static/dist/'+workerFile.slice(outdir.length+1))},
});
const outputs = {...worker.metafile.outputs, ...main.metafile.outputs};
const publicURL = file => '/static/dist/'+file.slice(outdir.length+1);
const aliases = {};
for (const [file, info] of Object.entries(outputs)) {
  if (!info.entryPoint) continue;
  const name = basename(info.entryPoint);
  const legacy = info.entryPoint.startsWith('frontend/')
    ? '/static/dist/'+name.replace(/\.mjs$/, '.js') : '/static/'+name;
  aliases[legacy] = publicURL(file);
  if (info.cssBundle) aliases['/static/dist/'+name.replace(/\.mjs$/, '.css')] = publicURL(info.cssBundle);
}
// Pages preload the main faces by their stable source names; the alias
// resolves each to its fingerprinted, immutable copy.
for (const [file, info] of Object.entries(outputs)) {
  const input = Object.keys(info.inputs || {})[0] || "";
  if (file.endsWith(".woff2") && input.startsWith("internal/web/ui/fonts/")) aliases["/static/fonts/"+basename(input)] = publicURL(file);
}
// Keep only the actual style/connection dependency closure in headless mode.
const headless = new Set();
function include(file) {
  if (headless.has(publicURL(file))) return;
  headless.add(publicURL(file));
  const info = outputs[file];
  if (!info) throw Error('Missing output dependency '+file);
  for (const dependency of info.imports) if (!dependency.external) include(dependency.path);
  if (info.cssBundle) include(info.cssBundle);
}
for (const [file, info] of Object.entries(outputs)) {
  if (['internal/web/ui/style.css', 'internal/web/ui/invitation.mjs', 'internal/web/ui/connect-ai.mjs'].includes(info.entryPoint)) include(file);
}
// Ship attribution and license texts for every package actually in the bundle.
const packages = new Set();
for (const input of [worker, main].flatMap(result => Object.keys(result.metafile.inputs))) {
  const match = input.match(/^(.*node_modules\/(?:@[^/]+\/)?[^/]+)\//);
  if (match) packages.add(match[1]);
}
const notices = [];
for (const directory of [...packages].sort()) {
  const pkg = JSON.parse(await readFile(directory+'/package.json','utf8'));
  const licenses = (await readdir(directory)).filter(name => /^(licen[cs]e|copying|notice)(\.|$)/i.test(name));
  if (!licenses.length) throw new Error('Missing license text for '+pkg.name);
  notices.push(pkg.name+' '+pkg.version+' ('+pkg.license+')\n'+(await Promise.all(licenses.map(name => readFile(directory+'/'+name,'utf8')))).join('\n'));
}
await writeFile(outdir+'/THIRD_PARTY_LICENSES.txt', notices.join('\n\n--------------------\n\n'));
await writeFile(outdir+'/manifest.json', JSON.stringify({
  version:1, aliases, files:Object.keys(outputs).map(publicURL).sort(), headless:[...headless].sort(),
}, null, 2)+'\n');
