package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// 状態が変わったという知らせを取りこぼしたときの保険として、知らせがなくても DB を確かめに行く間隔
const notificationFallbackInterval = 1 * time.Second

// ユーザーや椅子の ID ごとに、開いている SSE の接続へ「状態が変わった」と知らせる。
// channel は同じプロセスの中でしか届かないので、通知と状態を書き込む API は
// nginx で isu1 のアプリに寄せている（etc/isu1/nginx/sites-available/isuride.conf）。
type notifier struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newNotifier() *notifier {
	return &notifier{subs: map[string]map[chan struct{}]struct{}{}}
}

var (
	appNotifier   = newNotifier()
	chairNotifier = newNotifier()
)

// id 宛ての知らせを受け取る channel を返す。使い終わったら返り値の関数で解除する
func (n *notifier) subscribe(id string) (<-chan struct{}, func()) {
	// 容量1にしておくと、DB を確かめている最中に来た知らせも次の待ちで受け取れる
	ch := make(chan struct{}, 1)
	n.mu.Lock()
	if n.subs[id] == nil {
		n.subs[id] = map[chan struct{}]struct{}{}
	}
	n.subs[id][ch] = struct{}{}
	n.mu.Unlock()

	return ch, func() {
		n.mu.Lock()
		delete(n.subs[id], ch)
		if len(n.subs[id]) == 0 {
			delete(n.subs, id)
		}
		n.mu.Unlock()
	}
}

// id の接続すべてに知らせる。DB の更新をコミットしたあとに呼ぶ
func (n *notifier) notify(id string) {
	if id == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for ch := range n.subs[id] {
		select {
		case ch <- struct{}{}:
		default:
			// まだ受け取られていない知らせが残っているので、重ねなくてよい
		}
	}
}

// ライドに関わるユーザーと椅子の両方に知らせる
func notifyRide(userID, chairID string) {
	appNotifier.notify(userID)
	chairNotifier.notify(chairID)
}

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
// 送るものがなければ wake に知らせが来るまで待つ。
// 最初の1回だけ force を true で呼び、未通知の状態がなくても最新の状態を送らせる。
func serveNotificationSSE(ctx context.Context, w http.ResponseWriter, wake <-chan struct{}, send func(force bool) (bool, error)) {
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
		case <-wake:
		case <-time.After(notificationFallbackInterval):
		}
	}
}
