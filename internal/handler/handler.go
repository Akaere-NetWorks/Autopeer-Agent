package handler

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/akaere/autopeer-agent/internal/bird"
	"github.com/akaere/autopeer-agent/internal/crypto"
	"github.com/akaere/autopeer-agent/internal/store"
	"github.com/akaere/autopeer-agent/internal/wg"
	"github.com/akaere/autopeer-agent/internal/ws"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "handler")

type PeerAddPayload struct {
	PeerID         string  `json:"peer_id"`
	ASN            int64   `json:"asn"`
	RemoteWgPubkey string  `json:"remote_wg_pubkey"`
	RemoteEndpoint string  `json:"remote_endpoint"`
	RemoteLLA      string  `json:"remote_lla"`
	ListenPort     int     `json:"listen_port"`
	WgInterface    string  `json:"wg_interface"`
	MTU            *int    `json:"mtu,omitempty"`
	WgPreSharedKey *string `json:"wg_preshared_key,omitempty"`
}

type PeerRemovePayload struct {
	PeerID string `json:"peer_id"`
	ASN    int64  `json:"asn"`
}

type StatusRequestPayload struct {
	PeerID string `json:"peer_id,omitempty"`
}

type BirdControlPayload struct {
	PeerID    string `json:"peer_id"`
	ProtoName string `json:"proto_name"`
}

type Handler struct {
	wgMgr      *wg.Manager
	birdMgr    *bird.Manager
	wsClient   *ws.Client
	store      *store.Store
	agentToken string
	keypair    *crypto.KeyPair

	pendingKeyXMu sync.Mutex
	pendingKeyX   chan ws.Message
}

func NewHandler(wgMgr *wg.Manager, birdMgr *bird.Manager, s *store.Store, agentToken string, kp *crypto.KeyPair) *Handler {
	return &Handler{
		wgMgr:      wgMgr,
		birdMgr:    birdMgr,
		store:      s,
		agentToken: agentToken,
		keypair:    kp,
	}
}

func (h *Handler) SetWSClient(client *ws.Client) {
	h.wsClient = client
}

func (h *Handler) HandleMessage(msg ws.Message) {
	log.Debugf("HandleMessage: type=%s id=%s", msg.Type, msg.ID)

	switch msg.Type {
	case "peer.add":
		h.handlePeerAdd(msg)
	case "peer.remove":
		h.handlePeerRemove(msg)
	case "status.request":
		h.handleStatusRequest(msg)
	case "peers.sync":
		h.handlePeersSync(msg)
	case "peers.import":
		h.handlePeersImportCommand(msg)
	case "bird.details":
		h.handleBirdDetailsCommand(msg)
	case "agent.update":
		h.handleAgentUpdate(msg)
	case "agent.resume":
		h.handleAgentResume(msg)
	case "agent.rollback":
		h.handleAgentRollback(msg)
	case "network.ping":
		h.handleNetworkPing(msg)
	case "network.trace":
		h.handleNetworkTrace(msg)
	case "network.mtr":
		h.handleNetworkMTR(msg)
	case "network.bgp_route":
		h.handleBirdRoute(msg)
	case "key.init_ack":
		h.handleKeyInitAck(msg)
	case "key.auth_ack":
		h.handleKeyAuthAck(msg)
	case "bird.disable":
		h.handleBirdDisable(msg)
	case "bird.enable":
		h.handleBirdEnable(msg)
	default:
		log.Warnf("Unknown message type: %s", msg.Type)
	}
}

