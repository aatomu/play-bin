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
// HTTPステータスコードをキャプチャしてログに含めるためのラッパー。
type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// MARK: WriteHeader()
// 応答時にステータスコードを保存する。
func (lrw *loggingResponseWriter) WriteHeader(code int) {
	lrw.statusCode = code
	lrw.ResponseWriter.WriteHeader(code)
}

// MARK: Hijack()
// WebSocket等のアップグレードのために、基盤となるネットワーク接続を取得可能にする。
func (lrw *loggingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := lrw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, logger.InternalError("API", "ResponseWriter does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

// MARK: WithLogging()
// すべてのHTTPリクエストに対して、メソッド、パス（クエリ付き）、ステータス、処理時間を記録するミドルウェア。
func (s *Server) WithLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{w, http.StatusOK}
		// 次のハンドラー（実際のAPI処理）を実行
		next.ServeHTTP(lrw, r)

		// 規約に基づき [timestamp] [level] [service]: message 形式で出力
		// クエリパラメータ (?id=...) を含めるため r.RequestURI を使用する
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

// MARK: Websocket()
type wsBinaryWriter struct {
	*websocket.Conn
}

func (w *wsBinaryWriter) Write(p []byte) (int, error) {
	if w.Conn == nil {
		return 0, os.ErrInvalid
	}
	err := w.WriteMessage(websocket.BinaryMessage, p)
	return len(p), err
}
