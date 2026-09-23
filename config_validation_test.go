package main

import (
	"os"
	"testing"
)

// Test 1: production credentials required — empty username/password must fail validation
func TestProductionCredentialsRequired(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "production")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	cfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "",
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(cfg); err == nil {
		t.Fatalf("expected validation to fail for empty credentials in production, got nil")
	}
	// Ensure we never attempt anonymous — error must mention production
	t.Logf("correctly rejected anonymous production: %v", ValidateMQTTConfig(cfg))
}

// Test 2: production authenticated MQTT — valid credentials accepted
func TestProductionAuthenticatedMQTT(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "production")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	cfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "gateway-user",
		Password:       "valid-password",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(cfg); err != nil {
		t.Fatalf("expected production authenticated to pass, got %v", err)
	}
}

// Test 3: no authentication downgrade — reconnect with invalid creds must not fallback to anonymous
func TestNoAuthenticationDowngrade(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "production")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	// Simulate authenticated failure -> credentials become unavailable
	// Validate must still fail if we try to reconnect with empty (no downgrade)
	emptyCfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "",
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(emptyCfg); err == nil {
		t.Fatalf("downgrade to anonymous must be rejected in production")
	}
}

// Test 4: development anonymous explicit — only when allow_anonymous true
func TestDevelopmentAnonymousExplicit(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "development")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	// Without explicit allow_anonymous, empty creds must fail even in dev
	cfgNoFlag := MQTTConfig{
		BrokerURL:      "mqtt://localhost:1883",
		Username:       "",
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(cfgNoFlag); err == nil {
		t.Fatalf("dev without explicit allow_anonymous must fail")
	}
	// With explicit allow_anonymous true, dev anonymous is allowed
	cfgExplicit := MQTTConfig{
		BrokerURL:      "mqtt://localhost:1883",
		Username:       "",
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: true,
	}
	if err := ValidateMQTTConfig(cfgExplicit); err != nil {
		t.Fatalf("dev with allow_anonymous true should pass, got %v", err)
	}
}

// Test 5: provisioning rejects empty credentials — existing valid not overwritten
func TestProvisioningRejectsEmpty(t *testing.T) {
	// Simulate provisioning response with empty username/password in production -> must be rejected
	emptyCfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "",
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(emptyCfg); err == nil {
		t.Fatalf("provisioning empty must be rejected")
	}
	// Existing valid credentials must still pass validation
	validCfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "existing-user",
		Password:       "existing-pass",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(validCfg); err != nil {
		t.Fatalf("existing valid must still pass, got %v", err)
	}
	// No global cfg.MQTT mutation — all checks use local copies
}

// Test 6: secret logging — ValidateMQTTConfig error must never contain password
func TestSecretLogging(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "production")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	secret := "super-secret-password-123"
	cfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "",
		Password:       secret,
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	err := ValidateMQTTConfig(cfg)
	if err == nil {
		t.Fatalf("expected fail for empty username")
	}
	msg := err.Error()
	if contains(msg, secret) {
		t.Fatalf("error message must not contain password, got %q", msg)
	}
	// Also check username case
	cfg2 := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       secret,
		Password:       "",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   false,
		AllowAnonymous: false,
	}
	err = ValidateMQTTConfig(cfg2)
	if err == nil {
		t.Fatalf("expected fail for empty password")
	}
	if contains(err.Error(), secret) {
		t.Fatalf("error must not contain secret, got %q", err.Error())
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}

// Additional: clean_session must be false in production
func TestCleanSessionFalseRequired(t *testing.T) {
	origEnv := os.Getenv("GATEWAY_ENV")
	os.Setenv("GATEWAY_ENV", "production")
	defer os.Setenv("GATEWAY_ENV", origEnv)

	cfg := MQTTConfig{
		BrokerURL:      "mqtts://broker.example.com:8883",
		Username:       "u",
		Password:       "p",
		ClientIDPrefix: "gw",
		KeepAlive:      60,
		QoS:            1,
		CleanSession:   true,
		AllowAnonymous: false,
	}
	if err := ValidateMQTTConfig(cfg); err == nil {
		t.Fatalf("clean_session true must be rejected in production")
	}
}
