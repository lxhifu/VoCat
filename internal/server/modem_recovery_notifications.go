package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"vocat/internal/store"
)

// Modem recovery event kinds delivered through NotifyModemRecovery. They
// mirror the callbacks of the remoteproc recovery watcher.
const (
	ModemRecoveryCrashDetected = "crash_detected"
	ModemRecoverySucceeded     = "recovery_succeeded"
	ModemRecoveryFailed        = "recovery_failed"
	ModemRecoveryGaveUp        = "recovery_gave_up"
)

// ModemRecoveryNotification is a system-level alert about the modem's
// remote processor: a confirmed firmware crash and the outcome of the
// automatic recovery attempts. It is not tied to a configured device, so
// channel device filters apply with an empty device ID (only channels
// without a device scope receive it).
type ModemRecoveryNotification struct {
	Event      string
	RemoteProc string
	Time       time.Time
	Attempt    int
	Detail     string
}

func (value ModemRecoveryNotification) Title() string {
	switch value.Event {
	case ModemRecoveryCrashDetected:
		return "modem 固件崩溃"
	case ModemRecoverySucceeded:
		return "modem 已自动恢复"
	case ModemRecoveryFailed:
		return "modem 自动恢复失败"
	case ModemRecoveryGaveUp:
		return "modem 自动恢复已停止"
	default:
		return "modem 恢复事件"
	}
}

func (value ModemRecoveryNotification) Text() string {
	lines := []string{
		value.Title(),
		"处理器  " + value.RemoteProc,
		"时间  " + value.Time.Local().Format("2006-01-02 15:04:05"),
	}
	switch value.Event {
	case ModemRecoveryCrashDetected:
		lines = append(lines, fmt.Sprintf("状态  已确认崩溃，窗口内已尝试 %d 次", value.Attempt))
	case ModemRecoverySucceeded:
		lines = append(lines, fmt.Sprintf("结果  第 %d 次触发后恢复运行，稳定期结束后设备自动重连", value.Attempt))
	case ModemRecoveryFailed:
		lines = append(lines, fmt.Sprintf("尝试  第 %d 次", value.Attempt))
		if value.Detail != "" {
			lines = append(lines, "原因  "+value.Detail)
		}
	case ModemRecoveryGaveUp:
		lines = append(lines, fmt.Sprintf("尝试  窗口内已尝试 %d 次", value.Attempt))
		if value.Detail != "" {
			lines = append(lines, "原因  "+value.Detail)
		}
		lines = append(lines, "说明  本轮崩溃不再自动触发恢复，需人工介入")
	}
	return strings.Join(lines, "\n")
}

// DetailText 返回不含重复首行标题的正文，供 MeoW 等独立标题渠道使用。
func (value ModemRecoveryNotification) DetailText() string {
	lines := strings.SplitN(value.Text(), "\n", 2)
	if len(lines) == 2 {
		return lines[1]
	}
	return value.Text()
}

func (value ModemRecoveryNotification) outcome() string {
	switch value.Event {
	case ModemRecoverySucceeded:
		return "success"
	case ModemRecoveryFailed:
		return "failure"
	case ModemRecoveryGaveUp:
		return "gave_up"
	default:
		return "detected"
	}
}

// NotifyModemRecovery records a modem recovery event in the audit log and
// delivers it to all configured notification channels, reusing the same
// per-channel settings and senders as call/SMS/automatic-task alerts.
func (s *Server) NotifyModemRecovery(ctx context.Context, notification ModemRecoveryNotification) {
	if ctx == nil {
		ctx = context.Background()
	}
	if notification.Time.IsZero() {
		notification.Time = time.Now().UTC()
	}
	if strings.TrimSpace(notification.RemoteProc) == "" {
		notification.RemoteProc = "--"
	}
	action := "modem.recovery." + notification.Event
	if s.logger != nil {
		level := slog.LevelInfo
		if notification.Event != ModemRecoverySucceeded {
			level = slog.LevelError
		}
		s.logger.Log(ctx, level, "modem recovery event",
			"category", "hardware",
			"event", action,
			"remoteproc", notification.RemoteProc,
			"attempt", notification.Attempt,
			"detail", notification.Detail,
		)
	}
	if s.store != nil {
		details, _ := json.Marshal(map[string]any{
			"remoteproc": notification.RemoteProc,
			"attempt":    notification.Attempt,
			"detail":     notification.Detail,
		})
		if _, err := s.store.AppendAuditEvent(ctx, store.AuditEvent{
			Actor:      "system",
			Action:     action,
			EntityType: "remoteproc",
			EntityID:   notification.RemoteProc,
			Outcome:    notification.outcome(),
			Details:    details,
			CreatedAt:  notification.Time.UTC(),
		}); err != nil && s.logger != nil {
			s.logger.Warn("write modem recovery audit event failed", "category", "hardware", "action", action, "raw_error", err)
		}
	}

	destCtx := s.notificationDestinationContext(ctx)
	for _, channel := range notificationChannels {
		setting, err := s.store.NotificationSetting(destCtx, channel)
		if errors.Is(err, store.ErrNotFound) || (err == nil && !setting.Enabled) {
			continue
		}
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("read modem recovery notification setting", "channel", channel, "error", err)
			}
			continue
		}
		var config map[string]any
		if err := json.Unmarshal(setting.Config, &config); err != nil {
			if s.logger != nil {
				s.logger.Warn("decode modem recovery notification setting", "channel", channel, "error", err)
			}
			continue
		}
		if err := sendModemRecoveryNotification(destCtx, channel, config, notification); err != nil {
			if s.logger != nil {
				s.logger.Warn("send modem recovery notification", "category", "hardware", "channel", channel, "remoteproc", notification.RemoteProc, "raw_error", err)
			}
		}
	}
}

