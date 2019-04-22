/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/gravwell/ingest"
	"github.com/gravwell/ingest/entry"
)

const (
	defaultConfigLoc = `/opt/gravwell/etc/can_capture.conf`
)

var (
	configOverride = flag.String("config-file-override", "", "Override location for configuration file")
	verbose        = flag.Bool("v", false, "Display verbose status updates to stdout")

	ErrInvalidPacket error = errors.New("Invalid packet")

	confLoc      string
	totalPackets uint64
	totalBytes   uint64
	v            bool
)

type results struct {
	Bytes uint64
	Count uint64
	Error error
}

type sniffer struct {
	name      string
	Interface string
	TagName   string
	c         *Cansock
	tag       entry.EntryTag
	src       net.IP
	die       chan bool
	res       chan results
	active    bool
}

func init() {
	flag.Parse()
	if *configOverride == "" {
		confLoc = defaultConfigLoc
	} else {
		confLoc = *configOverride
	}
	v = *verbose
}

func main() {
	cfg, err := GetConfig(confLoc)
	if err != nil {
		log.Fatal("Failed to get configuration: ", err)
	}

	tags, err := cfg.Tags()
	if err != nil {
		log.Fatal("Failed to get tags from configuration: ", err)
	}
	conns, err := cfg.Targets()
	if err != nil {
		log.Fatal("Failed to get backend targets from configuration: ", err)
	}
	debugout("Handling %d tags over %d targets\n", len(tags), len(conns))

	//loop through our sniffers and get a config up for each
	var sniffs []sniffer
	for k, v := range cfg.Sniffer {
		if v == nil {
			closeSniffers(sniffs)
			log.Fatal("Invalid sniffer named ", k, ".  Nil struct")
			return
		}
		var src net.IP
		if v.Source_Override != `` {
			src = net.ParseIP(v.Source_Override)
			if src == nil {
				closeSniffers(sniffs)
				log.Fatal("Source-Override is invalid")
			}
		}
		c, err := New(v.Interface)
		if err != nil {
			log.Fatal("Failed to get new can interface on ", v.Interface, err)
		}
		sniffs = append(sniffs, sniffer{
			name:      k,
			src:       src,
			Interface: v.Interface,
			TagName:   v.Tag_Name,
			c:         c,
			die:       make(chan bool, 1),
			res:       make(chan results, 1),
		})
	}

	//fire up the ingesters
	igst, err := ingest.NewUniformIngestMuxer(conns, tags, cfg.Secret(), "", "", "")
	if err != nil {
		closeSniffers(sniffs)
		log.Fatal("Failed build our ingest system: ", err)
	}
	defer igst.Close()
	debugout("Started ingester muxer\n")
	if err := igst.Start(); err != nil {
		closeSniffers(sniffs)
		log.Fatal("Failed start our ingest system: ", err)
		return
	}
	debugout("Waiting for connections to indexers\n")
	if err := igst.WaitForHot(cfg.Timeout()); err != nil {
		closeSniffers(sniffs)
		log.Fatal("Timedout waiting for backend connections: ", err)
		return
	}
	debugout("Successfully connected to ingesters\n")

	//set tags and source for each sniffer
	for i := range sniffs {
		tag, err := igst.GetTag(sniffs[i].TagName)
		if err != nil {
			closeSniffers(sniffs)
			log.Fatal("Failed to resolve tag ", sniffs[i].TagName, ": ", err)
		}
		sniffs[i].tag = tag
	}

	start := time.Now()

	//register quit signals so we can die gracefully
	quitSig := make(chan os.Signal, 2)
	defer close(quitSig)
	signal.Notify(quitSig, os.Interrupt)

	for i := range sniffs {
		sniffs[i].active = true
		go canIngester(igst, sniffs[i])
	}

	<-quitSig
	requestClose(sniffs)
	res := gatherResponse(sniffs)
	closeHandles(sniffs)
	if err := igst.Close(); err != nil {
		log.Fatal("Failed to close ingester", err)
	}
	durr := time.Since(start)

	if err == nil {
		fmt.Printf("Completed in %v (%s)\n", durr, ingest.HumanSize(res.Bytes))
		fmt.Printf("Total Count: %s\n", ingest.HumanCount(res.Count))
		fmt.Printf("Entry Rate: %s\n", ingest.HumanEntryRate(res.Count, durr))
		fmt.Printf("Ingest Rate: %s\n", ingest.HumanRate(res.Bytes, durr))
	}

}

func rebuildPacketSource(s sniffer) (c *Cansock, ok bool) {
	var threwErr bool
	var err error
	for {
		//we sleep when we first come in
		select {
		case <-time.After(time.Second):
		case <-s.die:
			break
		}
		//sleep over, try to reopen our pcap device
		if c, err = New(s.Interface); err == nil {
			ok = true
			break
		}
		if !threwErr {
			threwErr = true
			debugout("Error: Failed to get cansock device on reopen (%v)\n", err)
		}
		c = nil
	}
	return
}

func packetExtractor(csock *Cansock, c chan []byte) {
	defer close(c)
	for {
		pkt, err := csock.Read()
		if err != nil {
			debugout("Failed to get packet from source: %v\n", err)
			break
		}
		c <- pkt
	}
}

func canIngester(igst *ingest.IngestMuxer, s sniffer) {
	count := uint64(0)
	totalBytes := uint64(0)

	//get a packet source
	ch := make(chan []byte, 1024)
	go packetExtractor(s.c, ch)
	var src net.IP
	if len(s.src) > 0 {
		src = s.src
	}

mainLoop:
	for {
		//check if we are supposed to die
		select {
		case _ = <-s.die:
			s.c.Close()
			break mainLoop
		case pkt, ok := <-ch: //get a packet
			if !ok {
				s.c.Close()
				s.c, ok = rebuildPacketSource(s)
				if !ok {
					break mainLoop
				}
				ch = make(chan []byte, 1024)
				go packetExtractor(s.c, ch)
				continue
			}
			e := &entry.Entry{
				TS:   entry.Now(),
				SRC:  src,
				Tag:  s.tag,
				Data: pkt,
			}
			if err := igst.WriteEntry(e); err != nil {
				s.c.Close()
				fmt.Fprintf(os.Stderr, "Failed to write entry: %v\n", err)
				s.res <- results{
					Bytes: 0,
					Count: 0,
					Error: err,
				}
				return
			}
			totalBytes += uint64(len(e.Data))
			count++
			if v {
				pkt, err := ExtractPacket(pkt)
				if err != nil {
					debugout("Failed to extract: %v\n", err)
				} else {
					debugout("%s\n", pkt.String())
				}
			}
		}
	}

	s.res <- results{
		Bytes: totalBytes,
		Count: count,
		Error: nil,
	}
}

func debugout(format string, args ...interface{}) {
	if !v {
		return
	}
	fmt.Printf(format, args...)
}

func addResults(dst *results, src results) {
	if dst == nil {
		return
	}
	dst.Bytes += src.Bytes
	dst.Count += src.Count
}

func requestClose(sniffs []sniffer) {
	for _, s := range sniffs {
		if s.active {
			s.die <- true
		}
	}
}

func gatherResponse(sniffs []sniffer) results {
	var r results
	for _, s := range sniffs {
		if s.active {
			addResults(&r, <-s.res)
		}
	}
	return r
}

func closeHandles(sniffs []sniffer) {
	for _, s := range sniffs {
		if s.c != nil {
			s.c.Close()
		}
	}
}

func closeSniffers(sniffs []sniffer) results {
	requestClose(sniffs)
	r := gatherResponse(sniffs)
	closeHandles(sniffs)
	return r
}