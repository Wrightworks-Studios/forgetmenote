import {test} from 'node:test';
import assert from 'node:assert/strict';
import {responseError} from './request.mjs';

const reply=(status,body)=>new Response(typeof body==='string'?body:JSON.stringify(body),{status});

test('a failed response names the server message in either error shape',async()=>{
 const browser=await responseError(reply(400,{error:'Choose a notebook.',code:'invalid_request'}));
 assert.equal(browser.message,'Choose a notebook.');assert.equal(browser.code,'invalid_request');assert.equal(browser.status,400);
 const action=await responseError(reply(409,{error:{code:'conflict',message:'The settings changed.'}}));
 assert.equal(action.message,'The settings changed.');assert.equal(action.code,'conflict');assert.equal(action.status,409);
 assert.equal((await responseError(reply(502,'Bad gateway'))).message,'Bad gateway');
 assert.equal((await responseError(reply(500,''))).message,'Request failed (500)');
 assert.equal((await responseError(reply(401,{error:'expired'}))).message,'Your session expired. Sign in again.');
});
