//go:build linux

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// See term_darwin.go for why ISIG is left enabled.

const (
	ioctlRead  = syscall.TCGETS
	ioctlWrite = syscall.TCSETS
)

type termState struct {
	original syscall.Termios
}

// winsize mirrors struct winsize, the argument to TIOCGWINSZ.
type winsize struct {
	Row    uint16
	Col    uint16
	Xpixel uint16
	Ypixel uint16
}

const ioctlWinsize = syscall.TIOCGWINSZ

// terminalWidth reports how many columns the terminal has, or 0 if it cannot be
// determined.
//
// The width is not a nicety here. The status line is pinned by moving the cursor
// up one row, and a status line wider than the terminal wraps onto a second row -
// after which every redraw is off by one and the whole screen scrambles.
func terminalWidth(f *os.File) int {
	if f == nil {
		return 0
	}

	var ws winsize
	if err := ioctl(f.Fd(), ioctlWinsize, unsafe.Pointer(&ws)); err != nil {
		return 0
	}

	return int(ws.Col)
}

func ioctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	if errno != 0 {
		return errno
	}

	return nil
}

func makeRaw(f *os.File) (*termState, error) {
	fd := f.Fd()

	var original syscall.Termios
	if err := ioctl(fd, ioctlRead, unsafe.Pointer(&original)); err != nil {
		return nil, err
	}

	raw := original
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN
	raw.Iflag &^= syscall.IXON | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.BRKINT
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := ioctl(fd, ioctlWrite, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}

	return &termState{original: original}, nil
}

func (s *termState) restore(f *os.File) {
	if s == nil {
		return
	}

	original := s.original
	_ = ioctl(f.Fd(), ioctlWrite, unsafe.Pointer(&original))
}
