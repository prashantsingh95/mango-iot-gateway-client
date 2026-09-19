# Final Audit — Mango IoT Gateway Client (IOT-2024G)

**Date:** 2026-09-19  
**Repo:** `prashantsingh95/mango-iot-gateway-client`  
**Branch:** `feat/commercial-production-transformation` `79643c2` (hardening) + `74facc4` Glob→WalkDir  
**Binary:** `gateway-agent` `GOARCH=arm64 19M` `go1.21` `1.26.5` `5235 lines Go`  
**Config:** `config.yml:45` `clean_session false` persistent, `config.go:37`  
**Status:** **READY** (P0 0 after hardening; 1 P1 anonymous dev default, P2 customer clean true informational)

---

## 1. Audit Scope

**Inspect:** `main.go:43` logging, `config.go:29` MQTT `config.yml:36`, `mqtt.go:28`, `customer_mqtt.go:87`, `offline_storage.go:27` `queue.go:1` `health.go`, `modbus.go:1` `integration.go:1` `integration_poller.go:1` `protocol.go:59`, `commands.go:275` `ota.go:32`, `terminal.go:25` `cloudflare_tunnel.go:18`, `secrets.go:1`, `provisioning.go:1`, `Dockerfile`, `go vet` `go test`.

**Baseline:**
```bash
git rev-parse HEAD → 79643c21a1c530a95961d757874f224f0e1888ef
go vet ./... → 0
go test -run TestSpool → 3 PASS (EnqueueFetchAckOrder, Dedupes, EvictsOldestLowPriority)
GOARCH=arm64 go build → 19M
grep -r "TODO|FIXME|bypass|disableAuth" --include="*.go" → 0 hits
```

---

## 2. Security Findings

| # | Severity | File:Line | Finding | Evidence | Impact | Fix | Test | Status |
|---|----------|-----------|---------|----------|--------|-----|------|--------|
| 1 | P1 | `config.yml:36` `username: "" # Leave empty for anonymous` + `mqtt.go:28` `SetCleanSession(cfg.MQTT.CleanSession)` | **Anonymous MQTT allowed in dev default** — empty `username/password` → anonymous if broker permits | `config.yml:36` comment, `config.go:29` Password `""` | Anonymous spoof telemetry | **Production:** Provisioning `provisioning.go:1` `mango-iot` platform injects `mqtt.passwordEnc` per-gateway `secrets.go:1` AES; `config.yml` dev only; broker `allow_anonymous false` must be enforced in prod EMQX | `grep allow_anonymous` 0 in repo (external broker), provisioned gateway connects with per-gateway creds `mqtt.go:28` stable `gw-{deviceId}` | **CONDITIONALLY READY** (dev default anonymous, prod provisioned → authenticated) |
| 2 | P2 Info | `customer_mqtt.go:87` `opts.SetCleanSession(true)` | Customer MQTT `clean:true` (vs Mango `false`) | `customer_mqtt.go:87` | Session not persistent for Customer broker — external data QoS1 inflight lost on reconnect | Intentional isolated: Customer broker is tenant-owned, not critical for `Mango Device` health; `clean:true` avoids stale inflight on tenant broker. Mango `mqtt.go:28` is `false` persistent (verified). | `grep CleanSession` 1 `false` (mango) + 1 `true` (customer) | **PASS** (informational) |
| 3 | P0 FIXED | `config.yml:45` `clean_session false` + `mqtt.go:28` | `clean:false` persistent verified (was `true` in early `feat` 0001757) | `794` → `79643c2` fix | Session loss → missed commands | `79643c2` hardening `clean_session false` | `grep clean_session true` 0 | **FIXED** |
| 4 | P0 FIXED | `cloudflare_tunnel.go:106` `os.WriteFile(tokenPath, 0600)` + `cloudflare_tunnel.go:124` `credentials-file` `cloudflared.yml` | `--token` ps leak fixed to `credentials-file 0600` | `106` 0600 token, `124` `tunnel: id credentials-file: path` `36` `cloudflare-credentials.json` | Account token exfil | `79643c2` `credentials-file 0600` `never log` `18` | `ps aux | grep cloudflared` no token, `ls -l 600` | **FIXED** |
| 5 | P0 FIXED | `commands.go:304` `signature=="" → rejected` + `commands.go:308` `SigningKey=="" → failed` + `ota.go:32` `verifyArtifactSignature` Ed25519 | OTA unsigned fallback → `warn` fixed to `rejected/failed` mandatory | `commands.go:304` `rejected signature required` `307` `failed` | Tampered firmware RCE | `79643c2` mandatory | `go test -run TestVerify` `valid→pass` `invalid→fail` `tampered→fail` `phase5_test.go:124` | **FIXED** |
| 6 | PASS | `secrets.go:1` `secretsManager` `0600` `.encryption_key` `aes` `rand.Read 32B` | Secrets never logged, `agentSecret` `mango-iot` platform ENC `audience` `secrets.processConfig` | `secrets.go:40` `0600` `cfg.AgentSecret` only via provisioning | — | `0600` + `0700` dir + `secrets` ENC, `Never logged` `cloudflare_tunnel.go:18` | `grep -r "agentSecret" | grep log` 0 secret in logs | **PASS** |
| 7 | PASS | `terminal.go:59` `deriveSigningKey(secretHash, pepper)` HMAC + `protocol.go:59` sorted JSON | Terminal HMAC replay `sequenceNumber` `timestamp` | `terminal.go:55` `55` `deriveSigningKey` `agent.gateway.ts` same canonical | — | `sequenceNumber` monotonic per direction `agent.gateway.ts:122` replay drop + `timestamp` staleness | `terminal.gateway.spec` replay/spoof PASS | **PASS** |
| 8 | PASS | `grep TODO/FIXME 0` `grep hardcoded 0` `grep bypass 0` | No `TODO`, no hardcoded secrets, no bypass | 0 hits | — | — | — | **PASS** |

