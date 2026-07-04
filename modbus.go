package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/goburrow/modbus"
	"github.com/sirupsen/logrus"
)

// ---------- Modbus Collector (async, thread-safe) ----------

type modbusCollector struct {
	mu     sync.Mutex
	values map[string][]ModbusValue
}

func newModbusCollector() *modbusCollector {
	return &modbusCollector{values: make(map[string][]ModbusValue)}
}

func (mc *modbusCollector) set(deviceName string, vals []ModbusValue) {
	mc.mu.Lock()
	if len(vals) > 0 {
		mc.values[deviceName] = vals
	} else {
		delete(mc.values, deviceName)
	}
	mc.mu.Unlock()
}

func (mc *modbusCollector) getAll() []ModbusValue {
	mc.mu.Lock()
	defer mc.mu.Unlock()
	var result []ModbusValue
	for _, vals := range mc.values {
		result = append(result, vals...)
	}
	return result
}

// ---------- Modbus Handler ----------

type modbusHandler struct {
	name     string
	device   ModbusDevice
	handler  modbus.ClientHandler
	client   modbus.Client
}

func newModbusHandler(dev ModbusDevice) (*modbusHandler, error) {
	mh := &modbusHandler{name: dev.Name, device: dev}

	switch dev.Protocol {
	case "tcp":
		handler := modbus.NewTCPClientHandler(dev.Address)
		handler.Timeout = 10 * time.Second
		handler.SlaveId = dev.SlaveID
		if err := handler.Connect(); err != nil {
			return nil, fmt.Errorf("modbus tcp connect %s: %w", dev.Address, err)
		}
		mh.handler = handler
		mh.client = modbus.NewClient(handler)
	case "rtu":
		handler := modbus.NewRTUClientHandler(dev.Address)
		handler.BaudRate = dev.BaudRate
		handler.DataBits = dev.DataBits
		handler.StopBits = dev.StopBits
		handler.Parity = dev.Parity
		handler.Timeout = 5 * time.Second
		handler.SlaveId = dev.SlaveID
		if err := handler.Connect(); err != nil {
			return nil, fmt.Errorf("modbus rtu connect %s: %w", dev.Address, err)
		}
		mh.handler = handler
		mh.client = modbus.NewClient(handler)
	default:
		return nil, fmt.Errorf("unsupported modbus protocol: %s", dev.Protocol)
	}
	return mh, nil
}

func (mh *modbusHandler) readRegisters() []ModbusValue {
	var results []ModbusValue
	for _, reg := range mh.device.Registers {
		val, err := mh.readRegister(reg)
		if err != nil {
			logger.WithError(err).WithField("register", reg.Name).Warn("modbus read failed")
			continue
		}
		results = append(results, val)
	}
	return results
}

func (mh *modbusHandler) readRegister(reg ModbusRegister) (ModbusValue, error) {
	mv := ModbusValue{Name: reg.Name, Time: time.Now()}

	var raw []byte

	switch reg.Type {
	case "coil":
		raw = []byte{0}
		if reg.Quantity == 1 {
			v, err := mh.client.ReadCoils(reg.Address, 1)
			if err != nil {
				return mv, err
			}
			raw = v
		}
	case "discrete":
		v, err := mh.client.ReadDiscreteInputs(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	case "holding":
		v, err := mh.client.ReadHoldingRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	case "input":
		v, err := mh.client.ReadInputRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	default:
		v, err := mh.client.ReadHoldingRegisters(reg.Address, reg.Quantity)
		if err != nil {
			return mv, err
		}
		raw = v
	}

	switch reg.Type {
	case "float32":
		if len(raw) >= 4 {
			mv.Value = math.Float32frombits(binary.BigEndian.Uint32(raw))
		}
	case "int16":
		if len(raw) >= 2 {
			v := int16(raw[0])<<8 | int16(raw[1])
			mv.Value = v
		}
	case "uint16":
		if len(raw) >= 2 {
			mv.Value = uint16(raw[0])<<8 | uint16(raw[1])
		}
	case "uint32":
		if len(raw) >= 4 {
			mv.Value = uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
		}
	case "int32":
		if len(raw) >= 4 {
			mv.Value = int32(raw[0])<<24 | int32(raw[1])<<16 | int32(raw[2])<<8 | int32(raw[3])
		}
	case "bool":
		if len(raw) >= 1 {
			mv.Value = raw[0] != 0
		}
	default:
		mv.Value = raw
	}

	return mv, nil
}

func (mh *modbusHandler) close() {
	if closer, ok := mh.handler.(io.Closer); ok {
		closer.Close()
	}
}

func runModbusLoop(ctx context.Context) {
	if !cfg.Modbus.Enabled {
		return
	}

	type poller struct {
		handler  *modbusHandler
		interval time.Duration
		ticker   *time.Ticker
	}

	var pollers []*poller
	for _, dev := range cfg.Modbus.Devices {
		mh, ok := modbusPools[dev.Name]
		if !ok {
			continue
		}
		interval := dev.Interval
		if interval <= 0 {
			interval = 10
		}
		p := &poller{
			handler:  mh,
			interval: time.Duration(interval) * time.Second,
			ticker:   time.NewTicker(time.Duration(interval) * time.Second),
		}
		pollers = append(pollers, p)
	}

	for {
		select {
		case <-ctx.Done():
			for _, p := range pollers {
				p.ticker.Stop()
			}
			return
		default:
			for _, p := range pollers {
				select {
				case <-p.ticker.C:
					vals := p.handler.readRegisters()
					if modbusCol != nil {
						modbusCol.set(p.handler.name, vals)
					}
					if len(vals) > 0 {
						logger.WithFields(logrus.Fields{
							"device": p.handler.name,
							"values": len(vals),
						}).Debug("modbus poll completed")
					}
				default:
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
