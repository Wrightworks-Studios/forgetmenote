import test from 'node:test';
import assert from 'node:assert/strict';
import {searchFromURL,searchToURL} from './search.mjs';
test('search URL retains query and explicit scope without carrying account credentials',()=>{
 const context={user:{id:'person'},project:{id:'book'},projects:[{id:'book'},{id:'other'}]};
 const snapshot={values:{search:'different words & Unicode Í','folder-filter':'notes/','tag-filter':'work','status-filter':'ready','search-archives':'lower','search-drafts':'exclude'},notebooks:['book','other'],corpus:'both',mode:'text',history:true,trash:true,collection:'archives',account:'person',project:'book'};
 const query=searchToURL(new URLSearchParams('project=book&folder=&page=account&category=account'),snapshot);
 assert.deepEqual(searchFromURL(query,context),snapshot);assert(!query.has('page'));assert(!query.has('account'));assert(!query.has('category'));
 const closed=searchToURL(query,null);assert.equal(closed.toString(),'project=book&folder=');assert.equal(searchFromURL(closed,context),null);
 query.set('search-mode','text"]');assert.equal(searchFromURL(query,context).mode,'all');
 query.append('search-notebook','hidden');assert.throws(()=>searchFromURL(query,context),/unavailable/);
});
