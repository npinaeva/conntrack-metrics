package main

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/mdlayher/netlink"
	"github.com/ti-mo/conntrack"
)

type tsConntrackEvent struct {
	event *conntrack.Event
	ts    time.Time
}

func eventTypeString(t uint8) string {
	switch t {
	case 1:
		return "new"
	case 2:
		return "update"
	case 3:
		return "destroy"
	default:
		return "unknown"
	}
}

func printEvent(event *conntrack.Event) {
	log.Printf("Event %s, flow: %+v, TCP: %+v", event.Type, event.Flow, event.Flow.ProtoInfo.TCP)
}

type EventsHandler struct {
	svcSubnet     netip.Prefix
	excludeSubnet *netip.Prefix
	nodeName      string
	printEvents   bool
	events        sync.Map
	workers       []*worker
}

func NewEventsHandler(svcSubnet netip.Prefix, excludeSubnet *netip.Prefix, nodeName string, printEvents bool) *EventsHandler {
	return &EventsHandler{
		svcSubnet:     svcSubnet,
		excludeSubnet: excludeSubnet,
		nodeName:      nodeName,
		printEvents:   printEvents,
	}
}

func (h *EventsHandler) Start(ctx context.Context, nDispatchers, nWorkers, workerQueueSize int, c chan *TsMsgs) {
	for i := 0; i < nWorkers; i++ {
		w := newWorker(h.nodeName, h.printEvents, workerQueueSize)
		h.workers = append(h.workers, w)
		go w.Start(ctx, i)
		go w.StartWaitQueue(ctx)
	}
	log.Printf("%d event handler workers started\n", nWorkers)

	go h.recordQueueSize(ctx, float64(workerQueueSize))
	for i := 0; i < nDispatchers; i++ {
		go h.dispatch(ctx, c, i)
	}
	log.Printf("%d event queue dispatchers started\n", nDispatchers)
}

