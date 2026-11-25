export function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg','svg');
  for (const [key,value] of Object.entries({class:'ui-icon',viewBox:'0 0 24 24',width:20,height:20,fill:'none',stroke:'currentColor','stroke-width':1.75,'stroke-linecap':'round','stroke-linejoin':'round','aria-hidden':'true',focusable:'false'})) svg.setAttribute(key,value);
  const paths = {
    folder:'M3 7V5h6l2 3h10v12H3Z',
    document:'M14 2H5v20h14V7ZM14 2v5h5M8 12h8M8 16h8',
    draft:'M14 2H5v20h14V7ZM14 2v5h5',
    inbox:'m4 4-2 10v6h20v-6L20 4ZM2 14h6l2 3h4l2-3h6',
    archive:'M3 3h18v5H3ZM5 8v13h14V8M9 12h6',
    review:'M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0ZM7 12l3 3 7-7',
    user:'M16 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0ZM4 22v-3a8 8 0 0 1 16 0v3',
    bell:'M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4',
    search:'M17 10a7 7 0 1 1-14 0 7 7 0 0 1 14 0ZM15 15l6 6',
    tag:'M3 3h8l10 10-8 8L3 11ZM7 7h.01',
    list:'M8 5h13M8 12h13M8 19h13M3 5h.01M3 12h.01M3 19h.01',
    grid:'M3 3h7v7H3ZM14 3h7v7h-7ZM3 14h7v7H3ZM14 14h7v7h-7Z',
    chevron:'m9 5 7 7-7 7',
    down:'m6 9 6 6 6-6',
    close:'m6 6 12 12M18 6 6 18',
    back:'m12 5-7 7 7 7M5 12h16',
    chat:'M21 11.5a8.5 8.5 0 0 1-8.5 8.5H4l-3 2 1.8-5.5A8.5 8.5 0 1 1 21 11.5Z',
    delete:'M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7M14 10v7',
    new:'M12 5v14M5 12h14',
    settings:'m9.5 3-.6 2.2-2 .9-2-.6-2.5 4.3L4 11.4v2.2l-1.6 1.6 2.5 4.3 2-.6 2 .9.6 2.2h5l.6-2.2 2-.9 2 .6 2.5-4.3-1.6-1.6v-2.2l1.6-1.6-2.5-4.3-2 .6-2-.9L14.5 3h-5ZM15 12.5a3 3 0 1 1-6 0 3 3 0 0 1 6 0Z',
    refresh:'M20 7v5h-5M4 17v-5h5M6.1 6a7.5 7.5 0 0 1 12.4 1L20 12M4 12l1.5 5A7.5 7.5 0 0 0 17.9 18',
    attach:'m8 12 7-7a4 4 0 0 1 5.7 5.7L10.3 21A6 6 0 0 1 1.8 12.5L12 2.3M6 14l7-7a2 2 0 0 1 2.8 2.8l-7 7',
    link:'M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-2 2M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l2-2',
    edit:'m16 3 5 5M3 21l5-1L21 7a2 2 0 0 0-5-5L3 15Z',
    history:'M3 4v5h5M3 9a9 9 0 1 1 0 6M12 7v5l3 2',
    more:'M5 12h.01M12 12h.01M19 12h.01',
    notebook:'M5 4a2 2 0 0 1 2-2h12v16H7a2 2 0 0 0-2 2ZM5 20a2 2 0 0 0 2 2h12v-4M9 6h6',
    upload:'M12 15V3M7 8l5-5 5 5M4 15v6h16v-6',
    assistant:'M12 3v3M6 6h12a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2ZM9 12h.01M15 12h.01M9.5 16h5',
    logout:'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9',
    rename:'M4 7V4h16v3M12 4v16M9 20h6',
    lock:'M5 11h14v10H5ZM8 11V7a4 4 0 0 1 8 0v4',
    unlock:'M5 11h14v10H5ZM8 11V7a4 4 0 0 1 7.5-2',
    copy:'M8 8h12v12H8ZM16 8V4H4v12h4'
  };
  const path = document.createElementNS(svg.namespaceURI,'path');path.setAttribute('d',paths[name]);svg.append(path);return svg;
}

// Replace a control's text while keeping its leading icon.
export function setLabel(control, text) {
  const glyph = control.querySelector(':scope > .ui-icon');
  control.replaceChildren(...(glyph ? [glyph] : []), text);
}
