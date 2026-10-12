#!/usr/bin/env python3
"""Generate the FileSync brand icon set from assets/brand/logo-master.webp.

Dev-time tool only (Python + Pillow, already on the dev machine).
The outputs are committed to the repo, so release builds never run
this script. Steps:

  1. Flood-fill from the image edges: near-white pixels connected to
     the border become transparent (interior white is untouched).
  2. Crop to the alpha bounding box, pad 6% per side, square canvas.
  3. Emit assets/brand/logo-256.png and a multi-size
     assets/brand/filesync.ico (16, 24, 32, 48, 64, 128, 256).

Self-checks at the end (assert -> non-zero exit): the ICO re-opens
with all 7 sizes, the PNG corner pixel is transparent, the center
pixel is not.
"""

from collections import deque
from pathlib import Path

from PIL import Image

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "assets" / "brand" / "logo-master.webp"
PNG_OUT = ROOT / "assets" / "brand" / "logo-256.png"
ICO_OUT = ROOT / "assets" / "brand" / "filesync.ico"
ICO_SIZES = [16, 24, 32, 48, 64, 128, 256]
NEAR_WHITE = 245  # all channels >= this count as background white


def knock_out_background(img: Image.Image) -> Image.Image:
    """Make border-connected near-white pixels transparent."""
    img = img.convert("RGBA")
    w, h = img.size
    px = img.load()
    seen = bytearray(w * h)
    queue = deque()

    def is_bg(x: int, y: int) -> bool:
        r, g, b, _ = px[x, y]
        return r >= NEAR_WHITE and g >= NEAR_WHITE and b >= NEAR_WHITE

    def seed(x: int, y: int) -> None:
        if not seen[y * w + x] and is_bg(x, y):
            seen[y * w + x] = 1
            queue.append((x, y))

    for x in range(w):
        seed(x, 0)
        seed(x, h - 1)
    for y in range(h):
        seed(0, y)
        seed(w - 1, y)
    while queue:
        x, y = queue.popleft()
        px[x, y] = (px[x, y][0], px[x, y][1], px[x, y][2], 0)
        for nx, ny in ((x - 1, y), (x + 1, y), (x, y - 1), (x, y + 1)):
            if 0 <= nx < w and 0 <= ny < h:
                seed(nx, ny)
    return img


def main() -> None:
    img = knock_out_background(Image.open(SRC))
    bbox = img.getbbox()
    assert bbox is not None, "logo became fully transparent"
    img = img.crop(bbox)
    # Pad 6% per side on a transparent square canvas.
    side = max(img.size)
    pad = round(side * 0.06)
    canvas_side = side + 2 * pad
    canvas = Image.new("RGBA", (canvas_side, canvas_side), (0, 0, 0, 0))
    canvas.paste(img, ((canvas_side - img.width) // 2, (canvas_side - img.height) // 2), img)

    logo256 = canvas.resize((256, 256), Image.LANCZOS)
    logo256.save(PNG_OUT)
    logo256.save(ICO_OUT, format="ICO", sizes=[(s, s) for s in ICO_SIZES])

    # --- self-checks -------------------------------------------------
    check = Image.open(PNG_OUT)
    assert check.getpixel((0, 0))[3] == 0, "PNG corner (0,0) is not transparent"
    assert check.getpixel((128, 128))[3] > 0, "PNG center is transparent"
    ico = Image.open(ICO_OUT)
    have = {s[0] for s in ico.info.get("sizes", set())}
    assert set(ICO_SIZES) <= have, f"ICO sizes missing: want {ICO_SIZES}, have {sorted(have)}"
    print(f"make-icons: wrote {PNG_OUT} and {ICO_OUT} (ICO sizes {sorted(have)})")


if __name__ == "__main__":
    main()
