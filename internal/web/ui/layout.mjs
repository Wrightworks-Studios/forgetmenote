import {overlays} from './overlay.mjs';
import {$} from './dom.mjs';

// Sidebar visibility and the phone reader bar. `version()` changes whenever the
// person toggles the sidebar, so slow navigation can avoid closing a newer menu.
// Focus mode hides both without changing the remembered sidebar preference.
export function initLayout({sidebarMenus,isEditing,isSearchOpen}) {
  const readerHeader=document.querySelector("#main > header");
  new ResizeObserver(()=>document.documentElement.style.setProperty("--reader-header-height",readerHeader.getBoundingClientRect().height+"px")).observe(readerHeader);
  const smallScreen = window.matchMedia("(max-width: 720px)");
  // Keep fixed controls outside the sticky header: Safari clips fixed descendants.
  const readerActionHomes=[$("mode"),$("chat-open"),$("note-context-toggle"),$("note-details-toggle").parentElement].map(node=>{
   const home=document.createComment('reader action home');node.before(home);return {node,home};
  });
  function syncReaderActions(){
   const bar=$("reader-actions"),reading=smallScreen.matches && !focusMode && !$("document").hidden && !isEditing() && !isSearchOpen();
   bar.hidden=!reading;
   for(const {node,home} of readerActionHomes){
    if(node.id==='chat-open' && !$("chat-panel").hidden)continue;
    if(reading){if(node.parentElement!==bar)bar.append(node);}
    else if(node.parentElement!==home.parentNode)home.after(node);
   }
  }
  const readerLayoutObserver=new MutationObserver(syncReaderActions);
  readerLayoutObserver.observe(document.body,{attributes:true,attributeFilter:['class']});
  readerLayoutObserver.observe($("document"),{attributes:true,attributeFilter:['hidden']});
  let desktopCollapsed = false,
    mobileSidebarOpen = false, sidebarVersion = 0, focusMode = false;
  try {
    desktopCollapsed =
      localStorage.getItem("gitnotes:sidebar-collapsed") === "true";
  } catch {}
  // Main is shared by sidebar, context and chat; derive its current owners instead
  // of restoring one overlay's stale snapshot after another changes on resize.
  function updateMainInert() {
    overlays().panel($("sidebar"),{active:smallScreen.matches && mobileSidebarOpen,modal:true,close:()=>setSidebar(false),covers:[$("main")]});
  }
  function updateSidebar() {
    const expanded = !focusMode && (smallScreen.matches ? mobileSidebarOpen : !desktopCollapsed);
    const toggleParent = expanded
      ? document.querySelector(".sidebar-heading")
      : document.querySelector("#main > header");
    if ($("menu").parentElement !== toggleParent) {
      if (expanded) toggleParent.append($("menu"));
      else toggleParent.prepend($("menu"));
    }
    $("sidebar").hidden = !expanded;
    if (!expanded) sidebarMenus.closeAll($("sidebar"));
    document.body.classList.toggle("sidebar-collapsed", !expanded);
    $("menu").setAttribute("aria-expanded", String(expanded));
    $("menu").setAttribute(
      "aria-label",
      expanded ? "Hide sidebar" : "Show sidebar",
    );
    $("menu").title = expanded ? "Hide sidebar" : "Show sidebar";
    $("sidebar-backdrop").hidden = !(smallScreen.matches && expanded);
    updateMainInert();
    syncReaderActions();
  }
  function setSidebar(expanded, focus = true) {
    // A pending navigation must not close a menu the user opened more recently.
    sidebarVersion++;
    if (smallScreen.matches) mobileSidebarOpen = expanded;
    else {
      desktopCollapsed = !expanded;
      try {
        localStorage.setItem(
          "gitnotes:sidebar-collapsed",
          String(desktopCollapsed),
        );
      } catch {}
    }
    updateSidebar();
    if (focus) $("menu").focus();
  }
  $("menu").onclick = () => setSidebar($("sidebar").hidden);
  $("sidebar-backdrop").onclick = () => setSidebar(false);

  smallScreen.addEventListener("change", () => {
    mobileSidebarOpen = false;
    updateSidebar();
  });
  return {
    smallScreen,setSidebar,update:updateSidebar,syncReaderActions,
    version:()=>sidebarVersion,
    // On phones an opened destination replaces the sidebar, unless the person
    // toggled it since `since`, the version read when that navigation began.
    closeOnPhone(since=sidebarVersion){
      if(smallScreen.matches && since===sidebarVersion)setSidebar(false,false);
      return smallScreen.matches;
    },
    snapshot:()=>({desktopCollapsed,mobileSidebarOpen}),
    restore(state){({desktopCollapsed,mobileSidebarOpen}=state);},
    closeMobile(){mobileSidebarOpen=false;},
    setFocusMode(active){
      if(focusMode===active)return;
      focusMode=active;
      if(active)mobileSidebarOpen=false;
      updateSidebar();
    },
  };
}
