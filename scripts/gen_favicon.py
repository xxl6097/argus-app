#!/usr/bin/env python3
"""Generate favicon.ico for the WiFi 考勤 dashboard.

Logo concept: a WiFi signal arc (top half) over a clock dial (bottom),
matching the dashboard's accent color (--accent: #4ec9b0). Renders at
multiple sizes so browsers / OS favicon caches pick the right one.
"""
from PIL import Image, ImageDraw
import math
from pathlib import Path

# Match dashboard.html's CSS variables.
ACCENT = "#4ec9b0"
BG     = "#0e1014"
MUTED  = "#8a8f98"


def render(size: int) -> Image.Image:
    """Render the logo at `size` pixels square. Drawn at 4× then
    downsampled with LANCZOS so curves stay crisp at small sizes."""
    scale = 4
    s = size * scale
    im = Image.new("RGBA", (s, s), BG)
    d = ImageDraw.Draw(im)

    # WiFi-signal arcs in the top portion (3 concentric arcs + dot).
    cx = s // 2
    cy = int(s * 0.62)  # arc origin sits below visible area
    arc_color = ACCENT
    line_w = max(2, int(s * 0.08))

    # Three arcs, increasing radius.
    for i, r_frac in enumerate([0.18, 0.32, 0.46]):
        r = int(s * r_frac)
        bbox = (cx - r, cy - r, cx + r, cy + r)
        # Arc spans -135° to -45° (top wedge).
        d.arc(bbox, start=-135, end=-45, fill=arc_color, width=line_w)

    # Center dot (signal source).
    dot_r = int(s * 0.05)
    d.ellipse((cx - dot_r, cy - dot_r, cx + dot_r, cy + dot_r), fill=arc_color)

    # Clock face below — circle with two hands at "8:30" (pointing
    # to the workday boundary feeling).
    clock_cx = cx
    clock_cy = int(s * 0.78)
    clock_r = int(s * 0.18)
    d.ellipse(
        (clock_cx - clock_r, clock_cy - clock_r,
         clock_cx + clock_r, clock_cy + clock_r),
        outline=arc_color, width=max(2, int(s * 0.025)),
    )
    # Hour hand: 8 o'clock direction (8 of 12 = 240° measured clockwise
    # from 12; in screen coords, 12 = -90° (up), so 8 = -90 + 8/12*360 = 150°).
    def hand(angle_deg: float, length_frac: float, width: int):
        rad = math.radians(angle_deg)
        x2 = clock_cx + int(clock_r * length_frac * math.cos(rad))
        y2 = clock_cy + int(clock_r * length_frac * math.sin(rad))
        d.line((clock_cx, clock_cy, x2, y2), fill=arc_color, width=width)
    # Hour hand pointing roughly to "9" (270°-ish in math coords = -90+360*9/12 = 180°).
    # Keep simple: 9 o'clock = pointing left, 12 = up.
    hand(180, 0.55, max(2, int(s * 0.03)))   # 9 o'clock (hour)
    hand(-90, 0.80, max(2, int(s * 0.025)))  # 12 (minute)

    return im.resize((size, size), Image.LANCZOS)


def main():
    sizes = [16, 32, 48, 64, 128, 256]
    images = [render(sz) for sz in sizes]
    out_dir = Path(__file__).resolve().parent.parent / "interval" / "web" / "assets"
    out_dir.mkdir(parents=True, exist_ok=True)
    ico_path = out_dir / "favicon.ico"
    # Pillow accepts a list of images via append_images for ICO.
    images[0].save(
        ico_path,
        format="ICO",
        sizes=[(sz, sz) for sz in sizes],
        append_images=images[1:],
    )
    # Also export a 256×256 PNG for README / docs reuse.
    images[-1].save(out_dir / "favicon-256.png")
    print(f"wrote {ico_path} ({sizes})")


if __name__ == "__main__":
    main()
