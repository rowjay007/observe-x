// ObserveX minimal time-series chart primitive — Phase E-1.
//
// Why hand-rolled instead of uPlot / Chart.js / ECharts:
//   * Zero dependencies. The operator console is a single Go binary
//     with embedded static assets and no build step; adding a 40 KB
//     vendor file plus its license headers fights that ethos.
//   * Surface we need is small. Time-series line/area, log/lin Y,
//     tooltip on hover, legend, HiDPI rendering. ~300 LOC.
//   * Tight integration. The chart reads the same NDJSON ObserveQL
//     emits, so the Metrics tab just passes streamed rows through.
//
// API:
//   const ch = new ObservexChart(canvasEl, { ylog: false });
//   ch.setSeries([
//     { name: "rps · checkout-api", color: "#58a6ff",
//       points: [{ t: Date, v: Number }, ...] },
//   ]);
//   ch.render();        // imperative; redraws on resize automatically.
//   ch.destroy();       // detach listeners.
//
// Tooltip on hover, X-axis time formatting (auto-picks
// seconds/minutes/hours/days), and a click-drag time-range select
// that fires `chart.onSelect({from, to})` are included.

class ObservexChart {
  constructor(canvas, opts = {}) {
    this.canvas = canvas;
    this.ctx = canvas.getContext("2d");
    this.opts = Object.assign(
      {
        padTop: 14,
        padRight: 14,
        padBottom: 28,
        padLeft: 54,
        gridColor: "rgba(255,255,255,0.06)",
        axisColor: "#9da7b1",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        fontSize: 10,
        ylog: false,
        fill: false,
        onSelect: null,
      },
      opts,
    );
    this.series = [];
    this._dragStart = null;
    this._mouse = null;

    this._onResize = () => this.render();
    window.addEventListener("resize", this._onResize);
    this.canvas.addEventListener("mousemove", (e) => this._onMouseMove(e));
    this.canvas.addEventListener("mouseleave", () => {
      this._mouse = null;
      this.render();
    });
    this.canvas.addEventListener("mousedown", (e) => {
      this._dragStart = this._toData(e);
    });
    this.canvas.addEventListener("mouseup", (e) => {
      if (!this._dragStart || !this.opts.onSelect) {
        this._dragStart = null;
        return;
      }
      const end = this._toData(e);
      const from = Math.min(this._dragStart.t, end.t);
      const to = Math.max(this._dragStart.t, end.t);
      this._dragStart = null;
      if (to - from > 1000) this.opts.onSelect({ from: new Date(from), to: new Date(to) });
      this.render();
    });
  }

  setSeries(series) {
    this.series = series.map((s) => ({
      name: s.name || "",
      color: s.color || "#58a6ff",
      points: (s.points || []).filter((p) => p.t != null && !isNaN(+p.t) && !isNaN(+p.v)),
    }));
  }

  destroy() {
    window.removeEventListener("resize", this._onResize);
  }

  // ── Public render entry ─────────────────────────────────────────
  render() {
    const dpr = window.devicePixelRatio || 1;
    const cssW = this.canvas.clientWidth || 600;
    const cssH = this.canvas.clientHeight || 220;
    if (this.canvas.width !== cssW * dpr || this.canvas.height !== cssH * dpr) {
      this.canvas.width = cssW * dpr;
      this.canvas.height = cssH * dpr;
    }
    const ctx = this.ctx;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const bounds = this._bounds();
    if (!bounds) {
      this._drawEmpty(cssW, cssH);
      return;
    }
    const plot = this._plotRect(cssW, cssH);
    this._drawGrid(plot, bounds);
    this._drawSeries(plot, bounds);
    this._drawAxes(plot, bounds);
    this._drawLegend(plot);
    this._drawCrosshair(plot, bounds);
  }

  // ── Internals ───────────────────────────────────────────────────
  _bounds() {
    let tMin = Infinity, tMax = -Infinity, vMin = Infinity, vMax = -Infinity, n = 0;
    for (const s of this.series) {
      for (const p of s.points) {
        const t = +p.t, v = +p.v;
        if (t < tMin) tMin = t;
        if (t > tMax) tMax = t;
        if (v < vMin) vMin = v;
        if (v > vMax) vMax = v;
        n++;
      }
    }
    if (n === 0) return null;
    if (vMin === vMax) {
      vMin -= 1;
      vMax += 1;
    }
    if (tMin === tMax) tMax = tMin + 60_000;
    // pad y by 8% top/bottom for readability
    const span = vMax - vMin;
    return { tMin, tMax, vMin: vMin - span * 0.08, vMax: vMax + span * 0.08 };
  }

  _plotRect(w, h) {
    const { padTop, padRight, padBottom, padLeft } = this.opts;
    return { x: padLeft, y: padTop, w: w - padLeft - padRight, h: h - padTop - padBottom };
  }

