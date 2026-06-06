package bird

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "bird")

const birdConfigTmpl = `protocol bgp dn42_{{ .ASNSuffix }} from dnpeers {
    neighbor {{ .RemoteLLA }} % '{{ .WgInterface }}' as {{ .ASN }};
}
`

var tmpl = template.Must(template.New("bird").Parse(birdConfigTmpl))

var routesRe = regexp.MustCompile(`Routes:\s+(\d+) imported,\s+(\d+) exported(?:,\s+(\d+) preferred)?`)

var neighborRe = regexp.MustCompile(`neighbor\s+(fe80:[0-9a-fA-F:]+)\s+%\s*'(\S+)'\s+as\s+(\d+)`)

var protoNameRe = regexp.MustCompile(`protocol\s+bgp\s+(\S+)\s+from\s+dnpeers`)

type Manager struct {
	peerDir   string
	birdcPath string
	mu        sync.RWMutex
}

func (m *Manager) lock()   { m.mu.Lock() }
func (m *Manager) unlock() { m.mu.Unlock() }

func (m *Manager) ConfigDir() string {
	return m.peerDir
}

type PeerConfig struct {
	ASN         int64
	ASNSuffix   string
	RemoteLLA   string
	WgInterface string
}

func NewManager(peerDir, birdcPath string) *Manager {
	return &Manager{
		peerDir:   peerDir,
		birdcPath: birdcPath,
	}
}

func (m *Manager) AddPeer(asn int64, remoteLLA, wgInterface string) error {
	log.WithFields(logrus.Fields{
		"asn":          asn,
		"remote_lla":   remoteLLA,
		"wg_interface": wgInterface,
	}).Debug("AddPeer")

	configPath := filepath.Join(m.peerDir, fmt.Sprintf("dn42_%d.conf", asn))

	cfg := PeerConfig{
		ASN:         asn,
		ASNSuffix:   fmt.Sprintf("%d", asn),
		RemoteLLA:   remoteLLA,
		WgInterface: wgInterface,
	}

	m.lock()
	defer m.unlock()

	f, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.WithError(err).WithField("config_path", configPath).Error("failed to create bird config file")
		return fmt.Errorf("create bird config: %w", err)
	}

	log.WithField("config_path", configPath).Debug("executing bird config template")
	if err := tmpl.Execute(f, cfg); err != nil {
		f.Close()
		log.WithError(err).WithField("config_path", configPath).Error("failed to write bird config")
		return fmt.Errorf("write bird config: %w", err)
	}
	f.Close()

	log.WithFields(logrus.Fields{
		"asn":          asn,
		"config_path":  configPath,
		"remote_lla":   remoteLLA,
		"wg_interface": wgInterface,
	}).Info("peer added")

	checkCtx, checkCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer checkCancel()
	checkCmd := exec.CommandContext(checkCtx, m.birdcPath, "configure", "check")
	if out, err := checkCmd.CombinedOutput(); err != nil {
		os.Remove(configPath)
		return fmt.Errorf("bird config check failed: %s", string(out))
	}

	return m.reloadLocked()
}

func (m *Manager) RemovePeer(asn int64) error {
	m.lock()
	defer m.unlock()

	log.WithField("asn", asn).Debug("RemovePeer")

	configPath := filepath.Join(m.peerDir, fmt.Sprintf("dn42_%d.conf", asn))

	os.Remove(configPath)

	log.WithFields(logrus.Fields{
		"asn":         asn,
		"config_path": configPath,
	}).Info("peer removed")

	return m.reloadLocked()
}

func (m *Manager) RemovePeerByFilename(filename string) error {
	m.lock()
	defer m.unlock()

	log.WithField("filename", filename).Debug("RemovePeerByFilename")

	configPath := filepath.Join(m.peerDir, filename)
	os.Remove(configPath)

	log.WithField("config_path", configPath).Info("peer removed by filename")

	return m.reloadLocked()
}

func (m *Manager) reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadLocked()
}

func (m *Manager) reloadLocked() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	log.WithField("birdc_path", m.birdcPath).Debug("reloading BIRD configuration")
	cmd := exec.CommandContext(ctx, m.birdcPath, "configure")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.WithError(err).WithField("output", string(out)).Error("birdc configure failed")
		return fmt.Errorf("birdc configure: %s: %w", string(out), err)
	}

	log.Info("BIRD reloaded successfully")
	return nil
}

// DisableProtocol disables a BIRD protocol without removing its config.
func (m *Manager) DisableProtocol(protoName string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	log.WithField("proto", protoName).Debug("DisableProtocol")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.birdcPath, "disable", protoName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("birdc disable %s: %s: %w", protoName, string(out), err)
	}

	log.WithField("proto", protoName).Info("protocol disabled")
	return nil
}

// EnableProtocol enables a previously disabled BIRD protocol.
func (m *Manager) EnableProtocol(protoName string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	log.WithField("proto", protoName).Debug("EnableProtocol")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.birdcPath, "enable", protoName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("birdc enable %s: %s: %w", protoName, string(out), err)
	}

	log.WithField("proto", protoName).Info("protocol enabled")
	return nil
}

type PeerState struct {
	Name  string
	State string
}

type PeerDetail struct {
	Name            string
	State           string
	RoutesImported  int
	RoutesExported  int
	RoutesPreferred int
	BGPUptimeSecs   int
}

