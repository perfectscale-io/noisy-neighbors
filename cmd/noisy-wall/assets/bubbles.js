/* The bubble view.
 *
 * One node. Inside it, one bubble per tenant:
 *
 *   the dashed ring   area = what the tenant asked the scheduler for
 *   the solid disc    area = what it is actually using
 *
 * A disc inside its ring is a tenant living within its declaration. A disc
 * that has burst its ring is one taking more than it declared. A ring with
 * almost nothing in it is capacity the scheduler will not offer to anybody
 * else. That is the whole vocabulary, and it needs no axis.
 *
 * Area, not radius: value maps to area, so radius goes as its square root.
 * Mapping value to radius instead would square every difference and make an
 * 11x overrun look like 121x, which is the sort of flattery that gets a demo
 * taken apart in the Q&A.
 */

(() => {
  "use strict";

  const { $, mem, cpu, short, verdict, toast, start } = window.NN;

  const W = 1000, H = 420;       // viewBox; the SVG scales to its container
  const PAD = 30;                // inset from the node box
  const GAP = 16;                // breathing room between bubbles
  const MIN_DISC = 2.5;          // a real but tiny usage must still be a dot
  const LABEL_ROOM = 76;         // vertical space under the row for its captions
  // Every bubble needs room for its own caption, however small the circle is.
  // Without this, two tiny neighbors print their names on top of each other.
  const MIN_SLOT = 158;

  let dim = "mem";
  const seenEvents = new Set();
  const placed = new Map();      // namespace -> {x, y, el...}
  let layoutKey = "";

  const askedOf = (t) => (dim === "mem" ? t.reqMem : t.reqCpu) || 0;
  const usingOf = (t) => (dim === "mem" ? t.useMem : t.useCpu);
  const fmt = () => (dim === "mem" ? mem : cpu);

  const svgEl = (tag, attrs) => {
    const e = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const k in attrs || {}) e.setAttribute(k, attrs[k]);
    return e;
  };

  /* ---------- layout ---------- */

  /* Bubbles are laid out once per set of tenants and then left alone.
   *
   * Re-solving positions every poll made the picture restless and the thing
   * being pointed at impossible to follow, which is the opposite of the point.
   * Only the radii animate after this; the slow CSS drift supplies the life
   * that re-packing was accidentally providing. */
  function layout(tenants) {
    // The footprint a bubble needs is its larger circle: a disc that has
    // outgrown its ring must still sit inside the node.
    const span = tenants.map((t) => {
      const a = askedOf(t), u = usingOf(t) || 0;
      return Math.sqrt(Math.max(a, u, 1));
    });

    const usableW = W - PAD * 2;
    const usableH = H - PAD * 2 - LABEL_ROOM;
    const maxSpan = Math.max(...span, 1);

    // Width of the row at a given scale, where every bubble also claims enough
    // horizontal room for its caption.
    const rowWidth = (s) =>
      span.reduce((acc, sp) => acc + Math.max(sp * 2 * s, MIN_SLOT), 0) +
      GAP * (tenants.length - 1);

    // Start from whichever constraint binds, then shrink until the captions
    // fit too. A closed form would have to invert a max(), so a few passes of
    // this is both shorter and easier to be sure about.
    let s = Math.min(usableW / (2 * (span.reduce((a, b) => a + b, 0) || 1)),
                     usableH / (2 * maxSpan));
    for (let i = 0; i < 40 && rowWidth(s) > usableW; i++) s *= 0.92;
    s = Math.max(s, 0.0001);

    // Center the row, so a cluster of small tenants does not sit hard left.
    let x = PAD + Math.max(0, (usableW - rowWidth(s)) / 2);
    const cy = PAD + usableH / 2;

    return tenants.map((t, i) => {
      const r = span[i] * s;
      const slot = Math.max(r * 2, MIN_SLOT);
      const cx = x + slot / 2;
      x += slot + GAP;
      return { ns: t.namespace, cx, cy, footprint: r, scale: s };
    });
  }

  function buildBubble() {
    const g = svgEl("g", { class: "bub" });
    const float = svgEl("g", { class: "float" });
    const pulse = svgEl("circle", { class: "pulse", r: 6 });
    const disc = svgEl("circle", { class: "disc", r: 0 });
    const ring = svgEl("circle", { class: "ring", r: 0 });
    const cap = svgEl("path", { class: "cap", d: "", opacity: 0 });
    const name = svgEl("text", { class: "name" });
    const say = svgEl("text", { class: "say" });
    const nums = svgEl("text", { class: "nums" });
    float.append(pulse, disc, ring, cap, name, say, nums);
    g.append(float);
    return { g, float, pulse, disc, ring, cap, name, say, nums };
  }

  /* ---------- render ---------- */

  function render(snap, tenants, roles) {
    const host = $("bubbles");
    const f = fmt();

    // Biggest claim first, so the eye lands on the tenant holding the most.
    tenants = [...tenants].sort((a, b) => askedOf(b) - askedOf(a) ||
      (usingOf(b) || 0) - (usingOf(a) || 0));

    // Re-solve only when the cast changes, or the resource being shown does.
    const key = dim + "|" + tenants.map((t) => t.namespace).join(",");
    const spots = layout(tenants);
    const relayout = key !== layoutKey;
    layoutKey = key;

    let worst = null;
    const seen = new Set();
    const rows = [];

    tenants.forEach((t, i) => {
      seen.add(t.namespace);
      const spot = spots[i];
      let b = placed.get(t.namespace);
      if (!b) {
        b = buildBubble();
        placed.set(t.namespace, b);
        host.appendChild(b.g);
        // Arrive at the right place rather than sliding in from the origin.
        b.g.setAttribute("transform", `translate(${spot.cx} ${spot.cy})`);
        b.float.style.animationDelay = `-${i * 1.6}s`;
      }
      if (relayout) b.g.setAttribute("transform", `translate(${spot.cx} ${spot.cy})`);

      const asked = askedOf(t);
      const using = usingOf(t);
      const v = verdict(t, roles[t.namespace] || "ok", asked, using,
        dim === "mem" ? "cpu" : "memory");
      if (!worst && (v.role === "oom" || v.role === "aggressor")) worst = { t, v };

      b.g.setAttribute("class", `bub r-${v.role}`);

      // Radii share one scale, so a circle twice the area is twice the value
      // on any bubble on screen.
      const rAsked = Math.sqrt(asked) * spot.scale;
      const rUsing = using == null || using <= 0
        ? 0
        : Math.max(Math.sqrt(using) * spot.scale, MIN_DISC);

      b.ring.style.r = `${rAsked}px`;
      b.ring.setAttribute("r", rAsked);
      b.ring.style.opacity = asked > 0 ? "1" : "0";

      b.disc.style.r = `${rUsing}px`;
      b.disc.setAttribute("r", rUsing);
      b.disc.classList.toggle("over", asked > 0 && using != null && using > asked * 1.02);

      // The throttling lid: an arc across the top of the disc. It says "held
      // down", which is not a size, so it must not be drawn as one.
      if (v.role === "throttled" && rUsing > 0) {
        const r = Math.max(rUsing, 12);
        b.cap.setAttribute("d", `M ${-r} 0 A ${r} ${r} 0 0 1 ${r} 0`);
        b.cap.setAttribute("opacity", "1");
      } else {
        b.cap.setAttribute("opacity", "0");
      }

      rows.push({ b, t, v, asked, using, reach: Math.max(rAsked, rUsing, 10) });
    });

    // Captions go on after every radius is known, so they can share a
    // baseline instead of stepping up and down with each circle.
    const baseline = rows.reduce((m, r) => Math.max(m, r.reach), 10) + 20;
    rows.forEach(({ b, t, v, asked, using }) => {
      b.name.textContent = short(t.namespace);
      b.name.setAttribute("y", baseline);
      b.say.textContent = v.text;
      b.say.setAttribute("y", baseline + 20);
      b.nums.textContent = asked === 0
        ? `using ${f(using)}`
        : `${f(asked)} \u2192 ${f(using)}`;
      b.nums.setAttribute("y", baseline + 39);
    });

    placed.forEach((b, ns) => {
      if (!seen.has(ns)) { b.g.remove(); placed.delete(ns); }
    });

    chrome(snap, tenants, worst);
    feed(snap);
  }

  function chrome(snap, tenants, worst) {
    const WORDS = ["zero", "one", "two", "three", "four", "five", "six",
      "seven", "eight", "nine", "ten", "eleven", "twelve"];
    const n = tenants.length;
    $("tcount").textContent = `${WORDS[n] || n} tenant${n === 1 ? "" : "s"}`;

    const sub = $("subhead");
    if (worst && worst.v.role === "oom") {
      sub.textContent = `${short(worst.t.namespace)} is being killed. It asked for the right amount.`;
      sub.className = "alarm";
    } else if (worst) {
      sub.textContent = `${short(worst.t.namespace)} is taking far more than it declared. Everyone else was placed around the smaller number.`;
      sub.className = "alarm";
    } else {
      sub.textContent = "Each of them told the scheduler what it needed.";
      sub.className = "";
    }

    const node = [...(snap.nodes || [])]
      .sort((a, b) => (b.tenantCount || 0) - (a.tenantCount || 0))[0];
    if (node) {
      const alloc = dim === "mem" ? node.allocMem : node.allocCpu;
      const req = dim === "mem" ? node.reqMem : node.reqCpu;
      const use = dim === "mem" ? node.useMem : node.useCpu;
      const pc = (v) => (alloc > 0 && v != null ? `${Math.round((v / alloc) * 100)}%` : "—");
      $("nodename").textContent = node.name + (node.instanceType ? ` · ${node.instanceType}` : "");
      // The percentages are printed because circles cannot tile a rectangle:
      // the whitespace around the bubbles is a packing artefact, not headroom,
      // and nobody should be inferring spare capacity from it.
      $("nodecap").textContent =
        `${fmt()(alloc)} allocatable · ${pc(req)} requested · ${pc(use)} in use`;
      $("nodebox").classList.toggle("pressure", !!node.memoryPressure);
    }

    $("dimname").textContent = dim === "mem" ? "memory" : "cpu";
    const src = $("source");
    src.textContent = snap.source;
    src.dataset.src = snap.source;
  }

  function feed(snap) {
    (snap.events || []).forEach((e) => {
      const id = `${e.at}|${e.kind}|${e.namespace}/${e.pod}`;
      if (seenEvents.has(id)) return;
      seenEvents.add(id);

      const who = short(e.namespace);
      let line = null;
      if (e.kind === "oomkill") {
        line = `${who} was just OOMKilled`;
        const b = placed.get(e.namespace);
        if (b) {
          b.pulse.classList.remove("go");
          void b.pulse.getBoundingClientRect(); // restart the animation
          b.pulse.classList.add("go");
        }
      } else if (e.kind === "added") {
        line = `${who} just landed on this node`;
      } else if (e.kind === "throttle") {
        line = `${who} started being throttled`;
      } else if (e.kind === "restart") {
        line = `${who} restarted`;
      }
      if (!line) return;

      $("feedtext").textContent = line;
      const el = $("feed");
      el.classList.add("hot");
      clearTimeout(feed.t);
      feed.t = setTimeout(() => el.classList.remove("hot"), 6000);
    });
  }

  // The hatch used for a tenant that declared nothing.
  (function defs() {
    const svg = $("stage");
    const d = svgEl("defs");
    const p = svgEl("pattern", {
      id: "hatch", width: 8, height: 8,
      patternTransform: "rotate(45)", patternUnits: "userSpaceOnUse",
    });
    p.appendChild(svgEl("rect", { width: 8, height: 8, fill: "#6d4bb8", "fill-opacity": ".16" }));
    p.appendChild(svgEl("rect", { width: 3.5, height: 8, fill: "#6d4bb8", "fill-opacity": ".72" }));
    d.appendChild(p);
    svg.insertBefore(d, svg.firstChild);
  })();

  start({
    render,
    onKey(ev, repaint) {
      switch (ev.key) {
        case "m": case "M": case "c": case "C":
          dim = dim === "mem" ? "cpu" : "mem";
          toast(dim === "mem" ? "showing memory" : "showing cpu");
          repaint();
          break;
        case "l": case "L":
          location.href = "/bars";
          break;
      }
    },
  });
})();