func (h *Handler) handlePeerAdd(msg ws.Message) {
	var payload PeerAddPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	log.Infof("Adding peer: ASN %d, interface %s", payload.ASN, payload.WgInterface)

	iface := payload.WgInterface
	if iface == "" {
		iface = wg.InterfaceName(payload.ASN)
	}

	if _, err := h.store.GetPeer(payload.PeerID); err == nil && h.wgMgr.IsPeerUp(iface) {
		h.sendResponse(msg.ID, true, "already active")
		return
	}

	if h.wgMgr.IsPeerUp(iface) {
		log.Warnf("peer interface %s already exists, tearing down first", iface)
		h.wgMgr.RemovePeerByIface(iface)
	}

	if err := h.wgMgr.AddPeerWithIface(iface, payload.ASN, payload.ListenPort, payload.RemoteWgPubkey, payload.RemoteEndpoint, payload.MTU, payload.WgPreSharedKey); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("wg add failed: %v", err))
		return
	}
	log.Debugf("handlePeerAdd: WG add success for ASN %d on %s", payload.ASN, iface)

	if err := h.birdMgr.AddPeer(payload.ASN, payload.RemoteLLA, iface); err != nil {
		h.wgMgr.RemovePeerByIface(iface)
		h.sendResponse(msg.ID, false, fmt.Sprintf("bird add failed: %v", err))
		return
	}
	log.Debugf("handlePeerAdd: BIRD add success for ASN %d", payload.ASN)

	if err := h.store.PutPeer(payload.PeerID, store.PeerInfo{
		ASN:                payload.ASN,
		RemoteLLA:          payload.RemoteLLA,
		WgInterface:        iface,
		BgpProtoName:       fmt.Sprintf("dn42_%d", payload.ASN),
		BirdConfigFilename: fmt.Sprintf("dn42_%d.conf", payload.ASN),
	}); err != nil {
		h.birdMgr.RemovePeer(payload.ASN)
		h.wgMgr.RemovePeerByIface(iface)
		h.sendResponse(msg.ID, false, fmt.Sprintf("store put failed: %v", err))
		return
	}
	log.Debugf("handlePeerAdd: store.PutPeer %s -> ASN %d", payload.PeerID, payload.ASN)

	h.sendResponse(msg.ID, true, "peer added successfully")
}

func (h *Handler) handlePeerRemove(msg ws.Message) {
	var payload PeerRemovePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	asn := payload.ASN
	if asn == 0 {
		if info, err := h.store.GetPeer(payload.PeerID); err == nil {
			log.Debugf("handlePeerRemove: resolved ASN %d from store for peer %s", info.ASN, payload.PeerID)
			asn = info.ASN
		}
	}

	if asn == 0 {
		h.sendResponse(msg.ID, false, "unknown peer")
		return
	}

	log.Infof("Removing peer: ASN %d", asn)

	var storedInfo *store.PeerInfo
	if info, err := h.store.GetPeer(payload.PeerID); err == nil {
		storedInfo = info
	}

	if storedInfo != nil && storedInfo.BirdConfigFilename != "" {
		if err := h.birdMgr.RemovePeerByFilename(storedInfo.BirdConfigFilename); err != nil {
			log.Warnf("bird remove warning: %v", err)
		}
	} else {
		if err := h.birdMgr.RemovePeer(asn); err != nil {
			log.Warnf("bird remove warning: %v", err)
		}
	}

	iface := ""
	if storedInfo != nil && storedInfo.WgInterface != "" {
		iface = storedInfo.WgInterface
	}
	if iface != "" {
		if err := h.wgMgr.RemovePeerByIface(iface); err != nil {
			log.Warnf("wg remove warning: %v", err)
		}
	} else {
		if err := h.wgMgr.RemovePeer(asn); err != nil {
			log.Warnf("wg remove warning: %v", err)
		}
	}

	h.store.DeletePeer(payload.PeerID)

	h.sendResponse(msg.ID, true, "peer removed successfully")
}

func (h *Handler) handleStatusRequest(msg ws.Message) {
	wgStats, err := h.wgMgr.GetStats()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("wg stats error: %v", err))
		return
	}

	birdStates, err := h.birdMgr.GetPeerStates()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("bird states error: %v", err))
		return
	}

	log.Debugf("handleStatusRequest: %d wg stats, %d bird states", len(wgStats), len(birdStates))

	status := map[string]interface{}{
		"wg_stats":    wgStats,
		"bird_states": birdStates,
	}

	data, err := json.Marshal(status)
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal status: %v", err))
		return
	}
	h.wsClient.Send(ws.Message{
		Type:    "status.response",
		ID:      msg.ID,
		Payload: data,
	})
}

func (h *Handler) sendResponse(msgID string, success bool, message string) {
	if h.wsClient == nil {
		return
	}

	var payload json.RawMessage
	if message != "" {
		payload, _ = json.Marshal(map[string]string{"message": message})
	}

	msg := ws.Message{
		Type:    "response",
		ID:      msgID,
		Payload: payload,
	}
	if success {
		t := true
		msg.Success = &t
	} else {
		msg.Error = message
	}

	if err := h.wsClient.Send(msg); err != nil {
		log.WithError(err).WithField("msg_id", msgID).Warn("sendResponse: failed to send response")
	}
}

