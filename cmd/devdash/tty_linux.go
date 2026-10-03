package main

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlGetWinsize = unix.TIOCGWINSZ
)
