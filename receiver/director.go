//
// Copyright 2016 Gregory Trubetskoy. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package receiver

import (
	"fmt"
	"log"
	"math"
	"time"

	"github.com/tgres/tgres/cluster"
)

var directorincomingDPMessages = func(rcv chan *cluster.Msg, dpCh chan interface{}) {
	defer func() { recover() }() // if we're writing to a closed channel below

	for {
		m, ok := <-rcv
		if !ok {
			return
		}

		// To get an event back:
		var dp incomingDP
		if err := m.Decode(&dp); err != nil {
			log.Printf("director: msg <- rcv data point decoding FAILED, ignoring this data point.")
			continue
		}

		maxHops := 2
		if dp.Hops > maxHops {
			log.Printf("director: dropping data point, max hops (%d) reached", maxHops)
			continue
		}

		dpCh <- &dp // See recover above
	}
}

var directorForwardDPToNode = func(dp *incomingDP, node *cluster.Node, snd chan *cluster.Msg) error {
	if dp.Hops == 0 { // we do not forward more than once
		if node.Ready() {
			dp.Hops++
			msg, _ := cluster.NewMsg(node, dp) // can't possibly error
			snd <- msg
		} else {
			return fmt.Errorf("directorForwardDPToNode: Node is not ready")
		}
	}
	return nil
}

var directorProcessDataPoint = func(cds *cachedDs, dsf dsFlusherBlocking) int {
	cnt, err := cds.processIncoming()
	if err != nil {
		log.Printf("directorProcessDataPoint [%v] error: %v", cds.Ident(), err)
	}
	if cds.PointCount() > 0 {
		dsf.flushDs(cds.DbDataSourcer, false)
	}
	return cnt
}

var directorProcessOrForward = func(dsc *dsCache, cds *cachedDs, clstr clusterer, dsf dsFlusherBlocking, snd chan *cluster.Msg) (accepted, forwarded int, dest string) {
	if clstr == nil {
		accepted = directorProcessDataPoint(cds, dsf)
		return accepted, 0, ""
	}

	for _, node := range clstr.NodesForDistDatum(&distDs{DbDataSourcer: cds.DbDataSourcer, dsc: dsc}) {
		if node.Name() == clstr.LocalNode().Name() {
			accepted = directorProcessDataPoint(cds, dsf)
		} else {
			dest = node.SanitizedAddr()
			for _, dp := range cds.incoming {
				if err := directorForwardDPToNode(dp, node, snd); err != nil {
					log.Printf("director: Error forwarding a data point: %v", err)
					// TODO For not ready error - sleep and return the dp to the channel?
					continue
				}
				forwarded++
			}
			cds.incoming = nil
			// Always clear RRAs to prevent it from being saved
			if pc := cds.PointCount(); pc > 0 {
				log.Printf("director: WARNING: Clearing DS with PointCount > 0: %v", pc)
			}
			cds.ClearRRAs(true)
		}
	}
	return
}

var directorProcessIncomingDP = func(dp *incomingDP, sr statReporter, dsc *dsCache, loaderCh chan interface{}, dsf dsFlusherBlocking, clstr clusterer, snd chan *cluster.Msg) {

	sr.reportStatCount("receiver.datapoints.total", 1)

	if math.IsNaN(dp.Value) {
		// NaN is meaningless, e.g. "the thermometer is
		// registering a NaN". Or it means that "for certain it is
		// offline", but that is not part of our scope. You can
		// only get a NaN by exceeding HB. Silently ignore it.
		return
	}

	cds := dsc.getByIdentOrCreateEmpty(dp.Ident)
	if cds == nil {
		sr.reportStatCount("receiver.datapoints.dropped", 1)
		if debug {
			log.Printf("director: No spec matched ident: %#v, ignoring data point", dp.Ident)
		}
		return
	}

	cds.appendIncoming(dp)

	if cds.Id() == 0 {
		// this DS needs to be loaded.
		loaderCh <- cds
	} else {
		accepted, forwarded, dest := directorProcessOrForward(dsc, cds, clstr, dsf, snd)
		if forwarded > 0 {
			sr.reportStatCount(fmt.Sprintf("receiver.forwarded_to.%s", dest), float64(forwarded))
			sr.reportStatCount("receiver.datapoints.forwarded", float64(forwarded))
		} else if accepted > 0 {
			sr.reportStatCount("receiver.datapoints.accepted", float64(accepted))
		}
	}
}

