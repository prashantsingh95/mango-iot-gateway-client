package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sirupsen/logrus"
)

// ---------- GPIO Initialization (sysfs) ----------

func initGPIO() {
	if !cfg.GPIO.Enabled {
		return
	}
	for _, s := range cfg.GPIO.Sensors {
		if s.Mode == "output" {
			pinStr := strconv.Itoa(s.Pin)
			os.WriteFile("/sys/class/gpio/export", []byte(pinStr), 0644)
			gpioDir := fmt.Sprintf("/sys/class/gpio/gpio%s", pinStr)
			for i := 0; i < 50; i++ {
				if _, err := os.Stat(gpioDir); err == nil {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			dirPath := gpioDir + "/direction"
			os.WriteFile(dirPath, []byte("out"), 0644)
			val := "0"
			if s.Default {
				val = "1"
			}
			valPath := gpioDir + "/value"
			os.WriteFile(valPath, []byte(val), 0644)
			logger.WithFields(logrus.Fields{"pin": s.Pin, "name": s.Name, "default": s.Default}).Info("GPIO output initialized")
		}
	}
}
