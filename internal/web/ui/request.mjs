// The browser's one JSON request boundary. A failed response becomes an Error
// with the server's message, HTTP status and stable code; an expired session
// has one actionable message on every page.
export async function responseError(response) {
  let message=await response.text(),code;
  try {const body=JSON.parse(message);message=body.error;code=body.code;} catch {}
  if(response.status===401)message='Your session expired. Sign in again.';
  const error=new Error(message || `Request failed (${response.status})`);
  error.status=response.status;error.code=code;
  return error;
}
export async function request(path,options={}) {
  const response=await fetch(path,{cache:'no-store',...options});
  if(!response.ok)throw await responseError(response);
  return response.json();
}
// Notebook routes name their notebook explicitly; instance routes use /api/.
export const notebookPath=id=>'/p/'+encodeURIComponent(id);
// Attachment URLs encode the notebook and every path segment; a revision pins the bytes.
export function assetURL(project,path,revision='') {
  return notebookPath(project)+'/assets/'+path.split('/').map(encodeURIComponent).join('/')+(revision?'?revision='+encodeURIComponent(revision):'');
}
export function post(path,value,options={}) {
  return request(path,{...options,method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(value)});
}
