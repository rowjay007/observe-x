// Trace waterfall + service-map renderers — Phase E-3.
//
// Both work on the row shape returned by the ObserveQL query against
// the `traces` table:
//   { trace_id, span_id, parent_span_id, service_name, operation_name,
//     start_time, end_time, duration_ns, status_code, attributes }
//
// Waterfall:
//   * normalises start/end to the root span's start
//   * draws horizontal bars laid out by start offset and width
//   * indents children one level per parent step (Gantt-style)
//
// Service-map:
//   * builds a graph of unique services with edges = span calls
//     parent.service → child.service, weighted by call count
//   * lays nodes out on a circle, draws labelled circles + edges

function renderWaterfall(host, spans) {
  host.innerHTML = "";
  if (!spans || !spans.length) {
    host.innerHTML = '<div style="color:var(--fg-2)">no spans</div>';
    return;
  }
  const norm = spans.map((s) => ({
    id: s.span_id,
    parent: s.parent_span_id || "",
    service: s.service_name || "",
    op: s.operation_name || "",
    status: String(s.status_code || "OK"),
    start: tsToMicros(s.start_time),
    end: tsToMicros(s.end_time),
    durNs: Number(s.duration_ns || 0),
  }));
  const t0 = Math.min(...norm.map((s) => s.start));
  const tn = Math.max(...norm.map((s) => s.end));
  const span = Math.max(1, tn - t0);

  // Build parent → children index for ordered traversal.
  const childrenOf = new Map();
  for (const s of norm) {
    if (!childrenOf.has(s.parent)) childrenOf.set(s.parent, []);
    childrenOf.get(s.parent).push(s);
  }
  for (const arr of childrenOf.values()) arr.sort((a, b) => a.start - b.start);
  const roots = norm.filter((s) => !norm.find((p) => p.id === s.parent));

  const ordered = [];
  const visit = (s, depth) => {
    ordered.push({ ...s, depth });
    for (const c of (childrenOf.get(s.id) || [])) visit(c, depth + 1);
  };
  for (const r of roots) visit(r, 0);

  for (const s of ordered) {
    const leftPct = ((s.start - t0) / span) * 100;
    const widthPct = Math.max(0.4, ((s.end - s.start) / span) * 100);
    const row = document.createElement("div");
    row.className = "span-row" + (s.depth > 0 ? " child" : "")
      + (s.status !== "OK" && s.status !== "" ? " error" : "");
    row.innerHTML = `
      <div class="label" title="${esc(s.service + ' · ' + s.op)}">
        ${"&nbsp;".repeat(s.depth * 4)}${esc(s.service)} · ${esc(s.op)}
      </div>
      <div style="flex:1; position:relative; height:10px;">
        <div class="bar" style="position:absolute; left:${leftPct.toFixed(2)}%; width:${widthPct.toFixed(2)}%;"></div>
      </div>
      <div class="dur">${(s.durNs / 1e6).toFixed(2)} ms</div>`;
    host.appendChild(row);
  }
}

function renderServiceMap(canvas, spans) {
  const ctx = canvas.getContext("2d");
  const dpr = window.devicePixelRatio || 1;
  const w = canvas.clientWidth || 500;
  const h = canvas.clientHeight || 220;
  canvas.width = w * dpr; canvas.height = h * dpr;
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, w, h);
  if (!spans || !spans.length) {
    ctx.fillStyle = "#9da7b1";
    ctx.font = "10px ui-monospace, SFMono-Regular, Menlo, monospace";
    ctx.textAlign = "center";
    ctx.fillText("no spans", w / 2, h / 2);
    return;
  }
  // Build service-level graph.
  const byID = new Map(spans.map((s) => [s.span_id, s]));
  const edges = new Map(); // "a→b" → count
  const nodes = new Set();
  for (const s of spans) {
    nodes.add(s.service_name || "?");
    if (s.parent_span_id) {
      const p = byID.get(s.parent_span_id);
      if (p && p.service_name && p.service_name !== s.service_name) {
        const k = p.service_name + "→" + s.service_name;
        edges.set(k, (edges.get(k) || 0) + 1);
      }
    }
  }
  const nodeList = Array.from(nodes);
  const cx = w / 2, cy = h / 2, r = Math.min(w, h) / 2 - 30;
  const positions = new Map();
  nodeList.forEach((n, i) => {
    const ang = (i / nodeList.length) * 2 * Math.PI - Math.PI / 2;
    positions.set(n, { x: cx + r * Math.cos(ang), y: cy + r * Math.sin(ang) });
  });
  // Edges.
  ctx.strokeStyle = "rgba(255,255,255,0.18)";
  ctx.lineWidth = 1;
  for (const [k, count] of edges) {
    const [a, b] = k.split("→");
    const pa = positions.get(a), pb = positions.get(b);
    if (!pa || !pb) continue;
    ctx.lineWidth = Math.min(4, 1 + Math.log2(count + 1));
    ctx.beginPath(); ctx.moveTo(pa.x, pa.y); ctx.lineTo(pb.x, pb.y); ctx.stroke();
    // Arrowhead.
    const dx = pb.x - pa.x, dy = pb.y - pa.y, len = Math.hypot(dx, dy);
    const ux = dx / len, uy = dy / len;
    const ax = pb.x - ux * 16, ay = pb.y - uy * 16;
    ctx.beginPath();
    ctx.moveTo(pb.x - ux * 8, pb.y - uy * 8);
    ctx.lineTo(ax - uy * 4, ay + ux * 4);
    ctx.lineTo(ax + uy * 4, ay - ux * 4);
    ctx.closePath();
    ctx.fillStyle = "rgba(255,255,255,0.35)";
    ctx.fill();
  }
  // Nodes.
  ctx.font = "11px ui-monospace, SFMono-Regular, Menlo, monospace";
  for (const n of nodeList) {
    const p = positions.get(n);
    ctx.beginPath();
    ctx.fillStyle = "#1f262e";
    ctx.strokeStyle = "#58a6ff";
    ctx.lineWidth = 1.5;
    ctx.arc(p.x, p.y, 18, 0, 2 * Math.PI);
    ctx.fill(); ctx.stroke();
    ctx.fillStyle = "#e6edf3";
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.fillText(n.slice(0, 10), p.x, p.y);
  }
}

function tsToMicros(v) {
  if (!v) return 0;
  // Best effort: parse string or pass-through number.
  const d = new Date(v).getTime();
  return isNaN(d) ? 0 : d * 1000;
}
function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]),
  );
}

window.renderWaterfall = renderWaterfall;
window.renderServiceMap = renderServiceMap;
