// Package sshkey manages the CLI's own SSH key, which the API adds to a
// sandbox's authorized_keys whenever the CLI connects.
package sshkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Path returns the private key path inside the config directory.
func Path(configDir string) string { return filepath.Join(configDir, "id_ed25519") }

// Ensure creates the key pair on first use and returns the private key path and
// the public key as an authorized_keys line.
func Ensure(configDir string) (keyPath, authorizedKey string, err error) {
	keyPath = Path(configDir)
	pubPath := keyPath + ".pub"
	if data, err := os.ReadFile(pubPath); err == nil {
		return keyPath, strings.TrimSpace(string(data)), nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "cloudex")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " cloudex"
	if err := os.WriteFile(pubPath, []byte(line+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return keyPath, line, nil
}
