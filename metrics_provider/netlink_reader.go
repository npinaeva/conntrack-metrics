package main

import (
	"context"
	"fmt"
	"golang.org/x/sys/unix"
	"log"
	"os"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ti-mo/netfilter"
)

type bufs struct {
	data    [][]byte
	headers []MmsgHdr
	hsptr   uintptr
	hslen   uintptr
	n       int
	err     error
}

func newBufs(size int) *bufs {
	b := &bufs{
		data:    make([][]byte, size),
		headers: make([]MmsgHdr, size),
	}
	for i := range b.data {
		b.data[i] = make([]byte, os.Getpagesize())
		iov := &syscall.Iovec{
			Base: &b.data[i][0],
		}
		iov.SetLen(len(b.data[i]))
		b.headers[i].Iov = iov
		b.headers[i].Iovlen = 1
	}
	b.hsptr = uintptr(unsafe.Pointer(&b.headers[0]))
	b.hslen = uintptr(size)
	return b
}

type TsMsgs struct {
	Msgs []*syscall.NetlinkMessage
}

type NetlinkReader struct {
	EventChan           chan *TsMsgs
	eventChanSize       float64
	socket              *NetlinkSocket
	emptyBufs, fullBufs chan *bufs
	errChan             chan error
	rcvMsgCounter       atomic.Int64
	nodeName            string
	recmmsgBatchSize    int
	bufsPerWorker       int
}

func NewNetlinkReader(socketBufSizeMB, eventQueueSize, recmmsgBatchSize, bufsPerWorker int, nodeName string) (*NetlinkReader, error) {
	conn, err := Subscribe(unix.NETLINK_NETFILTER,
		uint(netfilter.GroupCTNew),
		uint(netfilter.GroupCTUpdate),
		uint(netfilter.GroupCTDestroy))
	if err != nil {
		return nil, err
	}

	buffersize := 1024 * 1024 * socketBufSizeMB
	for ; buffersize > 1024; buffersize = buffersize / 2 {
		err = conn.SetReceiveBufferSize(buffersize, true)
		if err == nil {
			break
		}
	}
	log.Println("Set read buffer size to", buffersize/1024, "KB")

	return &NetlinkReader{
		EventChan:        make(chan *TsMsgs, eventQueueSize),
		eventChanSize:    float64(eventQueueSize),
		socket:           conn,
		errChan:          make(chan error, 1),
		rcvMsgCounter:    atomic.Int64{},
		nodeName:         nodeName,
		recmmsgBatchSize: recmmsgBatchSize,
		bufsPerWorker:    bufsPerWorker,
	}, nil

}

func (r *NetlinkReader) StartWorkers(ctx context.Context, numReceivers, numWorkers int) error {
	defer r.stop()

	r.emptyBufs, r.fullBufs = make(chan *bufs, numWorkers*r.bufsPerWorker), make(chan *bufs, numWorkers*r.bufsPerWorker)
	go r.recordMetrics(ctx)

	for id := 0; id < numWorkers; id++ {
		go r.msgParser(ctx, id)
	}
	log.Printf("%d netlink workers started\n", numWorkers)
	// let workers add empty buffers
	time.Sleep(time.Second)
	for i := 0; i < numReceivers; i++ {
		go r.receiver(ctx, i)
	}
	log.Printf("%d netlink receivers started\n", numReceivers)

	select {
	case err := <-r.errChan:
		return err
	case <-ctx.Done():
		log.Printf("Context is cancelled.")
		return nil
	}
}

func (r *NetlinkReader) stop() {
	r.socket.Close()
}

func (r *NetlinkReader) receiver(ctx context.Context, id int) {
	for {
		select {
		case emptyBuf := <-r.emptyBufs:
			emptyBuf.n, emptyBuf.err = r.socket.RecvMsgs(syscall.MSG_WAITFORONE, emptyBuf.hsptr, emptyBuf.hslen)
			r.rcvMsgCounter.Add(int64(emptyBuf.n))
			r.fullBufs <- emptyBuf
		case <-ctx.Done():
			log.Printf("Stopping receiver %v", id)
			return
		case <-r.errChan:
			return
		default:
			MetricConntrackNoBufsCounter.WithLabelValues(r.nodeName).Inc()
		}
	}
}

func (r *NetlinkReader) recordMetrics(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			n := r.rcvMsgCounter.Swap(0)
			if n <= 1000 {
				continue
			}
			MetricRcvMsgsSpeed.WithLabelValues(r.nodeName).Observe(float64(n))
			// record queue size
			queueLen := len(r.EventChan)
			if queueLen > 100 {
				MetricConntrackQueueSizeHist.WithLabelValues(r.nodeName).Observe(float64(queueLen))
				if float64(queueLen) > 0.9*r.eventChanSize {
					MetricErrorCounter.WithLabelValues(r.nodeName, ErrorEventQueueFull).Inc()
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func (r *NetlinkReader) msgParser(ctx context.Context, workerID int) {
	defer log.Println("Stopping conntrack worker", workerID)
	for i := 0; i < r.bufsPerWorker; i++ {
		buf := newBufs(r.recmmsgBatchSize)
		r.emptyBufs <- buf
	}
	for {
		select {
		case fullBuf := <-r.fullBufs:
			msgs, err := parseMsgs(fullBuf)
			if err != nil {
				select {
				case r.errChan <- fmt.Errorf("worker %d, error: %v", workerID, err):
					return
				default:
					return
				}
			}
			r.EventChan <- &TsMsgs{
				Msgs: msgs,
			}
			r.emptyBufs <- fullBuf
		case <-ctx.Done():
			return
		case <-r.errChan:
			return
		}
	}
}

func parseMsgs(b *bufs) ([]*syscall.NetlinkMessage, error) {
	if b.err != nil {
		return nil, fmt.Errorf("Receive() netlink error: %w", b.err)
	}
	res := make([]*syscall.NetlinkMessage, b.n)
	for i := 0; i < b.n; i++ {
		nr := int(b.headers[i].MsgLen)
		if nr < unix.NLMSG_HDRLEN {
			return nil, fmt.Errorf("got short message from netlink")
		}
		msgLen := NlmAlignOf(nr)
		raw, err := ParseNetlinkMessage(b.data[i][:msgLen])
		if err != nil {
			fmt.Println("ParseNetlinkMessage error:", err)
			return nil, err
		}
		res[i] = raw
	}
	return res, nil
}