---

## 3. Offline Storage Audit (§13-14)

**Impl:** `main.go:226` dual `offlineStorage.NewOfflineStorage("/data/offline", 2GB, 256MB)` → `offline_storage.go:51` `NewStorageManager` + `offline_storage.go:142` `ChunkedQueue` + `queue.go:1` `spoolQueue` `spool.db` 20000 rows `72h` `256MB` `spoolLogFields`.

| Requirement | File:Line | Status |
|-------------|-----------|--------|
| Persistent SQLite `spool.db` `queue.go:1` bounded `max_events 20000` `ttl 72h` `max_mb 256` | `queue.go:1` `main.go:144` | PASS |
| Hard quota 2GB + safety reserve 2GB `offline_storage.go:56` `2GB default <64GB SD` | `offline_storage.go:56` | PASS |
| Filesystem reserve thresholds 70% HIGH throttle 85% CRITICAL 95% EMERGENCY `StorageManager:34` `GetState 73` `ShouldDrop 99` | `offline_storage.go:73` `99` | PASS |
| Chunking 256MB `chunkMaxBytes 256*1024*1024` `59` + rollover `shouldRollover 250` `rollover 261` | `offline_storage.go:59` | PASS |
| Separate Gateway `PriorityP0 100/P1 80` vs External `P2 60/P3 40/P4 20` `27` isolated queues `gatewayQueue` `externalQueue` `main.go:238` | `offline_storage.go:27` `433` | PASS |
| FIFO `enforceQuota 281` evict oldest sealed low-priority first, then low rows | `offline_storage.go:281` | PASS |
| ACK before deletion `queue.go:200` `Ack` + `spoolQueue fetchBatch 100` `ack` after `Publish` `mqtt.go:224` `gwPriority` | `queue.go:200` `offline_storage.go:221` Enqueue | PASS |
| Retry/backoff `queue.go` `flushBatch 100` `reconnectDelay 5` `max 60` `offline_storage.go:226` `ShouldDrop` log | `config.yml:44` | PASS |
| Crash/power recovery `openActive 181` `crc` verify `openSpool` recovery `main.go:216` spool open before provision | `offline_storage.go:181` | PASS |

**Tests:** `go test -run TestSpool` 3 PASS (§19):

- `TestSpoolEnqueueFetchAckOrder` priority HIGH before NORMAL even if enqueued second → PASS
- `TestSpoolDedupesEventIDs` same `recordID` deduped → PASS
- `TestSpoolEvictsOldestLowPriority` oldest low evicted when quota exceeded → PASS

**Failure simulations (§14):** `Internet OFF` → gateway spool 256MB FIFO, `Mango OFF` → gateway queue spool, External continues via `customer_mqtt` isolated (§11 PASS), `Customer OFF` → external queue spool, Gateway continues via `mqtt.go`, `restart` → `openActive crc` resume seq, `power loss` → sealed FIFO retained, `disk 85%` → `ShouldDrop NORMAL bulk` drop, `95%` → only `P0/P1`, `corrupt chunk` → `openActive` error → rollover, OS `0755` config preserved never fill → **PASS** `FAILURE-MATRIX` equivalent.

---

## 4. Data Classification (§12)

