package metrics

import (
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/akaere/autopeer-agent/internal/bird"
	"github.com/akaere/autopeer-agent/internal/handler"
	"github.com/akaere/autopeer-agent/internal/store"
	"github.com/akaere/autopeer-agent/internal/wg"
	"github.com/akaere/autopeer-agent/internal/ws"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "metrics")

var processStart = time.Now()

type NodeMetrics struct {
	MemAllocMB   uint64 `json:"mem_alloc_mb,omitempty"`
	MemSysMB     uint64 `json:"mem_sys_mb,omitempty"`
	NumGoroutine int    `json:"num_goroutine,omitempty"`
	UptimeSecs   int64  `json:"uptime_secs,omitempty"`
}

type PeerMetric struct {
	PeerID           string   `json:"peer_id"`
	ASN              int64    `json:"asn"`
	WgInterface      string   `json:"wg_interface"`
	RxBytes          int64    `json:"wg_rx_bytes"`
	TxBytes          int64    `json:"wg_tx_bytes"`
	LastHandshake    string   `json:"wg_last_handshake"`
	BGPState         string   `json:"bgp_state"`
	RttMs            *float64 `json:"rtt_ms"`
	RoutesImported   *int     `json:"routes_imported,omitempty"`
	RoutesExported   *int     `json:"routes_exported,omitempty"`
	RoutesPreferred  *int     `json:"routes_preferred,omitempty"`
	BGPUptimeSecs    *int     `json:"bgp_uptime_secs,omitempty"`
	WgActualEndpoint string   `json:"wg_actual_endpoint,omitempty"`
}

type Collector struct {
	wsClient        *ws.Client
	wgMgr           *wg.Manager
	handler         *handler.Handler
	interval        time.Duration
	birdDetailEvery uint64
	done            chan struct{}
	version         string
	verMu           sync.RWMutex
	tickCount       uint64
}

func NewCollector(wsClient *ws.Client, wgMgr *wg.Manager, h *handler.Handler, intervalSec, birdDetailEvery int) *Collector {
	if birdDetailEvery < 1 {
		birdDetailEvery = 2
	}
	return &Collector{
		wsClient:        wsClient,
		wgMgr:           wgMgr,
		handler:         h,
		interval:        time.Duration(intervalSec) * time.Second,
		birdDetailEvery: uint64(birdDetailEvery),
		done:            make(chan struct{}),
	}
}

func (c *Collector) SetVersion(v string) {
	c.verMu.Lock()
	c.version = v
	c.verMu.Unlock()
}

func (c *Collector) versionStr() string {
	c.verMu.RLock()
	defer c.verMu.RUnlock()
	return c.version
}

func (c *Collector) Run() {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.collect()
		case <-c.done:
			return
		}
	}
}

func (c *Collector) collect() {
	log.Debug("collecting metrics")

	stats, err := c.wgMgr.GetStats()
	if err != nil {
		log.WithError(err).Error("metrics collection failed")
		return
	}

	ifToPeer := make(map[string]string)
	ifToInfo := make(map[string]store.PeerInfo)
	protoToWG := make(map[string]string)
	c.handler.ForEachPeer(func(peerID string, info store.PeerInfo) error {
		ifName := info.WgInterface
		if ifName == "" {
			ifName = wg.InterfaceName(info.ASN)
		}
		ifToPeer[ifName] = peerID
		ifToInfo[ifName] = info
		if info.BgpProtoName != "" {
			protoToWG[info.BgpProtoName] = ifName
		}
		return nil
	})

	birdByProto := make(map[string]bird.PeerState)
	for _, s := range c.handler.GetBirdStates() {
		birdByProto[s.Name] = s
	}

	var birdDetails []bird.PeerDetail
	c.tickCount++
	if c.tickCount%c.birdDetailEvery == 0 {
		if details, err := c.handler.GetBirdDetails(); err == nil {
			birdDetails = details
		}
	}
	birdDetailByProto := make(map[string]bird.PeerDetail)
	for _, d := range birdDetails {
		birdDetailByProto[d.Name] = d
	}

	var wg2 sync.WaitGroup
	sem := make(chan struct{}, 5)
	rttResults := make(map[string]*float64)
	var rttMu sync.Mutex

	for _, s := range stats {
		info, okInfo := ifToInfo[s.Name]
		if _, okPeer := ifToPeer[s.Name]; !okPeer {
			continue
		}
		if !okInfo || info.RemoteLLA == "" {
			continue
		}

		wg2.Add(1)
		go func(s wg.InterfaceStats, remoteLLA string) {
			defer wg2.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			rttMs := wg.PingPeer(remoteLLA, s.Name)
			if rttMs >= 0 {
				rttMu.Lock()
				rttResults[s.Name] = &rttMs
				rttMu.Unlock()
			}
		}(s, info.RemoteLLA)
	}
	wg2.Wait()

	metrics := make([]PeerMetric, 0, len(stats))
	for _, s := range stats {
		peerID, ok := ifToPeer[s.Name]
		if !ok {
			continue
		}

		rttPtr := rttResults[s.Name]

		m := PeerMetric{
			PeerID:           peerID,
			WgInterface:      s.Name,
			RxBytes:          s.RxBytes,
			TxBytes:          s.TxBytes,
			LastHandshake:    s.LastHandshake,
			RttMs:            rttPtr,
			WgActualEndpoint: s.ActualEndpoint,
		}

		if info, ok := ifToInfo[s.Name]; ok {
			m.ASN = info.ASN
			protoName := info.BgpProtoName
			if protoName == "" {
				protoName = fmt.Sprintf("dn42_%d", info.ASN)
			}
			if bs, ok := birdByProto[protoName]; ok {
				m.BGPState = bs.State
			}
			if bd, ok := birdDetailByProto[protoName]; ok {
				m.RoutesImported = &bd.RoutesImported
				m.RoutesExported = &bd.RoutesExported
				m.RoutesPreferred = &bd.RoutesPreferred
				m.BGPUptimeSecs = &bd.BGPUptimeSecs
			}
		}

		var rttLog float64
		if rttPtr != nil {
			rttLog = *rttPtr
		} else {
			rttLog = -1
		}
		log.WithFields(logrus.Fields{
			"peer":      peerID,
			"interface": s.Name,
			"rx_bytes":  s.RxBytes,
			"tx_bytes":  s.TxBytes,
			"rtt_ms":    rttLog,
			"bgp_state": m.BGPState,
		}).Debug("peer metrics collected")

		metrics = append(metrics, m)
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	nodeMetrics := NodeMetrics{
		MemAllocMB:   m.HeapAlloc / 1024 / 1024,
		MemSysMB:     m.Sys / 1024 / 1024,
		NumGoroutine: runtime.NumGoroutine(),
		UptimeSecs:   int64(time.Since(processStart).Seconds()),
	}

	data, err := json.Marshal(map[string]any{
		"peers":        metrics,
		"version":      c.versionStr(),
		"node_metrics": nodeMetrics,
	})
	if err != nil {
		log.WithError(err).Error("failed to marshal heartbeat payload")
		return
	}

	log.WithField("peer_count", len(metrics)).Debug("sending heartbeat")

	if err := c.wsClient.Send(ws.Message{
		Type:    "heartbeat",
		Payload: data,
	}); err != nil {
		log.WithError(err).Error("failed to send heartbeat")
		return
	}

	log.WithField("peer_count", len(metrics)).Info("heartbeat sent")
}

func (c *Collector) Stop() {
	close(c.done)
}
