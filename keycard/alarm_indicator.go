package keycard

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

const alarmLEDSetting = "alarm.armed-led"

func alarmIndicatorEnabled(setting, alarm, vehicle string) bool {
	return setting == "true" && alarm == "armed" && vehicle == "stand-by"
}

func (s *Service) refreshAlarmIndicator(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var setting, alarm, vehicle *redis.StringCmd
	_, err := s.redis.client.Raw().TxPipelined(ctx, func(p redis.Pipeliner) error {
		setting = p.HGet(ctx, "settings", alarmLEDSetting)
		alarm = p.HGet(ctx, "alarm", "status")
		vehicle = p.HGet(ctx, "vehicle", "state")
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		s.rgbLed.SetAlarmIndicator(false)
		s.logger.Debug("Alarm LED state unavailable", "error", err)
		return
	}
	s.rgbLed.SetAlarmIndicator(alarmIndicatorEnabled(setting.Val(), alarm.Val(), vehicle.Val()))
}

func (s *Service) watchAlarmIndicator(ctx context.Context) {
	sub := s.redis.client.Raw().Subscribe(ctx, "alarm", "settings", "vehicle")
	defer sub.Close()
	stop := context.AfterFunc(ctx, func() { _ = sub.Close() })
	defer stop()
	for ctx.Err() == nil {
		message, err := sub.Receive(ctx)
		if err != nil {
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
				(m.Channel == "settings" && m.Payload == alarmLEDSetting) ||
				(m.Channel == "vehicle" && m.Payload == "state") {
				s.refreshAlarmIndicator(ctx)
			}
		}
	}
}