  _xScale(plot, t, b) {
    return plot.x + ((t - b.tMin) / (b.tMax - b.tMin)) * plot.w;
  }
  _yScale(plot, v, b) {
    if (this.opts.ylog) {
      const lv = Math.log10(Math.max(1e-9, v));
      const lmin = Math.log10(Math.max(1e-9, b.vMin));
      const lmax = Math.log10(Math.max(1e-9, b.vMax));
      return plot.y + plot.h - ((lv - lmin) / (lmax - lmin)) * plot.h;
    }
    return plot.y + plot.h - ((v - b.vMin) / (b.vMax - b.vMin)) * plot.h;
  }

  _drawEmpty(w, h) {
    const ctx = this.ctx;
    ctx.fillStyle = this.opts.axisColor;
    ctx.font = `${this.opts.fontSize}px ${this.opts.fontFamily}`;
    ctx.textAlign = "center";
    ctx.textBaseline = "middle";
    ctx.fillText("no data", w / 2, h / 2);
  }

  _drawGrid(plot, b) {
    const ctx = this.ctx;
    ctx.strokeStyle = this.opts.gridColor;
    ctx.lineWidth = 1;
    // horizontal grid lines (y)
    const yTicks = this._yTicks(b);
    for (const t of yTicks) {
      const y = this._yScale(plot, t, b);
      ctx.beginPath();
      ctx.moveTo(plot.x, y);
      ctx.lineTo(plot.x + plot.w, y);
      ctx.stroke();
    }
    // vertical grid lines (x)
    const xTicks = this._xTicks(b);
    for (const t of xTicks) {
      const x = this._xScale(plot, t, b);
      ctx.beginPath();
      ctx.moveTo(x, plot.y);
      ctx.lineTo(x, plot.y + plot.h);
      ctx.stroke();
    }
  }

  _drawSeries(plot, b) {
    const ctx = this.ctx;
    for (const s of this.series) {
      if (!s.points.length) continue;
      ctx.strokeStyle = s.color;
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      let started = false;
      for (const p of s.points) {
        const x = this._xScale(plot, +p.t, b);
        const y = this._yScale(plot, +p.v, b);
        if (!started) {
          ctx.moveTo(x, y);
          started = true;
        } else ctx.lineTo(x, y);
      }
      ctx.stroke();
      if (this.opts.fill) {
        ctx.lineTo(this._xScale(plot, +s.points[s.points.length - 1].t, b), plot.y + plot.h);
        ctx.lineTo(this._xScale(plot, +s.points[0].t, b), plot.y + plot.h);
        ctx.closePath();
        ctx.fillStyle = s.color + "22";
        ctx.fill();
      }
    }
  }

  _drawAxes(plot, b) {
    const ctx = this.ctx;
    ctx.fillStyle = this.opts.axisColor;
    ctx.font = `${this.opts.fontSize}px ${this.opts.fontFamily}`;
    // y-axis labels
    ctx.textAlign = "right";
    ctx.textBaseline = "middle";
    for (const v of this._yTicks(b)) {
      const y = this._yScale(plot, v, b);
      ctx.fillText(formatNumber(v), plot.x - 6, y);
    }
    // x-axis labels
    ctx.textAlign = "center";
    ctx.textBaseline = "top";
    const xTicks = this._xTicks(b);
    const fmt = pickTimeFormat(b.tMax - b.tMin);
    for (const t of xTicks) {
      const x = this._xScale(plot, t, b);
      ctx.fillText(fmt(new Date(t)), x, plot.y + plot.h + 6);
    }
  }

  _drawLegend(plot) {
    const ctx = this.ctx;
    ctx.font = `${this.opts.fontSize}px ${this.opts.fontFamily}`;
    let x = plot.x;
    const y = plot.y - 6;
    for (const s of this.series) {
      ctx.fillStyle = s.color;
      ctx.fillRect(x, y - 8, 9, 9);
      x += 13;
      ctx.fillStyle = this.opts.axisColor;
      ctx.textAlign = "left";
      ctx.textBaseline = "alphabetic";
      ctx.fillText(s.name, x, y);
      x += ctx.measureText(s.name).width + 16;
    }
  }

