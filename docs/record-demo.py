#!/usr/bin/env python3
"""Record a real puffy run and turn it into docs/demo.gif.

    python3 docs/record-demo.py [target] [rounds]

The recording is genuine: puffy runs under a real pty, so it takes its live
code path and paints the same frames a person would see. Frames are recoverable
from the byte stream because Screen.Frame() begins every repaint with a
cursor-home (ESC[H) and clears each line as it writes it.

Pipeline: pty capture -> split frames -> ANSI to HTML -> Chrome screenshots ->
slice -> ImageMagick assembles the GIF -> verify it coalesces back.

Needs: Google Chrome (headless renderer) and ImageMagick 7 (`magick`). Both are
only needed to regenerate the GIF, never to build or run puffy.
"""
import fcntl
import html
import os
import pty
import re
import select
import struct
import subprocess
import sys
import termios
import time

TARGET = sys.argv[1] if len(sys.argv) > 1 else "1.1.1.1"
ROUNDS = sys.argv[2] if len(sys.argv) > 2 else "26"
INTERVAL = "400ms"

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
WORK = os.path.join(HERE, ".build")
OUT = os.path.join(HERE, "demo.gif")
BIN = os.path.join(ROOT, "puffy")
COMMAND = f"puffy trace {TARGET} --interval {INTERVAL} --no-dns"

# The pty is sized so the live view can draw every hop in both heatmap panels
# without eliding any, and so the frame is not mostly empty next to the taller
# closing summary. A 9-hop path needs about this much room.
COLS, ROWS = 104, 38
FONT_PX, LINE_PX, PAD_PX = 15, 21, 18
FRAME_H = ROWS * LINE_PX + 2 * PAD_PX  # recomputed once the frames are known
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

SURFACE = "#1a1a19"     # the dark chart surface the palette is stepped for
DEFAULT_FG = "#c3c2b7"  # secondary ink: what uncoloured terminal text reads as

SGR = re.compile(rb"\x1b\[([0-9;?]*)([a-zA-Z])")


def record() -> bytes:
    """Run puffy under a pty and return everything it painted."""
    pid, fd = pty.fork()
    if pid == 0:
        os.environ["COLORTERM"] = "truecolor"
        os.environ["TERM"] = "xterm-256color"
        # --no-dns keeps ISP hostnames out of the frame. Note what this does
        # NOT do: the addresses are still in the picture, and anyone can reverse
        # them back to the same names. It reduces what the recording states
        # outright, not what can be inferred from it. To publish a demo that
        # reveals nothing about the recording network, trace a target whose
        # path you do not mind exposing, or raise --first-ttl past your own
        # access segment.
        os.execvp(BIN, [BIN, "trace", TARGET, "--count", ROUNDS,
                        "--interval", INTERVAL, "--no-dns"])

    # Size the pty so puffy lays out for a known width.
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", ROWS, COLS, 0, 0))

    chunks = []
    while True:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            if not data:
                break
            chunks.append(data)
        if os.waitpid(pid, os.WNOHANG)[0] != 0:
            try:
                while True:
                    data = os.read(fd, 65536)
                    if not data:
                        break
                    chunks.append(data)
            except OSError:
                pass
            break
    return b"".join(chunks)

def split_frames(raw: bytes):
    """Return (alt_frames, tail) - repaints inside the alternate screen, and
    whatever was printed on the normal screen afterwards."""
    start = raw.find(b"\x1b[?1049h")
    end = raw.find(b"\x1b[?1049l")
    alt = raw[start:end] if start >= 0 and end > start else raw
    tail = raw[end + len(b"\x1b[?1049l"):] if end > 0 else b""

    parts = alt.split(b"\x1b[H")[1:]  # first chunk is just the screen setup
    return parts, tail


