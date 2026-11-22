package main

import (
	"syscall"
	"unsafe"
)

// isTTY says whether fd is a terminal: its terminal attributes can be read (a
// character device such as /dev/null cannot answer).
func isTTY(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TIOCGETA), uintptr(unsafe.Pointer(&t)))
	return e == 0
}
