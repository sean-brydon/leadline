// The lead weight as loose dots on a canvas. Every dot of the dithered
// picture is a particle on a spring to its place on the weight, and the
// weight hangs from a rope tied off above the frame: a damped pendulum.
//
//   - On load it is thrown in: the pendulum starts out past the right edge
//     and swings to rest, its dots trailing behind and catching up.
//   - The pointer pushes dots away; they spring back. Pushing through the
//     weight also sets it swinging.
//
// Dots are drawn snapped to the picture's own grid, so at rest the canvas is
// the dithered picture exactly, and in motion it stays hard pixels.
//
// plumb(canvas, data, options) starts it and returns a function that stops
// it. data is plumb-data.json: { w, h, cx, anchorY, tones, dots: [x, y, tone, ...] }
// in picture cells. options: x (where the rope hangs, as a fraction of the
// canvas width, default 0.72), intro (default true).
function plumb(canvas, data, options = {}) {
  const ctx = canvas.getContext("2d");
  const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
  const hangAt = options.x ?? 0.72;

  // Dots grouped by tone, so each tone is drawn with one fill.
  const order = [];
  for (let i = 0; i < data.dots.length; i += 3) order.push(i);
  order.sort((a, b) => data.dots[a + 2] - data.dots[b + 2]);
  const n = order.length;
  const toneEnd = data.tones.map((_, t) => order.filter((i) => data.dots[i + 2] <= t).length);

  // Home places, in cells, measured from the point the rope is tied to.
  const hx = new Float32Array(n);
  const hy = new Float32Array(n);
  order.forEach((src, i) => {
    hx[i] = data.dots[src] + 0.5 - data.cx;
    hy[i] = data.dots[src + 1] + 0.5 - data.anchorY;
  });
  const px = new Float32Array(n);
  const py = new Float32Array(n);
  const vx = new Float32Array(n);
  const vy = new Float32Array(n);

  // The pendulum. Slow and heavy: a period of about 2.6 seconds.
  const SWING = 2.4; // rad/s, the pendulum's natural frequency
  const SWING_DRAG = 1.2;
  const THROWN_FROM = -0.5; // rad: out past the right edge
  const THROWN_AT = 0.2; // rad/s, already moving in
  // Each dot's spring to its place on the weight.
  const SPRING = 90;
  const DRAG = 11;
  // The pointer, in cells.
  const REACH = 26;
  const SHOVE = 2600;
  const NUDGE = 0.0003; // how much shoving the dots turns the pendulum

  let theta = still || options.intro === false ? 0 : THROWN_FROM;
  let omega = still || options.intro === false ? 0 : THROWN_AT;
  let cell = 4;
  let originX = 0;
  let originY = 0;
  let mx = 1e6;
  let my = 1e6;
  let frame = 0;
  let last = 0;
  let quietFor = 0;

  const place = () => {
    const c = Math.cos(theta);
    const s = Math.sin(theta);
    for (let i = 0; i < n; i++) {
      px[i] = hx[i] * c - hy[i] * s;
      py[i] = hx[i] * s + hy[i] * c;
      vx[i] = -py[i] * omega;
      vy[i] = px[i] * omega;
    }
  };
  place();
  if (theta !== 0) {
    // Thrown: the dots start strung out behind the weight, the way spray
    // trails a cast line, and catch up as it swings in.
    for (let i = 0; i < n; i++) {
      const lag = Math.random() ** 2 * 70;
      px[i] += lag;
      py[i] += (Math.random() - 0.5) * 18 - lag * 0.25;
    }
  }

  const resize = () => {
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const w = canvas.clientWidth;
    const h = canvas.clientHeight;
    canvas.width = Math.round(w * dpr);
    canvas.height = Math.round(h * dpr);
    // The picture is as tall as the canvas, in whole pixels per cell.
    cell = Math.max(2, Math.round(h / data.h));
    originX = Math.round((w * hangAt) / cell) * cell;
    originY = data.anchorY * cell;
    ctx.setTransform(dpr * cell, 0, 0, dpr * cell, originX * dpr, originY * dpr);
    wake();
  };

  const step = (dt) => {
    const c = Math.cos(theta);
    const s = Math.sin(theta);
    const reach2 = REACH * REACH;
    let turn = 0;
    let moving = 0;
    for (let i = 0; i < n; i++) {
      const tx = hx[i] * c - hy[i] * s;
      const ty = hx[i] * s + hy[i] * c;
      // Drag is against the dot's place moving, not against the canvas: a
      // dot riding the swing isn't held back by it.
      let fx = SPRING * (tx - px[i]) - DRAG * (vx[i] + ty * omega);
      let fy = SPRING * (ty - py[i]) - DRAG * (vy[i] - tx * omega);
      const dx = px[i] - mx;
      const dy = py[i] - my;
      const d2 = dx * dx + dy * dy;
      if (d2 < reach2) {
        const d = Math.sqrt(d2) || 0.001;
        const push = (1 - d / REACH) ** 2 * SHOVE;
        fx += (dx / d) * push;
        fy += (dy / d) * push;
        turn += (dx / d) * push * ty;
      }
      vx[i] += fx * dt;
      vy[i] += fy * dt;
      px[i] += vx[i] * dt;
      py[i] += vy[i] * dt;
      moving = Math.max(moving, Math.abs(tx - px[i]) + Math.abs(ty - py[i]));
    }
    // The last sliver of a swing would take many seconds to die away and
    // can't be seen: it is damped harder so the weight comes to rest.
    const drag = Math.abs(theta) < 0.02 && Math.abs(omega) < 0.05 ? SWING_DRAG * 4 : SWING_DRAG;
    // A shove to the right swings the weight right, which is theta going down.
    omega += (-SWING * SWING * Math.sin(theta) - drag * omega - (turn / n) * NUDGE) * dt;
    theta += omega * dt;
    return moving;
  };

  const draw = () => {
    ctx.save();
    ctx.setTransform(1, 0, 0, 1, 0, 0);
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.restore();
    let i = 0;
    data.tones.forEach((tone, t) => {
      ctx.fillStyle = tone;
      for (; i < toneEnd[t]; i++) ctx.fillRect(Math.floor(px[i]), Math.floor(py[i]), 1, 1);
    });
  };

  const tick = (now) => {
    frame = 0;
    const dt = Math.min((now - last) / 1000, 1 / 30);
    last = now;
    // Two half steps keep the stiff springs calm at 60 frames a second.
    const moving = Math.max(step(dt / 2), step(dt / 2));
    draw();
    // Once everything has come to rest and the pointer is away, stop
    // drawing until something moves again.
    const resting = moving < 0.05 && Math.abs(theta) < 0.002 && Math.abs(omega) < 0.004;
    quietFor = resting ? quietFor + dt : 0;
    if (quietFor < 0.4) frame = requestAnimationFrame(tick);
  };
  const wake = () => {
    quietFor = 0;
    if (still) return draw();
    if (!frame) {
      last = performance.now();
      frame = requestAnimationFrame(tick);
    }
  };

  const onMove = (e) => {
    const box = canvas.getBoundingClientRect();
    mx = (e.clientX - box.left - originX) / cell;
    my = (e.clientY - box.top - originY) / cell;
    wake();
  };
  const onLeave = () => {
    mx = my = 1e6;
  };
  const watcher = new ResizeObserver(resize);
  watcher.observe(canvas);
  if (!still) {
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerdown", onMove);
    document.addEventListener("pointerleave", onLeave);
    window.addEventListener("blur", onLeave);
  }
  resize();

  return () => {
    cancelAnimationFrame(frame);
    watcher.disconnect();
    window.removeEventListener("pointermove", onMove);
    window.removeEventListener("pointerdown", onMove);
    document.removeEventListener("pointerleave", onLeave);
    window.removeEventListener("blur", onLeave);
  };
}
