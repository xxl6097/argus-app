#!/usr/bin/env python3
"""Mask MAC addresses in screenshots under docs/screenshots/.

For each PNG in the directory, run tesseract OCR to locate every
token matching a MAC pattern (XX:XX:XX:XX:XX:XX with any case / hex),
then apply a heavy Gaussian blur to that bounding box + a small
padding so the characters disappear but layout stays intact.

The original files are overwritten. A .bak copy is left next to each
so you can revert with `mv foo.png.bak foo.png` if needed.
"""
import re
import shutil
import sys
from pathlib import Path

from PIL import Image, ImageFilter
import pytesseract

MAC_RE = re.compile(r'^[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}$')
# OCR-tolerant variant: lowercase O is often misread for digit 0,
# and capital B for hex 8 inside Source Code Pro at small sizes.
# Same shape (12 hex chars in 6 colon-separated pairs), but the
# character class includes those visual look-alikes. Real timestamps
# (HH:MM:SS:HH:MM:SS faux-MACs) are filtered out by is_real_mac()
# requiring at least one A-F.
MAC_OCR_RE = re.compile(r'^[0-9A-FOoa-fb]{2}(?::[0-9A-FOoa-fb]{2}){5}$')
# Substring pattern: a MAC anywhere inside a longer OCR token. Catches
# tokens like "‘82:FB:8D:FB:B6:A6" where Tesseract glued punctuation
# to the start, or "EELGA:BF:2E:87:DF" where it misread a colon as
# extra characters. We extract just the MAC-shaped run from inside.
MAC_INSIDE_RE = re.compile(r'[0-9A-Fa-fOoBb]{2}(?::[0-9A-Fa-fOoBb]{2}){5}')
# Pad the bbox by this many pixels so surrounding glyphs (next column)
# stay untouched but the MAC text is fully covered.
PAD = 4

# Minimum fraction of a bbox's width that must be inside the image
# for us to trust it — filters out negative-x OCR artifacts.
MIN_INSIDE_FRAC = 0.5


def is_real_mac(s: str) -> bool:
    """Filter out timestamps like '07:52:47:20:06:14' that match the
    MAC grammar. Real MACs almost always contain at least one A-F hex
    digit; timestamps are 0-9 only."""
    if not (MAC_RE.match(s) or MAC_OCR_RE.match(s)):
        return False
    # Require at least one alphabetic char — distinguishes hex MAC
    # from a purely numeric timestamp.
    return any(c.isalpha() for c in s)


def extract_mac_inside(s: str) -> str:
    """If a longer OCR token contains a MAC-shaped substring, return
    just the MAC. Otherwise return ''."""
    m = MAC_INSIDE_RE.search(s)
    if m:
        candidate = m.group(0)
        if is_real_mac(candidate):
            return candidate
    # Tesseract sometimes reads ':' as 'L' or 'I' in narrow fonts. Try
    # substituting them and re-checking. Also substitute 'G' for '6'
    # (another common mis-read) — but only when at least one alpha
    # character remains so we don't accidentally match timestamps.
    for repl_src, repl_dst in [("L", ":"), ("I", ":"), ("T", ":"),
                               ("r", ":"), ("G", "6")]:
        if repl_src in s:
            patched = s.replace(repl_src, repl_dst)
            m = MAC_INSIDE_RE.search(patched)
            if m and is_real_mac(m.group(0)):
                return m.group(0)
    # Combined substitution: replace both L→: and G→6 simultaneously.
    # Catches tokens like "EELGABF:26:87:0F" where both errors occur.
    patched = s.replace("L", ":").replace("G", "6")
    m = MAC_INSIDE_RE.search(patched)
    if m and is_real_mac(m.group(0)):
        return m.group(0)
    return ""


