/* The bar view.
 *
 * One row per tenant: a dashed outline for what it asked the scheduler for, a
 * solid bar for what it is using, each row scaled to its own larger number.
 *
 * Formatting, the verdict, the role index, the controls and the stream are
 * shared with every other view in common.js, so this file is layout only.
 */

(() => {
  "use strict";

  const { $, mem, cpu, short, verdict, toast, start } = window.NN;

  let dim = "mem";
  const seenEvents = new Set();
  const rows = new Map(); // namespace -> elements
  let firstPaint = true;

  const WORDS = ["zero", "one", "two", "three", "four", "five", "six", "seven",
    "eight", "nine", "ten", "eleven", "twelve"];

  const fmt = () => (dim === "mem" ? mem : cpu);
  const askedOf = (t) => (dim === "mem" ? t.reqMem : t.reqCpu) || 0;
  const usingOf = (t) => (dim === "mem" ? t.useMem : t.useCpu);

  /* ---------- rendering ---------- */

  function buildRow(ns) {
    const li = document.createElement("li");
    li.className = "row";
    li.innerHTML =
      `<span class="who"></span>` +
      `<span class="track"><span class="asked"></span><span class="using"></span></span>` +
      `<span class="state"><span class="verdict"></span><span class="nums"></span></span>`;
    return {
      li,
      who: li.querySelector(".who"),
      asked: li.querySelector(".asked"),
      using: li.querySelector(".using"),
      verdict: li.querySelector(".verdict"),
      nums: li.querySelector(".nums"),
    };
  }

  function render(snap, tenantList, roles) {
    const f = fmt();
    const tenants = [...tenantList].sort(
      (a, b) => askedOf(b) - askedOf(a) || (usingOf(b) || 0) - (usingOf(a) || 0));

    /* Each row is scaled to its own larger number.
     *
     * A single scale across all rows was the first attempt and it did not
     * survive real values: these tenants span five orders of magnitude, from
     * 340Ki to 2Gi, so analytics asking for a gigabyte flattened every other
     * row to an invisible sliver. The comparison this view exists to show is
     * within a row -- what one tenant asked for against what it is using --
     * so that is the comparison each row is scaled to make legible. The
     * absolute figures are printed beside every bar, which is where
     * cross-tenant magnitude belongs.
     */
    const scaleOf = (t) => Math.max(askedOf(t) || 0, usingOf(t) || 0, 1);

    const host = $("tenants");
    const seen = new Set();
    let worst = null;

    tenants.forEach((t, i) => {
      seen.add(t.namespace);
      const scale = scaleOf(t);
      let r = rows.get(t.namespace);
      if (!r) {
        r = buildRow(t.namespace);
        rows.set(t.namespace, r);
        // Do not animate the whole table in on first paint; only genuine
        // arrivals after that.
        if (!firstPaint) {
          r.li.classList.add("arrived");
          setTimeout(() => r.li.classList.remove("arrived"), 600);
        }
      }
      if (host.children[i] !== r.li) host.insertBefore(r.li, host.children[i] || null);

      const asked = askedOf(t);
      const using = usingOf(t);
      const v = verdict(t, roles[t.namespace] || "ok", asked, using,
        dim === "mem" ? "cpu" : "memory");
      if (worst === null && (v.role === "oom" || v.role === "aggressor")) worst = { t, v };

      r.li.className = `row r-${v.role}` +
        (r.li.classList.contains("killed") ? " killed" : "");
      r.who.textContent = t.namespace.replace(/^team-/, "");

      r.asked.style.width = ((asked / scale) * 100).toFixed(2) + "%";
      // A tenant that declared nothing has no outline to draw: there is no
      // promise, which is exactly what the row should say.
      r.asked.hidden = asked === 0;

      if (using == null) {
        r.using.hidden = true;
      } else {
        r.using.hidden = false;
        r.using.style.width = ((using / scale) * 100).toFixed(2) + "%";
        r.using.classList.toggle("over", asked > 0 && using > asked * 1.02);
      }

      r.verdict.textContent = v.text;
      r.nums.textContent = asked === 0
        ? `asked nothing · using ${f(using)}`
        : `asked ${f(asked)} · using ${f(using)}`;
    });

    rows.forEach((r, ns) => {
      if (!seen.has(ns)) { r.li.remove(); rows.delete(ns); }
    });
    firstPaint = false;

    renderChrome(snap, tenants, worst);
    renderFeed(snap);
  }

  function renderChrome(snap, tenants, worst) {
    const n = tenants.length;
    $("tcount").textContent = `${WORDS[n] || n} tenant${n === 1 ? "" : "s"}`;

    const sub = $("subhead");
    if (worst && worst.v.role === "oom") {
      sub.textContent = `${worst.t.namespace.replace(/^team-/, "")} is being killed. It asked for the right amount.`;
      sub.className = "alarm";
    } else if (worst) {
      sub.textContent = `${worst.t.namespace.replace(/^team-/, "")} is taking far more than it declared. Everyone else was placed around the smaller number.`;
      sub.className = "alarm";
    } else {
      sub.textContent = "Each of them told the scheduler what it needed.";
      sub.className = "";
    }

    $("scalenote").textContent =
      `${dim === "mem" ? "memory" : "cpu"} \u00b7 dashed = asked for \u00b7 solid = using \u00b7 each row to its own scale`;

    const src = $("source");
    src.textContent = snap.source;
    src.dataset.src = snap.source;

    // The node this is about, named, so the picture is anchored to something
    // real rather than floating.
    const busiest = [...(snap.nodes || [])].sort(
      (a, b) => (b.tenantCount || 0) - (a.tenantCount || 0))[0];
    $("node").textContent = busiest
      ? `${busiest.name}${busiest.instanceType ? " · " + busiest.instanceType : ""}`
      : "";
  }

  function renderFeed(snap) {
    (snap.events || []).forEach((e) => {
      const id = `${e.at}|${e.kind}|${e.namespace}/${e.pod}`;
      if (seenEvents.has(id)) return;
      seenEvents.add(id);

      const who = e.namespace.replace(/^team-/, "");
      let line = null;
      if (e.kind === "oomkill") {
        line = `${who} was just OOMKilled`;
        const r = rows.get(e.namespace);
        if (r) {
          r.li.classList.add("killed");
          setTimeout(() => r.li.classList.remove("killed"), 2600);
        }
      } else if (e.kind === "added") {
        line = `${who} just landed on this node`;
      } else if (e.kind === "throttle") {
        line = `${who} started being throttled`;
      } else if (e.kind === "restart") {
        line = `${who} restarted`;
      }
      if (!line) return;

      const feed = $("feed");
      $("feedtext").textContent = line;
      feed.classList.add("hot");
      clearTimeout(renderFeed.t);
      renderFeed.t = setTimeout(() => feed.classList.remove("hot"), 6000);
    });
  }

  start({
    render,
    onKey(ev, repaint) {
      switch (ev.key) {
        case "m": case "M": case "c": case "C":
          dim = dim === "mem" ? "cpu" : "mem";
          toast(dim === "mem" ? "showing memory" : "showing cpu");
          repaint();
          break;
        case "o": case "O":
          location.href = "/";
          break;
      }
    },
  });
})();
