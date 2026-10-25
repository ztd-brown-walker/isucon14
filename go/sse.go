package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// 通知の SSE で、未通知の状態があるかを DB に確かめに行く間隔
const notificationPollInterval = 100 * time.Millisecond

// SSE の応答ヘッダーを書いて、すぐクライアントに送る
func startSSE(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return http.NewResponseController(w).Flush()
}

// 通知1件を `data: {JSON}` の1行で書き、すぐクライアントに送る
func writeSSE(w http.ResponseWriter, v interface{}) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

// 接続が切れるまで send を繰り返す。send が通知を送ったら待たずに次を確かめ、
// 送るものがなければ notificationPollInterval だけ待つ。
// 最初の1回だけ force を true で呼び、未通知の状態がなくても最新の状態を送らせる。
func serveNotificationSSE(ctx context.Context, w http.ResponseWriter, send func(force bool) (bool, error)) {
	if err := startSSE(w); err != nil {
		return
	}

	force := true
	for {
		sent, err := send(force)
		if err != nil {
			// ヘッダーを送ったあとなのでエラーは返せない。接続を切ってクライアントにつなぎ直してもらう
			if !errors.Is(err, context.Canceled) {
				slog.Error("notification sse failed", "error", err)
			}
			return
		}
		if sent {
			force = false
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(notificationPollInterval):
		}
	}
}
