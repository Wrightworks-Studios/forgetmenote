// Runs only in Cloudflare's public-page browser. Never opens access/age gates.
(async () => {
  const marker = '__FMN_CAPTURE_TOKEN__';
  const allowPromotionDismissal = __FMN_ALLOW_PROMOTION_DISMISSAL__;
  const deadline = performance.now() + 8000;
  const pause = (ms) => new Promise(resolve => setTimeout(resolve, Math.max(0, Math.min(ms, deadline - performance.now()))));
  const seen = new Set();
  const frames = new Map();
  const videos = new Set();
  const attemptedDialogs = [];
  let overlayAttempts = 0;
  const attemptedOverlays = new WeakSet();
  const dismissPromotions = () => {
    if (!allowPromotionDismissal || overlayAttempts >= 3) return;
    for (const button of document.querySelectorAll('.sqs-popup-overlay-close, button[aria-label="Close popup"], button[aria-label="Close dialog"]')) {
      const dialog = button.closest('.sqs-popup-overlay, [role="dialog"]');
      if (!dialog || attemptedOverlays.has(dialog) || button.closest('form') || button.hasAttribute('href')) continue;
      const text = dialog.innerText || '';
      if (/\bage\b|years? old|\badult|18\+|21\+|\b(?:1[3-9]|2[01])\s*(?:years?\s*)?(?:or\s*)?(?:older|over)\b|\b(?:over|under)\s*(?:1[3-9]|2[01])(?!\d)|\b(?:are you|you must be|i am|at least)\b.{0,24}\b(?:1[3-9]|2[01])(?!\d)|consent|sign in|log in|members? only|paywall|payment|subscribe.{0,20}(read|access)/i.test(text) || dialog.querySelector('input[type="password"],input[type="number"],input[type="date"],input[autocomplete="bday"]')) continue;
      if (getComputedStyle(dialog).visibility === 'hidden' || !dialog.getBoundingClientRect().height) continue;
      attemptedOverlays.add(dialog);
      overlayAttempts++;
      attemptedDialogs.push(dialog);
      button.click();
      if (overlayAttempts >= 3) break;
    }
    for (const button of document.querySelectorAll('.sqs-cookie-banner button, .sqs-cookie-banner-v2 button')) {
      if (overlayAttempts >= 3 || !/^(decline|reject(?: all)?|necessary only)$/i.test((button.innerText || '').trim())) continue;
      const banner = button.closest('.sqs-cookie-banner, .sqs-cookie-banner-v2');
      if (!banner || attemptedOverlays.has(banner) || !banner.getBoundingClientRect().height) continue;
      attemptedOverlays.add(banner); attemptedDialogs.push(banner); overlayAttempts++; button.click();
    }
  };
  const dimensions = () => ({
    width: Math.max(document.documentElement.scrollWidth, document.body?.scrollWidth || 0, innerWidth),
    height: Math.max(document.documentElement.scrollHeight, document.body?.scrollHeight || 0, innerHeight),
  });
  const observe = () => {
    for (const frame of document.querySelectorAll('iframe')) {
      const rect = frame.getBoundingClientRect();
      if (!rect.width || !rect.height) continue;
      if (!frames.has(frame)) {
        frames.set(frame, 'unknown');
        frame.addEventListener('load', () => frames.set(frame, 'loaded'), {once:true});
        try { if (frame.contentDocument?.URL !== 'about:blank' && frame.contentDocument?.readyState === 'complete') frames.set(frame, 'loaded'); else if(frame.contentDocument?.readyState === 'loading') frames.set(frame,'pending'); } catch (_) {}
      }
      frame.loading = 'eager';
    }
    for (const video of document.querySelectorAll('video')) if (video.getBoundingClientRect().height) videos.add(video);
    for (const img of document.images) {
      img.loading = 'eager';
      const rect = img.getBoundingClientRect();
      const style = getComputedStyle(img);
      if (rect.width > 0 && rect.height > 0 && rect.bottom > 0 && rect.top < innerHeight && style.display !== 'none' && style.visibility !== 'hidden') seen.add(img);
    }
  };
  let reachedBottom = false;
  let bottomHeight = 0;
  let error = false;
  try {
    window.scrollTo({top: 0, behavior: 'instant'});
    while (performance.now() < deadline) {
      dismissPromotions();
      observe();
      await pause(200);
      const height = dimensions().height;
      if (scrollY + innerHeight >= height - 2) {
        await pause(400);
        observe();
        if (dimensions().height === height) { reachedBottom = true; bottomHeight = height; break; }
      }
      window.scrollTo({top: Math.min(scrollY + Math.max(1, innerHeight * 0.75), height), behavior: 'instant'});
    }
    await Promise.race([
      Promise.allSettled([...seen].filter(img => img.isConnected && img.currentSrc).map(img => img.decode())),
      pause(1200),
    ]);
    if (document.fonts) await Promise.race([document.fonts.ready, pause(600)]);
  } catch (_) {
    error = true;
  } finally {
    window.scrollTo({top: 0, behavior: 'instant'});
    await pause(300);
    observe();
    const images = [...seen].filter(img => img.isConnected && (img.currentSrc || img.getAttribute('src')));
    const size = dimensions();
    reachedBottom = reachedBottom && size.height <= bottomHeight;
    const report = {
      version: 3, reachedBottom, timedOut: performance.now() >= deadline, error, dismissedOverlays: attemptedDialogs.filter(dialog => !dialog.isConnected || !dialog.getBoundingClientRect().height || getComputedStyle(dialog).visibility === 'hidden').length,
      pendingImages: images.filter(img => !img.complete).length,
      brokenImages: images.filter(img => img.complete && img.naturalWidth === 0).length,
      embedPlaceholderCount: [...document.querySelectorAll('[data-widget_type="video.default"]')].filter(el => el.getBoundingClientRect().width > 0 && el.getBoundingClientRect().height > 0 && !el.querySelector('iframe,video')).length,
      frameCount: frames.size, pendingFrames: [...frames.values()].filter(state => state === 'pending').length, unobservedFrames: [...frames.values()].filter(state => state === 'unknown').length,
      videoCount: videos.size, pendingVideos: [...videos].filter(video => video.readyState < 2 && !video.poster).length,
      fontsReady: !document.fonts || document.fonts.status === 'loaded',
      documentWidth: size.width, documentHeight: size.height,
    };
    const meta = document.createElement('meta');
    meta.name = marker;
    meta.content = JSON.stringify(report);
    document.head.prepend(meta);
  }
})();
