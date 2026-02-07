package logger

import (
	"fmt"
	"time"
)

// MARK: Log()
// 指定されたレベルとサービス名でログを出力する。
// 規約形式: [timestamp] [level] [service]: message
func Log(level, service, message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	fmt.Printf("[%s] [%s] [%s]: %s\n", timestamp, level, service, message)
}

// MARK: Logf()
// フォーマット指定付きでログを出力する。
func Logf(level, service, format string, v ...interface{}) {
	Log(level, service, fmt.Sprintf(format, v...))
}

// MARK: Internal()
// 内部エラーまたはシステムログを出力する。
func Internal(service, message string) {
	Log("Internal", service, message)
}

// MARK: Client()
// クライアント起因のエラーまたはログを出力する。
func Client(service, message string) {
	Log("Client", service, message)
}

// MARK: External()
// 外部APIやサービス起因のエラーまたはログを出力する。
func External(service, message string) {
	Log("External", service, message)
}

// MARK: Error()
// エラーレベルのログを出力する。
func Error(service string, err error) {
	if err != nil {
		Internal(service, err.Error())
	}
}

// MARK: InternalError()
// 内部エラーをログに出力し、フォーマットされたエラーオブジェクトを返す。
func InternalError(service, format string, v ...interface{}) error {
	msg := fmt.Sprintf(format, v...)
	Internal(service, msg)
	return fmt.Errorf("[%s] %s", service, msg)
}

// MARK: ClientError()
// クライアント起因のエラーをログに出力し、エラーオブジェクトを返す。
func ClientError(service, format string, v ...interface{}) error {
	msg := fmt.Sprintf(format, v...)
	Client(service, msg)
	return fmt.Errorf("[%s] %s", service, msg)
}

// MARK: ExternalError()
// 外部依存関係のエラーをログに出力し、エラーオブジェクトを返す。
func ExternalError(service, format string, v ...interface{}) error {
	msg := fmt.Sprintf(format, v...)
	External(service, msg)
	return fmt.Errorf("[%s] %s", service, msg)
}
