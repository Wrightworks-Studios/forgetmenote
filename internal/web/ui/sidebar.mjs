// These are disclosures containing ordinary buttons, not ARIA application menus.
// Native Tab navigation remains available; Escape returns to the opener.
export function initSidebarMenus() {
  const toggles = [...document.querySelectorAll('[data-sidebar-toggle]')];
  const panel = toggle => document.getElementById(toggle.getAttribute('aria-controls'));
  function close(toggle, focus = false) {
    panel(toggle).hidden = true;
    toggle.setAttribute('aria-expanded', 'false');
    if (focus) toggle.focus();
  }
  function closeAll(root = document) { for (const toggle of toggles) if(root.contains(toggle))close(toggle); }
  for (const toggle of toggles) {
    toggle.onclick = () => {
      const opening = panel(toggle).hidden;
      closeAll();
      if (opening) {
        panel(toggle).hidden = false;
        toggle.setAttribute('aria-expanded', 'true');
      }
    };
    panel(toggle).addEventListener('click', event => {
      if (event.target.closest('button:not(:disabled)')) close(toggle, true);
    }, true);
  }
  document.addEventListener('click', event => {
    for (const toggle of toggles)
      if (!toggle.parentElement.contains(event.target)) close(toggle);
  });
  document.addEventListener('focusin', event => {
    for (const toggle of toggles)
      if (!toggle.parentElement.contains(event.target)) close(toggle);
  });
  document.addEventListener('keydown', event => {
    if (event.key !== 'Escape' || document.querySelector('dialog[open]')) return;
    const toggle = toggles.find(toggle => !panel(toggle).hidden);
    if (toggle) {
      close(toggle, true);
      event.preventDefault();
    }
  });
  return {closeAll};
}
