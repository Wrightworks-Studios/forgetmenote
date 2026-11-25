import {element} from './dom.mjs';

export function relativeTime(value,now=Date.now()) {
  const timestamp=Date.parse(value);if(!Number.isFinite(timestamp))return 'Unknown time';
  const seconds=(timestamp-now)/1000;
  if(Math.abs(seconds)<45)return 'Just now';
  const [unit,scale]=Math.abs(seconds)<3600?['minute',60]:Math.abs(seconds)<86400?['hour',3600]:Math.abs(seconds)<604800?['day',86400]:Math.abs(seconds)<2629800?['week',604800]:Math.abs(seconds)<31557600?['month',2629800]:['year',31557600];
  return new Intl.RelativeTimeFormat(undefined,{numeric:'auto'}).format(Math.round(seconds/scale),unit);
}
export function timeElement(value) {
  const date=new Date(value),valid=Number.isFinite(date.getTime());
  return element('time',relativeTime(value),valid?{datetime:date.toISOString(),title:date.toLocaleString()}:{});
}
