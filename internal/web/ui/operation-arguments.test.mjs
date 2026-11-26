import {test} from 'node:test';
import assert from 'node:assert/strict';
import {argumentPlan} from './operation-arguments.mjs';

const schema={type:'object',required:['service'],properties:{
 service:{type:'string'},limit:{type:'integer',minimum:1},ratio:{type:'number'},errors:{type:'boolean'},
 level:{enum:['info','warn',2,null]},filter:{type:'object'},tags:{type:'array',items:{type:'string'}},window:{anyOf:[{type:'string'},{type:'integer'}]},
}};

test('flat properties get controls and the rest stay in JSON',()=>{
 const plan=argumentPlan(schema,{service:'api',level:2,filter:{status:500}});
 assert.deepEqual(plan.fields.map(field=>[field.name,field.kind,field.required,field.present]),[
  ['service','string',true,true],['limit','integer',false,false],['ratio','number',false,false],['errors','boolean',false,false],['level','enum',false,true],
 ]);
 assert.deepEqual(plan.otherNames,['filter','tags','window']);
 assert.deepEqual(plan.other,{filter:{status:500}});
 assert.equal(plan.json,true);
});

test('saved values a control cannot show as they are move to JSON',()=>{
 const plan=argumentPlan(schema,{service:7,limit:2.5,level:'debug',errors:'yes',extra:true});
 assert.deepEqual(plan.fields.map(field=>field.name),['ratio']);
 assert.deepEqual(plan.other,{service:7,limit:2.5,level:'debug',errors:'yes',extra:true});
 assert.deepEqual(plan.otherNames,['service','limit','errors','level','filter','tags','window']);
});

test('a flat schema needs no JSON field and a missing schema is all JSON',()=>{
 const flat=argumentPlan({type:'object',properties:{service:{type:'string'}},additionalProperties:false},{service:'api'});
 assert.equal(flat.json,false);assert.deepEqual(flat.other,{});
 assert.deepEqual(argumentPlan({type:'object',properties:{},additionalProperties:false},{}),{fields:[],other:{},otherNames:[],json:false});
 assert.deepEqual(argumentPlan(null,{query:{text:'a'}}),{fields:[],other:{query:{text:'a'}},otherNames:[],json:true});
 assert.deepEqual(argumentPlan({type:'object',properties:{x:{$ref:'#/$defs/x'}}},{}).otherNames,['x']);
});
