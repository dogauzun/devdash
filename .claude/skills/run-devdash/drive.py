#!/usr/bin/env python3
"""Run the devdash dashboard in a pty, send it keys, and print its screen as text.

    drive.py [--size 140x40] [--cwd DIR] [--cmd "./devdash --no-color"] [--lines N] STEP...

Each STEP is typed into the dashboard and the screen is printed after it (and once before
the first). A step is literal keys plus names in angle brackets: <up> <down> <left> <right>
<enter> <esc> <tab> <backspace> <pgup> <pgdown> <home> <end> <ctrl-c> <ctrl-u>, and
<wait:SECONDS> to let the dashboard refresh. The selected row (reverse video) is marked "» ".

Python 3 standard library only. The screen is rebuilt by a small terminal emulator (tested
by drive_test.py), and a resize before every print makes the dashboard repaint in full; the
emulator also follows the incremental updates after it (scroll regions, tabs).
"""
import argparse
import codecs
import fcntl
import os
import pty
import re
import select
import shlex
import signal
import struct
import sys
import termios
import time

KEYS = {
    "up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
    "enter": "\r", "esc": "\x1b", "tab": "\t", "backspace": "\x7f",
    "pgup": "\x1b[5~", "pgdown": "\x1b[6~", "home": "\x1b[H", "end": "\x1b[F",
    "ctrl-c": "\x03", "ctrl-u": "\x15",
}
TOKEN = re.compile(r"<([a-z-]+(?::[0-9.]+)?)>|(.)", re.S)
CSI = re.compile(r"\x1b\[([0-9;:?<>=! ]*)([@-~])")
STRING = re.compile(r"\x1b[\]P_^X].*?(?:\x07|\x1b\\)", re.S)  # OSC, DCS, APC, PM, SOS