def to_html(chunk: bytes) -> str:
    """Convert one frame's bytes into HTML, honouring the SGR codes puffy emits
    (truecolor and 256-colour foregrounds, bold on/off, reset)."""
    out, colour, bold, open_span = [], None, False, False

    def close():
        nonlocal open_span
        if open_span:
            out.append("</span>")
            open_span = False

    def open_style():
        nonlocal open_span
        style = []
        if colour:
            style.append(f"color:{colour}")
        if bold:
            style.append("font-weight:700")
        if style:
            out.append(f'<span style="{";".join(style)}">')
            open_span = True

    i = 0
    while i < len(chunk):
        m = SGR.match(chunk, i)
        if m:
            # Both groups must be decoded: m.group(2) is bytes, and comparing
            # b"m" to "m" is silently False, which drops every colour.
            params, final = m.group(1).decode(), m.group(2).decode()
            if final == "m":
                close()
                codes = [c for c in params.split(";") if c != ""] or ["0"]
                j = 0
                while j < len(codes):
                    c = codes[j]
                    if c == "0":
                        colour, bold = None, False
                    elif c == "1":
                        bold = True
                    elif c == "22":
                        bold = False
                    elif c == "38" and j + 1 < len(codes):
                        if codes[j + 1] == "2" and j + 4 < len(codes):
                            r, g, b = codes[j + 2:j + 5]
                            colour = f"rgb({r},{g},{b})"
                            j += 4
                        elif codes[j + 1] == "5" and j + 2 < len(codes):
                            colour = xterm(int(codes[j + 2]))
                            j += 2
                    j += 1
                open_style()
            # Every other final byte (K, H, ?25l, ...) is cursor/erase control
            # with no visual weight once the frame is laid out as text.
            i = m.end()
            continue

        ch = chunk[i:i + 1]
        if ch == b"\r":
            i += 1
            continue
        if ch == b"\n":
            close()
            out.append("\n")
            i += 1
            open_style()
            continue
        # Consume a whole UTF-8 rune.
        n = 1
        b0 = chunk[i]
        if b0 >= 0xF0:
            n = 4
        elif b0 >= 0xE0:
            n = 3
        elif b0 >= 0xC0:
            n = 2
        out.append(html.escape(chunk[i:i + n].decode("utf8", "replace")))
        i += n
    close()
    return "".join(out)