func (h *Handler) sendResponseChecked(msgID string, success bool, message string) error {
	if h.wsClient == nil {
		return fmt.Errorf("wsClient is nil")
	}

	var payload json.RawMessage
	if message != "" {
		payload, _ = json.Marshal(map[string]string{"message": message})
	}

	msg := ws.Message{
		Type:    "response",
		ID:      msgID,
		Payload: payload,
	}
	if success {
		t := true
		msg.Success = &t
	} else {
		msg.Error = message
	}
	return h.wsClient.Send(msg)
}

func (h *Handler) ForEachPeer(fn func(peerID string, info store.PeerInfo) error) error {
	return h.store.ForEachPeer(fn)
}

type syncResult struct {
	Success []string `json:"applied"`
	Failed  []string `json:"failed"`
}

func (h *Handler) handlePeersSync(msg ws.Message) {
	var payload struct {
		Peers []struct {
			PeerID             string  `json:"peer_id"`
			ASN                int64   `json:"asn"`
			RemoteEndpoint     string  `json:"remote_endpoint"`
			RemoteWgPubkey     string  `json:"remote_wg_pubkey"`
			RemoteLLA          string  `json:"remote_lla"`
			ListenPort         int     `json:"listen_port"`
			WgInterface        string  `json:"wg_interface"`
			BgpProtoName       string  `json:"bgp_proto_name"`
			BirdConfigFilename string  `json:"bird_config_filename"`
			MTU                *int    `json:"mtu,omitempty"`
			WgPreSharedKey     *string `json:"wg_preshared_key,omitempty"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		log.Errorf("peers.sync unmarshal error: %v", err)
		return
	}

	m := make(map[string]store.PeerInfo, len(payload.Peers))
	result := syncResult{
		Success: make([]string, 0),
		Failed:  make([]string, 0),
	}
	for i, p := range payload.Peers {
		log.Debugf("handlePeersSync: processing peer %d/%d: id=%s asn=%d", i+1, len(payload.Peers), p.PeerID, p.ASN)

		if p.ASN <= 0 || p.PeerID == "" {
			continue
		}

		iface := p.WgInterface
		if iface == "" {
			iface = wg.InterfaceName(p.ASN)
		}

		failed := false

		wgConf := filepath.Join(h.wgMgr.ConfigDir(), iface+".conf")
		_, wgStatErr := os.Stat(wgConf)
		wgExists := !os.IsNotExist(wgStatErr)

		if p.RemoteWgPubkey != "" {
			if !wgExists || !h.wgMgr.IsPeerUp(iface) {
				log.Infof("peers.sync: recreating WG interface %s for peer %s", iface, p.PeerID)
				if err := h.wgMgr.AddPeerWithIface(iface, p.ASN, p.ListenPort, p.RemoteWgPubkey, p.RemoteEndpoint, p.MTU, p.WgPreSharedKey); err != nil {
					log.Errorf("peers.sync: wg add failed for peer %s: %v", p.PeerID, err)
					failed = true
				}
			}
		} else if !wgExists {
			log.Warnf("peers.sync: skip peer %s, no pubkey and no existing WG config", p.PeerID)
			continue
		}

		birdConf := p.BirdConfigFilename
		if birdConf == "" {
			birdConf = fmt.Sprintf("dn42_%d.conf", p.ASN)
		}
		birdConfPath := filepath.Join(h.birdMgr.ConfigDir(), birdConf)
		if _, err := os.Stat(birdConfPath); os.IsNotExist(err) {
			log.Infof("peers.sync: recreating BIRD config %s for peer %s", birdConf, p.PeerID)
			if err := h.birdMgr.AddPeer(p.ASN, p.RemoteLLA, iface); err != nil {
				log.Errorf("peers.sync: bird add failed for peer %s: %v", p.PeerID, err)
				failed = true
			}
		}

		if failed {
			result.Failed = append(result.Failed, p.PeerID)
		} else {
			result.Success = append(result.Success, p.PeerID)
		}

		protoName := p.BgpProtoName
		if protoName == "" {
			protoName = fmt.Sprintf("dn42_%d", p.ASN)
		}

		birdFilenameForStore := p.BirdConfigFilename
		if birdFilenameForStore == "" {
			birdFilenameForStore = fmt.Sprintf("dn42_%d.conf", p.ASN)
		}
		m[p.PeerID] = store.PeerInfo{
			ASN:                p.ASN,
			RemoteLLA:          p.RemoteLLA,
			WgInterface:        iface,
			BgpProtoName:       protoName,
			BirdConfigFilename: birdFilenameForStore,
		}
	}

	// Collect stale peers before overwriting the store, then tear them down
	// after ReplaceAll so the store is always consistent with the desired state
	// even if a teardown step fails or the process crashes mid-way.
	var stalePeers []store.PeerInfo
	h.store.ForEachPeer(func(peerID string, info store.PeerInfo) error {
		if _, exists := m[peerID]; !exists {
			stalePeers = append(stalePeers, info)
		}
		return nil
	})

	h.store.ReplaceAll(m)
	log.Debugf("handlePeersSync: ReplaceAll stored %d peers", len(m))

	for _, info := range stalePeers {
		iface := info.WgInterface
		if iface == "" {
			iface = wg.InterfaceName(info.ASN)
		}
		log.Infof("peers.sync: removing stale peer ASN=%d (iface=%s)", info.ASN, iface)
		if info.BirdConfigFilename != "" {
			h.birdMgr.RemovePeerByFilename(info.BirdConfigFilename)
		} else {
			h.birdMgr.RemovePeer(info.ASN)
		}
		h.wgMgr.RemovePeerByIface(iface)
	}

	log.Infof("peers.sync: loaded %d active peers from center", len(payload.Peers))

	data, err := json.Marshal(result)
	if err != nil {
		log.WithError(err).Error("handlePeersSync: marshal result failed")
		return
	}
	h.wsClient.Send(ws.Message{
		Type:    "response",
		ID:      msg.ID,
		Payload: data,
	})
}

func (h *Handler) RequestSync() {
	if h.wsClient == nil {
		return
	}
	h.wsClient.Send(ws.Message{
		Type: "peers.sync",
	})
}

type ImportedPeer struct {
	ASN                int64  `json:"asn"`
	RemotePubkey       string `json:"remote_pubkey"`
	RemoteEndpoint     string `json:"remote_endpoint"`
	RemoteLLA          string `json:"remote_lla"`
	WgListenPort       int    `json:"wg_listen_port"`
	WgInterface        string `json:"wg_interface_name"`
	BgpProtoName       string `json:"bgp_proto_name"`
	BirdConfigFilename string `json:"bird_config_filename"`
	WgManaged          bool   `json:"wg_managed"`
}

func (h *Handler) handlePeersImportCommand(msg ws.Message) {
	wgPeers, err := h.wgMgr.ScanPeers()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("wg scan error: %v", err))
		return
	}

	bgpPeers, err := h.birdMgr.ScanPeers()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("bird scan error: %v", err))
		return
	}
	log.Debugf("handlePeersImportCommand: scanned %d WG peers, %d BGP peers", len(wgPeers), len(bgpPeers))

	bgpByIface := make(map[string]bird.ScannedBGP)
	bgpByASN := make(map[int64]bird.ScannedBGP)
	for _, bp := range bgpPeers {
		bgpByIface[bp.WgInterface] = bp
		bgpByASN[bp.ASN] = bp
	}

	imported := make([]ImportedPeer, 0)
	matchedBGP := make(map[string]bool)

	for _, wp := range wgPeers {
		var realASN int64
		var remoteLLA string
		var bgpProto string
		var birdFilename string
		var matched bool

		if bp, ok := bgpByIface[wp.InterfaceName]; ok {
			realASN = bp.ASN
			remoteLLA = bp.RemoteLLA
			bgpProto = bp.ProtoName
			birdFilename = bp.Filename
			matched = true
			matchedBGP[bp.WgInterface] = true
		} else if bp, ok := bgpByASN[wp.ASN]; ok {
			realASN = bp.ASN
			remoteLLA = bp.RemoteLLA
			bgpProto = bp.ProtoName
			birdFilename = bp.Filename
			matched = true
			matchedBGP[bp.WgInterface] = true
		}

		if !matched {
			realASN = wp.ASN
		}

		if tracked, _ := h.store.IsASNTracked(realASN); tracked {
			continue
		}
		imported = append(imported, ImportedPeer{
			ASN: realASN, RemotePubkey: wp.RemotePubkey,
			RemoteEndpoint: wp.RemoteEndpoint, RemoteLLA: remoteLLA,
			WgListenPort: wp.ListenPort, WgInterface: wp.InterfaceName,
			BgpProtoName: bgpProto, BirdConfigFilename: birdFilename,
			WgManaged: true,
		})
	}

	for _, bp := range bgpPeers {
		if tracked, _ := h.store.IsASNTracked(bp.ASN); tracked {
			continue
		}
		if matchedBGP[bp.WgInterface] {
			continue
		}
		imported = append(imported, ImportedPeer{
			ASN: bp.ASN, RemoteLLA: bp.RemoteLLA,
			WgInterface: bp.WgInterface, BgpProtoName: bp.ProtoName,
			BirdConfigFilename: bp.Filename,
			WgManaged:          false,
		})
	}

	data, err := json.Marshal(map[string]any{"peers": imported})
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal peers: %v", err))
		return
	}
	h.wsClient.Send(ws.Message{
		Type: "response", ID: msg.ID, Payload: data,
	})
	log.Infof("ImportPeers: scanned %d peers for center", len(imported))
}

func (h *Handler) GetBirdStates() []bird.PeerState {
	states, err := h.birdMgr.GetPeerStates()
	if err != nil {
		log.Errorf("GetBirdStates error: %v", err)
		return nil
	}
	return states
}

func (h *Handler) GetBirdDetails() ([]bird.PeerDetail, error) {
	return h.birdMgr.GetPeerDetails()
}

func (h *Handler) handleBirdDetailsCommand(msg ws.Message) {
	details, err := h.birdMgr.GetPeerDetails()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("birdc error: %v", err))
		return
	}

	type detailResp struct {
		Name           string `json:"name"`
		State          string `json:"state"`
		RoutesImported int    `json:"routes_imported"`
		RoutesExported int    `json:"routes_exported"`
		UptimeSecs     int    `json:"bgp_uptime_secs"`
	}
	resp := make([]detailResp, 0, len(details))
	for _, d := range details {
		log.Debugf("handleBirdDetailsCommand: protocol %s state=%s imported=%d exported=%d uptime=%ds",
			d.Name, d.State, d.RoutesImported, d.RoutesExported, d.BGPUptimeSecs)
		resp = append(resp, detailResp{
			Name: d.Name, State: d.State,
			RoutesImported: d.RoutesImported,
			RoutesExported: d.RoutesExported,
			UptimeSecs:     d.BGPUptimeSecs,
		})
	}

	data, err := json.Marshal(map[string]any{"details": resp})
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal details: %v", err))
		return
	}
	h.wsClient.Send(ws.Message{
		Type: "response", ID: msg.ID, Payload: data,
	})
	log.Infof("bird.details: returned %d protocols", len(resp))
}

func (h *Handler) handleAgentUpdate(msg ws.Message) {
	var payload struct {
		UpdateID string `json:"update_id"`
		Version  string `json:"version"`
		URL      string `json:"url"`
		SHA256   string `json:"sha256"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	if appliedID, _ := h.store.GetUpdateID(); appliedID == payload.UpdateID {
		h.sendResponse(msg.ID, true, "already applied")
		return
	}

	execPath, err := os.Executable()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("get executable path: %v", err))
		return
	}
	currentHash, err := sha256File(execPath)
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("hash current binary: %v", err))
		return
	}
	if currentHash == payload.SHA256 {
		h.sendResponse(msg.ID, true, "already applied (hash match)")
		h.store.PutUpdateID(payload.UpdateID)
		return
	}

	currentVer, _ := h.store.GetVersion()
	if currentVer == "" {
		h.sendResponse(msg.ID, false, "cannot update: current version unknown (store may be cleared)")
		return
	}

	log.Infof("Agent update: downloading %s (version=%s)", payload.URL, payload.Version)

	log.Debugf("handleAgentUpdate: DownloadAndApply url=%s version=%s", payload.URL, payload.Version)
	if err := DownloadAndApply(payload.URL, payload.SHA256, payload.Version, currentVer, h.agentToken); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("update failed: %v", err))
		return
	}

	data, err := json.Marshal(map[string]string{"update_id": payload.UpdateID})
	if err != nil {
		log.WithError(err).Error("handleAgentUpdate: marshal update_id failed")
	} else {
		if err := h.wsClient.Send(ws.Message{Type: "agent.updating", Payload: data}); err != nil {
			log.WithError(err).Warn("handleAgentUpdate: failed to send agent.updating message")
		}
	}

	h.store.PutVersion(payload.Version)
	log.Debugf("handleAgentUpdate: store.PutVersion %s", payload.Version)
	h.store.PutUpdateID(payload.UpdateID)
	log.Debugf("handleAgentUpdate: store.PutUpdateID %s", payload.UpdateID)

	if err := h.sendResponseChecked(msg.ID, true, "update applied, restarting"); err != nil {
		log.WithError(err).Warn("handleAgentUpdate: failed to send ACK, proceeding with restart anyway")
	}
	log.Info("Agent update: restarting...")
	h.store.Close()
	time.Sleep(2 * time.Second)
	if h.wsClient != nil {
		h.wsClient.Close()
	}
	RestartSelf()
}

