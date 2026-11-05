package keycard

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	alarmLEDSetting          = "alarm.armed-led"
	alarmLEDHibernateSetting = "alarm.armed-led-hibernate"
)

func knownIndicatorPowerState(state string) bool {
	if state == "running" {
		return true
	}
	base := strings.TrimSuffix(strings.TrimSuffix(state, "-pending"), "-imminent")
	switch base {
	case "suspending", "hibernating", "hibernating-manual", "hibernating-timer", "hibernating-for":
		return true
	}
	return false
}

func poweringOff(state string) bool {
	switch state {
	case "hibernating", "hibernating-manual", "hibernating-timer", "hibernating-for":
		return true
	}
	return false
}

func enteringLowPower(state string) bool {
	return knownIndicatorPowerState(state) && state != "running" && !strings.HasSuffix(state, "-pending")
}

func alarmIndicatorEnabled(setting, hibernateSetting, alarm, vehicle, power string) bool {
	if setting != "true" || alarm != "armed" || !knownIndicatorPowerState(power) {
		return false
	}
	if enteringLowPower(power) && strings.HasPrefix(power, "hibernating") && hibernateSetting != "" && hibernateSetting != "true" {
		return false
	}
	switch vehicle {
	case "stand-by", "waiting-hibernation", "waiting-hibernation-advanced", "waiting-hibernation-seatbox", "waiting-hibernation-confirm":
		return true
	}
	return false
}

func (s *Service) refreshAlarmIndicator(ctx context.Context) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var setting, hibernateSetting, alarm, vehicle, power *redis.StringCmd
	_, err := s.redis.client.Raw().TxPipelined(ctx, func(p redis.Pipeliner) error {
		setting = p.HGet(ctx, "settings", alarmLEDSetting)
		hibernateSetting = p.HGet(ctx, "settings", alarmLEDHibernateSetting)
		alarm = p.HGet(ctx, "alarm", "status")
		vehicle = p.HGet(ctx, "vehicle", "state")
		power = p.HGet(ctx, "power-manager", "state")
		return nil
	})
	// Cancellation is shutdown, not evidence that the alarm was disarmed.
	if parent.Err() != nil {
		return
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		s.rgbLed.SetAlarmIndicator(false)
		s.logger.Debug("Alarm LED state unavailable", "error", err)
		return
	}
	s.rgbLed.SetAlarmIndicator(alarmIndicatorEnabled(setting.Val(), hibernateSetting.Val(), alarm.Val(), vehicle.Val(), power.Val()))
	s.rgbLed.SetPowerState(power.Val())
}

func (s *Service) watchAlarmIndicator(ctx context.Context) {
	sub := s.redis.client.Raw().Subscribe(ctx, "alarm", "settings", "vehicle", "power-manager")
	defer sub.Close()
	stop := context.AfterFunc(ctx, func() { _ = sub.Close() })
	defer stop()
	for ctx.Err() == nil {
		message, err := sub.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.rgbLed.SetAlarmIndicator(false)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		switch m := message.(type) {
		case *redis.Subscription:
			// Re-read durable state after initial subscription or reconnect.
			s.refreshAlarmIndicator(ctx)
		case *redis.Message:
			if (m.Channel == "alarm" && m.Payload == "status") ||
				(m.Channel == "settings" && (m.Payload == alarmLEDSetting || m.Payload == alarmLEDHibernateSetting)) ||
				(m.Channel == "vehicle" && m.Payload == "state") ||
				(m.Channel == "power-manager" && m.Payload == "state") {
				s.refreshAlarmIndicator(ctx)
			}
		}
	}
}
