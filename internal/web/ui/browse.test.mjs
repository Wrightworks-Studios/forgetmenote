import {test} from 'node:test';
import assert from 'node:assert/strict';
import {coverURL} from './browse.mjs';
test('cover URLs stay in the current project and pin historical images',()=>{
 assert.equal(coverURL('demo','assets/my cover.webp','abc'),'/p/demo/assets/assets/my%20cover.webp?revision=abc');
 for(const cover of ['https://example.org/p.png','//example.org/p.png','assets/../private.png','assets/%2e%2e/p.png','assets/.hidden/p.png','assets/a.svg','assets/a.png?x','assets//a.png',null]) assert.equal(coverURL('demo',cover),null);
});