func (h *Handler) handleAgentResume(msg ws.Message) {
	log.Info("agent.resume: merge complete, resuming normal operations")
	h.sendResponse(msg.ID, true, "resumed")
}

func (h *Handler) handleAgentRollback(msg ws.Message) {
	oldVer, _ := h.store.GetVersion()
	if oldVer == "" {
		h.sendResponse(msg.ID, false, "no previous version to roll back to")
		return
	}

	backupPath := BackupPath(oldVer)
	log.Debugf("handleAgentRollback: backup path=%s version=%s", backupPath, oldVer)
	if err := RollbackFrom(backupPath); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("rollback failed: %v", err))
		return
	}

	h.sendResponse(msg.ID, true, "rollback applied, restarting")
	log.Info("Agent rollback: restarting...")
	RestartSelf()
}

func (h *Handler) MakeKeyExchangeHandler() ws.KeyExchangeHandler {
	return func(client *ws.Client) error {
		if h.keypair == nil {
			log.Debug("no keypair, skipping key exchange")
			return nil
		}

		ch := make(chan ws.Message, 1)
		h.pendingKeyXMu.Lock()
		h.pendingKeyX = ch
		h.pendingKeyXMu.Unlock()

		if client.HasServerPubKey() {
			return h.doKeyAuth(client, ch)
		}
		return h.doKeyInit(client, ch)
	}
}

