package api

import (
	"bufio"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/gorilla/websocket"
	"github.com/play-bin/internal/logger"
)

// MARK: loggingResponseWriter
// HTTPステータスコードをキャプチャして、完了後のアクセスログに含めるためのラッパー。
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// MARK: WriteHeader()
// 応答時にステータスコードを内部に保存し、呼び出し元（ブラウザ等）へも通常通り送信する。
func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// MARK: Hijack()
// WebSocket等のアップグレードのために、通常提供されない基盤のネットワーク接続を外部から取得可能にする。
func (lrw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := lrw.ResponseWriter.(http.Hijacker)
	if !ok {
		// 標準のResponseWriterがHijackerを実装していない場合、アップグレード不可能な内部バグとして扱う。
		return nil, nil, logger.InternalError("API", "ResponseWriter does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

// MARK: WithLogging()
// すべてのHTTPリクエストに対して、メソッド、パス（クエリ付き）、ステータス、処理時間を記録する共通ミドルウェア。
func (s *Server) WithLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// デフォルトは200 OKとするが、WriteHeaderが呼ばれればその値で上書きされる。
		lrw := &loggingResponseWriter{w, http.StatusOK}

		// 次のハンドラー（実際のAPI処理）を実行し、一連の処理が完了するのを待機する。
		next.ServeHTTP(lrw, r)

		// 規約に基づき [timestamp] [level] [service]: message 形式でアクセス情報を出力する。
		// クエリパラメータ (?id=...) を含めた完全なリクエスト内容を追跡するため RequestURI を使用する。
		logger.Logf("Internal", "Access", "%s %s %s %d %v %s",
			r.Method,
			r.RequestURI,
			r.RemoteAddr,
			lrw.statusCode,
			time.Since(start),
			r.UserAgent(),
		)
	})
}

// MARK: wsBinaryWriter
// WebSocket経由でバイナリデータを送信するための、io.Writer互換ラッパー。
type wsBinaryWriter struct {
	*websocket.Conn
}

// MARK: Write()
// バイナリメッセージとして送信を行い、送信失敗時には正規のエラーを返却する。
func (w *wsBinaryWriter) Write(p []byte) (int, error) {
	if w.Conn == nil {
		return 0, os.ErrInvalid
	}
	err := w.WriteMessage(websocket.BinaryMessage, p)
	return len(p), err
}
