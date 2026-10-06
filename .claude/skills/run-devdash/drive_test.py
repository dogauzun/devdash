#!/usr/bin/env python3
"""Tests for drive.py's terminal emulator: python3 .claude/skills/run-devdash/drive_test.py"""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from drive import Screen  # noqa: E402


def rows(screen):
    return ["".join(r).rstrip() for r in screen.cells]


def fed(cols, nrows, *chunks):
    s = Screen(cols, nrows)
    for c in chunks:
        s.feed(c)
    return s


class ScreenTest(unittest.TestCase):
    def test_scroll_region(self):  # DEV-214
        s = fed(10, 5, b"a\r\nb\r\nc\r\nd\r\ne",
                b"\x1b[2;4r",  # margins rows 2-4, cursor home
                b"\x1b[S")  # region up one line
        self.assertEqual(rows(s), ["a", "c", "d", "", "e"])
        s.feed(b"\x1b[4;1H\nx")  # LF at the bottom margin scrolls the region
        self.assertEqual(rows(s), ["a", "d", "", "x", "e"])
        s.feed(b"\x1b[5;1H\ny")  # below the region: LF at the last row does not scroll
        self.assertEqual(rows(s), ["a", "d", "", "x", "y"])
        s.feed(b"\x1b[rz")  # reset to full screen, cursor home
        self.assertEqual(rows(s), ["z", "d", "", "x", "y"])
        s.feed(b"\x1b[5;1H\nw")  # LF at the last row now scrolls the whole screen
        self.assertEqual(rows(s), ["d", "", "x", "y", "w"])

    def test_insert_delete_reverse_index(self):  # DEV-214
        s = fed(10, 5, b"a\r\nb\r\nc\r\nd\r\ne", b"\x1b[2;4r")
        s.feed(b"\x1b[3;1H\x1b[L")  # insert at row 3: d drops off the bottom margin
        self.assertEqual(rows(s), ["a", "b", "", "c", "e"])
        s.feed(b"\x1b[2;1H\x1b[2M")  # delete two at row 2
        self.assertEqual(rows(s), ["a", "c", "", "", "e"])
        s.feed(b"\x1b[5;1H\x1b[L")  # outside the region: ignored
        self.assertEqual(rows(s), ["a", "c", "", "", "e"])
        s.feed(b"\x1b[T")  # region down one line
        self.assertEqual(rows(s), ["a", "", "c", "", "e"])
        s.feed(b"\x1b[2;1H\x1bMq")  # reverse index at the top margin scrolls down
        self.assertEqual(rows(s), ["a", "q", "", "c", "e"])
        s.feed(b"\x1b[4;3H\x1bDr")  # index at the bottom margin scrolls, column kept
        self.assertEqual(rows(s), ["a", "", "c", "  r", "e"])
        s.feed(b"\x1b[4;4r\x1b[S")  # an invalid region is ignored: rows 2-4 scroll
        self.assertEqual(rows(s), ["a", "c", "  r", "", "e"])

    def test_tabs(self):  # DEV-203: the renderer's ECH plus hard tabs on Linux
        s = fed(40, 2, b"watcher\x1b[20X\t\t\t 84  14m\r\n")
        self.assertEqual(rows(s)[0], "watcher" + " " * 17 + " 84  14m")
        self.assertEqual(rows(fed(10, 1, b"ab\t\tZ"))[0], "ab       Z")  # clamped to the last column

    def test_split_utf8(self):  # DEV-203: a character cut by a read boundary
        s = fed(10, 1, "▾ x".encode()[:2], "▾ x".encode()[2:])
        self.assertEqual(rows(s)[0], "▾ x")


if __name__ == "__main__":
    unittest.main()
