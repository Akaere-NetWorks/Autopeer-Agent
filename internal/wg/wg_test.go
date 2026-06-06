package wg

import (
	"bytes"
	"strings"
	"testing"
)

func render(t *testing.T, cfg PeerConfig) string {
	t.Helper()
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, cfg); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	return buf.String()
}

func TestConfigTemplatePresharedKey(t *testing.T) {
	psk := "ZmFrZS1wcmVzaGFyZWQta2V5LWZvci10ZXN0aW5nLTAwMD0="
	out := render(t, PeerConfig{
		ASN:            4242420123,
		PrivateKey:     "priv",
		ListenPort:     51820,
		OurLLA:         "fe80::1",
		RemotePubkey:   "pub",
		RemoteEndpoint: "192.0.2.1:51820",
		PresharedKey:   &psk,
	})
	if !strings.Contains(out, "PresharedKey = "+psk) {
		t.Errorf("expected PresharedKey line in config, got:\n%s", out)
	}
	// PresharedKey belongs to the [Peer] section, after PublicKey.
	if strings.Index(out, "PublicKey") > strings.Index(out, "PresharedKey") {
		t.Errorf("PresharedKey must appear after PublicKey:\n%s", out)
	}
}

func TestConfigTemplateNoPresharedKey(t *testing.T) {
	out := render(t, PeerConfig{
		ASN:            4242420123,
		PrivateKey:     "priv",
		ListenPort:     51820,
		OurLLA:         "fe80::1",
		RemotePubkey:   "pub",
		RemoteEndpoint: "192.0.2.1:51820",
	})
	if strings.Contains(out, "PresharedKey") {
		t.Errorf("PresharedKey must be absent when not set, got:\n%s", out)
	}
}
