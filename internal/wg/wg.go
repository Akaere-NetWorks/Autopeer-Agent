package wg

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "wg")

const wgConfigTmpl = `# AutoPeer ASN: {{ .ASN }}
[Interface]
PrivateKey = {{ .PrivateKey }}
ListenPort = {{ .ListenPort }}
Address = {{ .OurLLA }}/64
{{ if .MTU }}MTU = {{ .MTU }}
{{ end }}Table = off

[Peer]
PublicKey = {{ .RemotePubkey }}
{{ if .PresharedKey }}PresharedKey = {{ .PresharedKey }}
{{ end }}AllowedIPs = 172.20.0.0/14, fd00::/8, fe80::/10
Endpoint = {{ .RemoteEndpoint }}
PersistentKeepalive = 25
`

var tmpl = template.Must(template.New("wg").Parse(wgConfigTmpl))

type Manager struct {
	wgDir       string
	birdPeerDir string
	privateKey  string
	ourLLA      string
}

type PeerConfig struct {
	ASN            int64
	PrivateKey     string
	ListenPort     int
	OurLLA         string
	RemotePubkey   string
	RemoteEndpoint string
	MTU            *int
	PresharedKey   *string
}

func (m *Manager) AddPeerWithIface(ifName string, asn int64, listenPort int, remotePubkey, remoteEndpoint string, mtu *int, psk *string) error {
	log.WithFields(logrus.Fields{
		"asn":       asn,
		"interface": ifName,
		"port":      listenPort,
	}).Debug("adding peer")

	if asn <= 0 || remotePubkey == "" || remoteEndpoint == "" {
		return fmt.Errorf("invalid peer config: asn=%d pubkey=%q endpoint=%q", asn, remotePubkey, remoteEndpoint)
	}

	configPath := filepath.Join(m.wgDir, ifName+".conf")

	cfg := PeerConfig{
		ASN:            asn,
		PrivateKey:     m.privateKey,
		ListenPort:     listenPort,
		OurLLA:         m.ourLLA,
		RemotePubkey:   remotePubkey,
		RemoteEndpoint: remoteEndpoint,
		MTU:            mtu,
		PresharedKey:   psk,
	}

	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("create wg config: %w", err)
	}

	if err := tmpl.Execute(f, cfg); err != nil {
		f.Close()
		return fmt.Errorf("write wg config: %w", err)
	}
	f.Close()

	log.WithField("interface", ifName).Debug("wrote wireguard config")

	cmd := exec.Command("wg-quick", "up", ifName)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wg-quick up %s: %s: %w", ifName, string(out), err)
	}

	log.WithField("interface", ifName).Info("interface started")

	return nil
}

func NewManager(wgDir, birdPeerDir, privateKey, ourLLA string) *Manager {
	return &Manager{
		wgDir:       wgDir,
		birdPeerDir: birdPeerDir,
		privateKey:  privateKey,
		ourLLA:      ourLLA,
	}
}

func InterfaceName(asn int64) string {
	return fmt.Sprintf("dn42%d", asn)
}

func (m *Manager) ConfigDir() string {
	return m.wgDir
}

func (m *Manager) AddPeer(asn int64, listenPort int, remotePubkey, remoteEndpoint string, mtu *int, psk *string) error {
	return m.AddPeerWithIface(InterfaceName(asn), asn, listenPort, remotePubkey, remoteEndpoint, mtu, psk)
}

func (m *Manager) RemovePeer(asn int64) error {
	ifName := InterfaceName(asn)

	log.WithFields(logrus.Fields{
		"asn":       asn,
		"interface": ifName,
	}).Debug("removing peer")

	return m.RemovePeerByIface(ifName)
}

func (m *Manager) RemovePeerByIface(ifName string) error {
	configPath := filepath.Join(m.wgDir, ifName+".conf")
	defer os.Remove(configPath)

	cmd := exec.Command("wg-quick", "down", ifName)
	if _, err := cmd.CombinedOutput(); err != nil {
		log.WithError(err).WithField("interface", ifName).Warn("wg-quick down failed")
	}

	log.WithField("interface", ifName).Info("interface stopped")

	return nil
}

func (m *Manager) IsPeerUp(ifName string) bool {
	cmd := exec.Command("wg", "show", ifName)
	return cmd.Run() == nil
}

type InterfaceStats struct {
	Name           string
	RxBytes        int64
	TxBytes        int64
	LastHandshake  string
	ActualEndpoint string
}

func (m *Manager) GetStats() ([]InterfaceStats, error) {
	log.Debug("getting stats")

	cmd := exec.Command("wg", "show", "all", "dump")
	out, err := cmd.Output()
	if err != nil {
		log.WithError(err).Error("wg show command failed")
		return nil, fmt.Errorf("wg show: %w", err)
	}

	log.Debug("wg show command executed")

	stats := make([]InterfaceStats, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	var currentIf string

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}

		if len(fields) == 5 {
			currentIf = fields[0]
		} else if len(fields) >= 9 && currentIf != "" {
			handshake := fields[5]
			rx, rxErr := strconv.ParseInt(fields[6], 10, 64)
			tx, txErr := strconv.ParseInt(fields[7], 10, 64)
			if rxErr != nil || txErr != nil {
				log.WithFields(logrus.Fields{
					"interface": currentIf,
					"fields":    len(fields),
					"rx_err":    rxErr,
					"tx_err":    txErr,
				}).Warn("GetStats: failed to parse rx/tx bytes")
			}

			actualEndpoint := ""
			if len(fields) > 3 {
				actualEndpoint = fields[3]
			}
			stats = append(stats, InterfaceStats{
				Name:           currentIf,
				RxBytes:        rx,
				TxBytes:        tx,
				LastHandshake:  handshake,
				ActualEndpoint: actualEndpoint,
			})
		}
	}

	log.WithField("count", len(stats)).Debug("collected stats")

	return stats, nil
}

