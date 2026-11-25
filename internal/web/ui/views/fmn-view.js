// Loaded first in every view document. The view runs with an opaque origin and
// no network; this bridge is its only channel, to the app page holding its
// frame. That page answers a fixed list of read calls about the view's own
// notebook and sizes the frame to the height reported here.
(() => {
  const pending = new Map();
  const framed = parent !== window;
  let next = 0, height = 0;
  const send = message => parent.postMessage({fmn: 1, ...message}, '*');
  addEventListener('message', event => {
    const data = event.data;
    if (event.source !== parent || !data || data.fmn !== 1 || !pending.has(data.id)) return;
    const {resolve, reject} = pending.get(data.id);
    pending.delete(data.id);
    if (data.ok) resolve(data.result);
    else reject(new Error(String(data.error || 'The view call failed')));
  });
  function call(name, args) {
    if (!framed) return Promise.reject(new Error('Open this view in its note to read notebook data.'));
    return new Promise((resolve, reject) => {
      const id = ++next;
      pending.set(id, {resolve, reject});
      send({id, call: name, args});
    });
  }
  function report() {
    const measured = Math.ceil(document.documentElement.getBoundingClientRect().height);
    if (framed && measured !== height) send({resize: (height = measured)});
  }
  if (framed) send({hello: true});
  new ResizeObserver(report).observe(document.documentElement);
  addEventListener('load', report);
  Object.defineProperty(window, 'fmn', {
    value: Object.freeze({
      notes: Object.freeze({
        list: (folder = '') => call('notes.list', {folder}),
        read: path => call('notes.read', {path}),
        search: query => call('notes.search', {query})
      }),
      theme: () => call('view.theme', {})
    })
  });
})();
