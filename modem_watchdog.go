package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ---------- Modem self-healing watchdog ----------
//
// Independent from the MQTT watchdog (watchdog.go) and the boot-time
// fix-4g.sh: this loop watches the *cellular data path* at runtime and
// escalates when the network is gone:
//
//	healthy = USB modem present -> ModemManager state ok -> ppp0 up ->
//	            default route via ppp0 -> ping via ppp0 answers
//
// Each failing tick logs a machine-readable cause (self-diagnosis):
// usb-missing | sim-missing | modem-down | ppp-down | no-route | ping-fail.
//
// Escalation (consecutive failures, with cooldowns so a flapping link does
// not wedge the box):
//  1-2 : `nmcli connection up airtel` (re-dial, cheap)
//  3   : `systemctl restart ModemManager` (stuck bearer/probe)
//  4+  : hat power-cycle via GPIO (6=pd W_DISABLE off, 20/21/22=dh power;
//        max once per 10 min), then re-dial
//  10+ : reboot ONLY when modem_watchdog.reboot_on_failure is true,
//        otherwise loop back to level 1-3 retries (never give up).
//
// A success resets the failure counter. The first 4 minutes after process
// start are grace (fix-4g.sh owns early boot).

func startModemWatchdog(ctx context.Context) {
	mw := cfg.ModemWatchdog
	if !mw.Enabled {
		return
	}
	interval := time.Duration(mw.IntervalSec) * time.Second
	if interval <= 0 {
		interval = 60 * time.Second
	}
	targets := mw.PingTargets
	if len(targets) == 0 {
		targets = []string{"8.8.8.8", "1.1.1.1"}
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	failures := 0
	var lastPowerCycle time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if time.Since(startTime) < 4*time.Minute {
				continue
			}
			if ok, cause := modemNetHealthy(targets); ok {
				if failures > 0 {
					logger.WithField("downtime_ticks", failures).Info("modem-watchdog: network recovered")
				}
				failures = 0
				continue
			} else {
				failures++
				action := modemRecover(ctx, failures, cause, &lastPowerCycle, mw)
				logger.WithFields(map[string]interface{}{
					"cause":    cause,
					"failures": failures,
					"action":   action,
				}).Warn("modem-watchdog: network down")
				if failures >= 10 {
					if mw.RebootOnFailure {
						logger.Error("modem-watchdog: prolonged outage, rebooting")
						exec.Command("sudo", "reboot").Run()
						return
					}
					// Loop L1-L3 forever instead of rebooting.
					failures = 3
				}
			}
		}
	}
}

// modemNetHealthy runs the layered data-path checks. It returns false with
// the first failing layer as the diagnosis.
func modemNetHealthy(targets []string) (bool, string) {
	if out, err := modemRunCmd(5*time.Second, "lsusb"); err != nil || !strings.Contains(out, "1bc7") {
		if _, err := modemRunCmd(3*time.Second, "ls", "/dev/ttyUSB2"); err != nil {
			return false, "usb-missing"
		}
	}
	mm, err := modemRunCmd(10*time.Second, "mmcli", "-m", "0")
	if err != nil {
		return false, "modem-down"
	}
	if strings.Contains(mm, "sim-missing") {
		return false, "sim-missing"
	}
	if !strings.Contains(mm, "state: connected") && !strings.Contains(mm, "state: registered") {
		return false, "modem-down"
	}
	if out, err := modemRunCmd(5*time.Second, "ip", "-4", "addr", "show", "ppp0"); err != nil || !strings.Contains(out, "inet ") {
		return false, "ppp-down"
	}
	if out, err := modemRunCmd(5*time.Second, "ip", "route", "show", "default"); err != nil || !strings.Contains(out, "ppp0") {
		return false, "no-route"
	}
	for _, t := range targets {
		if _, err := modemRunCmd(8*time.Second, "ping", "-I", "ppp0", "-c", "2", "-W", "3", t); err == nil {
			return true, ""
		}
	}
	return false, "ping-fail"
}

// modemRecover executes one escalation level and returns its name for logs.
func modemRecover(ctx context.Context, failures int, cause string, lastPowerCycle *time.Time, mw ModemWatchdogConfig) string {
	switch {
	case failures <= 2:
		modemRunCmd(20*time.Second, "nmcli", "connection", "up", "airtel")
		return "redial"
	case failures == 3:
		modemRunCmd(30*time.Second, "systemctl", "restart", "ModemManager")
		select {
		case <-ctx.Done():
		case <-time.After(15 * time.Second):
		}
		modemRunCmd(20*time.Second, "nmcli", "connection", "up", "airtel")
		return "modemmanager-restart"
	default:
		if mw.PowerCycle && time.Since(*lastPowerCycle) > 10*time.Minute {
			*lastPowerCycle = time.Now()
			// Hat power rails + RF enable (matches fix-4g.sh proven state).
			modemRunCmd(5*time.Second, "raspi-gpio", "set", "6", "ip", "pd")
			for _, g := range []string{"20", "21", "22"} {
				modemRunCmd(5*time.Second, "raspi-gpio", "set", g, "op", "dh")
			}
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Second):
			}
			modemRunCmd(20*time.Second, "nmcli", "connection", "up", "airtel")
			return "power-cycle"
		}
		modemRunCmd(20*time.Second, "nmcli", "connection", "up", "airtel")
		return "redial-cooldown"
	}
}

func modemRunCmd(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
