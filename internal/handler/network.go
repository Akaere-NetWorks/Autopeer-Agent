package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akaere/autopeer-agent/internal/ws"
)

// dn42Ranges are the DN42 address ranges the looking glass is allowed to probe
// in addition to public unicast space. Defined as a package-level list so the
// allowlist is easy to audit and tune.
var dn42Ranges = func() []*net.IPNet {
	cidrs := []string{
		"172.20.0.0/14", // DN42 IPv4
		"172.31.0.0/16", // DN42 IPv4 (legacy/extra)
		"10.127.0.0/16", // DN42 anycast IPv4
		"fd00::/8",      // DN42 IPv6 (ULA subset)
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}()

func isDN42(ip net.IP) bool {
	for _, n := range dn42Ranges {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// validateNetworkTarget enforces the looking-glass target allowlist: public
// unicast addresses and DN42 ranges are permitted; loopback, link-local,
// multicast, unspecified and non-DN42 private/ULA space are rejected. DN42
// space is explicitly allowed even though it falls in RFC1918 / ULA ranges.
func validateNetworkTarget(target string) error {
	ip := net.ParseIP(target)
	if ip == nil {
		return fmt.Errorf("target %s is not a valid IP address", target)
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return fmt.Errorf("target %s is a reserved address", target)
	}
	if isDN42(ip) {
		return nil
	}
	if ip4 := ip.To4(); ip4 != nil {
		// Non-DN42 private / broadcast IPv4 is blocked.
		if ip4[0] == 10 || (ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) || (ip4[0] == 192 && ip4[1] == 168) || (ip4[0] == 169 && ip4[1] == 254) {
			return fmt.Errorf("target %s is a non-DN42 private address", target)
		}
		if ip4[3] == 255 && bytesEqual(ip4, []byte{255, 255, 255, 255}) {
			return fmt.Errorf("target %s is a broadcast address", target)
		}
	} else {
		// Non-DN42 ULA (fc00::/7) is blocked; public IPv6 is allowed.
		if ip[0]&0xfe == 0xfc {
			return fmt.Errorf("target %s is a non-DN42 ULA address", target)
		}
	}
	return nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// resolveAndValidate accepts a literal IP or a hostname. Hostnames are resolved
// (3s timeout) and the first usable A/AAAA record that passes the allowlist is
// returned, so commands run against a pinned IP rather than re-resolving. The
// returned string is the IP to use as the command target.
func resolveAndValidate(target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("missing target")
	}
	if strings.HasPrefix(target, "-") {
		return "", fmt.Errorf("invalid target: must not start with '-'")
	}

	if ip := net.ParseIP(target); ip != nil {
		if err := validateNetworkTarget(target); err != nil {
			return "", err
		}
		return target, nil
	}

	// Treat as a hostname and resolve.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", target)
	if err != nil {
		return "", fmt.Errorf("could not resolve %s: %v", target, err)
	}
	var lastErr error
	for _, ip := range ips {
		s := ip.String()
		if err := validateNetworkTarget(s); err != nil {
			lastErr = err
			continue
		}
		return s, nil
	}
	if lastErr != nil {
		return "", fmt.Errorf("%s resolved only to disallowed addresses: %v", target, lastErr)
	}
	return "", fmt.Errorf("could not resolve %s to a usable address", target)
}

func (h *Handler) handleNetworkPing(msg ws.Message) {
	var payload struct {
		Target string `json:"target"`
		Count  int    `json:"count,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	resolved, err := resolveAndValidate(payload.Target)
	if err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}
	if payload.Count <= 0 {
		payload.Count = 4
	}
	if payload.Count > 20 {
		payload.Count = 20
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ping", "-c", strconv.Itoa(payload.Count), "-W", "3", resolved)
	output, err := cmd.CombinedOutput()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("ping failed: %s", strings.TrimSpace(string(output))))
		return
	}

	result := parsePingOutput(string(output), payload.Target)
	result.ResolvedIP = resolved
	data, err := json.Marshal(result)
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal result: %v", err))
		return
	}

	success := true
	h.wsClient.Send(ws.Message{
		Type:    "response",
		ID:      msg.ID,
		Success: &success,
		Payload: data,
	})
}

func (h *Handler) handleNetworkTrace(msg ws.Message) {
	var payload struct {
		Target  string `json:"target"`
		MaxHops int    `json:"max_hops,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	resolved, err := resolveAndValidate(payload.Target)
	if err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}
	if payload.MaxHops <= 0 {
		payload.MaxHops = 30
	}
	if payload.MaxHops > 64 {
		payload.MaxHops = 64
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "traceroute", "-m", strconv.Itoa(payload.MaxHops), "-w", "3", "-q", "3", resolved)
	output, err := cmd.CombinedOutput()
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("traceroute failed: %s", strings.TrimSpace(string(output))))
		return
	}

	hops := parseTracerouteOutput(string(output))
	result := map[string]interface{}{
		"hops":        hops,
		"resolved_ip": resolved,
	}
	data, err := json.Marshal(result)
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal result: %v", err))
		return
	}

	success := true
	h.wsClient.Send(ws.Message{
		Type:    "response",
		ID:      msg.ID,
		Success: &success,
		Payload: data,
	})
}