def collect_hits(data):
    """Yield (x, y, w, h, text) for every token that looks like a MAC,
    plus second-pass joins of adjacent fragments on the same line."""
    n = len(data["text"])
    seen = set()
    # First pass: tokens that are themselves a full MAC (or contain
    # one with surrounding OCR noise).
    for i in range(n):
        txt = data["text"][i].strip()
        if is_real_mac(txt):
            yield (data["left"][i], data["top"][i],
                   data["width"][i], data["height"][i], txt)
            seen.add(i)
            continue
        if inside := extract_mac_inside(txt):
            yield (data["left"][i], data["top"][i],
                   data["width"][i], data["height"][i], inside)
            seen.add(i)
            continue
        # Last-resort heuristic: a token that (a) contains ≥4 colons,
        # (b) is 10-20 chars, (c) is ≥50% hex-ish characters. This
        # catches badly-noised MACs like "EELGABF:26:87:0F" that no
        # amount of character substitution recovers cleanly.
        colons = txt.count(":") + txt.count("L") + txt.count("I")
        if 4 <= colons <= 8 and 10 <= len(txt) <= 20:
            hex_ish = sum(1 for c in txt if c.upper() in "0123456789ABCDEFOILTG")
            if hex_ish >= len(txt) * 0.75:
                yield (data["left"][i], data["top"][i],
                       data["width"][i], data["height"][i], f"<noisy:{txt}>")
                seen.add(i)
    # Second pass: adjacent tokens on the same line that *together*
    # form a MAC (covers OCR splitting like "82:F8:8D" + "FB:B6:A6").
    # Requires the first token to already contain a colon, to avoid
    # accidentally fusing two HH:MM:SS timestamps in different columns.
    for i in range(n):
        if i in seen:
            continue
        seg = data["text"][i].strip()
        if not seg or ":" not in seg:
            continue
        for j in range(i + 1, min(i + 6, n)):
            if data["line_num"][j] != data["line_num"][i]:
                break
            seg = seg + data["text"][j].strip()
            if is_real_mac(seg) or extract_mac_inside(seg):
                x = data["left"][i]
                y = min(data["top"][k] for k in range(i, j + 1))
                right = data["left"][j] + data["width"][j]
                bottom = max(data["top"][k] + data["height"][k] for k in range(i, j + 1))
                yield (x, y, right - x, bottom - y, seg)
                for k in range(i, j + 1):
                    seen.add(k)
                break


def mask_macs(path: Path) -> int:
    # Restore from backup if present so re-runs are idempotent.
    backup = path.with_suffix(path.suffix + ".bak")
    if backup.exists():
        shutil.copy2(backup, path)
    im = Image.open(path).convert("RGB")

    all_hits = []
    # Run OCR in multiple modes and union results — different PSMs
    # catch different layouts (table cells vs sparse text).
    for psm in ("6", "11", "12"):
        data = pytesseract.image_to_data(
            im,
            config=f"--psm {psm} -c tessedit_char_whitelist=0123456789ABCDEFabcdef:",
            output_type=pytesseract.Output.DICT,
        )
        for hit in collect_hits(data):
            all_hits.append(hit)
    # Also a relaxed run (no whitelist) to catch table cells where the
    # surrounding glyphs distract whitelisted OCR.
    data = pytesseract.image_to_data(im, config="--psm 6", output_type=pytesseract.Output.DICT)
    for hit in collect_hits(data):
        all_hits.append(hit)

    # Dedupe overlapping bboxes (same MAC found by multiple PSMs).
    # Also drop bboxes whose bulk sits outside the image (OCR noise).
    unique = []
    for x, y, w, h, txt in all_hits:
        if x < 0 or y < 0 or x + w > im.width + 4 or y + h > im.height + 4:
            # Severely clipped — likely OCR noise on a border.
            continue
        for ux, uy, uw, uh, _ in unique:
            if abs(x - ux) < 8 and abs(y - uy) < 8:
                break
        else:
            unique.append((x, y, w, h, txt))

    if not unique:
        return 0

    # Back up original on first run.
    if not backup.exists():
        shutil.copy2(path, backup)

    # Apply a strong blur to each bbox. Blur preserves the cell's
    # background color better than a flat rectangle.
    for x, y, w, h, txt in unique:
        box = (
            max(0, x - PAD),
            max(0, y - PAD),
            min(im.width, x + w + PAD),
            min(im.height, y + h + PAD),
        )
        region = im.crop(box).filter(ImageFilter.GaussianBlur(radius=6))
        im.paste(region, box)
        print(f"  masked {txt!r} at {box}")

    im.save(path, optimize=True)
    return len(unique)


def main():
    # Script lives in docs/screenshots/ and operates on the same dir.
    root = Path(__file__).resolve().parent
    if not root.is_dir():
        print(f"not a directory: {root}", file=sys.stderr)
        return 1
    total = 0
    for p in sorted(root.glob("*.png")):
        n = mask_macs(p)
        print(f"{p.name}: masked {n} MAC(s)")
        total += n
    print(f"total masked: {total}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
