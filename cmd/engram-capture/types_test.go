package main

import (
	"testing"
	"time"
)

func TestMessageFieldsAreStableForWireConversion(t *testing.T) {
	ts := time.Date(2026, 6, 13, 18, 55, 22, 0, time.UTC)
	msg := Message{Role: "human", Text: "hello", Ts: ts, MsgID: "m1"}

	if msg.Role != "human" || msg.Text != "hello" || msg.MsgID != "m1" {
		t.Fatalf("message fields changed unexpectedly: %#v", msg)
	}
	if got := msg.Ts.Format(time.RFC3339Nano); got != "2026-06-13T18:55:22Z" {
		t.Fatalf("timestamp format mismatch: %s", got)
	}
}
