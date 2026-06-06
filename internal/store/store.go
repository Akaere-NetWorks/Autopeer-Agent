package store

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("pkg", "store")

var (
	bucketPeers     = []byte("peers")
	bucketMeta      = []byte("meta")
	bucketKeys      = []byte("keys")
	keyVersion      = []byte("version")
	keyUpdateID     = []byte("update_id")
	keyLocalPriv    = []byte("local_priv")
	keyLocalPub     = []byte("local_pub")
	keyServerPub    = []byte("server_pub")
)

type PeerInfo struct {
	ASN                int64  `json:"asn"`
	RemoteLLA          string `json:"remote_lla"`
	WgInterface        string `json:"wg_interface"`
	BgpProtoName       string `json:"bgp_proto_name"`
	BirdConfigFilename string `json:"bird_config_filename,omitempty"`
}

type Store struct {
	db *bolt.DB
}

func Open(path string) (*Store, error) {
	log.WithField("path", path).Debug("opening store")

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		log.WithError(err).WithField("path", path).Error("bolt open failed")
		return nil, fmt.Errorf("bolt open: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketPeers); err != nil {
			log.WithError(err).Error("create peers bucket failed")
			return fmt.Errorf("create peers bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(bucketMeta); err != nil {
			log.WithError(err).Error("create meta bucket failed")
			return fmt.Errorf("create meta bucket: %w", err)
		}
		if _, err := tx.CreateBucketIfNotExists(bucketKeys); err != nil {
			log.WithError(err).Error("create keys bucket failed")
			return fmt.Errorf("create keys bucket: %w", err)
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}

	log.WithField("path", path).Info("store opened successfully")
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	log.Debug("closing store")
	return s.db.Close()
}

func (s *Store) PutPeer(peerID string, info PeerInfo) error {
	log.WithField("peer_id", peerID).WithField("asn", info.ASN).Debug("putting peer")

	val, err := json.Marshal(info)
	if err != nil {
		log.WithError(err).WithField("peer_id", peerID).Error("marshal peer info failed")
		return err
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPeers).Put([]byte(peerID), val)
	})
	if err != nil {
		log.WithError(err).WithField("peer_id", peerID).Error("put peer failed")
		return err
	}

	log.WithField("peer_id", peerID).Info("peer stored")
	return nil
}

func (s *Store) GetPeer(peerID string) (*PeerInfo, error) {
	log.WithField("peer_id", peerID).Debug("getting peer")

	var info PeerInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketPeers).Get([]byte(peerID))
		if data == nil {
			log.WithField("peer_id", peerID).Error("peer not found")
			return fmt.Errorf("peer %s not found", peerID)
		}
		return json.Unmarshal(data, &info)
	})
	if err != nil {
		return nil, err
	}
	return &info, nil
}

func (s *Store) DeletePeer(peerID string) error {
	log.WithField("peer_id", peerID).Debug("deleting peer")

	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPeers).Delete([]byte(peerID))
	})
	if err != nil {
		log.WithError(err).WithField("peer_id", peerID).Error("delete peer failed")
		return err
	}

	log.WithField("peer_id", peerID).Info("peer deleted")
	return nil
}

func (s *Store) ForEachPeer(fn func(peerID string, info PeerInfo) error) error {
	log.Debug("iterating all peers")
	return s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketPeers).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var info PeerInfo
			if err := json.Unmarshal(v, &info); err != nil {
				log.WithError(err).WithField("peer_id", string(k)).Warn("skipping peer with unmarshal error in ForEachPeer")
				continue
			}
			if err := fn(string(k), info); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) PeerCount() (int, error) {
	log.Debug("counting peers")
	var count int
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPeers)
		if b == nil {
			return nil
		}
		count = b.Stats().KeyN
		return nil
	})
	return count, err
}

func (s *Store) ReplaceAll(peers map[string]PeerInfo) error {
	log.WithField("count", len(peers)).Debug("replacing all peers")

	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketPeers)
		var keys [][]byte
		c := b.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			key := make([]byte, len(k))
			copy(key, k)
			keys = append(keys, key)
		}
		for _, k := range keys {
			if err := b.Delete(k); err != nil {
				log.WithError(err).WithField("peer_id", string(k)).Error("delete existing peer during replace failed")
				return err
			}
		}
		for id, info := range peers {
			val, err := json.Marshal(info)
			if err != nil {
				log.WithError(err).WithField("peer_id", id).Error("marshal peer info during replace failed")
				return err
			}
			if err := b.Put([]byte(id), val); err != nil {
				log.WithError(err).WithField("peer_id", id).Error("put peer during replace failed")
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	log.WithField("count", len(peers)).Info("replaced all peers")
	return nil
}

func (s *Store) IsASNTracked(asn int64) (bool, error) {
	log.WithField("asn", asn).Debug("checking if ASN is tracked")
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketPeers).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var info PeerInfo
			if err := json.Unmarshal(v, &info); err != nil {
				continue
			}
			if info.ASN == asn {
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}

func (s *Store) GetVersion() (string, error) {
	log.Debug("getting version")
	var v string
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketMeta).Get(keyVersion)
		if data != nil {
			v = string(data)
		}
		return nil
	})
	return v, err
}

func (s *Store) PutVersion(v string) error {
	log.WithField("version", v).Debug("putting version")
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyVersion, []byte(v))
	})
}

func (s *Store) GetUpdateID() (string, error) {
	log.Debug("getting update ID")
	var v string
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket(bucketMeta).Get(keyUpdateID)
		if data != nil {
			v = string(data)
		}
		return nil
	})
	return v, err
}

func (s *Store) PutUpdateID(id string) error {
	log.WithField("update_id", id).Debug("putting update ID")
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMeta).Put(keyUpdateID, []byte(id))
	})
}

func (s *Store) HasKeyPair() bool {
	log.Debug("checking for key pair")
	var exists bool
	s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKeys)
		if b == nil {
			return nil
		}
		if b.Get(keyLocalPriv) != nil && b.Get(keyLocalPub) != nil {
			exists = true
		}
		return nil
	})
	return exists
}

func (s *Store) PutKeyPair(priv, pub []byte) error {
	log.Debug("putting key pair")
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKeys)
		if err := b.Put(keyLocalPriv, priv); err != nil {
			return err
		}
		return b.Put(keyLocalPub, pub)
	})
}

func (s *Store) GetKeyPair() ([]byte, []byte, error) {
	log.Debug("getting key pair")
	var priv, pub []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKeys)
		if b == nil {
			return fmt.Errorf("keys bucket not found")
		}
		priv = b.Get(keyLocalPriv)
		pub = b.Get(keyLocalPub)
		if priv == nil || pub == nil {
			return fmt.Errorf("key pair not found")
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	privCopy := make([]byte, len(priv))
	copy(privCopy, priv)
	pubCopy := make([]byte, len(pub))
	copy(pubCopy, pub)
	return privCopy, pubCopy, nil
}

func (s *Store) PutServerPubKey(pub []byte) error {
	log.Debug("putting server public key")
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketKeys).Put(keyServerPub, pub)
	})
}

func (s *Store) GetServerPubKey() ([]byte, error) {
	log.Debug("getting server public key")
	var pub []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketKeys)
		if b == nil {
			return fmt.Errorf("keys bucket not found")
		}
		pub = b.Get(keyServerPub)
		if pub == nil {
			return fmt.Errorf("server public key not found")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	pubCopy := make([]byte, len(pub))
	copy(pubCopy, pub)
	return pubCopy, nil
}
