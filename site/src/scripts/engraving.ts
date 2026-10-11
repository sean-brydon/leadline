// The engraving as loose dots. Every dot of the dithered picture is a
// particle on a spring to its own place: the pointer pushes dots away and
// they spring back.
//
// On load the sailor makes his cast: the line and the lead start in his hand
// and fly out to where the engraving has them, nearest first.
//
// It draws at the picture's own size, one canvas pixel a dot, and CSS scales
// the canvas up with hard pixels, exactly as it does the still <img>. At
// rest the canvas is that picture dot for dot, and nothing runs until the
// pointer moves again.

/** The part of the picture that is cast, and the hand it leaves, in dots. */
const CAST = { left: 100, right: 282, top: 60, bottom: 180, hand: { x: 284, y: 80 } };
const CAST_AFTER_MS = 250;
const CAST_TAKES_MS = 520;

// A dot's spring to its place, a little underdamped so it lands with a bounce.
const SPRING = 90;
const DRAG = 13;
// The pointer, in dots.
const REACH = 36;
const SHOVE = 2400;

/**
 * engrave turns the loaded picture into dots on the canvas and returns a
 * function that stops it. The caller swaps the canvas in for the image.
 */
export function engrave(img: HTMLImageElement, canvas: HTMLCanvasElement): () => void {
  const w = img.naturalWidth;
  const h = img.naturalHeight;
  canvas.width = w;
  canvas.height = h;
  const ctx = canvas.getContext("2d")!;
  ctx.drawImage(img, 0, 0);
  const source = new Uint32Array(ctx.getImageData(0, 0, w, h).data.buffer);

  // Every pixel that isn't clear is a dot.
  let n = 0;
  for (let i = 0; i < source.length; i++) if (source[i] >>> 24) n++;
  const hx = new Float32Array(n);
  const hy = new Float32Array(n);
  const px = new Float32Array(n);
  const py = new Float32Array(n);
  const vx = new Float32Array(n);
  const vy = new Float32Array(n);
  const ink = new Uint32Array(n);
  const born = new Float64Array(n); // when the dot first shows, in ms
  const home = new Uint8Array(n); // 1 while the dot sits in its place

  const start = performance.now();
  const reach = Math.hypot(CAST.hand.x - CAST.left, CAST.bottom - CAST.hand.y);
  for (let i = 0, k = 0; i < source.length; i++) {
    if (!(source[i] >>> 24)) continue;
    const x = i % w;
    const y = (i - x) / w;
    hx[k] = px[k] = x;
    hy[k] = py[k] = y;
    ink[k] = source[i];
    home[k] = 1;
    if (x >= CAST.left && x < CAST.right && y >= CAST.top && y < CAST.bottom) {
      // The further along the line, the later it leaves the hand.
      const far = Math.hypot(x - CAST.hand.x, y - CAST.hand.y) / reach;
      born[k] = start + CAST_AFTER_MS + far * CAST_TAKES_MS;
      px[k] = CAST.hand.x + (Math.random() - 0.5) * 3;
      py[k] = CAST.hand.y + (Math.random() - 0.5) * 3;
      home[k] = 0;
    }
    k++;
  }

  const frame = ctx.createImageData(w, h);
  const out = new Uint32Array(frame.data.buffer);
  let mx = 1e6;
  let my = 1e6;
  let raf = 0;
  let last = 0;
  let quietFor = 0;

  // step moves the dots on and reports how many are still on their way.
  const step = (dt: number, now: number): number => {
    const reach2 = REACH * REACH;
    let busy = 0;
    for (let i = 0; i < n; i++) {
      if (born[i] > now) {
        busy++;
        continue;
      }
      const dx = px[i] - mx;
      const dy = py[i] - my;
      const d2 = dx * dx + dy * dy;
      if (home[i] && d2 >= reach2) continue;
      let fx = SPRING * (hx[i] - px[i]) - DRAG * vx[i];
      let fy = SPRING * (hy[i] - py[i]) - DRAG * vy[i];
      if (d2 < reach2) {
        const d = Math.sqrt(d2) || 0.001;
        const push = (1 - d / REACH) ** 2 * SHOVE;
        fx += (dx / d) * push;
        fy += (dy / d) * push;
      }
      vx[i] += fx * dt;
      vy[i] += fy * dt;
      px[i] += vx[i] * dt;
      py[i] += vy[i] * dt;
      const speed = Math.abs(vx[i]) + Math.abs(vy[i]);
      if (d2 >= reach2 && speed < 0.5 && Math.abs(hx[i] - px[i]) + Math.abs(hy[i] - py[i]) < 0.05) {
        px[i] = hx[i];
        py[i] = hy[i];
        vx[i] = vy[i] = 0;
        home[i] = 1;
      } else {
        home[i] = 0;
        // A dot held off its place by a still pointer isn't on its way anywhere.
        if (speed >= 0.5) busy++;
      }
    }
    return busy;
  };

  const draw = (now: number) => {
    out.fill(0);
    for (let i = 0; i < n; i++) {
      if (born[i] > now) continue;
      const x = (px[i] + 0.5) | 0;
      const y = (py[i] + 0.5) | 0;
      if (x >= 0 && x < w && y >= 0 && y < h) out[y * w + x] = ink[i];
    }
    ctx.putImageData(frame, 0, 0);
  };

  const tick = (now: number) => {
    raf = 0;
    const dt = Math.min((now - last) / 1000, 1 / 30);
    last = now;
    // Two half steps keep the stiff springs calm at 60 frames a second.
    step(dt / 2, now);
    const busy = step(dt / 2, now);
    draw(now);
    quietFor = busy ? 0 : quietFor + dt;
    if (quietFor < 0.3) raf = requestAnimationFrame(tick);
  };
  const wake = () => {
    quietFor = 0;
    if (raf) return;
    last = performance.now();
    raf = requestAnimationFrame(tick);
  };

  const onMove = (e: PointerEvent) => {
    const box = canvas.getBoundingClientRect();
    mx = ((e.clientX - box.left) / box.width) * w;
    my = ((e.clientY - box.top) / box.height) * h;
    // Far from the picture there is nothing to push.
    if (mx > -REACH && mx < w + REACH && my > -REACH && my < h + REACH) wake();
  };
  const onLeave = () => {
    mx = my = 1e6;
    wake();
  };
  window.addEventListener("pointermove", onMove, { passive: true });
  window.addEventListener("pointerdown", onMove, { passive: true });
  document.documentElement.addEventListener("pointerleave", onLeave);
  window.addEventListener("blur", onLeave);

  draw(start);
  wake();

  return () => {
    cancelAnimationFrame(raf);
    window.removeEventListener("pointermove", onMove);
    window.removeEventListener("pointerdown", onMove);
    document.documentElement.removeEventListener("pointerleave", onLeave);
    window.removeEventListener("blur", onLeave);
  };
}
