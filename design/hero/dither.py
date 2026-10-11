"""Ordered (Bayer) dither of a greyscale image onto a small palette, one
output pixel per dot. The page scales it up with hard pixels.

  dither.py in.png out.png --w 480 --h 300 --palette 0b1220,134e4a,2dd4bf
      [--fade left|right|top|bottom|none ...] [--gamma 1.0] [--contrast 1.0]
      [--invert] [--crop cx,cy] [--transparent-first]

The palette runs dark to light along the image's brightness. With
--transparent-first the first colour is left clear, so the page shows through.
A fade dissolves the picture dot by dot toward that edge.
"""
import argparse
import numpy as np
from PIL import Image, ImageOps

p = argparse.ArgumentParser()
p.add_argument("src"); p.add_argument("dst")
p.add_argument("--w", type=int, required=True); p.add_argument("--h", type=int, required=True)
p.add_argument("--palette", required=True)
p.add_argument("--fade", action="append", default=[])   # edge[:start:end] as fractions
p.add_argument("--gamma", type=float, default=1.0)
p.add_argument("--contrast", type=float, default=1.0)
p.add_argument("--brightness", type=float, default=0.0)
p.add_argument("--invert", action="store_true")
p.add_argument("--crop", default="0.5,0.5")
p.add_argument("--transparent-first", action="store_true")
p.add_argument("--bayer", type=int, default=8)
p.add_argument("--clear", action="append", default=[])  # cx,cy,rx,ry as fractions: the picture dissolves inside
a = p.parse_args()

def bayer(n):
    m = np.array([[0]])
    while m.shape[0] < n:
        m = np.block([[4 * m, 4 * m + 2], [4 * m + 3, 4 * m + 1]])
    return (m + 0.5) / (n * n)

cx, cy = map(float, a.crop.split(","))
img = ImageOps.fit(Image.open(a.src).convert("L"), (a.w, a.h), Image.LANCZOS, centering=(cx, cy))
v = np.asarray(img, dtype=np.float64) / 255.0
if a.invert:
    v = 1 - v
v = np.clip((v - 0.5) * a.contrast + 0.5 + a.brightness, 0, 1) ** a.gamma

pal = [tuple(int(c[i:i + 2], 16) for i in (0, 2, 4)) for c in a.palette.split(",")]
n = len(pal)
t = np.tile(bayer(a.bayer), (a.h // a.bayer + 1, a.w // a.bayer + 1))[: a.h, : a.w]
# Each pixel sits between two palette steps; the threshold picks one.
scaled = v * (n - 1)
idx = np.clip(np.floor(scaled) + ((scaled - np.floor(scaled)) > t), 0, n - 1).astype(int)

keep = np.ones((a.h, a.w))
ys, xs = np.mgrid[0 : a.h, 0 : a.w]
for f in a.fade:
    parts = f.split(":")
    edge = parts[0]
    start = float(parts[1]) if len(parts) > 1 else 0.0   # fully gone here
    end = float(parts[2]) if len(parts) > 2 else 0.45    # fully kept from here
    d = {"left": xs / a.w, "right": 1 - xs / a.w, "top": ys / a.h, "bottom": 1 - ys / a.h}[edge]
    keep = np.minimum(keep, np.clip((d - start) / max(end - start, 1e-6), 0, 1))
for c in a.clear:
    ex, ey, rx, ry = map(float, c.split(","))
    d = np.sqrt(((xs / a.w - ex) / rx) ** 2 + ((ys / a.h - ey) / ry) ** 2)
    keep = np.minimum(keep, np.clip((d - 0.7) / 0.5, 0, 1))
# A second, offset threshold so the dissolve doesn't line up with the tones.
t2 = np.roll(t, (3, 5), axis=(0, 1))
shown = keep > t2

out = np.zeros((a.h, a.w, 4), dtype=np.uint8)
for i, c in enumerate(pal):
    out[idx == i] = (*c, 255)
if a.transparent_first:
    out[idx == 0, 3] = 0
out[~shown, 3] = 0
Image.fromarray(out, "RGBA").save(a.dst, optimize=True)
print(a.dst, out.shape, f"{(out[..., 3] > 0).mean():.0%} drawn")
