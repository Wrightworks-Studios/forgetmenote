// Shared by reading and visual editing. Only these URLs can create a player.
export function videoSource(href) {
  if(typeof href!=='string' || /[\\\s<>]/.test(href))return null;
  if(!/^[a-z][a-z0-9+.-]*:/i.test(href) && !href.startsWith('//') && /\.(mp4|webm)$/i.test(href))return {kind:'video',url:href};
  let url;try {url=new URL(href);}catch {return null;}
  if(url.protocol!=='https:' || url.username || url.password || url.port)return null;
  const host=url.hostname, parts=url.pathname.split('/').filter(Boolean);
  let id;
  if(host==='youtu.be' && parts.length===1)id=parts[0];
  if(['youtube.com','www.youtube.com','m.youtube.com','www.youtube-nocookie.com'].includes(host)) {
    if(url.pathname==='/watch')id=url.searchParams.get('v');
    else if(['embed','shorts','live'].includes(parts[0]) && parts.length===2)id=parts[1];
  }
  if(id && /^[\w-]{11}$/.test(id))return {kind:'iframe',url:'https://www.youtube-nocookie.com/embed/'+id};
  if(['vimeo.com','www.vimeo.com','player.vimeo.com'].includes(host)) {
    const match=url.pathname.match(/^\/(?:video\/)?(\d+)(?:\/([a-f0-9]+))?\/?$/i);
    if(match) {
      const hash=match[2] || url.searchParams.get('h');
      if(hash && !/^[a-f0-9]+$/i.test(hash))return null;
      return {kind:'iframe',url:'https://player.vimeo.com/video/'+match[1]+(hash?'?h='+hash:'')};
    }
  }
  return null;
}
// The outside host a video link's player loads from (YouTube's or Vimeo's),
// which learns the viewer's address and the video when it loads; null for a
// notebook MP4/WebM, which this app serves, and for links that make no player.
export function videoHost(href) {
  const source=videoSource(href);
  return source?.kind==='iframe'?new URL(source.url).hostname:null;
}
function markdownLink(label,url,title='') {
  return '['+label.replace(/[\\\[\]]/g,'\\$&').replace(/[\r\n]/g,' ')+'](<'+url.replace(/[<>\s]/g,c=>encodeURIComponent(c))+'>'+ (title?' "'+title+'"':'')+')';
}
export function attachmentMarkdown(path,name,type) {
  const url='/'+path.split('/').map(encodeURIComponent).join('/');
  return (type.startsWith('image/')?'!':'')+markdownLink(name,url,videoSource(url)?'video':'');
}
export function videoMarkdown(url) {
  url=url.trim();
  if(!videoSource(url))throw new Error('Use a YouTube or Vimeo video link, or a notebook MP4/WebM path.');
  return markdownLink('Video',url,'video');
}
export function videoPlayer(href,assetURL) {
  const source=videoSource(href);
  if(!source)return null;
  const src=source.kind==='video'?assetURL(source.url):source.url;
  if(!src)return null;
  const wrapper=document.createElement('div');wrapper.className='note-video';wrapper.contentEditable='false';
  const player=document.createElement(source.kind);
  if(source.kind==='video') {player.controls=true;player.preload='metadata';player.playsInline=true;}
  else {player.title='Embedded video';player.loading='lazy';player.allow='fullscreen; picture-in-picture; encrypted-media';player.allowFullscreen=true;player.referrerPolicy='strict-origin-when-cross-origin';}
  player.src=src;wrapper.append(player);return wrapper;
}
export function renderVideos(root,assetURL) {
  for(const paragraph of root.querySelectorAll('p')) {
    const link=paragraph.firstElementChild;
    if(paragraph.children.length!==1 || link?.tagName!=='A' || link.title!=='video' || paragraph.textContent.trim()!==link.textContent.trim())continue;
    const player=videoPlayer(link.getAttribute('href'),assetURL);
    if(player)paragraph.after(player);
  }
}