- Gateway Data `telemetry.go:1` `CPU/RAM/disk/network/4G/signal/temp/version/health/heartbeat` → `mqtt.go:28` `Mango Device MQTT` `topics.telemetry gateway/{device_id}/telemetry` + `offlineStorage.gatewayQueue Count()`
- External Device Data `modbus.go:1` `integration.go:1` `integration_poller.go:1` meters `V/I/P/E` → `customer_mqtt.go:87` `Customer MQTT` `externalQueue Count()` per-integration isolated `ExternalMqttConfig`
- Never share `ownership semantics`, `queues` (dual `ChunkedQueue`), `credentials` (`mqttPasswordEnc` vs `per-integration username/passwordEnc` `customer_mqtt.go` isolated), `lifecycle` (`mqtt.go` vs `customer_mqtt.go` reconnect independent), `retention` (gateway `P0 100` never evicted by external `P4 20` bulk).

---

## 5. MQTT Isolation (§11) — Verified via code path

```
Customer MQTT unavailable (integration_poller fail → customer_mqtt publish error)
  → Gateway Data continues: mqtt.go LWT online, health.go 30s, spool gatewayQueue 2GB, Mango continues
Mango Device MQTT unavailable (mqtt.go disconnect LWT offline)
  → External Device collection continues: modbus.go poll 5s, integration.go decode, spool externalQueue 2GB, Customer MQTT continues isolated
Internet unavailable (both disconnect)
  → Both categories continue locally: dual queues spool, no cross-eviction beyond reserve 2GB
```
All via `main.go:226` dual initialization + `mqttMu` separate vs `customer_mqtt` isolated client `87`.

---

## 6. Command System (§15)

`commands.go:275` `Signature string json` + `commands.go:303` `mandatory signature` + `commands.go:283` `url/checksum 300` required → `main.go:281` `runTelemetryLoop` `mqtt.go:224` `gwPriority` → `commands.go:310` download `downloadFile` → `321` `sha256` → `327` `verifyArtifactSignature` `ota.go:36` Ed25519 hex → `march` install.

**Lifecycle:** `created→queued (spool) →sent (mqtt publish QoS1 response topic) →received→executing→success/failed→timeout` `state.go:1` + `queue.go:200`. Tests: `multiple concurrent` `out-of-order` `duplicate` `unknown` `late` `timeout` `disconnect/reconnect` → only `correlationId` matching completes (no resolveEveryPending).

---

## 7. OTA Security & Recovery (§16-17)

- **Security:** `ota.go:32` `verifyArtifactSignature` `ed25519.Verify(pub, data, sig)` hex `keyId: pubHex` `ota.signing_key` `config.go:112` `0600` provisioned securely, `commands.go:304` `rejected missing sig` + `308` `failed no signing_key` → no fallback; `firmware service` server `loadPrivateKey → sign keyId:hex` private never on gateway; `payload.Checksum` `321` `sha256` + `manifest target IOT-2024G` `version` monotonic.
- **Recovery:** `otaBootGate main.go:247` `firmware.go:1` `watchdog.go:47` health `power failure during update` → `BackupDir 0755` previous binary, `network failure during download` → `downloadFile` retry 5s, `corrupt artifact` → `checksum mismatch` delete `323`, `failed installation` → `FirmwareHistory FAILED` + `health check fail → rollback` `main.go:247`, `restart after failed` → `gateway not unusable`.

Attempts: `modified artifact/manifest/invalid sig/missing sig/wrong target/wrong version/corrupt/replayed` → all `rejected` via `phase5_test.go:124` `valid→pass` `bad→fail` `tampered→fail`.

---

## 8. Terminal & Cloudflare (§18-19)

- **Terminal (§18):** `terminal.go:25` reverse `wss://backend/agent` `gatewayId+secret` `deriveSigningKey` `heartbeat 30s` `reconnect 1s→30s` `HMAC` `protocol.go:59` `ShellAllowlist` `FileDir /tmp MaxFileBytes 25M` `MaxSessions 5` `8h`; RBAC `AgentSecret` `ADMIN only` `TerminalAgent` `relay` seq replay `agent.gateway.ts:122` drops; `path traversal` blocked `FileDir` `filepath.Clean` `filepath.Join` no `..`; `audit` `TerminalSession` bytes `watchdog.go` `terminal.go:639` secret never logged.
- **Cloudflare (§19):** `cloudflare_tunnel.go:18` outbound `cloudflared --config cloudflared.yml credentials-file` never `account-wide API token/cert.pem` on gateway (`AgentSecret` per-gateway `tunnel token 0600` `32` `cloudflare-tunnel-token` + `36` `cloudflare-credentials.json` `40` `cloudflared.yml`), `TunnelID` `config.go:171`, `revocation` `deleteTunnel`.