func (h *EventsHandler) dispatch(ctx context.Context, c chan *TsMsgs, id int) {
	defer log.Printf("Dispatcher %d stopped", id)
	nWorkers := len(h.workers)
	for {
		select {
		case msgs := <-c:
			MetricConntrackBatchSizeHist.WithLabelValues(h.nodeName).Observe(float64(len(msgs.Msgs)))
			for _, msg := range msgs.Msgs {
				startTime := time.Now()
				// Decode event and send on channel
				event := new(conntrack.Event)
				netlinkMsg := netlink.Message{
					Header: sysToHeader(msg.Header),
					Data:   msg.Data,
				}
				err := event.Unmarshal(netlinkMsg)
				if err != nil {
					log.Printf("ERROR: failed unmarshalling event from msg %+v: %v\n"+
						"Event %+v", netlinkMsg, err, event)
					continue
				}
				//if h.printEvents {
				//	printEvent(event)
				//}

				if h.interestingEvent(event) {
					// send same flow to the same queueNum to ensure correct events ordering
					queueNum := event.Flow.ID % uint32(nWorkers)
					h.workers[queueNum].eventQueue <- &tsConntrackEvent{
						event: event,
						ts:    msgs.Ts,
					}
					MetricConntrackEventsCounter.WithLabelValues(h.nodeName, eventTypeString(uint8(event.Type))).Inc()
					MetricDispatchTime.WithLabelValues(h.nodeName).Observe(time.Now().Sub(startTime).Seconds())
					MetricEventTillWorkerQueueTime.WithLabelValues(h.nodeName, eventTypeString(uint8(event.Type))).Observe(time.Now().Sub(msgs.Ts).Seconds())
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

func sysToHeader(r syscall.NlMsghdr) netlink.Header {
	// NB: the memory layout of Header and syscall.NlMsgHdr must be
	// exactly the same for this unsafe cast to work
	return *(*netlink.Header)(unsafe.Pointer(&r))
}

func (h *EventsHandler) interestingEvent(event *conntrack.Event) bool {
	return event.Flow != nil &&
		h.svcSubnet.Contains(event.Flow.TupleOrig.IP.DestinationAddress) &&
		(h.excludeSubnet == nil || !h.excludeSubnet.Contains(event.Flow.TupleOrig.IP.DestinationAddress)) &&
		(event.Type == conntrack.EventUpdate && event.Flow.TupleOrig.IP.DestinationAddress != event.Flow.TupleReply.IP.SourceAddress ||
			event.Type == conntrack.EventDestroy && !event.Flow.Timestamp.Stop.IsZero())
}

func (h *EventsHandler) recordQueueSize(ctx context.Context, workerQueueSize float64) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for i := range h.workers {
				queueLen := len(h.workers[i].eventQueue)
				if queueLen > 100 {
					MetricConntrackWorkerQueueSizeHist.WithLabelValues(h.nodeName, "events").Observe(float64(queueLen))
					if float64(queueLen) > 0.9*workerQueueSize {
						MetricErrorCounter.WithLabelValues(h.nodeName, ErrorWorkerEventQueueFull).Inc()
					}
				}
				queueLen = len(h.workers[i].readyFlows)
				if queueLen > 100 {
					MetricConntrackWorkerQueueSizeHist.WithLabelValues(h.nodeName, "metrics").Observe(float64(queueLen))
					if float64(queueLen) > 0.9*workerQueueSize {
						MetricErrorCounter.WithLabelValues(h.nodeName, ErrorWorkerMetricsQueueFull).Inc()
					}
				}
				queueLen = len(h.workers[i].waitQueue)
				if queueLen > 100 {
					MetricConntrackWorkerQueueSizeHist.WithLabelValues(h.nodeName, "wait").Observe(float64(queueLen))
					if float64(queueLen) > 0.9*workerQueueSize {
						MetricErrorCounter.WithLabelValues(h.nodeName, ErrorWorkerWaitQueueFull).Inc()
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

type flowEvent int

const (
	created = iota
	seenReply
	tcpFin
	deleted
)

type eventRecord struct {
	ts     time.Time
	nonTCP bool
	event  flowEvent
	// for delete event
	toSvcBytes, fromSvcBytes float64
}

func printEventRecords(records []*eventRecord) string {
	res := ""
	for _, r := range records {
		res += fmt.Sprintf("ts: %v, event: %v\n", r.ts, r.event)
	}
	return res
}

type flowInfo struct {
	seenReply     time.Time
	eventRecords  []*eventRecord
	seenReplyFlow *conntrack.Flow
	tcpFinSeen    bool
	tcpFin        time.Time
}

type worker struct {
	nodeName    string
	printEvents bool
	flowInfos   map[uint32]*flowInfo
	eventQueue  chan *tsConntrackEvent
	readyFlows  chan uint32
	waitQueue   chan uint32
}

func newWorker(nodeName string, printEvents bool, workerQueueSize int) *worker {
	return &worker{
		nodeName:    nodeName,
		printEvents: printEvents,
		flowInfos:   map[uint32]*flowInfo{},
		eventQueue:  make(chan *tsConntrackEvent, workerQueueSize),
		readyFlows:  make(chan uint32, workerQueueSize),
		waitQueue:   make(chan uint32, workerQueueSize),
	}
}

func (w *worker) StartWaitQueue(ctx context.Context) {
	w.waitQueue <- 0
	w.waitQueue <- 0
	for {
		select {
		case flowID := <-w.waitQueue:
			if flowID == 0 {
				time.Sleep(1 * time.Second)
				w.waitQueue <- 0
			} else {
				w.readyFlows <- flowID
			}
		case <-ctx.Done():
			return
		}
	}
}

func (w *worker) Start(ctx context.Context, workerNum int) {
	defer log.Printf("Worker %d stopped\n", workerNum)
	for {
		select {
		case event := <-w.eventQueue:
			w.handleConntrackEvent(event)
		case flowID := <-w.readyFlows:
			w.recordMetrics(flowID)
		case <-ctx.Done():
			return
		}
	}
}

func (w *worker) scheduleRecording(flowID uint32) {
	w.waitQueue <- flowID
}

func (w *worker) handleConntrackEvent(event *tsConntrackEvent) {
	handlerStart := time.Now()
	defer func() {
		eventType := eventTypeString(uint8(event.event.Type))
		MetricHandlingTime.WithLabelValues(w.nodeName, eventType).Observe(time.Now().Sub(handlerStart).Seconds())
		MetricEventInSystemTime.WithLabelValues(w.nodeName, eventType).Observe(time.Now().Sub(event.ts).Seconds())
	}()

	flow := event.event.Flow
	if w.printEvents {
		printEvent(event.event)
	}
	fi, ok := w.flowInfos[flow.ID]
	if !ok {
		fi = &flowInfo{
			eventRecords: make([]*eventRecord, 0, 8),
		}
		w.flowInfos[flow.ID] = fi
	}
	if event.event.Type == conntrack.EventDestroy {
		if !flow.Timestamp.Stop.IsZero() {
			connectionLifetime := flow.Timestamp.Stop.Sub(flow.Timestamp.Start)
			MetricSvcDuration.WithLabelValues(w.nodeName).Observe(connectionLifetime.Seconds())
			MetricSvcPacketsHist.WithLabelValues(w.nodeName, "to-svc").Observe(float64(flow.CountersOrig.Packets))
			MetricSvcPacketsHist.WithLabelValues(w.nodeName, "from-svc").Observe(float64(flow.CountersReply.Packets))
			MetricSvcBytesHist.WithLabelValues(w.nodeName, "to-svc").Observe(float64(flow.CountersOrig.Bytes))
			MetricSvcBytesHist.WithLabelValues(w.nodeName, "from-svc").Observe(float64(flow.CountersReply.Bytes))

			fi.eventRecords = append(fi.eventRecords,
				&eventRecord{
					ts:    flow.Timestamp.Start,
					event: created,
				},
				&eventRecord{
					ts:           flow.Timestamp.Stop,
					event:        deleted,
					toSvcBytes:   float64(flow.CountersOrig.Bytes),
					fromSvcBytes: float64(flow.CountersReply.Bytes),
				},
			)
			w.scheduleRecording(flow.ID)
		}
	}
	if event.event.Type == conntrack.EventUpdate {
		tcpState := getTCPState(flow)
		if tcpState == 2 || tcpState == 0 && event.event.Flow.Status.SeenReply() && !event.event.Flow.Status.Assured() {
			fi.eventRecords = append(fi.eventRecords, &eventRecord{
				ts:     event.ts,
				event:  seenReply,
				nonTCP: tcpState == 0,
			})
		}
		if tcpState == 4 {
			fi.eventRecords = append(fi.eventRecords, &eventRecord{
				ts:    event.ts,
				event: tcpFin,
			})
		}
	}
}

func correctNonTCPEvents(events []*eventRecord) bool {
	return len(events) > 2 && events[0].event == created && events[1].event == seenReply && events[1].nonTCP == true && events[2].event == deleted
}

// return correct, tcpFinIdx, deleteIdx
func correctTCPEvents(events []*eventRecord) (bool, int, int) {
	correct := len(events) > 3 && events[0].event == created && events[1].event == seenReply &&
		(events[2].event == tcpFin && events[3].event == deleted ||
			events[3].event == tcpFin && events[2].event == deleted)
	if !correct {
		return false, -1, -1
	}
	// tcpFin was recorded after kernel delete timestamp, may happen because of delay
	tcpFinIdx := 2
	deleteIdx := 3
	if events[3].event == tcpFin && events[2].event == deleted {
		tcpFinIdx = 3
		deleteIdx = 2
	}
	return true, tcpFinIdx, deleteIdx
}

func (w *worker) recordMetrics(flowID uint32) {
	fi, ok := w.flowInfos[flowID]
	if !ok {
		log.Printf("ERROR: flowID doesn't have an entry")
		return
	}
	sort.Slice(fi.eventRecords, func(i, j int) bool {
		return fi.eventRecords[i].ts.Before(fi.eventRecords[j].ts)
	})
	//conntrack flows may be reused together with TCP/UDP ports, when there are many connections.
	// As the result, we can have events for multiple flow "iterations" in the same flowInfo.
	if fi.eventRecords[0].event != created {
		// delete event wasn't received for the first flow iteration
		log.Printf("no delete event for flow: \n%s", printEventRecords(fi.eventRecords))
		for i, er := range fi.eventRecords {
			if er.event == created {
				fi.eventRecords = fi.eventRecords[i:]
				break
			}
		}
	}
	if correctNonTCPEvents(fi.eventRecords) {
		// only seen_reply event is recorded for nonTCP
		establishedLatency := fi.eventRecords[1].ts.Sub(fi.eventRecords[0].ts)
		MetricSvcSeenReplyLatency.WithLabelValues(w.nodeName).Observe(establishedLatency.Seconds())
		MetricSvcSeenReplyLatencySummary.WithLabelValues(w.nodeName).Observe(establishedLatency.Seconds())

		fi.eventRecords = fi.eventRecords[3:]
	} else if tcpEvents, tcpFinIdx, deleteIdx := correctTCPEvents(fi.eventRecords); tcpEvents {
		establishedLatency := fi.eventRecords[1].ts.Sub(fi.eventRecords[0].ts)
		MetricSvcSeenReplyLatency.WithLabelValues(w.nodeName).Observe(establishedLatency.Seconds())
		MetricSvcSeenReplyLatencySummary.WithLabelValues(w.nodeName).Observe(establishedLatency.Seconds())

		duration := fi.eventRecords[tcpFinIdx].ts.Sub(fi.eventRecords[0].ts)
		MetricSvcTCPFinLatency.WithLabelValues(w.nodeName).Observe(duration.Seconds())
		MetricSvcTCPFinLatencySummary.WithLabelValues(w.nodeName).Observe(duration.Seconds())
		MetricSvcTCPFinThroughput.WithLabelValues(w.nodeName, "to-svc").Observe(fi.eventRecords[deleteIdx].toSvcBytes / duration.Seconds())
		MetricSvcTCPFinThroughput.WithLabelValues(w.nodeName, "from-svc").Observe(fi.eventRecords[deleteIdx].fromSvcBytes / duration.Seconds())
		MetricSvcTCPFinThroughputSummary.WithLabelValues(w.nodeName, "to-svc").Observe(fi.eventRecords[deleteIdx].toSvcBytes / duration.Seconds())
		MetricSvcTCPFinThroughputSummary.WithLabelValues(w.nodeName, "from-svc").Observe(fi.eventRecords[deleteIdx].fromSvcBytes / duration.Seconds())

		fi.eventRecords = fi.eventRecords[4:]
	} else {
		deletedIdx := -1
		for i, er := range fi.eventRecords {
			if er.event == deleted {
				deletedIdx = i
				break
			}
		}

		// some event is missing, clean up first flow iteration events
		log.Printf("events order is wrong: \n%s", printEventRecords(fi.eventRecords))
		fi.eventRecords = fi.eventRecords[deletedIdx+1:]
		MetricErrorCounter.WithLabelValues(w.nodeName, ErrorWrongEventsOrder).Inc()
	}
	if len(fi.eventRecords) == 0 {
		delete(w.flowInfos, flowID)
		return
	}
}

// 0 mean no-TCP, which is never received because we do not listen to new conntrack events
func getTCPState(flow *conntrack.Flow) uint8 {
	if flow.ProtoInfo.TCP == nil {
		return 0
	}
	return flow.ProtoInfo.TCP.State
}
