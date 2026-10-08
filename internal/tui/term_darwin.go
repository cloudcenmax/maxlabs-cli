//go:build darwin

package tui

import (
	"os"
	"syscall"
	"unsafe"
)

// A terminal's line discipline is what gives a program whole lines and echoes
// keys for it. To see Shift+Tab or an arrow key, the shell has to turn that off
// and read bytes itself.
//
// ISIG is deliberately LEFT ON. With it off, Ctrl+C stops generating SIGINT and
// arrives as byte 0x03, which means a wedged read can no longer be interrupted
// from the keyboard - a bad trade for tidiness. Ctrl+C therefore still reaches
// the signal handler, and only Enter, Backspace and the escape sequences are
// decoded here.

const (
	ioctlRead  = syscall.TIOCGETA
	ioctlWrite = syscall.TIOCSETA
)

// termState is what has to be put back on the way out.
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

// ioctl issues one ioctl.
func ioctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg))
	if errno != 0 {
		return errno
	}

	return nil
}

// makeRaw switches the terminal to character-at-a-time, no-echo input.
func makeRaw(f *os.File) (*termState, error) {
	fd := f.Fd()

	var original syscall.Termios
	if err := ioctl(fd, ioctlRead, unsafe.Pointer(&original)); err != nil {
		return nil, err
	}

	raw := original

	// Character at a time, no echo. ISIG stays, so Ctrl+C still signals.
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.IEXTEN

	// No input post-processing: a typed carriage return must arrive as \r and
	// not be rewritten to \n underneath us, and Ctrl+S must not freeze output.
	raw.Iflag &^= syscall.IXON | syscall.ICRNL | syscall.INPCK | syscall.ISTRIP | syscall.BRKINT

	// VMIN=1, VTIME=0: block until at least one byte is available.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := ioctl(fd, ioctlWrite, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}

	return &termState{original: original}, nil
}

// restore puts the terminal back the way it was.
func (s *termState) restore(f *os.File) {
	if s == nil {
		return
	}

	original := s.original
	_ = ioctl(f.Fd(), ioctlWrite, unsafe.Pointer(&original))
}
