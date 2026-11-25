import {test} from 'node:test';
import assert from 'node:assert/strict';
import {routeMethod} from './navigation.mjs';

test('every route publisher shares one rule for adding or replacing history entries',()=>{
 const note='/?project=p&collection=notes&note=A.md',inbox='/?area=inbox&project=p';
 assert.equal(routeMethod(note,note,{push:true}),null,'the shown address is left alone');
 assert.equal(routeMethod(note+'#intro',inbox,{push:true}),'pushState','a requested entry is added once routing is ready');
 assert.equal(routeMethod(note,inbox),'replaceState','updates replace the current entry unless asked to add one');
 assert.equal(routeMethod(note,inbox,{push:true,ready:false}),'replaceState','the initial route replaces the entry the page loaded with');
 assert.equal(routeMethod(note,inbox,{push:true,restoring:true}),'replaceState','Back and Forward restorations never add entries');
 assert.equal(routeMethod(inbox+'#stale',inbox,{push:true}),'pushState','the fragment is part of the compared address');
});