def xterm(n: int) -> str:
    if n < 16:
        base = [(0, 0, 0), (128, 0, 0), (0, 128, 0), (128, 128, 0), (0, 0, 128),
                (128, 0, 128), (0, 128, 128), (192, 192, 192), (128, 128, 128),
                (255, 0, 0), (0, 255, 0), (255, 255, 0), (0, 0, 255),
                (255, 0, 255), (0, 255, 255), (255, 255, 255)][n]
        return "rgb(%d,%d,%d)" % base
    if n < 232:
        n -= 16
        lv = [0, 95, 135, 175, 215, 255]
        return "rgb(%d,%d,%d)" % (lv[n // 36], lv[(n // 6) % 6], lv[n % 6])
    v = 8 + (n - 232) * 10
    return f"rgb({v},{v},{v})"


def dedupe(frames):
    """Drop repaints that changed nothing: they cost GIF bytes and show nothing."""
    out = []
    for f in frames:
        if not out or f != out[-1]:
            out.append(f)
    return out


def main():
    os.makedirs(WORK, exist_ok=True)
    os.chdir(WORK)
    print(f"recording {COMMAND} for {ROUNDS} rounds…")
    raw = record()
    open("capture.bin", "wb").write(raw)
    print(f"captured {len(raw)} bytes")
    alt, tail = split_frames(raw)
    print(f"{len(alt)} repaints, {len(tail)} bytes of summary")

    live = dedupe([to_html(f) for f in alt])
    # The first repaints are the "discovering the path" state; keep one.
    while len(live) > 1 and "discovering" in live[1]:
        live.pop(0)
    print(f"{len(live)} distinct live frames")

    summary = to_html(tail).strip("\n")

    # A typed prompt opens the animation, and the summary closes it. Holding a
    # frame is just repeating it, since the GIF delay is uniform.
    prompt = (f'<span style="color:#0ca30c">$</span> '
              f'<span style="color:#ffffff">{html.escape(COMMAND)}</span>')
    frames = [prompt] + live + [summary]
    # Centiseconds per frame. The recording redrew every 200ms, so 18 is close
    # to real time; the first and last frames are held long enough to read.
    delays = [110] + [18] * len(live) + [450]

    # Crop to the tallest frame instead of the pty's 30 rows. The live view only
    # fills about 22 of them, and half an empty terminal is half the GIF.
    global FRAME_H
    used = max(f.count("\n") for f in frames) + 1
    FRAME_H = used * LINE_PX + 2 * PAD_PX
    print(f"tallest frame is {used} lines -> {FRAME_H}px")

    os.makedirs("frames", exist_ok=True)
    for f in os.listdir("frames"):
        os.remove(os.path.join("frames", f))

    width = 1000
    # Frames are rendered in batches. A single tall strip is faster but Chrome
    # silently stops painting past roughly 16k device pixels and returns the
    # rest as flat background, which slices into blank frames.
    BATCH = 8
    idx = 0
    for start in range(0, len(frames), BATCH):
        batch = frames[start:start + BATCH]
        body = "".join(f'<div class="f"><pre>{f}</pre></div>' for f in batch)
        page = f"""<!DOCTYPE html><html><head><meta charset="utf-8"><style>
* {{ margin:0; padding:0; box-sizing:border-box; }}
body {{ background:{SURFACE}; }}
.f {{
  width:100%; height:{FRAME_H}px; padding:{PAD_PX}px;
  background:{SURFACE}; overflow:hidden;
}}
pre {{
  font: {FONT_PX}px/{LINE_PX}px Menlo, "SF Mono", Monaco, monospace;
  color:{DEFAULT_FG};
  white-space:pre; font-variant-ligatures:none;
  -webkit-font-smoothing:antialiased;
}}
</style></head><body>{body}</body></html>"""
        open("frames.html", "w").write(page)

        total = FRAME_H * len(batch)
        subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--hide-scrollbars",
                        "--force-device-scale-factor=2", "--virtual-time-budget=4000",
                        f"--window-size={width},{total}",
                        "--screenshot=strip.png",
                        "file://" + os.path.abspath("frames.html")],
                       check=True, capture_output=True)
        # The strip is rendered at 2x for crisp text; slice at that scale.
        subprocess.run(["magick", "strip.png", "-crop", f"{width*2}x{FRAME_H*2}",
                        "+repage", f"frames/f-{idx:04d}-%02d.png"], check=True)
        idx += 1
        print(f"  rendered {start + len(batch)}/{len(frames)}")

    names = sorted(os.listdir("frames"))
    n = len(names)
    if n != len(frames):
        sys.exit(f"sliced {n} images but expected {len(frames)}")

    # A frame that came back flat means Chrome gave up on that batch. Catch it
    # here: a blank frame is invisible in a summary line but obvious in the GIF.
    blank = []
    for name in names:
        sd = subprocess.run(["magick", f"frames/{name}", "-format",
                             "%[fx:standard_deviation]", "info:"],
                            capture_output=True, text=True, check=True).stdout
        if float(sd) < 0.01:
            blank.append(name)
    if blank:
        sys.exit(f"{len(blank)} frames rendered blank, first is {blank[0]}")
    print(f"sliced {n} images, none blank")

    # Each input gets its own -delay, placed before it. No -fuzz: it discards
    # frames whose change is small in area, which is exactly what a single
    # sparkline cell flipping to red looks like.
    cmd = ["magick", "-loop", "0"]
    for name, d in zip(names, delays):
        cmd += ["-delay", str(d), f"frames/{name}"]
    # -layers Optimize only. Adding OptimizeTransparency shaves ~10% more but
    # leaves a disposal method that does not composite partial frames onto the
    # previous one: -coalesce comes back as fragments, and so does playback.
    cmd += ["-resize", f"{width}x", "-layers", "Optimize", "-colors", "128", OUT]
    subprocess.run(cmd, check=True)
    verify(OUT, n)
    size = os.path.getsize(OUT)
    print(f"wrote {OUT}: {size/1e6:.2f} MB, {n} frames")


def verify(path, expected_frames):
    """A correct GIF coalesces back into full frames. An over-optimised one
    comes back as sparse fragments, which is invisible in a file listing and
    obvious the moment anyone plays it."""
    out = subprocess.run(["magick", "identify", path],
                         capture_output=True, text=True, check=True).stdout
    got = len(out.strip().splitlines())
    if got != expected_frames:
        sys.exit(f"{path} has {got} frames, expected {expected_frames}")

    os.makedirs("verify", exist_ok=True)
    for f in os.listdir("verify"):
        os.remove(os.path.join("verify", f))
    subprocess.run(["magick", path, "-coalesce", "+adjoin", "verify/v-%03d.png"],
                   check=True)
    names = sorted(os.listdir("verify"))
    # Compare each reconstructed frame's detail against its source frame.
    for i, name in enumerate(names):
        sd = float(subprocess.run(["magick", f"verify/{name}", "-format",
                                   "%[fx:standard_deviation]", "info:"],
                                  capture_output=True, text=True,
                                  check=True).stdout)
        if i > 0 and sd < 0.05:
            sys.exit(f"frame {i} coalesced to sd={sd:.4f}: the GIF is not "
                     f"compositing partial frames")
    print(f"verified: {len(names)} frames coalesce to full pictures")


main()
