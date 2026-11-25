const choices=new Set(['system','light','dark','parchment']);
let theme;

// Appearance belongs to this browser document, across notebook and account
// pages. Resolve System consistently while preserving an explicit choice.
export function initTheme() {
 if(theme)return theme;
 const system=matchMedia('(prefers-color-scheme: dark)');let choice='system';
 try {const saved=localStorage.getItem('gitnotes:theme');if(choices.has(saved))choice=saved;}catch{}
 const apply=()=>{
  document.documentElement.dataset.theme=choice==='system'?(system.matches?'dark':'light'):choice;
  // An installed app's title bar follows the chosen theme, not only the system's.
  const color=getComputedStyle(document.documentElement).getPropertyValue('--bg').trim();
  if(color)for(const meta of document.querySelectorAll('meta[name=theme-color]'))meta.content=color;
 };
 apply();system.addEventListener('change',()=>{if(choice==='system')apply();});
 theme={choice:()=>choice,select(value){
  if(!choices.has(value))return;
  choice=value;apply();try{localStorage.setItem('gitnotes:theme',value);}catch{}
 }};
 return theme;
}