var rttRe = regexp.MustCompile(`(?:time|rtt)[=<]([0-9.]+)\s*ms`)

type ScannedPeer struct {
	InterfaceName  string
	ASN            int64
	RemotePubkey   string
	RemoteEndpoint string
	ListenPort     int
	OurLLA         string
}

var birdASNRe = regexp.MustCompile(`\bas\s+(\d+)\s*;`)

var asnFromNameRe = regexp.MustCompile(`\d+`)

func extractASNFromIface(name string) (int64, bool) {
	digits := asnFromNameRe.FindString(name)
	if digits == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

func (m *Manager) readASNFromWGConfig(ifName string) (int64, error) {
	confPath := filepath.Join(m.wgDir, ifName+".conf")
	data, err := os.ReadFile(confPath)
	if err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`# AutoPeer ASN: (\d+)`)
	matches := re.FindSubmatch(data)
	if matches == nil {
		return 0, fmt.Errorf("no ASN comment in %s", confPath)
	}
	return strconv.ParseInt(string(matches[1]), 10, 64)
}

func (m *Manager) ScanPeers() ([]ScannedPeer, error) {
	log.Debug("scanning peers")

	entries, err := os.ReadDir(m.wgDir)
	if err != nil {
		log.WithError(err).Error("read wg dir failed")
		return nil, fmt.Errorf("read wg dir: %w", err)
	}

	peers := make([]ScannedPeer, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}

		log.WithField("file", entry.Name()).Debug("found peer config")

		ifName := strings.TrimSuffix(entry.Name(), ".conf")

		var asn int64
		var asnOk bool

		if n, ok := extractASNFromIface(ifName); ok {
			asn = n
			asnOk = true
		} else {
			n, err := m.readASNFromWGConfig(ifName)
			if err == nil {
				asn = n
				asnOk = true
			}
		}

		if !asnOk {
			log.WithField("iface", ifName).Warn("ScanPeers: skip, cannot determine ASN")
			continue
		}

		cmd := exec.Command("wg", "show", ifName, "dump")
		out, err := cmd.Output()
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		var remotePubkey, remoteEndpoint string
		var listenPort int

		for scanner.Scan() {
			line := scanner.Text()
			fields := strings.Split(line, "\t")
			if len(fields) < 2 {
				continue
			}

			if len(fields) == 4 {
				listenPort, _ = strconv.Atoi(fields[2])
			} else if len(fields) >= 8 {
				remotePubkey = fields[0]
				remoteEndpoint = fields[2]
			}
		}

		if remotePubkey == "" {
			continue
		}

		peers = append(peers, ScannedPeer{
			InterfaceName:  ifName,
			ASN:            asn,
			RemotePubkey:   remotePubkey,
			RemoteEndpoint: remoteEndpoint,
			ListenPort:     listenPort,
			OurLLA:         m.ourLLA,
		})
	}

	log.WithField("count", len(peers)).Debug("scanned peers")

	return peers, nil
}

func PingPeer(remoteLLA, ifName string) float64 {
	log.WithFields(logrus.Fields{
		"interface": ifName,
		"lla":       remoteLLA,
	}).Debug("pinging peer")

	ip := net.ParseIP(remoteLLA)
	if ip == nil || (!ip.IsLinkLocalUnicast() && !ip.IsGlobalUnicast()) {
		log.WithField("lla", remoteLLA).Warn("PingPeer: invalid or non-unicast remoteLLA, skipping ping")
		return -1
	}
	if ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() {
		log.WithField("lla", remoteLLA).Warn("PingPeer: remoteLLA is loopback/multicast/unspecified, skipping ping")
		return -1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ping", "-6", "-c", "1", "-W", "3", "-I", ifName, remoteLLA)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.WithFields(logrus.Fields{
			"interface": ifName,
			"lla":       remoteLLA,
		}).Debug("ping failed")
		return -1
	}

	matches := rttRe.FindSubmatch(out)
	if len(matches) < 2 {
		log.WithFields(logrus.Fields{
			"interface": ifName,
			"lla":       remoteLLA,
		}).Debug("ping: no rtt match")
		return -1
	}
	rtt, err := strconv.ParseFloat(string(matches[1]), 64)
	if err != nil {
		log.WithError(err).WithFields(logrus.Fields{
			"interface": ifName,
			"lla":       remoteLLA,
		}).Debug("ping: parse rtt failed")
		return -1
	}

	log.WithFields(logrus.Fields{
		"interface": ifName,
		"lla":       remoteLLA,
		"rtt_ms":    rtt,
	}).Debug("ping result")

	return rtt
}
