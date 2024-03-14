package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

type NetlinkSocket struct {
	fd  int32
	lsa unix.SockaddrNetlink
	sync.Mutex
}

func Subscribe(protocol int, groups ...uint) (*NetlinkSocket, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, protocol)
	if err != nil {
		return nil, err
	}

	s := &NetlinkSocket{
		fd: int32(fd),
	}
	s.lsa.Family = unix.AF_NETLINK

	for _, g := range groups {
		s.lsa.Groups |= (1 << (g - 1))
	}

	if err := unix.Bind(fd, &s.lsa); err != nil {
		unix.Close(fd)
		return nil, err
	}

	return s, nil
}

func (s *NetlinkSocket) Close() {
	fd := int(atomic.SwapInt32(&s.fd, -1))
	unix.Close(fd)
}

// SetReceiveBufferSize allows to set a receive buffer size on the socket
func (s *NetlinkSocket) SetReceiveBufferSize(size int, force bool) error {
	opt := unix.SO_RCVBUF
	if force {
		opt = unix.SO_RCVBUFFORCE
	}
	return unix.SetsockoptInt(int(s.fd), unix.SOL_SOCKET, opt, size)
}

type MmsgHdr struct {
	syscall.Msghdr
	MsgLen uint64
}

func (s *NetlinkSocket) RecvMsgs(flags int, hs, hslen uintptr) (int, time.Time, error) {
	fd := int(atomic.LoadInt32(&s.fd))
	if fd < 0 {
		return 0, time.Time{}, fmt.Errorf("Receive called on a closed socket")
	}
	return recvmmsg(uintptr(fd), hs, hslen, flags)
}

func recvmmsg(s uintptr, hs, hslen uintptr, flags int) (int, time.Time, error) {
	n, _, errno := syscall.Syscall6(syscall.SYS_RECVMMSG, s, hs, hslen, uintptr(flags), 0, 0)
	return int(n), time.Now(), errnoErr(errno)
}

var (
	errEAGAIN error = syscall.EAGAIN
	errEINVAL error = syscall.EINVAL
	errENOENT error = syscall.ENOENT
)

// errnoErr returns common boxed Errno values, to prevent
// allocations at runtime.
func errnoErr(e syscall.Errno) error {
	switch e {
	case 0:
		return nil
	case syscall.EAGAIN:
		return errEAGAIN
	case syscall.EINVAL:
		return errEINVAL
	case syscall.ENOENT:
		return errENOENT
	}
	return e
}

// Round the length of a netlink message up to align it properly.
// Taken from syscall/netlink_linux.go by The Go Authors under BSD-style license.
func NlmAlignOf(msglen int) int {
	return (msglen + syscall.NLMSG_ALIGNTO - 1) & ^(syscall.NLMSG_ALIGNTO - 1)
}

func ParseNetlinkMessage(b []byte) (*syscall.NetlinkMessage, error) {
	var msg *syscall.NetlinkMessage
	if len(b) >= syscall.NLMSG_HDRLEN {
		h, dbuf, _, err := netlinkMessageHeaderAndData(b)
		if err != nil {
			return nil, err
		}
		data := make([]byte, int(h.Len)-syscall.NLMSG_HDRLEN)
		copy(data, dbuf[:int(h.Len)-syscall.NLMSG_HDRLEN])
		msg = &syscall.NetlinkMessage{Header: *h, Data: data}
	}
	return msg, nil
}

func netlinkMessageHeaderAndData(b []byte) (*syscall.NlMsghdr, []byte, int, error) {
	h := (*syscall.NlMsghdr)(unsafe.Pointer(&b[0]))
	l := NlmAlignOf(int(h.Len))
	if int(h.Len) < syscall.NLMSG_HDRLEN || l > len(b) {
		return nil, nil, 0, syscall.EINVAL
	}
	return h, b[syscall.NLMSG_HDRLEN:], l, nil
}
