// Keep unchanged nodes in place: moving a focused row through appendChild
// blurs its controls even when its content and final order are unchanged.
export function reconcileNodes(parent, nodes) {
  let next=parent.firstChild;
  for(const node of nodes) {
    if(node===next)next=next.nextSibling;
    else parent.insertBefore(node,next);
  }
  while(next) {const stale=next;next=next.nextSibling;stale.remove();}
}