func (h *Handler) handleNetworkMTR(msg ws.Message) {
	var payload struct {
		Target string `json:"target"`
		Cycles int    `json:"cycles,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	resolved, err := resolveAndValidate(payload.Target)
	if err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}
	if payload.Cycles <= 0 {
		payload.Cycles = 5
	}
	if payload.Cycles > 10 {
		payload.Cycles = 10
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// Prefer JSON report; fall back to plain text report if -j is unsupported.
	cmd := exec.CommandContext(ctx, "mtr", "-r", "-c", strconv.Itoa(payload.Cycles), "-j", resolved)
	output, err := cmd.CombinedOutput()
	asJSON := true
	if err != nil {
		cmd = exec.CommandContext(ctx, "mtr", "-r", "-c", strconv.Itoa(payload.Cycles), resolved)
		output, err = cmd.CombinedOutput()
		asJSON = false
		if err != nil {
			h.sendResponse(msg.ID, false, fmt.Sprintf("mtr failed: %s", strings.TrimSpace(string(output))))
			return
		}
	}

	result := map[string]interface{}{
		"target":      payload.Target,
		"resolved_ip": resolved,
	}
	if asJSON && json.Valid(output) {
		result["report"] = json.RawMessage(output)
	} else {
		result["raw"] = strings.TrimSpace(string(output))
	}

	data, err := json.Marshal(result)
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal result: %v", err))
		return
	}

	success := true
	h.wsClient.Send(ws.Message{
		Type:    "response",
		ID:      msg.ID,
		Success: &success,
		Payload: data,
	})
}

func (h *Handler) handleBirdRoute(msg ws.Message) {
	var payload struct {
		Target   string `json:"target"`
		Detailed bool   `json:"detailed,omitempty"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid payload: %v", err))
		return
	}

	target := strings.TrimSpace(payload.Target)
	if target == "" {
		h.sendResponse(msg.ID, false, "missing target")
		return
	}
	// BGP route lookups accept a literal IP or CIDR prefix only (no hostnames).
	if strings.HasPrefix(target, "-") {
		h.sendResponse(msg.ID, false, "invalid target: must not start with '-'")
		return
	}
	if strings.Contains(target, "/") {
		if _, _, err := net.ParseCIDR(target); err != nil {
			h.sendResponse(msg.ID, false, fmt.Sprintf("invalid prefix: %s", target))
			return
		}
	} else if net.ParseIP(target) == nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("invalid IP/prefix: %s (hostnames are not allowed for route lookups)", target))
		return
	}

	output, err := h.birdMgr.ShowRoute(target, payload.Detailed)
	if err != nil {
		h.sendResponse(msg.ID, false, err.Error())
		return
	}

	data, err := json.Marshal(map[string]interface{}{
		"target": target,
		"output": output,
	})
	if err != nil {
		h.sendResponse(msg.ID, false, fmt.Sprintf("marshal result: %v", err))
		return
	}

	success := true
	h.wsClient.Send(ws.Message{
		Type:    "response",
		ID:      msg.ID,
		Success: &success,
		Payload: data,
	})
}

