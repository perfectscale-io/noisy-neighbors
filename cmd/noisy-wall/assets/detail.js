/* The wall renderer.
 *
 * One job: draw the gap between what tenants promised and what they are doing.
 *
 * The grammar, per node and per dimension (CPU, then memory):
 *
 *   the box        the node's allocatable capacity
 *   the tiles      one per pod, width = that pod's REQUEST as a share of the box
 *   the fill       inside each tile, height = usage as a share of its own request
 *   the usage bar  drawn under the tiles from the left edge, width = ACTUAL usage
 *   the limits line  where the node sits if every tenant peaked at once
 *
 * The moment the demo exists for is the usage bar overtaking the right edge of
 * the tiles: real consumption passing everything the scheduler was told about.
 */

(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const MIB = 1024 * 1024;

  // Pods below this share of the box would be invisible. A floor keeps them on
  // screen; anything drawn at the floor is labeled, never silently widened.
  const MIN_TILE_PCT = 1.1;
  const MAX_EVENTS = 40;

  let lastSeq = -1;
  let stage = 3;
  // "node" draws the box to allocatable, which answers "does it fit". "fit"
  // draws it to the largest number in play, which is the only way to read pod
  // names and per-tile spill when the node is far larger than its tenants.
  // Both are honest; each answers a different question, so the axis says which.
  let scaleMode = "fit";
  const seenEvents = new Set();
  const killFlash = new Map(); // pod key -> expiry ms
  const arrived = new Map();

  /* ---------- formatting ---------- */

  const cpu = (m) => `${m}m`;

  function mem(b) {
    if (b === 0) return "0B";
    const units = ["B", "Ki", "Mi", "Gi", "Ti"];
    let i = 0, v = b;
    while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
    return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)}${units[i]}`;
  }

  const pct = (part, whole) => (whole > 0 ? (part / whole) * 100 : 0);
  const fmtPct = (p) => `${p.toFixed(0)}%`;

  function clock(iso) {
    const d = new Date(iso);
    return d.toLocaleTimeString([], { hour12: false });
  }

  function shortPod(name) {
    // Pod names carry two hashes. The workload is the readable part.
    const parts = name.split("-");
    if (parts.length > 2) return parts.slice(0, parts.length - 2).join("-");
    return name;
  }

  function esc(s) {
    return String(s).replace(/[&<>"']/g, (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }

  /* ---------- header ---------- */

  function renderHeader(snap) {
    const src = $("source");
    src.textContent = snap.source;
    src.dataset.src = snap.source;

    $("stamp").textContent = clock(snap.at);
    const nodes = snap.nodes.length;
    const tenants = new Set();
    snap.nodes.forEach((n) => (n.tenants || []).forEach((t) => tenants.add(t)));
    let pods = 0;
    snap.nodes.forEach((n) => (pods += (n.pods || []).length));
    $("nodecount").textContent =
      `${nodes} node${nodes === 1 ? "" : "s"} · ${pods} pods · ${tenants.size} tenants`;

    $("tiers").innerHTML = (snap.caps || []).map((c) => `
      <div class="tier ${c.available ? "on" : "off"}">
        <span class="mark">${c.available ? "✓" : "✗"}</span>
        <b>${esc(c.name)}</b>
        <span>${esc(c.available ? c.tier : c.reason || c.tier)}</span>
      </div>`).join("");

    const nm = snap.notMeasured || [];
    $("notmeasured").hidden = nm.length === 0;
    $("nmlist").innerHTML = nm.map((x) => `<li>${esc(x)}</li>`).join("");
  }

  /* ---------- nodes, rendered in place ---------- */

  /* Tiles are reconciled rather than rebuilt.
   *
   * The first version of this replaced innerHTML every frame, which looked
   * identical in a screenshot and was wrong on stage: a brand-new element has
   * no previous value, so every CSS transition was skipped and the fills
   * snapped instead of growing. Growth is the thing the audience is watching,
   * so elements are keyed by pod and their styles are updated in place. */

  const nodeEls = new Map(); // node name -> { root, header, dims: {cpu, mem} }

  function el(tag, cls, parent) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (parent) parent.appendChild(e);
    return e;
  }

  function buildNode(name) {
    const root = el("article", "node");
    const header = el("header", "", root);
    const h3 = el("h3", "", header);
    h3.textContent = name;
    const sev = el("span", "sev", header);
    const meta = el("span", "meta", header);
    const tenantsEl = el("span", "meta", header);
    const press = el("span", "sev sev-CRITICAL", header);
    press.textContent = "MEMORY PRESSURE";
    press.hidden = true;

    const dims = el("div", "dims", root);
    const mk = () => {
      const wrap = el("div", "dim", dims);
      const label = el("div", "dim-label", wrap);
      const left = el("span", "", label);
      const nums = el("span", "nums", label);
      const reqS = el("span", "", nums);
      const limS = el("span", "", nums);
      const useS = el("span", "", nums);
      const box = el("div", "box", wrap);
      const usage = el("div", "usage-bar", box);
      const limLine = el("div", "commit-line", box);
      return { box, usage, limLine, left, reqS, limS, useS, tiles: new Map() };
    };
    return { root, sev, meta, tenantsEl, press, cpu: mk(), mem: mk() };
  }

  function updateDim(d, node, isCPU) {
    const alloc = isCPU ? node.allocCpu : node.allocMem;
    const req = isCPU ? node.reqCpu : node.reqMem;
    const lim = isCPU ? node.limCpu : node.limMem;
    const use = isCPU ? node.useCpu : node.useMem;
    const fmt = isCPU ? cpu : mem;

    // The axis the box is drawn against. "node" answers "does it fit"; "fit"
    // is the only way to read names and per-tile spill when the node is far
    // larger than the tenants on it. The percentages never zoom.
    let axis = alloc;
    if (scaleMode === "fit") {
      axis = Math.max(req, lim, use || 0) * 1.15;
      axis = Math.min(Math.max(axis, alloc * 0.12), alloc);
    }
    const zoomed = axis < alloc * 0.995;

    const reqOfNode = pct(req, alloc);
    const limOfNode = pct(lim, alloc);
    const useOfNode = use == null ? null : pct(use, alloc);
    // A node using more than it requested only means something once it is
    // materially loaded; below that the crossing is arithmetic, not a problem.
    const hot = useOfNode != null && useOfNode > reqOfNode && useOfNode > 50;

    d.left.innerHTML = `${isCPU ? "CPU" : "MEMORY"} &middot; ${fmt(alloc)} allocatable` +
      (zoomed ? ` <em class="zoom">axis ${fmt(Math.round(axis))}</em>` : "");
    d.reqS.innerHTML = `requested <b>${fmtPct(reqOfNode)}</b>`;
    d.limS.innerHTML = `limits <b>${fmtPct(limOfNode)}${limOfNode > 100 ? " ⚠" : ""}</b>`;
    d.useS.innerHTML = `actual <b>${useOfNode == null ? "not measured" : fmtPct(useOfNode)}</b>`;
    d.useS.className = hot ? "over" : "";

    if (useOfNode == null) {
      d.usage.hidden = true;
    } else {
      d.usage.hidden = false;
      d.usage.style.width = Math.min(pct(use, axis), 100).toFixed(2) + "%";
      d.usage.classList.toggle("hot", hot);
    }

    const limPct = pct(lim, axis);
    if (limPct > 0 && limPct <= 99.5) {
      d.limLine.hidden = false;
      d.limLine.style.left = limPct.toFixed(2) + "%";
      d.limLine.dataset.label = `limits ${fmtPct(limOfNode)}`;
    } else {
      d.limLine.hidden = true;
    }

    const seen = new Set();
    (node.pods || []).forEach((p) => {
      const key = `${p.namespace}/${p.name}`;
      seen.add(key);

      const pr = isCPU ? p.reqCpu : p.reqMem;
      const pu = isCPU ? (p.useCpu ?? null) : (p.useMem ?? null);
      const hasReq = isCPU ? p.hasCpuRequest : p.hasMemRequest;

      let t = d.tiles.get(key);
      if (!t) {
        const div = el("div", "tile", d.box);
        const spill = el("div", "spill", div);
        const fill = el("div", "fill", div);
        const name = el("span", "name", div);
        t = { div, spill, fill, name };
        d.tiles.set(key, t);
        div.classList.add("arrived");
        setTimeout(() => div.classList.remove("arrived"), 700);
      }

      t.div.style.width = Math.max(pct(pr, axis), MIN_TILE_PCT).toFixed(3) + "%";

      // Fill is usage against this pod's own request. With no request there is
      // no denominator: it declared nothing and is consuming something, so the
      // tile fills and spills rather than reading as empty.
      let fillPct = 0, spill = false;
      if (pu != null) {
        if (hasReq && pr > 0) {
          const ratio = pu / pr;
          fillPct = Math.min(ratio, 1) * 100;
          spill = ratio > 1.02;
        } else if (pu > 0) {
          fillPct = 100;
          spill = true;
        }
      }
      t.fill.style.height = fillPct.toFixed(1) + "%";
      t.spill.hidden = !spill;

      t.div.className = `tile r-${p.role}${p.system ? " sys" : ""}` +
        (killFlash.get(key) > Date.now() ? " killed" : "");

      const label = p.system
        ? shortPod(p.name)
        : `${p.namespace.replace(/^team-/, "")}/${shortPod(p.name)}`;
      if (t.name.textContent !== label) t.name.textContent = label;

      t.div.title = [
        key,
        `role: ${p.role}`,
        `qos: ${p.qos}`,
        `request: ${fmt(pr)}${hasReq ? "" : " (none declared)"}`,
        pu != null ? `using: ${fmt(pu)}` : "usage: not measured",
        p.throttlePct != null ? `throttled: ${p.throttlePct.toFixed(0)}% of periods` : null,
        p.restarts ? `restarts: ${p.restarts}` : null,
      ].filter(Boolean).join("\n");
    });

    d.tiles.forEach((t, key) => {
      if (!seen.has(key)) { t.div.remove(); d.tiles.delete(key); }
    });
  }

  function renderNodes(snap) {
    // Tenant nodes first, and the busiest among those: the node the demo is
    // about should never be the one you have to scroll to.
    const nodes = [...snap.nodes].sort((a, b) => {
      const rank = (n) => (n.severity === "CRITICAL" ? 0 : n.severity === "WARNING" ? 1 : 2);
      if (rank(a) !== rank(b)) return rank(a) - rank(b);
      return (b.tenantCount || 0) - (a.tenantCount || 0);
    });

    const host = $("nodes");
    const seen = new Set();

    nodes.forEach((n, i) => {
      seen.add(n.name);
      let e = nodeEls.get(n.name);
      if (!e) { e = buildNode(n.name); nodeEls.set(n.name, e); }
      if (host.children[i] !== e.root) host.insertBefore(e.root, host.children[i] || null);

      e.root.className = `node sev-${n.severity}`;
      e.sev.className = `sev sev-${n.severity}`;
      e.sev.textContent = n.severity;
      e.meta.textContent = [n.instanceType, n.zone].filter(Boolean).join(" · ");
      const ts = n.tenants || [];
      e.tenantsEl.textContent =
        `${ts.length} tenant${ts.length === 1 ? "" : "s"}${ts.length ? ": " + ts.join(", ") : ""}`;
      e.press.hidden = !n.memoryPressure;

      updateDim(e.cpu, n, true);
      updateDim(e.mem, n, false);
    });

    nodeEls.forEach((e, name) => {
      if (!seen.has(name)) { e.root.remove(); nodeEls.delete(name); }
    });
  }

  /* ---------- rail ---------- */

  function renderFindings(snap) {
    const rows = [];
    (snap.aggressors || []).forEach((f) => rows.push({ ...f, cls: "aggressor", kind: "aggressor" }));
    (snap.exposed || []).forEach((f) => rows.push({ ...f, cls: "exposed", kind: "exposed" }));

    if (!rows.length) {
      $("findings").innerHTML = '<p class="quiet">Nothing to report on this frame.</p>';
      return;
    }
    $("findings").innerHTML = rows.map((f) => `
      <div class="finding ${f.cls}">
        <span class="kind">${esc(f.kind)}</span>
        <span>
          <b class="who">${esc(f.workload || f.tenant)}</b>
          <span class="what">${esc(f.message)}</span>
        </span>
      </div>`).join("");
  }

  function renderEvents(snap) {
    const list = $("events");
    (snap.events || []).forEach((e) => {
      // Events arrive per frame; de-duplicate so a reconnect does not replay
      // the rail and make one kill look like three.
      const id = `${e.at}|${e.kind}|${e.namespace}/${e.pod}`;
      if (seenEvents.has(id)) return;
      seenEvents.add(id);

      if (e.kind === "oomkill") killFlash.set(`${e.namespace}/${e.pod}`, Date.now() + 4000);
      if (e.kind === "added") arrived.set(`${e.namespace}/${e.pod}`, Date.now() + 1500);

      const li = document.createElement("li");
      li.className = `${e.kind} fresh`;
      li.innerHTML = `<span class="t">${clock(e.at)}</span>
        <span class="msg"><b>${esc(e.namespace.replace(/^team-/, ""))}/${esc(shortPod(e.pod))}</b> ${esc(e.message)}</span>`;
      list.prepend(li);
      setTimeout(() => li.classList.remove("fresh"), 2600);
    });
    while (list.children.length > MAX_EVENTS) list.removeChild(list.lastChild);
  }

  function renderTenants(snap) {
    const rows = [...(snap.tenants || [])].sort((a, b) => {
      if (a.system !== b.system) return a.system ? 1 : -1;
      return (b.reqMem || 0) - (a.reqMem || 0);
    });
    $("tenantrows").innerHTML = rows.map((t) => `
      <tr class="${t.system ? "sys" : ""}">
        <td>${esc(t.namespace)}</td>
        <td class="n">${t.pods}</td>
        <td class="n">${cpu(t.reqCpu)}</td>
        <td class="n">${mem(t.reqMem)}</td>
        <td class="n">${t.useCpu == null ? "&mdash;" : cpu(t.useCpu)}</td>
        <td class="n">${t.useMem == null ? "&mdash;" : mem(t.useMem)}</td>
        <td class="n">${t.strandedMem > 0 ? mem(t.strandedMem) : "&mdash;"}</td>
        <td class="n ${t.oomKills ? "bad" : ""}">${t.oomKills || 0}</td>
        <td class="n ${t.throttled ? "warn" : ""}">${t.throttled || 0}</td>
      </tr>`).join("");
  }

  /* ---------- toast + keys ---------- */

  let toastTimer = null;
  function toast(msg, isErr) {
    const el = $("toast");
    el.textContent = msg;
    el.classList.toggle("err", !!isErr);
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (el.hidden = true), isErr ? 7000 : 3200);
  }

  function setStage(n) {
    stage = n;
    document.body.dataset.stage = String(n);
    toast(["", "pods on nodes", "what each tenant requested", "what each tenant is actually using"][n]);
  }

  async function press(action) {
    try {
      const r = await fetch(`/api/button${action ? "?action=" + action : ""}`, { method: "POST" });
      const body = await r.json().catch(() => ({}));
      if (!r.ok) { toast(body.error || `button failed (${r.status})`, true); return; }
      toast(action === "reset" ? "aggressor scaled to 0" : "aggressor scaled to 1");
    } catch (e) {
      toast(`button failed: ${e.message}`, true);
    }
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
    switch (ev.key) {
      case "1": setStage(1); break;
      case "2": setStage(2); break;
      case "3": setStage(3); break;
      case "4": window.open("/tenant", "tenantview", "width=760,height=900"); break;
      case "b": case "B": press(); break;
      case "r": case "R": press("reset"); break;
      case "s": case "S":
        scaleMode = scaleMode === "fit" ? "node" : "fit";
        toast(scaleMode === "fit"
          ? "axis: scaled to the workload, so names and spill are readable"
          : "axis: the whole node, so you can see how much of it is claimed");
        if (lastSnap) { lastSeq = -1; render(lastSnap); }
        break;
      case "Escape": location.href = "/"; break;
      case "?": toast("S axis \u00b7 1 pods · 2 requests · 3 usage · 4 tenant view · B button · R reset"); break;
    }
  });

  /* ---------- stream ---------- */

  let lastSnap = null;

  function render(snap) {
    if (snap.seq === lastSeq) return;
    lastSeq = snap.seq;
    lastSnap = snap;
    renderHeader(snap);
    renderEvents(snap); // before nodes, so a kill flash is set for this paint
    renderNodes(snap);
    renderFindings(snap);
    renderTenants(snap);
  }

  function connect() {
    const es = new EventSource("/api/stream");
    const dot = $("livedot");

    es.onopen = () => { dot.className = "dot"; };
    es.onmessage = (ev) => {
      dot.className = "dot";
      try { render(JSON.parse(ev.data)); }
      catch (e) { console.error("bad frame", e); }
    };
    es.onerror = () => {
      // EventSource reconnects on its own. Say so rather than going blank: a
      // wall frozen without explanation reads as a broken demo.
      dot.className = "dot stale";
      $("source").textContent = "reconnecting";
    };
  }

  // Draw whatever the server already has, so the page is never blank while the
  // first poll completes.
  fetch("/api/snapshot")
    .then((r) => r.json())
    .then((s) => { if (!s.pending) render(s); })
    .catch(() => {})
    .finally(connect);
})();