func reportOverrunQueueSize(queue *fifoQueue, sr statReporter, nap time.Duration) {
	for {
		time.Sleep(nap) // TODO this should be a ticker really
		sr.reportStatGauge("receiver.queue_len", float64(queue.size()))
	}
}

var loader = func(loaderCh, dpCh chan interface{}, dsc *dsCache, sr statReporter) {

	var queue = &fifoQueue{}

	go func() {
		for {
			time.Sleep(time.Second)
			sr.reportStatGauge("receiver.load_queue_len", float64(queue.size()))
		}
	}()

	loaderOutCh := make(chan interface{})
	go elasticCh(loaderCh, loaderOutCh, queue)

	for {
		x, ok := <-loaderOutCh
		if !ok {
			log.Printf("loader: channel closed, closing director channel and exiting...")
			close(dpCh)
			return
		}

		cds := x.(*cachedDs)

		if cds.spec != nil { // nil spec means it's been loaded already
			if err := dsc.fetchOrCreateByIdent(cds); err != nil {
				log.Printf("loader: database error: %v", err)
				continue
			}
		}

		if cds.Created() {
			sr.reportStatCount("receiver.created", 1)
		}

		dpCh <- cds
	}
}

var director = func(wc wController, dpCh chan interface{}, clstr clusterer, sr statReporter, dsc *dsCache, dsf dsFlusherBlocking) {
	wc.onEnter()
	defer wc.onExit()

	var (
		clusterChgCh chan bool
		snd, rcv     chan *cluster.Msg
		queue        = &fifoQueue{}
	)

	if clstr != nil {
		clusterChgCh = clstr.NotifyClusterChanges() // Monitor Cluster changes
		snd, rcv = clstr.RegisterMsgType()          // Channel for event forwards to other nodes and us
		go directorincomingDPMessages(rcv, dpCh)
		log.Printf("director: marking cluster node as Ready.")
		clstr.Ready(true)
	}

	go reportOverrunQueueSize(queue, sr, time.Second)

	dpOutCh := make(chan interface{}, 128)
	go elasticCh(dpCh, dpOutCh, queue)

	loaderCh := make(chan interface{}, 128)
	go loader(loaderCh, dpCh, dsc, sr)

	wc.onStarted()

	for {
		var (
			x   interface{}
			dp  *incomingDP
			cds *cachedDs
			ok  bool
		)
		select {
		case _, ok = <-clusterChgCh:
			if ok {
				if err := clstr.Transition(45 * time.Second); err != nil {
					log.Printf("director: Transition error: %v", err)
				}
			}
			continue
		case x, ok = <-dpOutCh:
			switch x := x.(type) {
			case *incomingDP:
				dp = x
			case *cachedDs:
				cds = x
			case nil:
				// close signal
			default:
				log.Printf("director(): unknown type: %T", x)
			}
		}

		if !ok {
			log.Printf("director: exiting the director goroutine.")
			return
		}

		if dp != nil {
			// if the dp ident is not found, it will be submitted to
			// the loader, which will return it to us through the dpCh
			// as a cachedDs.
			directorProcessIncomingDP(dp, sr, dsc, loaderCh, dsf, clstr, snd)
		} else if cds != nil {
			// this came from the loader, we do not need to look it up
			accepted, forwarded, dest := directorProcessOrForward(dsc, cds, clstr, dsf, snd)
			if forwarded > 0 {
				sr.reportStatCount(fmt.Sprintf("receiver.forwarded_to.%s", dest), float64(forwarded))
				sr.reportStatCount("receiver.datapoints.forwarded", float64(forwarded))
			} else if accepted > 0 {
				sr.reportStatCount("receiver.datapoints.accepted", float64(accepted))
			}
		} else {
			// signal to exit
			log.Printf("director: closing loader channel.")
			close(loaderCh)
		}
	}
}