  _drawCrosshair(plot, b) {
    if (!this._mouse) return;
    const ctx = this.ctx;
    const mx = this._mouse.x, my = this._mouse.y;
    if (mx < plot.x || mx > plot.x + plot.w || my < plot.y || my > plot.y + plot.h) return;
    ctx.strokeStyle = "rgba(255,255,255,0.15)";
    ctx.setLineDash([3, 3]);
    ctx.beginPath();
    ctx.moveTo(mx, plot.y);
    ctx.lineTo(mx, plot.y + plot.h);
    ctx.stroke();
    ctx.setLineDash([]);

    // tooltip: find the closest point per series at this t
    const t = b.tMin + ((mx - plot.x) / plot.w) * (b.tMax - b.tMin);
    const lines = [pickTimeFormat(b.tMax - b.tMin)(new Date(t))];
    for (const s of this.series) {
      if (!s.points.length) continue;
      let nearest = s.points[0];
      let best = Math.abs(+nearest.t - t);
      for (const p of s.points) {
        const d = Math.abs(+p.t - t);
        if (d < best) { best = d; nearest = p; }
      }
      lines.push(`${s.name}: ${formatNumber(+nearest.v)}`);
    }
    drawTooltip(ctx, mx + 8, my + 8, lines, this.opts);
  }

  _yTicks(b) {
    const target = 5;
    const step = niceStep((b.vMax - b.vMin) / target);
    const start = Math.ceil(b.vMin / step) * step;
    const out = [];
    for (let v = start; v <= b.vMax; v += step) out.push(v);
    return out;
  }
  _xTicks(b) {
    const span = b.tMax - b.tMin;
    const target = 6;
    const step = niceTimeStep(span / target);
    const start = Math.ceil(b.tMin / step) * step;
    const out = [];
    for (let t = start; t <= b.tMax; t += step) out.push(t);
    return out;
  }

  _onMouseMove(e) {
    const rect = this.canvas.getBoundingClientRect();
    this._mouse = { x: e.clientX - rect.left, y: e.clientY - rect.top };
    this.render();
  }
  _toData(e) {
    const rect = this.canvas.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const b = this._bounds();
    const plot = this._plotRect(this.canvas.clientWidth, this.canvas.clientHeight);
    const t = b ? b.tMin + ((x - plot.x) / plot.w) * (b.tMax - b.tMin) : Date.now();
    return { t };
  }
}

// ── Helpers ───────────────────────────────────────────────────────
function niceStep(raw) {
  if (raw <= 0) return 1;
  const exp = Math.floor(Math.log10(raw));
  const base = Math.pow(10, exp);
  const f = raw / base;
  let nice;
  if (f < 1.5) nice = 1;
  else if (f < 3) nice = 2;
  else if (f < 7) nice = 5;
  else nice = 10;
  return nice * base;
}

function niceTimeStep(ms) {
  const steps = [
    1_000, 5_000, 10_000, 30_000,
    60_000, 5 * 60_000, 15 * 60_000, 30 * 60_000,
    3_600_000, 6 * 3_600_000, 12 * 3_600_000,
    86_400_000, 7 * 86_400_000, 30 * 86_400_000,
  ];
  for (const s of steps) if (s >= ms) return s;
  return 365 * 86_400_000;
}

function pickTimeFormat(span) {
  if (span < 60_000) return (d) => `${d.getMinutes()}:${pad(d.getSeconds())}`;
  if (span < 6 * 3_600_000) return (d) => `${pad(d.getHours())}:${pad(d.getMinutes())}`;
  if (span < 7 * 86_400_000) return (d) => `${d.getMonth() + 1}/${d.getDate()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  return (d) => `${d.getMonth() + 1}/${d.getDate()}`;
}
function pad(n) { return n < 10 ? "0" + n : "" + n; }

function formatNumber(v) {
  if (v === 0) return "0";
  const abs = Math.abs(v);
  if (abs >= 1e12) return (v / 1e12).toFixed(1) + "T";
  if (abs >= 1e9) return (v / 1e9).toFixed(1) + "G";
  if (abs >= 1e6) return (v / 1e6).toFixed(1) + "M";
  if (abs >= 1e3) return (v / 1e3).toFixed(1) + "k";
  if (abs >= 1) return (Math.round(v * 100) / 100).toString();
  if (abs >= 0.01) return v.toFixed(3);
  return v.toExponential(1);
}

function drawTooltip(ctx, x, y, lines, opts) {
  ctx.font = `${opts.fontSize}px ${opts.fontFamily}`;
  const padX = 8, padY = 6, lh = opts.fontSize + 4;
  let w = 0;
  for (const l of lines) w = Math.max(w, ctx.measureText(l).width);
  const h = padY * 2 + lh * lines.length;
  ctx.fillStyle = "rgba(15,20,28,0.92)";
  ctx.strokeStyle = "rgba(255,255,255,0.15)";
  ctx.fillRect(x, y, w + padX * 2, h);
  ctx.strokeRect(x + 0.5, y + 0.5, w + padX * 2, h);
  ctx.fillStyle = "#e6edf3";
  ctx.textAlign = "left";
  ctx.textBaseline = "middle";
  for (let i = 0; i < lines.length; i++) {
    ctx.fillText(lines[i], x + padX, y + padY + lh * (i + 0.5));
  }
}

window.ObservexChart = ObservexChart;
