import {prepareSettingsPage,closeSettingsPage} from './settings-pages.mjs';
import {initTheme} from './theme.mjs';
import {openDialog} from './overlay.mjs';
import {$,busy} from './dom.mjs';
import {feedback} from './feedback.mjs';

export function initSettings({api, post, notice, remember}) {
  for (const nav of document.querySelectorAll('[data-settings-categories]')) {
    const dialog=nav.closest('dialog'), buttons=[...nav.querySelectorAll('button')];
    const choose=button=>{
      for(const item of buttons)item.setAttribute('aria-pressed',String(item===button));
      for(const panel of dialog.querySelectorAll('[data-settings-panel]'))panel.hidden=panel.dataset.settingsPanel!==button.dataset.settingsCategory;
    };
    for(const button of buttons)button.onclick=()=>choose(button);
    choose(buttons[0]);
  }
  function preference(id, apply) {
    const control = $(id);
    const choices = [...control.querySelectorAll('input[type="radio"]')];
    const value = () => choices.length ? choices.find(choice => choice.checked).value : control.value;
    let saved;
    try { saved = localStorage.getItem('gitnotes:' + id); } catch {}
    if (choices.some(choice => choice.value === saved)) choices.forEach(choice => { choice.checked = choice.value === saved; });
    else if (!choices.length && [...control.options].some(option => option.value === saved)) control.value = saved;
    apply(value());
    control.onchange = () => {
      apply(value());
      try { localStorage.setItem('gitnotes:' + id, value()); } catch {}
    };
  }
  const theme=initTheme(),themeControls=[...$('theme').querySelectorAll('input')];
  themeControls.forEach(input=>{input.checked=input.value===theme.choice();});
  $('theme').onchange=()=>theme.select(themeControls.find(input=>input.checked).value);
  preference("reading-font", (v) => (document.documentElement.dataset.readingFont = v));
  preference("font-size", (v) =>
    document.documentElement.style.setProperty("--reading-size", v),
  );
  function renderAccount(account) {
    $("password-settings").hidden = $("password-form").hidden = !account.passwordAvailable || !account.username;
    $("password-form").reset();
    const recovery = account.user === '@local-admin';
    $("account-summary").textContent = recovery ? 'Recovery administrator' : account.displayName || account.username || (account.oidc?.length ? 'Identity provider connected' : 'Authenticated account');
    $("github-summary").hidden = !account.github;
    $("github-summary").textContent = account.github ? 'GitHub: @' + account.github.login + (account.github.attribution ? ' · attribution enabled' : ' · connected') : '';
    const linkedIdentities = (account.oidc || []).map(i => i.issuer + ' · ' + i.subject);
    $("account-user").textContent = account.displayName || account.username || (recovery?'Recovery administrator':'Your account');
    $("account-user").title = 'Account identifier: '+account.user;
    $("oidc-link").hidden = !account.oidcAvailable || recovery;
    $("oidc-identities").textContent = recovery
      ? 'Recovery accounts cannot link an OIDC identity.'
      : linkedIdentities.join('\n') || (account.oidcAvailable ? 'No OIDC identity linked.' : 'OIDC sign-in is not configured on this server.');
    const privateName=account.displayName || account.username || 'Your account';
    const publicAttribution=account.github?.attribution && account.author.email!==account.local.email;
    $("account-author").textContent = publicAttribution ? account.author.name+' <'+account.author.email+'>' : 'Private notebook identity · '+privateName;
    $("account-local").textContent = 'Private notebook identity · ' + privateName;
    $("account-local").hidden = account.author.name === account.local.name && account.author.email === account.local.email;
    $("github-connection").textContent = account.github ? 'Connected as @' + account.github.login : account.githubAvailable ? 'Not connected.' : 'GitHub attribution is not configured on this server.';
    $("github-connect").hidden = !!account.github || !account.githubAvailable;
    $("github-connect").disabled = !account.githubAvailable;
    $("github-disconnect").hidden = !account.github;
    $("github-attribution-form").hidden = !account.github;
    $("github-email").value = account.github?.email || '';
    $("github-attribution-enabled").checked = !!account.github?.attribution;
    $("github-email").required = !!account.github?.attribution;
  }
  async function openAccount() {
    const page=prepareSettingsPage($('account-dialog'));
    try {
      const account=await api('/account');if(!page.active())return;renderAccount(account);$("account-status").textContent='';page.show();
    } catch(e){if(page.active())notice(e.message);}
  }
  $("account-open").onclick=openAccount;
  $("sessions-revoke").onclick=async()=>{
   try { await post('/account/sessions/revoke',{}); window.location.assign('/login'); }
   catch(e){feedback($("account-status"),e.message);}
  };
  $("github-connect").onclick=async()=>{
    $("github-connect").disabled=true;feedback($("account-status"),'Connecting…',{level:'info'});
    try {remember();const result=await post('/account/github/connect',{});window.location.assign(result.url);}
    catch(e){feedback($("account-status"),e.message);$("github-connect").disabled=false;}
  };
  const saved=text=>feedback($("account-status"),text,{level:'success'});
  $("github-disconnect").onclick=()=>busy($("github-disconnect"),async()=>{
    renderAccount(await post('/account/github/disconnect',{}));saved('Disconnected. Future commits use your local identity.');
  },{report:$("account-status")});
  $("github-attribution-enabled").onchange=()=>{$("github-email").required=$("github-attribution-enabled").checked;};
  $("github-attribution-form").onsubmit=event=>{
    event.preventDefault();
    return busy($("github-attribution-form"),async()=>{
      renderAccount(await post('/account/github/attribution',{email:$("github-email").value,enabled:$("github-attribution-enabled").checked}));
      saved('Attribution saved for future commits.');
    },{controls:event.submitter,report:$("account-status")});
  };


  $("password-form").onsubmit = async (event) => {
    event.preventDefault();
    if ($("password-new").value !== $("password-confirm").value) {
      feedback($("account-status"),'The new passwords must match.'); return;
    }
    try {
      await post('/account/password', {current: $("password-current").value, password: $("password-new").value});
      $("password-form").reset();
      window.location.assign('/login');
    } catch (error) { feedback($("account-status"),error.message); }
  };

  $("setup-open").onclick = () => { closeSettingsPage($("account-dialog")); openDialog($("setup-dialog")); };
  $("setup-form").onsubmit = event => {
    event.preventDefault();
    if ($("setup-password").value !== $("setup-confirm").value) {
      feedback($("setup-status"),'The passwords must match.'); return;
    }
    return busy($("setup-form"), async () => {
      await post('/account/setup', {username: $("setup-username").value.trim(), password: $("setup-password").value});
      $("setup-form").reset();
      window.location.assign('/');
    }, {controls: event.submitter, report: $("setup-status")});
  };
  return {openAccount,
    update(user) { $("setup-open").hidden = !user.setupAvailable; },
    initialize(user) {
      $("setup-open").hidden = !user.setupAvailable;
      if (user.setupAvailable) openDialog($("setup-dialog"));
    }
  };
}