class Screen:
    """The subset of xterm the dashboard's renderer uses. feed takes the pty's bytes."""

    def __init__(self, cols, rows):
        self.cols, self.rows = cols, rows
        self.y = self.x = 0
        self.reverse = False
        self.pending = ""
        self.decode = codecs.getincrementaldecoder("utf-8")("replace").decode  # keeps a cut char
        self.wipe()
        self.reset_region()

    def reset_region(self):
        self.top, self.bot = 0, self.rows - 1

    def scroll(self, at, n):
        """Move rows at..bot up n lines (down for n < 0), blanking the rows that open up."""
        for grid, fill in ((self.cells, " "), (self.marks, False)):
            for _ in range(min(abs(n), self.bot - at + 1)):
                if n > 0:
                    del grid[at]
                    grid.insert(self.bot, [fill] * self.cols)
                else:
                    del grid[self.bot]
                    grid.insert(at, [fill] * self.cols)

    def wipe(self):
        self.cells = [[" "] * self.cols for _ in range(self.rows)]
        self.marks = [[False] * self.cols for _ in range(self.rows)]

    def feed(self, data):
        buf = self.pending + self.decode(data)
        i = 0
        while i < len(buf):
            c = buf[i]
            if c != "\x1b":
                self.put(c)
                i += 1
                continue
            if i + 1 >= len(buf):
                break  # the rest of the sequence comes with the next read
            kind = buf[i + 1]
            if kind == "[":
                m = CSI.match(buf, i)
                if not m:
                    if len(buf) - i < 32:
                        break
                    i += 2
                    continue
                self.csi(m.group(1), m.group(2))
                i = m.end()
            elif kind in "]P_^X":
                m = STRING.match(buf, i)
                if not m:
                    break
                i = m.end()
            else:
                if kind == "D":  # index
                    self.put("\n")
                elif kind == "M":  # reverse index
                    if self.y == self.top:
                        self.scroll(self.top, -1)
                    else:
                        self.y = max(0, self.y - 1)
                i += 3 if kind in "()" else 2
        self.pending = buf[i:]

    def put(self, c):
        if c == "\r":
            self.x = 0
        elif c == "\n":
            if self.y == self.bot:
                self.scroll(self.top, 1)
            else:
                self.y = min(self.rows - 1, self.y + 1)
        elif c == "\t":  # tab stops every 8 columns, as the renderer sets them (CSI ? 5 W)
            self.x = min(self.cols - 1, (self.x // 8 + 1) * 8)
        elif c == "\b":
            self.x = max(0, self.x - 1)
        elif c >= " ":
            self.x = min(self.x, self.cols - 1)
            self.cells[self.y][self.x] = c
            self.marks[self.y][self.x] = self.reverse
            self.x += 1

    def csi(self, params, final):
        if params and params[0] in "?<>=!":
            return  # private modes and queries
        a = [int(p) if p.isdigit() else 0 for p in re.split("[;:]", params.strip())]
        n = a[0] or 1
        row, blank = self.cells[self.y], [" "]
        if final in "Hf":
            self.y = min(self.rows - 1, n - 1)
            self.x = min(self.cols - 1, ((a[1] if len(a) > 1 else 1) or 1) - 1)
        elif final == "A":
            self.y = max(0, self.y - n)
        elif final == "B":
            self.y = min(self.rows - 1, self.y + n)
        elif final == "C":
            self.x = min(self.cols - 1, self.x + n)
        elif final == "D":
            self.x = max(0, self.x - n)
        elif final == "G":
            self.x = min(self.cols - 1, n - 1)
        elif final == "d":
            self.y = min(self.rows - 1, n - 1)
        elif final == "m":
            for v in a:
                if v == 7:
                    self.reverse = True
                elif v in (0, 27):
                    self.reverse = False
        elif final == "K":
            if a[0] == 0:
                row[self.x:] = blank * (self.cols - self.x)
            elif a[0] == 1:
                row[:self.x + 1] = blank * (self.x + 1)
            else:
                row[:] = blank * self.cols
        elif final == "J":
            if a[0] in (2, 3):
                self.wipe()
            elif a[0] == 0:
                row[self.x:] = blank * (self.cols - self.x)
                for r in range(self.y + 1, self.rows):
                    self.cells[r] = blank * self.cols
        elif final == "X":
            row[self.x:self.x + n] = blank * len(row[self.x:self.x + n])
        elif final == "P":
            del row[self.x:self.x + n]
            row += blank * (self.cols - len(row))
        elif final == "@":
            row[self.x:self.x] = blank * n
            del row[self.cols:]
        elif final == "r":  # margins, 1-based and inclusive; an invalid pair is ignored
            top = (a[0] or 1) - 1
            bot = min(self.rows, (a[1] if len(a) > 1 else 0) or self.rows) - 1
            if top < bot:
                self.top, self.bot = top, bot
                self.y = self.x = 0
        elif final == "S":
            self.scroll(self.top, n)
        elif final == "T":
            self.scroll(self.top, -n)
        elif final in "LM" and self.top <= self.y <= self.bot:
            self.scroll(self.y, -n if final == "L" else n)
            self.x = 0

    def lines(self):
        out = []
        for cells, marks in zip(self.cells, self.marks):
            text = "".join(cells).rstrip()
            out.append(("» " if sum(marks) > 8 else "  ") + text if text else "")
        while out and not out[-1]:
            out.pop()
        return out


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--size", default="140x40", help="COLSxROWS (default 140x40)")
    ap.add_argument("--cwd", default=".", help="directory the dashboard runs in (its `here` project)")
    ap.add_argument("--cmd", default="./devdash --no-color", help="command, resolved before --cwd applies")
    ap.add_argument("--lines", type=int, default=0, help="print only the first N lines of each screen")
    ap.add_argument("steps", nargs="*", metavar="STEP")
    args = ap.parse_args()
    cols, rows = (int(v) for v in args.size.lower().split("x"))
    cmd = shlex.split(args.cmd)
    if os.sep in cmd[0]:
        cmd[0] = os.path.abspath(cmd[0])

    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(args.cwd)
        os.environ["TERM"] = "xterm-256color"
        os.execvp(cmd[0], cmd)
    screen = Screen(cols, rows)

    def resize(c):
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, c, 0, 0))
        os.kill(pid, signal.SIGWINCH)

    def pump(seconds):
        end = time.time() + seconds
        while (left := end - time.time()) > 0:
            if not select.select([fd], [], [], left)[0]:
                return True
            try:
                data = os.read(fd, 65536)
            except OSError:
                return False
            if not data:
                return False
            screen.feed(data)
        return True

    def show(label):
        for c in (cols - 1, cols):  # a resize makes the dashboard repaint in full
            screen.wipe()
            screen.reset_region()  # as a terminal does on a resize
            resize(c)
            alive = pump(0.5)
        print(f"=== {label}")
        lines = screen.lines()
        print("\n".join(lines[:args.lines] if args.lines else lines))
        if not alive:
            sys.exit("the dashboard exited")

    resize(cols)
    pump(3.0)  # the first snapshot
    show("start")
    for n, step in enumerate(args.steps, 1):
        for name, char in TOKEN.findall(step):
            if name.startswith("wait:"):
                pump(float(name[5:]))
                continue
            if name and name not in KEYS:
                sys.exit(f"unknown key <{name}> in step {n}")
            os.write(fd, (KEYS[name] if name else char).encode())
            pump(0.15)
        pump(0.6)
        show(f"{n}: {step}")

    for key in (b"\x1b", b"q"):  # esc closes a modal or prompt, q quits
        os.write(fd, key)
        pump(0.3)
    try:
        os.kill(pid, signal.SIGTERM)
    except ProcessLookupError:
        pass


if __name__ == "__main__":
    main()
