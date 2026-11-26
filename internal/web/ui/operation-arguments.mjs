import {element as node} from './dom.mjs';

// A data operation's arguments as form controls, generated from its input JSON
// Schema. Flat string, number, integer, boolean and enum properties get one
// control each. Everything else the schema declares (nested objects, arrays,
// combinators, references), and every saved value its control could not show
// as it is, stays in one JSON field. A control left as it was rendered returns
// its saved value unchanged, so a saved form gives back exactly the arguments it
// was given. The server validates the arguments however they were built.

const combinators=['$ref','anyOf','oneOf','allOf','not','if','const'];
const isObject=value=>!!value&&typeof value==='object'&&!Array.isArray(value);
function kindOf(schema){
  if(!isObject(schema)||combinators.some(key=>key in schema))return '';
  if('enum' in schema)return Array.isArray(schema.enum)&&schema.enum.length&&schema.enum.every(value=>value===null||['string','number','boolean'].includes(typeof value))?'enum':'';
  return ['string','number','integer','boolean'].includes(schema.type)?schema.type:'';
}
function fits(kind,schema,value){
  if(kind==='string')return typeof value==='string';
  if(kind==='number')return typeof value==='number'&&Number.isFinite(value);
  if(kind==='integer')return Number.isInteger(value);
  if(kind==='boolean')return typeof value==='boolean';
  return schema.enum.includes(value);
}

// What argumentFields renders for `schema` and the saved `values`: a field per
// representable property, the saved values only JSON can carry (`other`), the
// declared properties without a control (`otherNames`), and whether the JSON
// field is shown at all (`json`).
export function argumentPlan(schema,values={}){
  values=isObject(values)?values:{};
  if(!isObject(schema)||!isObject(schema.properties))return {fields:[],other:{...values},otherNames:[],json:true};
  const required=new Set(Array.isArray(schema.required)?schema.required:[]),fields=[],otherNames=[];
  for(const [name,property] of Object.entries(schema.properties)){
    const kind=kindOf(property),present=Object.hasOwn(values,name);
    if(!kind||present&&!fits(kind,property,values[name])){otherNames.push(name);continue;}
    fields.push({name,kind,schema:property,required:required.has(name),present,value:values[name]});
  }
  const other=Object.fromEntries(Object.entries(values).filter(([name])=>!fields.some(field=>field.name===name)));
  return {fields,other,otherNames,json:otherNames.length>0||Object.keys(other).length>0};
}

let described=0;
function control(field){
  const {kind,schema,present,value,required}=field,title=typeof schema.title==='string'&&schema.title?schema.title:field.name;
  const wrap=node('div',undefined,'operation-argument');let input,label;
  if(kind==='boolean'){
    input=node('input');input.type='checkbox';input.checked=present?value:schema.default===true;
    label=node('label');label.append(input,document.createTextNode(title));
  }else{
    label=node('label',title);
    if(kind==='enum'){
      input=node('select');
      if(!required||!present)input.append(new Option(required?'Choose a value':schema.default!==undefined?`Default (${String(schema.default)})`:'Not set',''));
      schema.enum.forEach((choice,index)=>input.append(new Option(choice===null?'None':String(choice),String(index))));
      input.value=present?String(schema.enum.indexOf(value)):'';
    }else{
      input=node('input');input.autocomplete='off';
      if(kind==='string'){input.type='text';input.placeholder=schema.format==='date'?'YYYY-MM-DD':schema.format==='date-time'?'2026-01-31T09:00:00Z':'';}
      else{input.type='number';input.step='any';input.inputMode=kind==='integer'?'numeric':'decimal';}
      input.value=present?String(value):'';
    }
    label.append(input);
  }
  // A label's text would also take in the selected option or typed value.
  input.dataset.argument=field.name;input.setAttribute('aria-label',title);wrap.append(label);
  if(typeof schema.description==='string'&&schema.description){const hint=node('small',schema.description);hint.id='operation-argument-'+(++described);input.setAttribute('aria-describedby',hint.id);wrap.append(hint);}
  const state=()=>kind==='boolean'?String(input.checked):input.value,initial=state();
  // Constraints are checked here rather than by the browser, which cannot point
  // at a control inside a closed section and would refuse the submit silently.
  function read(){
    if(state()===initial&&(present||!required))return present?{value}:null;
    if(kind==='boolean')return {value:input.checked};
    if(input.validity?.badInput)throw Error(`${title} must be a number.`);
    if(input.value===''){if(required)throw Error(`${title} is required.`);return null;}
    if(kind==='enum')return {value:schema.enum[Number(input.value)]};
    if(kind==='string')return {value:input.value};
    const number=Number(input.value);
    if(!Number.isFinite(number))throw Error(`${title} must be a number.`);
    if(kind==='integer'&&!Number.isInteger(number))throw Error(`${title} must be a whole number.`);
    if(typeof schema.minimum==='number'&&number<schema.minimum)throw Error(`${title} must be at least ${schema.minimum}.`);
    if(typeof schema.maximum==='number'&&number>schema.maximum)throw Error(`${title} must be at most ${schema.maximum}.`);
    return {value:number};
  }
  return {wrap,read};
}

// The controls for an operation's arguments. read() returns the arguments
// object in the saved key order, or throws an Error naming the field to fix.
export function argumentFields(schema,values={}){
  values=isObject(values)?values:{};
  const plan=argumentPlan(schema,values),element=node('div',undefined,'operation-arguments');
  const fields=plan.fields.map(field=>({name:field.name,...control(field)}));
  element.append(...fields.map(field=>field.wrap));
  let json=null;
  if(plan.json){
    const schemaless=!isObject(schema)||!isObject(schema.properties),wrap=node('div',undefined,'operation-argument operation-argument-json');
    const label=node('label',schemaless?'Arguments (JSON)':'Other arguments (JSON)');
    json=node('textarea');json.rows=4;json.spellcheck=false;json.placeholder='{}';json.setAttribute('aria-label',label.textContent);
    json.value=Object.keys(plan.other).length?JSON.stringify(plan.other,null,2):'';
    label.append(json);wrap.append(label);
    if(plan.otherNames.length)wrap.append(node('small','Set here: '+plan.otherNames.join(', ')));
    element.append(wrap);
  }
  const initialJSON=json?.value;
  function readJSON(){
    if(!json)return {};
    if(json.value===initialJSON)return plan.other;
    if(!json.value.trim())return {};
    let parsed;try{parsed=JSON.parse(json.value);}catch{throw Error('Other arguments must be a JSON object.');}
    if(!isObject(parsed))throw Error('Other arguments must be a JSON object.');
    for(const name of Object.keys(parsed))if(fields.some(field=>field.name===name))throw Error(`Set ${name} in its own field, not in the JSON.`);
    return parsed;
  }
  function read(){
    const results=new Map();
    for(const field of fields){const result=field.read();if(result)results.set(field.name,result.value);}
    for(const [name,value] of Object.entries(readJSON()))results.set(name,value);
    const order=[...new Set([...Object.keys(values),...results.keys()])].filter(name=>results.has(name));
    return Object.fromEntries(order.map(name=>[name,results.get(name)]));
  }
  return {element,read};
}
