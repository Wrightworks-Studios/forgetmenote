import {test} from 'node:test';
import assert from 'node:assert/strict';
import {keepOrder,notebookChoices} from './pickers.mjs';

test('a chosen list keeps its saved order and adds new choices in offered order',()=>{
 assert.deepEqual(keepOrder(['reports','archive'],['archive','default','reports']),['reports','archive','default']);
 assert.deepEqual(keepOrder(['5','1'],['1']),['1']);
 assert.deepEqual(keepOrder([],['1','5']),['1','5']);
});

test('private inboxes and assistant notebooks are not offered',()=>{
 assert.deepEqual(notebookChoices([{id:'default',name:'My notes'},{id:'inbox',name:'Inbox',protectedInbox:'alice'},{id:'helper',name:'Helper',assistantOwner:'alice'},{id:'plain',name:'plain'}]),[['default','My notes (default)'],['plain','plain']]);
});