func (h *Handler) doKeyInit(client *ws.Client, ch chan ws.Message) error {
	log.Info("sending key.init")

	payload, _ := json.Marshal(map[string]string{
		"pubkey": crypto.PubKeyHex(h.keypair.PublicKey),
	})

	err := client.Send(ws.Message{
		Type:    "key.init",
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("send key.init: %w", err)
	}

	select {
	case msg := <-ch:
		return h.handleKeyInitAckPayload(msg, client)
	case <-time.After(15 * time.Second):
		return fmt.Errorf("timeout waiting for key.init_ack")
	}
}

func (h *Handler) doKeyAuth(client *ws.Client, ch chan ws.Message) error {
	log.Info("sending key.auth")

	nonce, err := crypto.NewNonce()
	if err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}

	serverPub, err := crypto.PublicKeyFromHex(h.GetServerPubKeyCached())
	if err != nil {
		return fmt.Errorf("parse server pubkey: %w", err)
	}

	shared, err := crypto.DeriveSharedSecret(h.keypair.PrivateKey, serverPub)
	if err != nil {
		return fmt.Errorf("ECDH: %w", err)
	}

	proof := crypto.ComputeAuthProof(shared, nonce)

	payload, _ := json.Marshal(map[string]interface{}{
		"node_id": client.NodeID(),
		"pubkey":  crypto.PubKeyHex(h.keypair.PublicKey),
		"nonce":   nonce,
		"proof":   proof,
	})

	err = client.Send(ws.Message{
		Type:    "key.auth",
		Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("send key.auth: %w", err)
	}

	select {
	case msg := <-ch:
		return h.handleKeyAuthAckPayload(msg, client, nonce)
	case <-time.After(15 * time.Second):
		return fmt.Errorf("timeout waiting for key.auth_ack")
	}
}

func (h *Handler) handleKeyInitAck(msg ws.Message) {
	if msg.Error != "" {
		log.WithField("error", msg.Error).Error("key.init rejected by center")
		if msg.Error == "reset_required" {
			log.Error("pubkey reset required: admin must clear node pubkey via center admin panel")
		}
	}
	h.pendingKeyXMu.Lock()
	ch := h.pendingKeyX
	h.pendingKeyXMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (h *Handler) handleKeyAuthAck(msg ws.Message) {
	h.pendingKeyXMu.Lock()
	ch := h.pendingKeyX
	h.pendingKeyXMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (h *Handler) handleKeyInitAckPayload(msg ws.Message, client *ws.Client) error {
	if msg.Error != "" {
		return fmt.Errorf("key.init rejected by center: %s", msg.Error)
	}

	var payload struct {
		Pubkey string `json:"pubkey"`
		Nonce  []byte `json:"nonce"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parse key.init_ack: %w", err)
	}

	if payload.Pubkey == "" {
		return fmt.Errorf("key.init_ack missing pubkey")
	}

	serverPub, err := crypto.PublicKeyFromHex(payload.Pubkey)
	if err != nil {
		return fmt.Errorf("invalid server pubkey: %w", err)
	}

	shared, err := crypto.DeriveSharedSecret(h.keypair.PrivateKey, serverPub)
	if err != nil {
		return fmt.Errorf("ECDH: %w", err)
	}

	encKey, err := crypto.DeriveEncKey(shared, payload.Nonce)
	if err != nil {
		return fmt.Errorf("derive enc key: %w", err)
	}

	h.store.PutServerPubKey([]byte(payload.Pubkey))
	client.SetServerPubKey([]byte(payload.Pubkey))
	client.EnableEncryption(encKey)

	log.Info("key exchange complete, encryption enabled")
	return nil
}

func (h *Handler) handleKeyAuthAckPayload(msg ws.Message, client *ws.Client, authNonce []byte) error {
	var payload struct {
		Pubkey      string `json:"pubkey"`
		AuthNonce   []byte `json:"auth_nonce"`
		CenterNonce []byte `json:"center_nonce"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parse key.auth_ack: %w", err)
	}

	if payload.Pubkey == "" {
		return fmt.Errorf("key.auth_ack missing pubkey")
	}

	ephemeralPub, err := crypto.PublicKeyFromHex(payload.Pubkey)
	if err != nil {
		return fmt.Errorf("invalid ephemeral pubkey: %w", err)
	}

	sessionShared, err := crypto.DeriveSharedSecret(h.keypair.PrivateKey, ephemeralPub)
	if err != nil {
		return fmt.Errorf("session ECDH: %w", err)
	}

	combinedNonce := append(append([]byte{}, authNonce...), payload.CenterNonce...)
	sessionKey, err := crypto.DeriveEncKey(sessionShared, combinedNonce)
	if err != nil {
		return fmt.Errorf("derive session key: %w", err)
	}

	client.EnableEncryption(sessionKey)

	log.Info("key auth complete, encryption enabled")
	return nil
}

func (h *Handler) GetServerPubKeyCached() string {
	pub, err := h.store.GetServerPubKey()
	if err != nil {
		return ""
	}
	return string(pub)
}

func (h *Handler) handleBirdDisable(msg ws.Message) {
	var payload BirdControlPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	protoName := payload.ProtoName
	if protoName == "" {
		info, err := h.store.GetPeer(payload.PeerID)
		if err != nil {
			h.sendResponse(msg.ID, false, "peer not found")
			return
		}
		protoName = info.BgpProtoName
		if protoName == "" {
			protoName = fmt.Sprintf("dn42_%d", info.ASN)
		}
	}

	if err := h.birdMgr.DisableProtocol(protoName); err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}
	h.sendResponse(msg.ID, true, "protocol disabled")
}

func (h *Handler) handleBirdEnable(msg ws.Message) {
	var payload BirdControlPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	protoName := payload.ProtoName
	if protoName == "" {
		info, err := h.store.GetPeer(payload.PeerID)
		if err != nil {
			h.sendResponse(msg.ID, false, "peer not found")
			return
		}
		protoName = info.BgpProtoName
		if protoName == "" {
			protoName = fmt.Sprintf("dn42_%d", info.ASN)
		}
	}

	if err := h.birdMgr.EnableProtocol(protoName); err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}
	h.sendResponse(msg.ID, true, "protocol enabled")
}
