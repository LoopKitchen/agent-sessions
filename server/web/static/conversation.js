"use strict";

// The continuous reader's loader. The server renders every page as whole
// turns (a prompt, its work, its answer); this script only fetches the next
// page when the reader nears the end, appends its turns, retries a failed
// fetch, and reveals the element a URL fragment names. It assembles nothing:
// the fold that decides what a turn is lives in the server's derive runner,
// and a second copy of it here was how the page and the list came to
// disagree about how many turns a session had.
(() => {
  const transcript = document.querySelector(".transcript");
  const continuation = document.querySelector(".transcript-next");
  if (!transcript || !continuation) return;

  // The inspector: a file link inside the side panel swaps the panel in
  // place so the reader keeps their scroll position in the conversation.
  let selection = 0;
  document.addEventListener("click", async event => {
    if (event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
    if (!(event.target instanceof Element)) return;
    const link = event.target.closest(".artifacts a, .preview-versions a, .preview-actions a");
    if (!link?.closest(".session-inspector") || (link.target && link.target !== "_self")) return;
    const url = new URL(link.href, location.href);
    if (url.origin !== location.origin || url.pathname !== location.pathname) return;
    event.preventDefault();
    const current = ++selection;
    const inspector = document.querySelector(".session-inspector");
    inspector.setAttribute("aria-busy", "true");
    try {
      const response = await fetch(url, { credentials: "same-origin" });
      if (!response.ok) throw new Error("Preview unavailable");
      const doc = new DOMParser().parseFromString(await response.text(), "text/html");
      const preview = doc.querySelector(".session-inspector");
      if (!preview) throw new Error("Preview unavailable");
      if (current !== selection) return;
      inspector.replaceWith(preview);
      document.querySelector(".session-layout").classList.toggle("inspecting", !!preview.querySelector(".file-preview"));
      history.replaceState(null, "", url);
      if (innerWidth <= 1000) preview.scrollIntoView();
    } catch {
      if (current !== selection) return;
      let error = inspector.querySelector(".preview-load-error");
      if (!error) {
        error = document.createElement("p");
        error.className = "preview-load-error";
        error.setAttribute("role", "status");
        inspector.prepend(error);
      }
      error.textContent = "Could not load the preview. Click the file to retry.";
    } finally {
      if (current === selection) inspector.removeAttribute("aria-busy");
    }
  });

  const status = continuation.querySelector("[role=status]");
  const retry = continuation.querySelector(".transcript-retry");
  const seen = new Set();
  const turnIDs = new Set();
  let loading = false;
  let failed = false;

  // A turn that arrives twice (a page boundary re-served after a redeploy)
  // is dropped by id, the page's own anchor, so nothing renders twice.
  function register(root) {
    for (const turn of Array.from(root.querySelectorAll(".turn[id]"))) {
      if (turnIDs.has(turn.id)) turn.remove();
      else turnIDs.add(turn.id);
    }
  }

  let revealedTarget;
  function fragmentID() {
    try { return decodeURIComponent(location.hash.slice(1)); } catch { return ""; }
  }
  function revealFragment() {
    const id = fragmentID();
    if (!id) return true;
    const target = document.getElementById(id);
    if (!target || !transcript.contains(target)) return false;
    for (let parent = target.parentElement; parent; parent = parent.parentElement) {
      if (parent.tagName === "DETAILS") parent.open = true;
    }
    if (target !== revealedTarget) target.scrollIntoView();
    revealedTarget = target;
    return true;
  }

  function appendPage(page) {
    register(page);
    for (const node of Array.from(page.children)) {
      if (node.classList.contains("empty") && transcript.querySelector(".turn")) continue;
      transcript.append(node);
    }
    if (transcript.querySelector(".turn")) transcript.querySelector(":scope > .empty")?.remove();
    revealFragment();
  }

  register(transcript);
  revealFragment();
  window.addEventListener("hashchange", () => { revealedTarget = null; revealFragment(); });
  if (!continuation.dataset.next) return;

  async function load() {
    const next = continuation.dataset.next;
    if (loading || failed || !next) return;
    loading = true;
    retry.hidden = true;
    status.textContent = "Loading more of the conversation...";
    try {
      const url = new URL(next, location.href);
      const archivePath = location.pathname.replace(/\/conversation$/, "");
      if (url.origin !== location.origin || url.pathname !== archivePath) throw new Error("Invalid continuation");
      url.searchParams.set("reader", "1");
      if (seen.has(url.href)) throw new Error("Invalid continuation");
      const response = await fetch(url, { credentials: "same-origin" });
      if (!response.ok) throw new Error("Transcript unavailable");
      const doc = new DOMParser().parseFromString(await response.text(), "text/html");
      const page = doc.querySelector(".transcript");
      const tail = doc.querySelector(".transcript-next");
      if (!page || !tail || tail.dataset.next === next) throw new Error("Invalid transcript page");
      appendPage(page);
      seen.add(url.href);
      continuation.dataset.next = tail.dataset.next || "";
      status.textContent = continuation.dataset.next ? "More of the conversation loads as you scroll." : "End of captured conversation";
    } catch {
      failed = true;
      status.textContent = "Could not load more of the conversation. The turns already loaded are still here.";
      retry.hidden = false;
    } finally {
      loading = false;
      if (!failed && continuation.dataset.next) {
        // A fragment naming a turn not yet on the page keeps loading until it
        // arrives or the pages run out; otherwise wait for the scroll.
        if (fragmentID() && !revealFragment()) load();
        else requestAnimationFrame(loadWhenNear);
      }
    }
  }

  function loadWhenNear() {
    if (continuation.getBoundingClientRect().top < innerHeight + 600) load();
  }

  window.addEventListener("scroll", loadWhenNear, { passive: true });
  retry.addEventListener("click", () => { failed = false; load(); });
  new IntersectionObserver(entries => {
    if (entries.some(entry => entry.isIntersecting)) load();
  }, { rootMargin: "600px" }).observe(continuation);
  if (fragmentID() && !revealFragment()) load();
})();
