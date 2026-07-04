package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// ---------- Watchdog ----------

func startWatchdog(ctx context.Context) {
	if !cfg.Watchdog.Enabled {
		return
	}

	startupGrace := time.Duration(cfg.Watchdog.Interval*cfg.Watchdog.MaxMissedPings) * time.Second

	ticker := time.NewTicker(time.Duration(cfg.Watchdog.Interval) * time.Second)
	defer ticker.Stop()

	missed := 0
	maxMissed := cfg.Watchdog.MaxMissedPings

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(startTime) < startupGrace {
				continue
			}
			if !isConnected() {
				missed++
				logger.WithField("missed", missed).Warn("watchdog: MQTT disconnected")
				if missed >= maxMissed {
					logger.Error("watchdog: max missed pings, taking action")
					switch cfg.Watchdog.Action {
					case "restart":
						os.Exit(0)
					case "reboot":
						exec.Command("sudo", "reboot").Run()
					default:
						if cfg.Watchdog.Action != "" {
							tokens := strings.Fields(cfg.Watchdog.Action)
							if len(tokens) > 0 {
								if len(tokens) == 1 {
									exec.Command(tokens[0]).Run()
								} else {
									exec.Command(tokens[0], tokens[1:]...).Run()
								}
							}
						}
					}
					return
				}
			} else {
				missed = 0
				sendStatus("ONLINE")
			}
		}
	}
}
