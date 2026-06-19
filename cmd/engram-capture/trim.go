package main

import (
	"fmt"
	"strings"
	"time"
)

type TrimConfig struct {
	MaxMessageBytes int
}

func DefaultTrimConfig() TrimConfig {
	return TrimConfig{MaxMessageBytes: 64 * 1024}
}

func BuildIngestBatch(tr Transcript, cfg TrimConfig, now time.Time, endedAfter time.Duration, machine, username string) IngestBatch {
	msgs := make([]IngestMessage, 0, len(tr.Messages))
	for _, msg := range tr.Messages {
		text := trimMessage(msg, cfg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		msgs = append(msgs, IngestMessage{
			Role:  msg.Role,
			Text:  text,
			Ts:    msg.Ts.Format(time.RFC3339Nano),
			MsgID: msg.MsgID,
		})
	}
	return IngestBatch{
		Tool:            tr.Tool,
		SessionID:       tr.SessionID,
		ParentSessionID: tr.ParentSessionID,
		Title:           tr.Title,
		Project:         tr.Project,
		Machine:         machine,
		Username:        username,
		Messages:        msgs,
		SessionEnded:    !tr.ModTime.IsZero() && now.Sub(tr.ModTime) >= endedAfter,
	}
}

func trimMessage(msg Message, cfg TrimConfig) string {
	text := strings.TrimSpace(msg.Text)
	if cfg.MaxMessageBytes <= 0 || len(text) <= cfg.MaxMessageBytes {
		return text
	}
	return fmt.Sprintf("[large %s message omitted: %d bytes]", msg.Role, len(text))
}
