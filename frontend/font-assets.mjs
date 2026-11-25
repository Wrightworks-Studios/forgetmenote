import {readFile} from 'node:fs/promises';
import {createHash} from 'node:crypto';

// Ordinary builds need no Python font toolchain. These pinned, unsubstituted
// deliverables are verified in full by scripts/fonts.py when regenerated.
export async function verifyFontAssets(directory='internal/web/ui/fonts') {
  const manifest=JSON.parse(await readFile(directory+'/manifest.json','utf8'));
  if(manifest.version!==1 || manifest.fonts?.length!==4)throw Error('Invalid font manifest');
  const names=new Set();
  for(const font of manifest.fonts) {
    for(const [name,hash,bytes] of [[font.source,font.sourceSHA256,font.sourceBytes],[font.compressed,font.compressedSHA256,font.compressedBytes]]) {
      if(!/^[A-Za-z0-9-]+\.(ttf|woff2)$/.test(name) || names.has(name))throw Error('Invalid font path');
      names.add(name);
      const data=await readFile(directory+'/'+name);
      if(data.length!==bytes || createHash('sha256').update(data).digest('hex')!==hash)throw Error('Font changed without verified regeneration: '+name);
    }
  }
}