// sendModemRecoveryNotification 按渠道发送恢复事件，仅 MeoW 使用无标题正文。
// 恢复事件不属于任何一台配置设备，设备过滤统一按空设备 ID 参与：只投递到
// 未按设备限定范围的渠道，与文本渠道的既有语义保持一致。
func sendModemRecoveryNotification(ctx context.Context, channel string, config map[string]any, message ModemRecoveryNotification) error {
	deviceID := ""
	if channel == "meow" {
		return sendMeowNotification(ctx, config, &deviceID, message.Title(), message.DetailText())
	}
	switch channel {
	case "telegram":
		return sendTelegramTextNotification(ctx, config, deviceID, message.Text())
	case "bark":
		return sendBarkTextNotification(ctx, config, deviceID, message.Title(), message.DetailText())
	case "email":
		return sendEmailTextNotification(ctx, config, deviceID, message.Title()+" - "+message.RemoteProc, message.Text())
	case "pushplus":
		return sendPushplusTextNotification(ctx, config, deviceID, message.Title(), message.DetailText())
	case "webhook":
		return sendModemRecoveryWebhook(ctx, config, message)
	case "wecom":
		return sendWecomNotification(ctx, config, &deviceID, wecomModemRecoveryValues(message))
	case "lark":
		return sendLarkNotification(ctx, config, &deviceID, larkModemRecoveryValues(message))
	default:
		return fmt.Errorf("unsupported notification channel %q", channel)
	}
}

func sendModemRecoveryWebhook(ctx context.Context, config map[string]any, message ModemRecoveryNotification) error {
	payload, _ := json.Marshal(map[string]any{
		"event":       "modem.recovery." + message.Event,
		"message":     message.Text(),
		"timestamp":   message.Time.UTC().Format(time.RFC3339),
		"remoteproc":  message.RemoteProc,
		"attempt":     message.Attempt,
		"detail":      message.Detail,
		"outcome":     message.outcome(),
		"device_id":   "",
		"device_name": "",
	})
	client, err := restrictedHTTPClient(ctx, durationMilliseconds(configInt(config, "timeout_ms"), 5*time.Second), "")
	if err != nil {
		return err
	}
	retries := configInt(config, "retry_max")
	for _, destination := range configStrings(config, "urls") {
		parsed, err := validateOutboundURL(ctx, destination, false)
		if err != nil {
			return err
		}
		var sendErr error
		for attempt := 0; attempt <= retries; attempt++ {
			request, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), bytes.NewReader(payload))
			if requestErr != nil {
				return fmt.Errorf("create modem recovery webhook request: %w", requestErr)
			}
			for name, value := range configStringMap(config, "headers") {
				request.Header.Set(name, value)
			}
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("User-Agent", "vocat-modem-recovery/1")
			if secret := configString(config, "secret"); secret != "" {
				signature := hmac.New(sha256.New, []byte(secret))
				_, _ = signature.Write(payload)
				request.Header.Set("X-vocat-Signature", "sha256="+hex.EncodeToString(signature.Sum(nil)))
			}
			sendErr = performNotificationRequest(client, request, false)
			if sendErr == nil {
				break
			}
		}
		if sendErr != nil {
			return sendErr
		}
	}
	return nil
}

func wecomModemRecoveryValues(message ModemRecoveryNotification) wecomTemplateValues {
	return wecomTemplateValues{
		"event":      "modem.recovery." + message.Event,
		"title":      message.Title(),
		"message":    message.Text(),
		"timestamp":  message.Time.UTC().Format(time.RFC3339),
		"content":    message.DetailText(),
		"remoteproc": message.RemoteProc,
		"time":       message.Time.Local().Format("2006-01-02 15:04:05"),
	}
}

func larkModemRecoveryValues(message ModemRecoveryNotification) larkTemplateValues {
	return larkTemplateValues{
		"event":      "modem.recovery." + message.Event,
		"title":      message.Title(),
		"message":    message.Text(),
		"timestamp":  message.Time.UTC().Format(time.RFC3339),
		"content":    message.DetailText(),
		"remoteproc": message.RemoteProc,
		"time":       message.Time.Local().Format("2006-01-02 15:04:05"),
	}
}