func (m *Manager) GetPeerStates() ([]PeerState, error) {
	log.Debug("GetPeerStates")

	m.mu.RLock()
	defer m.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.birdcPath, "show", "protocols")
	out, err := cmd.Output()
	if err != nil {
		log.WithError(err).Error("birdc show protocols failed")
		return nil, fmt.Errorf("birdc show protocols: %w", err)
	}

	states := make([]PeerState, 0)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}

		if fields[1] != "BGP" {
			continue
		}

		name := fields[0]
		state := strings.ToLower(fields[5])

		states = append(states, PeerState{
			Name:  name,
			State: state,
		})
	}

	log.WithField("count", len(states)).Debug("GetPeerStates completed")
	return states, nil
}

// maxRouteOutput caps the size of birdc route output returned to the center to
// avoid flooding the WebSocket frame with very large routing tables.
const maxRouteOutput = 64 * 1024

// ShowRoute runs `birdc show route ...` for a looking-glass query. For a single
// IP it uses `show route for <ip>` (best-matching route); for a CIDR prefix it
// uses `show route <prefix>`. When detailed is true, `all` is appended to print
// full per-route attributes. The raw text output is returned (trimmed and
// capped at maxRouteOutput bytes).
func (m *Manager) ShowRoute(target string, detailed bool) (string, error) {
	log.Debugf("ShowRoute target=%s detailed=%v", target, detailed)

	m.mu.RLock()
	defer m.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	args := []string{"show", "route"}
	if strings.Contains(target, "/") {
		args = append(args, target)
	} else {
		args = append(args, "for", target)
	}
	if detailed {
		args = append(args, "all")
	}

	cmd := exec.CommandContext(ctx, m.birdcPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("birdc show route: %s", strings.TrimSpace(string(out)))
	}

	text := strings.TrimSpace(string(out))
	if len(text) > maxRouteOutput {
		text = text[:maxRouteOutput] + "\n... (output truncated)"
	}
	return text, nil
}

func parseSinceToUptimeSecs(since string) int {
	now := time.Now()
	if t, err := time.ParseInLocation("15:04:05", since, time.Local); err == nil {
		start := time.Date(now.Year(), now.Month(), now.Day(),
			t.Hour(), t.Minute(), t.Second(), 0, time.Local)
		if start.After(now) {
			start = start.Add(-24 * time.Hour)
		}
		return int(now.Sub(start).Seconds())
	}
	if t, err := time.ParseInLocation("2006-01-02", since, time.Local); err == nil {
		return int(now.Sub(t).Seconds())
	}
	log.WithField("since", since).Warn("failed to parse BIRD uptime string")
	return 0
}

func (m *Manager) GetPeerDetails() ([]PeerDetail, error) {
	log.Debug("GetPeerDetails")

	m.mu.RLock()
	defer m.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.birdcPath, "show", "protocols", "all")
	out, err := cmd.Output()
	if err != nil {
		log.WithError(err).Error("birdc show protocols all failed")
		return nil, fmt.Errorf("birdc show protocols all: %w", err)
	}

	details := make([]PeerDetail, 0)
	var current *PeerDetail

	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, " ") && len(strings.Fields(line)) >= 2 && strings.Fields(line)[1] == "BGP" {
			if current != nil {
				details = append(details, *current)
			}
			fields := strings.Fields(line)
			state := ""
			if len(fields) >= 6 {
				state = strings.ToLower(fields[5])
			}
			since := ""
			if len(fields) >= 5 {
				since = fields[4]
			}
			current = &PeerDetail{
				Name:          fields[0],
				State:         state,
				BGPUptimeSecs: parseSinceToUptimeSecs(since),
			}
			continue
		}

		if current != nil {
			if matches := routesRe.FindStringSubmatch(line); matches != nil {
				imported := 0
				exported := 0
				preferred := 0
				fmt.Sscan(matches[1], &imported)
				fmt.Sscan(matches[2], &exported)
				if len(matches) > 3 && matches[3] != "" {
					fmt.Sscan(matches[3], &preferred)
				}
				current.RoutesImported += imported
				current.RoutesExported += exported
				current.RoutesPreferred += preferred
			}
		}
	}

	if current != nil {
		details = append(details, *current)
	}

	log.WithField("count", len(details)).Debug("GetPeerDetails completed")
	return details, nil
}

type ScannedBGP struct {
	Name        string
	ProtoName   string
	Filename    string
	ASN         int64
	RemoteLLA   string
	WgInterface string
}

func (m *Manager) ScanPeers() ([]ScannedBGP, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	log.WithField("peer_dir", m.peerDir).Debug("ScanPeers")

	entries, err := os.ReadDir(m.peerDir)
	if err != nil {
		log.WithError(err).WithField("peer_dir", m.peerDir).Error("failed to read bird peer dir")
		return nil, fmt.Errorf("read bird peer dir: %w", err)
	}

	peers := make([]ScannedBGP, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		path := filepath.Join(m.peerDir, entry.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			log.WithError(err).WithField("file", path).Warn("skipping file during scan, failed to read")
			continue
		}

		matches := neighborRe.FindStringSubmatch(string(data))
		if len(matches) < 4 {
			continue
		}

		remoteLLA := matches[1]
		wgInterface := matches[2]
		asn, err := strconv.ParseInt(matches[3], 10, 64)
		if err != nil {
			continue
		}

		protoName := ""
		if pm := protoNameRe.FindStringSubmatch(string(data)); len(pm) >= 2 {
			protoName = pm[1]
		}

		peers = append(peers, ScannedBGP{
			Name:        strings.TrimSuffix(entry.Name(), ".conf"),
			ProtoName:   protoName,
			Filename:    entry.Name(),
			ASN:         asn,
			RemoteLLA:   remoteLLA,
			WgInterface: wgInterface,
		})
	}

	log.WithField("count", len(peers)).Debug("ScanPeers completed")
	return peers, nil
}