type pingResult struct {
	Target      string    `json:"target"`
	ResolvedIP  string    `json:"resolved_ip,omitempty"`
	Sent        int       `json:"sent"`
	Received    int       `json:"received"`
	LossPercent float64   `json:"loss_percent"`
	MinRTT      float64   `json:"min_rtt_ms"`
	AvgRTT      float64   `json:"avg_rtt_ms"`
	MaxRTT      float64   `json:"max_rtt_ms"`
	RTTs        []float64 `json:"rtts"`
}

var (
	rttLineRe   = regexp.MustCompile(`rtt min/avg/max/mdev = ([\d.]+)/([\d.]+)/([\d.]+)/([\d.]+) ms`)
	statsLineRe = regexp.MustCompile(`(\d+) packets transmitted, (\d+) received(?:, ([\d.]+)% packet loss)?`)
	seqRTTRe    = regexp.MustCompile(`icmp_seq=(\d+).*time=([\d.]+)\s*ms`)
)

func parsePingOutput(output, target string) pingResult {
	result := pingResult{Target: target}

	if m := statsLineRe.FindStringSubmatch(output); len(m) >= 3 {
		result.Sent, _ = strconv.Atoi(m[1])
		result.Received, _ = strconv.Atoi(m[2])
		if len(m) >= 4 && m[3] != "" {
			result.LossPercent, _ = strconv.ParseFloat(m[3], 64)
		}
	}

	if m := rttLineRe.FindStringSubmatch(output); len(m) >= 4 {
		result.MinRTT, _ = strconv.ParseFloat(m[1], 64)
		result.AvgRTT, _ = strconv.ParseFloat(m[2], 64)
		result.MaxRTT, _ = strconv.ParseFloat(m[3], 64)
	}

	matches := seqRTTRe.FindAllStringSubmatch(output, -1)
	rtts := make([]float64, 0, len(matches))
	for _, m := range matches {
		rtt, _ := strconv.ParseFloat(m[2], 64)
		rtts = append(rtts, rtt)
	}
	result.RTTs = rtts

	return result
}

type traceHop struct {
	TTL  int       `json:"ttl"`
	IP   string    `json:"ip"`
	RTTs []float64 `json:"rtts"`
}

var traceLineRe = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`)

func parseTracerouteOutput(output string) []traceHop {
	lines := strings.Split(output, "\n")
	hops := make([]traceHop, 0)

	for _, line := range lines {
		m := traceLineRe.FindStringSubmatch(line)
		if len(m) < 3 {
			continue
		}

		ttl, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		rest := m[2]
		fields := strings.Fields(rest)

		var ip string
		rtts := make([]float64, 0)

		i := 0
		for i < len(fields) {
			f := fields[i]
			if f == "*" {
				i++
				continue
			}

			if strings.HasPrefix(f, "(") && strings.HasSuffix(f, ")") {
				ip = strings.Trim(f, "()")
				i++
				continue
			}

			if !strings.Contains(f, "(") && !strings.Contains(f, ")") && !strings.HasPrefix(f, "ms") && !strings.Contains(f, "ms") {
				if ip == "" {
					ip = f
				}
				i++
				continue
			}

			rttStr := f
			rttStr = strings.TrimSuffix(rttStr, "ms")
			rtt, err := strconv.ParseFloat(rttStr, 64)
			if err == nil {
				rtts = append(rtts, rtt)
			}
			i++
		}

		hops = append(hops, traceHop{
			TTL:  ttl,
			IP:   ip,
			RTTs: rtts,
		})
	}

	return hops
}
