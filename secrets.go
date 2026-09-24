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

// cryptField normalizes one secret field. The in-memory value is ALWAYS
// plaintext afterwards. It returns the value to persist in the file (enc:)
// and whether the file needs rewriting. This fixes the old bug where freshly
// encrypted values were left as ciphertext in memory, so the process ran with
// enc:... passwords/tokens until the next restart.
func (sm *secretsManager) cryptField(value *string, fieldName string) (fileValue string, changed bool, err error) {
	if *value == "" {
		return "", false, nil
	}
	if strings.HasPrefix(*value, "enc:") {
		dec, derr := sm.decrypt(*value)
		if derr != nil {
			return "", false, fmt.Errorf("decrypt %s: %w", fieldName, derr)
		}
		*value = dec
		return "", false, nil
	}
	enc, eerr := sm.encrypt(*value)
	if eerr != nil {
		return "", false, fmt.Errorf("encrypt %s: %w", fieldName, eerr)
	}
	return enc, true, nil
}

func (sm *secretsManager) processConfig(cfg *Config) error {
	if sm == nil {
		return nil
	}
	// Collect encrypted file values; cfg itself stays plaintext in memory.
	type pending struct {
		apply func(*Config)
	}
	var pend []pending

	if v, changed, err := sm.cryptField(&cfg.MQTT.Password, "mqtt password"); err != nil {
		return err
	} else if changed {
		v := v
		pend = append(pend, pending{apply: func(c *Config) { c.MQTT.Password = v }})
	}

	if v, changed, err := sm.cryptField(&cfg.Gateway.ProvisionToken, "provision token"); err != nil {
		return err
	} else if changed {
		v := v
		pend = append(pend, pending{apply: func(c *Config) { c.Gateway.ProvisionToken = v }})
	}

	for i := range cfg.LocalBroker.Users {
		if v, changed, err := sm.cryptField(&cfg.LocalBroker.Users[i].Password, "local broker password"); err != nil {
			return err
		} else if changed {
			v, i := v, i
			pend = append(pend, pending{apply: func(c *Config) { c.LocalBroker.Users[i].Password = v }})
		}
	}

	if v, changed, err := sm.cryptField(&cfg.LocalClient.Password, "local client password"); err != nil {
		return err
	} else if changed {
		v := v
		pend = append(pend, pending{apply: func(c *Config) { c.LocalClient.Password = v }})
	}

	if len(pend) == 0 {
		return nil
	}
	// Marshal a copy with ciphertext so memory keeps plaintext. Deep-copy the
	// users slice (it shares backing storage with cfg).
	fileCfg := *cfg
	fileUsers := make([]LocalBrokerUser, len(cfg.LocalBroker.Users))
	copy(fileUsers, cfg.LocalBroker.Users)
	fileCfg.LocalBroker.Users = fileUsers
	for _, p := range pend {
		p.apply(&fileCfg)
	}
	data, err := yaml.Marshal(&fileCfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configPath(), data, 0600); err != nil {
		return fmt.Errorf("rewrite config: %w", err)
	}
	logger.Info("secrets: encrypted plaintext secrets in config file")
	return nil
}