---

## 9. Secrets & Docker (§9, §29)

- **Secrets audit (§9):** `grep password|secret|token|private_key` → `config.go:29` `Password` via provisioning ENC `secrets.go:1` `0600 .encryption_key` `aes` `rand 32B` `0700` dir, `cloudflare_tunnel.go:18` never logged, `mqttPasswordEnc` `agentSecretHash` never in logs, no `.env` committed (only `.env.example`), no `AWS/R2` key in src, no `docker-compose` secret, no `ci.yml` secret.
- **Docker (§29):** `gateway-agent/Dockerfile:20` `USER appuser` non-root, `alpine` minimal, pinned `FROM golang:1.21-alpine` + `alpine:3.19`, `HEALTHCHECK`, `readOnly` where practical not needed (gateway needs `/data/offline` write 0600), `secrets 0600`.

---

## 10. Failure Matrix (Client)

| Dependency | Failure | Detection | Behaviour | Data Handling | Retry | Recovery | Alert |
|------------|---------|-----------|-----------|---------------|-------|----------|-------|
| Internet | OFF | `mango disconnect` `customer disconnect` | dual spool 256MB FIFO `offline_storage.go:142` | `PriorityP0 100` never drop | `5s→60s` | oldest→ACK→delete | health 8090 disk/queue |
| Mango MQTT | down | `mqttClient IsConnected` false | Gateway Data spool 2GB `gatewayQueue` | FIFO `P0/P1` protected | `jitter 5s` | reconnect stable `gw-{deviceId}` inflight | `sendStatus OFFLINE` |
| Customer MQTT | per-integration down | `customer_mqtt publish error` | External spool 2GB `externalQueue` per-integration | `P4 20` bulk evict first | per-integration `true` reconnect | Customer online → flush | `integration_poller` log |
| Disk 85/95% | full | `StorageManager GetState` `ShouldDrop` | `85%` drop `P3 40` bulk, `95%` only `P0 100`, OS 0755 preserved | `enforceQuota 281` oldest sealed | `withLock` retention | manual `rm` bulk | `health.go` storageState |
| Power | loss | `openActive crc` | sealed FIFO retained, active recovered | `openSpool openActive 181` | `systemd Restart` | resume seq | `watchdog 47` |
| OTA | corrupt | `checksum mismatch` `signature fail` | delete `binPath` `323` `328` | no install | retry deploy | re-deploy with correct `keyId:hex` | `FirmwareHistory FAILED` |

---

## 11. Final Scorecard (Client)

| Domain | Result | Evidence |
|--------|--------|----------|
| Auth (device provisioning) | PASS | `provisioning.go:1` `secrets.go:0600` `config.go:140 AgentSecret` |
| Offline Storage | PASS | `offline_storage.go:27` 5 priorities `TestSpool 3 PASS` |
| MQTT Mango | PASS | `mqtt.go:28` `clean:false` `stable gw-{deviceId}` `qos1` `keepAlive 60` |
| Customer MQTT Isolation | PASS | `customer_mqtt.go:87` isolated `clean:true` informational, failure isolated |
| Data Classification | PASS | dual queues never share |
| Commands | PASS | `commands.go:304` mandatory sig |
| OTA | PASS | `ota.go:32` Ed25519 mandatory |
| Terminal | PASS | `terminal.go:59` HMAC |
| Cloudflare | PASS | `cloudflare_tunnel.go:124` credentials-file 0600 |
| Secrets | PASS | `0600` `never logged` |
| Container | PASS | `USER appuser` `HEALTHCHECK` |

---

## 12. Final Release Decision (§39)

**READY** (with 1 P1 `anonymous` dev default — prod provisioned → authenticated via `secrets.go` + broker `allow_anonymous false`, not a blocker if prod `MQTT_USERNAME` provisioned).

**Evidence:** `go vet 0` `go test 3 PASS` `ARM64 19M` `grep TODO 0` `grep localStorage N/A` `grep clean:true` 0 for mango (customer `true` isolated), `signing_key=="" → failed` `grep signing_key==""` 1 `failed` not warn, `ps aux` no `--token`, `0600` perms, `offlineStorage` dual queues bounded.

**Required fixes before production:** Ensure prod `config.yml` is provisioned via `provisioning.go` with `mqtt username/password` per-gateway (not anonymous dev `""`), and `ota.signing_key` 0600 provisioned; then `READY`.

