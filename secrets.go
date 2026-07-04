package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// ---------- Secrets Encryption ----------

type secretsManager struct {
	key     []byte
	keyPath string
	mu      sync.Mutex
}

func newSecretsManager(keyPath string) *secretsManager {
	sm := &secretsManager{keyPath: keyPath}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		sm.key = make([]byte, 32)
		if _, err := rand.Read(sm.key); err != nil {
			logger.WithError(err).Warn("secrets: key generation failed, secrets will be stored in plaintext")
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			logger.WithError(err).Warn("secrets: cannot create key dir")
			return nil
		}
		if err := os.WriteFile(keyPath, sm.key, 0600); err != nil {
			logger.WithError(err).Warn("secrets: cannot write key file")
			return nil
		}
		logger.Info("secrets: encryption key generated")
	} else {
		sm.key = data
	}
	return sm
}

func (sm *secretsManager) encrypt(plaintext string) (string, error) {
	if plaintext == "" || sm == nil {
		return plaintext, nil
	}
	block, err := aes.NewCipher(sm.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return "enc:" + base64.StdEncoding.EncodeToString(sealed), nil
}

func (sm *secretsManager) decrypt(encrypted string) (string, error) {
	if !strings.HasPrefix(encrypted, "enc:") || sm == nil {
		return encrypted, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted[4:])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(sm.key)
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := aead.NonceSize()
	if len(raw) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (sm *secretsManager) processConfig(cfg *Config) error {
	if sm == nil {
		return nil
	}
	var needsRewrite bool

	if cfg.MQTT.Password != "" {
		if strings.HasPrefix(cfg.MQTT.Password, "enc:") {
			dec, err := sm.decrypt(cfg.MQTT.Password)
			if err != nil {
				return fmt.Errorf("decrypt mqtt password: %w", err)
			}
			cfg.MQTT.Password = dec
		} else {
			enc, err := sm.encrypt(cfg.MQTT.Password)
			if err != nil {
				return fmt.Errorf("encrypt mqtt password: %w", err)
			}
			cfg.MQTT.Password = enc
			needsRewrite = true
		}
	}

	if cfg.Gateway.ProvisionToken != "" {
		if strings.HasPrefix(cfg.Gateway.ProvisionToken, "enc:") {
			dec, err := sm.decrypt(cfg.Gateway.ProvisionToken)
			if err != nil {
				return fmt.Errorf("decrypt provision token: %w", err)
			}
			cfg.Gateway.ProvisionToken = dec
		} else {
			enc, err := sm.encrypt(cfg.Gateway.ProvisionToken)
			if err != nil {
				return fmt.Errorf("encrypt provision token: %w", err)
			}
			cfg.Gateway.ProvisionToken = enc
			needsRewrite = true
		}
	}

	if needsRewrite {
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal config: %w", err)
		}
		if err := os.WriteFile(configPath(), data, 0644); err != nil {
			return fmt.Errorf("rewrite config: %w", err)
		}
		logger.Info("secrets: encrypted plaintext secrets in config file")
	}
	return nil
}
