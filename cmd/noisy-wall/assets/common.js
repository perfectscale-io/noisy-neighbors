/* Shared by every view of the wall.
 *
 * Formatting, the verdict, the stream and the controls live here on purpose.
 * Two separate bugs in this repository were the same bug: a view re-deriving a
 * judgement the analyzer had already made, and then disagreeing with the scan
 * on screen. There is one verdict function and every view calls it.
 */

window.NN = (() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  function mem(b) {
    if (b == null) return "—";
    if (b === 0) return "0";
    const u = ["B", "Ki", "Mi", "Gi", "Ti"];
    let i = 0, v = b;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)}${u[i]}`;
  }
  const cpu = (m) => (m == null ? "—" : `${m}m`);
  const short = (ns) => ns.replace(/^team-/, "");

  function shortPod(name) {
    const parts = name.split("-");
    return parts.length > 2 ? parts.slice(0, parts.length - 2).join("-") : name;
  }

  /* The role the analyzer gave this tenant's worst pod, so every view is
   * colored by the judgement the scan prints rather than a second opinion. */
  function rolesByTenant(snap) {
    const rank = ["oom", "aggressor", "throttled", "stranded", "besteffort", "ok"];
    const out = {};
    (snap.nodes || []).forEach((n) => (n.pods || []).forEach((p) => {
      if (p.system) return;
      const cur = out[p.namespace];
      if (cur == null || rank.indexOf(p.role) < rank.indexOf(cur)) out[p.namespace] = p.role;
    }));
    return out;
  }

  /* One line of plain English per tenant.
   *
   * The role decides the wording; the numbers only fill it in. That order
   * matters: branching on the tenant counters instead meant that a poll
   * returning a zero throttle delta made a row claim a container was idle
   * while the analyzer, the scan and the console all still called it
   * throttled.
   *
   * The wording never says a neighbor caused anything. This lab contains
   * co-tenancy and exposure, not proven causation, and the tool's own output
   * holds to that distinction. */
  function verdict(t, role, asked, using, other) {
    const ratio = asked > 0 && using != null ? using / asked : null;
    const times = (r) => (r >= 10 ? Math.round(r) : r.toFixed(1));

    switch (role) {
      case "oom": {
        const n = t.restarts || t.oomKills;
        return { role, text: n > 1 ? `killed ${n} times` : "killed once" };
      }
      case "throttled":
        return { role, text: "throttled" };
      case "aggressor":
        if (asked === 0) return { role, text: "took it without asking" };
        // The analyzer judges CPU and memory separately, so a workload can be
        // an aggressor on one while sitting well inside its request on the
        // other. Printing this view's ratio regardless produced "0.0x what it
        // asked for" on a container that had genuinely overrun its CPU: a
        // true role attached to a number from the wrong dimension.
        if (ratio == null || ratio < 1.5) {
          return { role, text: other ? `over on ${other}` : "over on its other resource" };
        }
        return { role, text: `${times(ratio)}× what it asked for` };
      case "stranded":
        // Same trap as the aggressor case: the role can come from the other
        // dimension. A pod idle on CPU but using all of its memory request
        // must not be captioned "using almost none of it" on the memory view.
        if (ratio != null && ratio > 0.5) {
          return { role, text: other ? `idle on ${other}` : "idle on its other resource" };
        }
        return { role, text: "using almost none of it" };
      case "besteffort":
        return { role, text: "asked for nothing" };
    }

    // No role from the analyzer. Say what the numbers say and nothing more.
    if (using == null) return { role: "ok", text: "not measured" };
    if (ratio != null && ratio >= 1.5) {
      return { role: "aggressor", text: `${times(ratio)}× what it asked for` };
    }
    if (ratio != null && ratio <= 0.5) return { role: "stranded", text: "using almost none of it" };
    return { role: "ok", text: "about right" };
  }

  /* ---------- controls ---------- */

  let toastTimer = null;
  function toast(msg, isErr) {
    const el = $("toast");
    if (!el) return;
    el.textContent = msg;
    el.classList.toggle("err", !!isErr);
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => (el.hidden = true), isErr ? 7000 : 3400);
  }

  async function press(action) {
    try {
      const r = await fetch(`/api/button${action ? "?action=" + action : ""}`, { method: "POST" });
      const body = await r.json().catch(() => ({}));
      if (!r.ok) { toast(body.error || `button failed (${r.status})`, true); return; }
      toast(action === "reset" ? "aggressor scaled back to zero" : "aggressor scaled to one");
    } catch (e) {
      toast(`button failed: ${e.message}`, true);
    }
  }

  /* ---------- stream ---------- */

  /* start wires a view up: it fetches the scoping config, paints whatever the
   * server already has so the page is never blank while the first poll runs,
   * then follows the stream. */
  function start(opts) {
    const { render, onKey } = opts;
    let lastSeq = -1;
    let showOnly = [];
    let lastSnap = null;

    const paint = (snap, force) => {
      if (!force && snap.seq === lastSeq) return;
      lastSeq = snap.seq;
      lastSnap = snap;
      const tenants = (snap.tenants || [])
        .filter((t) => !t.system && t.pods > 0)
        .filter((t) => showOnly.length === 0 || showOnly.includes(t.namespace));
      render(snap, tenants, rolesByTenant(snap));
    };

    document.addEventListener("keydown", (ev) => {
      if (ev.metaKey || ev.ctrlKey || ev.altKey) return;
      switch (ev.key) {
        case "b": case "B": press(); return;
        case "r": case "R": press("reset"); return;
        case "t": case "T":
          window.open("/tenant", "tenantview", "width=800,height=940"); return;
        case "d": case "D": location.href = "/detail"; return;
      }
      if (onKey) onKey(ev, () => lastSnap && paint(lastSnap, true));
    });

    fetch("/api/config")
      .then((r) => r.json())
      .then((c) => { showOnly = c.tenants || []; })
      .catch(() => {})
      .then(() => fetch("/api/snapshot"))
      .then((r) => r.json())
      .then((s) => { if (!s.pending) paint(s); })
      .catch(() => {})
      .finally(() => {
        const es = new EventSource("/api/stream");
        es.onmessage = (ev) => {
          try { paint(JSON.parse(ev.data)); } catch (e) { console.error("bad frame", e); }
        };
        es.onerror = () => {
          // EventSource reconnects on its own. Say so rather than going quiet:
          // a frozen picture with no explanation reads as a broken demo.
          const src = $("source");
          if (src) src.textContent = "reconnecting";
        };
      });
  }

  return { $, mem, cpu, short, shortPod, verdict, rolesByTenant, toast, press, start };
})();
