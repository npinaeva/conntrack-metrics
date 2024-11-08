package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const NetfilterConntrackAcctSetting = "/proc/sys/net/netfilter/nf_conntrack_acct"
const NetfilterTimestampSetting = "/proc/sys/net/netfilter/nf_conntrack_timestamp"

func EnableConntrackFeatures() error {
	for _, file := range []string{NetfilterTimestampSetting, NetfilterConntrackAcctSetting} {
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if strings.Trim(string(content), " \n") == "0" {
			err = os.WriteFile(file, []byte("1"), 0644)
			if err != nil {
				return err
			}
			log.Println("Enabled conntrack feature (" + file + " = 1)")
			log.Println("Connections that are already open cannot be tracked.")
		} else {
			log.Printf("Conntrack feature %s is already enabled.\n", file)
		}
	}
	return nil
}

type logWriter struct{}

func (writer logWriter) Write(bytes []byte) (int, error) {
	return fmt.Print(time.Now().UTC().Format("2006-01-02T15:04:05.999999Z ") + string(bytes))
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-c
		log.Printf("Received a signal, terminating.")
		cancel()
	}()
	log.SetFlags(0)
	log.SetOutput(new(logWriter))
	svcSubnet := flag.String("svc-subnet", "10.96.0.0/16", "service subnet is used to filter conntrack entries")
	excludeSubnet := flag.String("exclude-subnet", "", "subnet of svc-subnet that should not be accounted for")
	nodeName := flag.String("node-name", "", "node, in which this script is running")
	summaryTimeout := flag.Int("summary-timeout", 0, "summaries are metrics that expose very precise quantiles, "+
		"but they expire every X minutes. May be useful for benchmarking, max value is 100.")
	metricsBindAddress := flag.String("metrics-bind-address", "", "metric bind address")
	printEvents := flag.Bool("print-events", false, "print conntrack events")

	flag.Parse()
	svcPrefix, err := netip.ParsePrefix(*svcSubnet)
	log.Printf("using svc subnet %v, node-name %s", *svcSubnet, *nodeName)
	if err != nil {
		log.Fatalf("Failed to parse svc prefix: %v", err)
	}
	var excludePrefix *netip.Prefix
	if *excludeSubnet != "" {
		ep, err := netip.ParsePrefix(*excludeSubnet)
		excludePrefix = &ep
		log.Printf("excluding svc subnet %v", *excludeSubnet)
		if err != nil {
			log.Fatalf("Failed to parse exclude subnet prefix: %v", err)
		}
	}

	err = EnableConntrackFeatures()
	if err != nil {
		log.Fatalf("Failed to enable conntrack features: %v", err)
	}

	if *summaryTimeout < 1 {
		log.Fatalf("summary-timeout should be at least 1, got %d", *summaryTimeout)
	}

	if *summaryTimeout > 100 {
		log.Fatalf("summary-timeout should be less than 100, got %d", *summaryTimeout)
	}
	registerMetrics(*nodeName, *summaryTimeout)
	go startMetricsServer(*metricsBindAddress, ctx)

	reader, err := NewNetlinkReader(200, 200000, 150, 200, *nodeName)
	if err != nil {
		log.Fatal(err)
	}
	handler := NewEventsHandler(svcPrefix, excludePrefix, *nodeName, *printEvents)
	handler.Start(ctx, 2, 5, 50000, reader.EventChan)
	if err = reader.StartWorkers(ctx, 5, 10); err != nil {
		log.Println(err)
		cancel()
	}
	return
}
