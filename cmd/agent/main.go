package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/akaere/autopeer-agent/internal/bird"
	"github.com/akaere/autopeer-agent/internal/config"
	"github.com/akaere/autopeer-agent/internal/crypto"
	"github.com/akaere/autopeer-agent/internal/handler"
	"github.com/akaere/autopeer-agent/internal/metrics"
	"github.com/akaere/autopeer-agent/internal/store"
	"github.com/akaere/autopeer-agent/internal/wg"
	"github.com/akaere/autopeer-agent/internal/ws"
	"github.com/sirupsen/logrus"
)

var (
	Version   = "dev"
	BuildTime = "unknown"
)

var log = logrus.WithField("pkg", "main")

const (
	defaultStateDir = "/var/lib/autopeer-agent"
	stateDirPerm    = 0700
)

func main() {
	configPath := flag.String("config", "/etc/autopeer-agent/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.WithError(err).Fatal("failed to load config")
	}

	log.WithFields(logrus.Fields{
		"version":    Version,
		"build":      BuildTime,
		"node_id":    cfg.NodeID,
		"center_url": cfg.CenterURL,
		"store_path": cfg.StorePath,
	}).Debug("config loaded")

	log.WithFields(logrus.Fields{"version": Version, "build": BuildTime, "node_id": cfg.NodeID}).Info("starting agent")

	if cfg.StorePath == "" {
		cfg.StorePath = filepath.Join(defaultStateDir, "peers.db")
	}
	if err := ensureStoreDirectory(cfg.StorePath); err != nil {
		log.WithError(err).Fatal("failed to prepare store directory")
	}
	if err := ensurePrivateStateDirectory(defaultStateDir); err != nil {
		log.WithError(err).Fatal("failed to prepare state directory")
	}
	peerStore, err := store.Open(cfg.StorePath)
	if err != nil {
		log.WithError(err).Fatal("failed to open store")
	}
	defer peerStore.Close()

	peerStore.PutVersion(Version)

	var agentKP *crypto.KeyPair
	if peerStore.HasKeyPair() {
		privBytes, _, err := peerStore.GetKeyPair()
		if err != nil {
			log.WithError(err).Warn("failed to load key pair, generating new one")
		} else {
			priv, err := crypto.PrivateKeyFromHex(string(privBytes))
			if err != nil {
				log.WithError(err).Warn("failed to parse private key, generating new one")
			} else {
				agentKP = &crypto.KeyPair{
					PrivateKey: priv,
					PublicKey:  priv.PublicKey(),
				}
				log.WithField("pubkey_prefix", crypto.PubKeyHex(agentKP.PublicKey)[:16]).Info("loaded existing key pair")
			}
		}
	}

	if agentKP == nil {
		agentKP, err = crypto.GenerateKeyPair()
		if err != nil {
			log.WithError(err).Fatal("failed to generate key pair")
		}
		privBytes := []byte(crypto.PrivKeyHex(agentKP.PrivateKey))
		pubBytes := []byte(crypto.PubKeyHex(agentKP.PublicKey))
		if err := peerStore.PutKeyPair(privBytes, pubBytes); err != nil {
			log.WithError(err).Fatal("failed to store key pair")
		}
		log.WithField("pubkey_prefix", crypto.PubKeyHex(agentKP.PublicKey)[:16]).Info("generated and stored new key pair")
	}

	wgMgr := wg.NewManager(cfg.WgDir, cfg.BirdPeerDir, cfg.OurWgPrivateKey, cfg.OurLLA)
	birdMgr := bird.NewManager(cfg.BirdPeerDir, cfg.BirdcPath)

	h := handler.NewHandler(wgMgr, birdMgr, peerStore, cfg.AgentToken, agentKP)

	wsURL := cfg.CenterURL + "/api/v1/agent/ws?node_id=" + cfg.NodeID
	wsClient := ws.NewClient(wsURL, cfg.AgentToken, cfg.NodeID, cfg.ReconnectMaxInterval, h.HandleMessage)
	h.SetWSClient(wsClient)

	wsClient.SetKeyPair(agentKP)

	serverPubBytes, err := peerStore.GetServerPubKey()
	if err == nil && len(serverPubBytes) > 0 {
		wsClient.SetServerPubKey(serverPubBytes)
		log.Debug("loaded server pubkey from store")
	}

	wsClient.SetOnKeyExchange(h.MakeKeyExchangeHandler())

	wsClient.SetOnConnect(func() {
		h.RequestSync()
	})

	collector := metrics.NewCollector(wsClient, wgMgr, h, cfg.HeartbeatInterval, cfg.BirdDetailInterval)
	collector.SetVersion(Version)
	go collector.Run()

	go wsClient.Run()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.WithField("signal", s).Debug("signal received")

	log.Info("shutting down")
	collector.Stop()
	wsClient.Close()
}

func ensureStoreDirectory(storePath string) error {
	storeDir := filepath.Dir(storePath)
	if storeDir == "." {
		return nil
	}

	if isWithinDefaultStateDirectory(storeDir) {
		return ensurePrivateStateDirectory(storeDir)
	}

	info, err := os.Stat(storeDir)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("store parent is not a directory: %s", storeDir)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}

	return os.MkdirAll(storeDir, stateDirPerm)
}

func ensurePrivateStateDirectory(path string) error {
	if err := os.MkdirAll(path, stateDirPerm); err != nil {
		return err
	}
	return os.Chmod(path, stateDirPerm)
}

func isWithinDefaultStateDirectory(path string) bool {
	root := filepath.Clean(defaultStateDir)
	cleanPath := filepath.Clean(path)
	if cleanPath == root {
		return true
	}
	rel, err := filepath.Rel(root, cleanPath)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
