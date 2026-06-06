package config

import (
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

var log = logrus.WithField("pkg", "config")

type Config struct {
	CenterURL            string `yaml:"center_url"`
	AgentToken           string `yaml:"agent_token"`
	NodeID               string `yaml:"node_id"`
	BirdPeerDir          string `yaml:"bird_peer_dir"`
	WgDir                string `yaml:"wg_dir"`
	BirdcPath            string `yaml:"birdc_path"`
	OurASN               int    `yaml:"our_asn"`
	OurLLA               string `yaml:"our_lla"`
	OurWgPrivateKey      string `yaml:"our_wg_private_key"`
	HeartbeatInterval       int `yaml:"heartbeat_interval"`
	BirdDetailInterval      int `yaml:"bird_detail_interval"`
	ReconnectMaxInterval    int `yaml:"reconnect_max_interval"`
	StorePath            string `yaml:"store_path"`
}

func maskToken(t string) string {
	if len(t) > 8 {
		return t[:8] + "..."
	}
	return "***"
}

func Load(path string) (*Config, error) {
	log.WithField("path", path).Debug("loading config file")

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}

	cfg := &Config{
		BirdPeerDir:          "/etc/bird/dn42/peer/",
		WgDir:                "/etc/wireguard/",
		BirdcPath:            "/usr/sbin/birdc",
		OurASN:               4242420000,
		HeartbeatInterval:       60,
		BirdDetailInterval:      2,
		ReconnectMaxInterval:    60,
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	log.WithFields(logrus.Fields{
		"center_url":            cfg.CenterURL,
		"agent_token":           maskToken(cfg.AgentToken),
		"node_id":               cfg.NodeID,
		"bird_peer_dir":         cfg.BirdPeerDir,
		"wg_dir":                cfg.WgDir,
		"birdc_path":            cfg.BirdcPath,
		"our_asn":               cfg.OurASN,
		"our_lla":               cfg.OurLLA,
		"our_wg_private_key":    "***",
		"heartbeat_interval":    cfg.HeartbeatInterval,
		"reconnect_max_interval": cfg.ReconnectMaxInterval,
		"store_path":            cfg.StorePath,
	}).Debug("loaded config values")

	if cfg.CenterURL == "" {
		log.Debug("validation failed: center_url is required")
		return nil, fmt.Errorf("center_url is required")
	}
	if cfg.NodeID == "" {
		log.Debug("validation failed: node_id is required")
		return nil, fmt.Errorf("node_id is required")
	}
	if cfg.OurLLA == "" {
		log.Debug("validation failed: our_lla is required")
		return nil, fmt.Errorf("our_lla is required")
	}
	if cfg.OurWgPrivateKey == "" {
		log.Debug("validation failed: our_wg_private_key is required")
		return nil, fmt.Errorf("our_wg_private_key is required")
	}

	log.Info("config loaded successfully")

	return cfg, nil
}
