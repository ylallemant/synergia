package identity

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/argon2"
)

// Identity holds the worker's Ed25519 keypair and derived fingerprint.
type Identity struct {
	PrivateKey  ed25519.PrivateKey
	PublicKey   ed25519.PublicKey
	Fingerprint string // SHA256(public_key) as hex
}

// LoadOrCreate loads an existing identity from dataDir, or generates a new one.
func LoadOrCreate(dataDir string) (*Identity, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	encPath := filepath.Join(dataDir, "identity.enc")
	pubPath := filepath.Join(dataDir, "identity.pub")
	fpPath := filepath.Join(dataDir, "fingerprint")

	// Try loading existing identity
	if _, err := os.Stat(encPath); err == nil {
		return load(encPath, pubPath)
	}

	// Generate new keypair
	log.Info().Str("dir", dataDir).Msg("generating new worker identity")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate keypair: %w", err)
	}

	id := &Identity{
		PrivateKey:  priv,
		PublicKey:   pub,
		Fingerprint: fingerprint(pub),
	}

	// Encrypt and save private key
	if err := saveEncrypted(encPath, priv); err != nil {
		return nil, fmt.Errorf("save private key: %w", err)
	}

	// Save public key as PEM
	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "ED25519 PUBLIC KEY",
		Bytes: pub,
	})
	if err := os.WriteFile(pubPath, pubPEM, 0644); err != nil {
		return nil, fmt.Errorf("save public key: %w", err)
	}

	// Save fingerprint as plain text
	if err := os.WriteFile(fpPath, []byte(id.Fingerprint+"\n"), 0644); err != nil {
		return nil, fmt.Errorf("save fingerprint: %w", err)
	}

	log.Info().Str("fingerprint", id.Fingerprint).Msg("identity created")
	return id, nil
}

func load(encPath, pubPath string) (*Identity, error) {
	// Load public key
	pubPEM, err := os.ReadFile(pubPath)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, _ := pem.Decode(pubPEM)
	if block == nil {
		return nil, fmt.Errorf("invalid PEM in %s", pubPath)
	}
	pub := ed25519.PublicKey(block.Bytes)

	// Load and decrypt private key
	priv, err := loadEncrypted(encPath)
	if err != nil {
		return nil, fmt.Errorf("load private key: %w", err)
	}

	id := &Identity{
		PrivateKey:  priv,
		PublicKey:   pub,
		Fingerprint: fingerprint(pub),
	}

	log.Info().Str("fingerprint", id.Fingerprint).Msg("identity loaded")
	return id, nil
}

// Sign signs a payload with the worker's private key and returns the hex-encoded signature.
func (id *Identity) Sign(data []byte) string {
	sig := ed25519.Sign(id.PrivateKey, data)
	return hex.EncodeToString(sig)
}

func fingerprint(pub ed25519.PublicKey) string {
	hash := sha256.Sum256(pub)
	return hex.EncodeToString(hash[:])
}

// deriveKey derives an AES-256 key from machine-local secrets using Argon2id.
func deriveKey() []byte {
	hostname, _ := os.Hostname()
	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME")
	}

	// Combine machine-local values as salt material
	salt := sha256.Sum256([]byte(hostname + ":" + username + ":synergia-worker"))

	return argon2.IDKey(
		[]byte("synergia-worker-identity"),
		salt[:],
		1,       // time
		64*1024, // memory (64MB)
		4,       // threads
		32,      // key length (AES-256)
	)
}

func saveEncrypted(path string, privKey ed25519.PrivateKey) error {
	key := deriveKey()

	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(privKey), nil)
	return os.WriteFile(path, ciphertext, 0600)
}

func loadEncrypted(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	key := deriveKey()

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt failed (identity may be corrupted or machine changed): %w", err)
	}

	return ed25519.PrivateKey(plaintext), nil
}
