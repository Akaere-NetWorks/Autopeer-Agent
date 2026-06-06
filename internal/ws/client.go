package ws

import (
	"crypto/cipher"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/akaere/autopeer-agent/internal/crypto"
	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "ws")

type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
}

type MessageHandler func(msg Message)

type KeyExchangeHandler func(c *Client) error

type sendItem struct {
	msgType int
	data    []byte
}

type Client struct {
	url       string
	token     string
	nodeID    string
	conn      *websocket.Conn
	mu        sync.Mutex
	handler   MessageHandler
	onConnect func()
	onKeyX    KeyExchangeHandler
	done      chan struct{}
	doneOnce  sync.Once
	connDone  chan struct{}
	sendCh    chan sendItem
	reconnMax time.Duration

	encrypted  bool
	aead       cipher.AEAD
	keypair    *crypto.KeyPair
	serverPub  []byte
	hasKeypair bool
}

func NewClient(url, token, nodeID string, reconnMaxSec int, handler MessageHandler) *Client {
	return &Client{
		url:       url,
		token:     token,
		nodeID:    nodeID,
		handler:   handler,
		done:      make(chan struct{}),
		reconnMax: time.Duration(reconnMaxSec) * time.Second,
	}
}

func (c *Client) SetKeyPair(kp *crypto.KeyPair) {
	c.keypair = kp
	c.hasKeypair = kp != nil
}

func (c *Client) SetServerPubKey(pub []byte) {
	if pub != nil {
		c.serverPub = make([]byte, len(pub))
		copy(c.serverPub, pub)
	}
}

func (c *Client) HasKeyPair() bool {
	return c.hasKeypair
}

func (c *Client) HasServerPubKey() bool {
	return len(c.serverPub) > 0
}

func (c *Client) SetOnKeyExchange(fn KeyExchangeHandler) {
	c.onKeyX = fn
}

func (c *Client) EnableEncryption(sessionKey crypto.SessionKey) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var err error
	c.aead, err = crypto.NewAEAD(sessionKey)
	if err != nil {
		log.WithError(err).Error("failed to create AEAD")
		return
	}
	c.encrypted = true
	log.Info("encryption enabled on client")
}

func (c *Client) IsEncrypted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.encrypted
}

func (c *Client) SetOnConnect(fn func()) {
	c.onConnect = fn
}

func (c *Client) Run() {
	backoff := time.Second
	for {
		select {
		case <-c.done:
			return
		default:
		}

		log.Debug("starting connection attempt")

		c.mu.Lock()
		c.encrypted = false
		c.aead = nil
		c.mu.Unlock()

		err := c.connect()
		if err != nil {
			log.WithError(err).WithField("backoff", backoff).Warn("websocket connect error")
			select {
			case <-time.After(backoff):
			case <-c.done:
				return
			}
			backoff = time.Duration(math.Min(float64(backoff*2), float64(c.reconnMax)))
			continue
		}

		backoff = time.Second

		readDone := make(chan struct{})
		go func() {
			c.readLoop()
			close(readDone)
		}()

		if c.onKeyX != nil {
			log.Debug("executing key exchange handler")
			if err := c.onKeyX(c); err != nil {
				log.WithError(err).Warn("key exchange failed, closing connection")
				c.mu.Lock()
				if c.conn != nil {
					c.conn.Close()
				}
				c.mu.Unlock()
				<-readDone
				continue
			}
		}

		if c.onConnect != nil {
			c.onConnect()
		}
		<-readDone
	}
}

func (c *Client) connect() error {
	log.WithFields(logrus.Fields{"url": c.url, "has_keypair": c.hasKeypair, "has_server_pub": len(c.serverPub) > 0}).Debug("connecting to websocket")

	header := http.Header{}
	header.Set("X-Node-ID", c.nodeID)

	useToken := c.token != ""
	if c.hasKeypair && len(c.serverPub) > 0 {
		if c.token == "" {
			useToken = false
		} else {
			useToken = true
		}
	}

	if useToken {
		header.Set("X-Agent-Token", c.token)
		log.Debug("connecting with token auth")
	} else {
		log.Debug("connecting with key auth (no token)")
	}

	conn, _, err := websocket.DefaultDialer.Dial(c.url, header)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}

	c.mu.Lock()
	if c.connDone != nil {
		close(c.connDone)
	}
	c.connDone = make(chan struct{})
	c.sendCh = make(chan sendItem, 256)
	c.conn = conn
	c.mu.Unlock()

	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	go c.writeLoop(conn, c.connDone)

	log.Info("websocket connected to center")
	return nil
}

func (c *Client) writeLoop(conn *websocket.Conn, connDone chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case item, ok := <-c.sendCh:
			if !ok {
				return
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(item.msgType, item.data); err != nil {
				log.WithError(err).Error("write error in writeLoop")
				return
			}
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				log.WithError(err).Error("ping write error")
				return
			}
		case <-connDone:
			return
		case <-c.done:
			return
		}
	}
}

func (c *Client) readLoop() {
	c.mu.Lock()
	conn := c.conn
	connDone := c.connDone
	c.mu.Unlock()

	if conn == nil {
		return
	}

	conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			log.WithError(err).Error("websocket read error")
			if connDone != nil {
				select {
				case <-connDone:
				default:
					close(connDone)
				}
			}
			c.mu.Lock()
			if c.connDone == connDone {
				c.connDone = nil
			}
			c.conn = nil
			c.mu.Unlock()
			conn.Close()
			return
		}

		conn.SetReadDeadline(time.Now().Add(90 * time.Second))

		c.mu.Lock()
		isEncrypted := c.encrypted && c.aead != nil
		aead := c.aead
		c.mu.Unlock()

		if isEncrypted {
			if msgType != websocket.BinaryMessage {
				log.Warn("expected binary message in encrypted mode")
				continue
			}
			if aead == nil {
				continue
			}
			plain, decErr := crypto.Decrypt(aead, data)
			if decErr != nil {
				log.WithError(decErr).Error("decrypt message failed")
				continue
			}
			data = plain
		}

		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			log.WithError(err).Warn("invalid message received")
			continue
		}

		log.WithField("type", msg.Type).Debug("message received")

		c.handler(msg)
	}
}

func (c *Client) Send(msg Message) error {
	log.WithField("type", msg.Type).Debug("sending message")

	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return fmt.Errorf("not connected")
	}

	var msgType int
	var wireData []byte

	if c.encrypted && c.aead != nil {
		wireData, err = crypto.Encrypt(c.aead, data)
		msgType = websocket.BinaryMessage
	} else {
		wireData = data
		msgType = websocket.TextMessage
	}
	sendCh := c.sendCh
	c.mu.Unlock()

	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	select {
	case sendCh <- sendItem{msgType: msgType, data: wireData}:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

func (c *Client) Close() {
	c.doneOnce.Do(func() {
		close(c.done)
	})
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
}

func (c *Client) NodeID() string {
	return c.nodeID
}
