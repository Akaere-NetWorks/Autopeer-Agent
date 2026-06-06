package crypto

import (
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

type KeyPair struct {
	PrivateKey *ecdh.PrivateKey
	PublicKey  *ecdh.PublicKey
}

type SessionKey [chacha20poly1305.KeySize]byte

func GenerateKeyPair() (*KeyPair, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key pair: %w", err)
	}
	return &KeyPair{
		PrivateKey: priv,
		PublicKey:  priv.PublicKey(),
	}, nil
}

func PublicKeyFromHex(hexStr string) (*ecdh.PublicKey, error) {
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("decode public key hex: %w", err)
	}
	return ecdh.X25519().NewPublicKey(data)
}

func PrivateKeyFromHex(hexStr string) (*ecdh.PrivateKey, error) {
	data, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, fmt.Errorf("decode private key hex: %w", err)
	}
	return ecdh.X25519().NewPrivateKey(data)
}

func PubKeyHex(pub *ecdh.PublicKey) string {
	return hex.EncodeToString(pub.Bytes())
}

func PrivKeyHex(priv *ecdh.PrivateKey) string {
	return hex.EncodeToString(priv.Bytes())
}

func DeriveSharedSecret(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	return priv.ECDH(pub)
}

func DeriveEncKey(shared []byte, nonce []byte) (SessionKey, error) {
	var key SessionKey
	kdf := hkdf.New(sha256.New, shared, nonce, []byte("autopeer-encryption-key"))
	if _, err := io.ReadFull(kdf, key[:]); err != nil {
		return key, fmt.Errorf("derive enc key: %w", err)
	}
	return key, nil
}

func NewAEAD(key SessionKey) (cipher.AEAD, error) {
	return chacha20poly1305.NewX(key[:])
}

func Encrypt(aead cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	out := make([]byte, len(nonce)+len(plaintext)+aead.Overhead())
	copy(out[:len(nonce)], nonce)
	aead.Seal(out[len(nonce):len(nonce)], nonce, plaintext, nil)
	return out, nil
}

func Decrypt(aead cipher.AEAD, data []byte) ([]byte, error) {
	nonceSize := aead.NonceSize()
	if len(data) < nonceSize+aead.Overhead() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := data[:nonceSize]
	ciphertext := data[nonceSize:]
	plain, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return plain, nil
}

func ComputeAuthProof(shared []byte, nonce []byte) []byte {
	mac := hmac.New(sha256.New, shared)
	mac.Write([]byte("autopeer-key-auth"))
	mac.Write(nonce)
	return mac.Sum(nil)
}

func NewNonce() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return b, nil
}
