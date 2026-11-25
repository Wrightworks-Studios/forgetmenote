import {test} from 'node:test';
import assert from 'node:assert/strict';
import {relativeTime} from './time.mjs';

test('human time describes recent past future and damaged values',()=>{
 const now=Date.parse('2026-10-10T12:00:00Z'),format=(value,unit)=>new Intl.RelativeTimeFormat(undefined,{numeric:'auto'}).format(value,unit);
 assert.equal(relativeTime('2026-10-10T11:59:50Z',now),'Just now');
 assert.equal(relativeTime('2026-10-10T11:58:00Z',now),format(-2,'minute'));
 assert.equal(relativeTime('2026-10-10T13:00:00Z',now),format(1,'hour'));
 assert.equal(relativeTime('2025-10-10T12:00:00Z',now),format(-12,'month'));
 assert.equal(relativeTime('invalid',now),'Unknown time');
});
