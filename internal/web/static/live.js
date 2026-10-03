// Keeps parts of the page current while it's open: what's playing, the
// newest listens, the note about sorting out an import and the pictures
// found for an item. Each part is an
// element with data-live, refilled from /live. Pages work the same without
// this, just as of when they loaded.
(() => {
  "use strict";

  const quick = 5000; // while the sorting note counts down
  const slow = 15000;
  let timer = 0;
  let busy = false;

  const parts = () => [...document.querySelectorAll("[data-live]")];

  async function refresh() {
    const live = parts();
    if (!live.length || busy) return;
    busy = true;
    try {
      const names = live.map((el) => el.dataset.live).join(",");
      const res = await fetch("/live?parts=" + names, { headers: { Accept: "application/json" } });
      if (res.ok) apply(await res.json());
    } catch {
      // Offline or restarting. Try again next time.
    } finally {
      busy = false;
      schedule();
    }
  }

  function apply(data) {
    for (const el of parts()) {
      const name = el.dataset.live;
      if (name === "sorting") {
        const s = data.sorting;
        if (!s) continue;
        if (s.left <= 0) el.remove();
        else if (el.textContent !== s.text) el.textContent = s.text;
        continue;
      }
      const html = data[name.split(":")[0]]; // "picture:artist:12" is answered as "picture"
      if (typeof html !== "string") continue;
      // Only parts that changed are replaced, so nothing flickers and
      // screen readers only hear about real changes.
      const next = document.createElement("div");
      next.innerHTML = html;
      if (next.innerHTML !== el.innerHTML) el.innerHTML = html;
    }
  }

  function schedule() {
    clearTimeout(timer);
    if (document.hidden || !parts().length) return;
    const waiting = document.querySelector('[data-live="sorting"], [data-looking]');
    timer = setTimeout(refresh, waiting ? quick : slow);
  }

  document.addEventListener("visibilitychange", () => {
    if (document.hidden) clearTimeout(timer);
    else refresh();
  });
  schedule();
})();
