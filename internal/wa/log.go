package wa

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// slogLogger adapts slog to whatsmeow's logger, dropping records below min
// (whatsmeow is very chatty at debug level).
type slogLogger struct {
	l   *slog.Logger
	min slog.Level
}

func newLogger(l *slog.Logger, min slog.Level) waLog.Logger { return slogLogger{l, min} }

func (s slogLogger) log(level slog.Level, msg string, args []any) {
	if level < s.min || !s.l.Enabled(context.Background(), level) {
		return
	}
	s.l.Log(context.Background(), level, fmt.Sprintf(msg, args...))
}

func (s slogLogger) Errorf(msg string, args ...any) { s.log(slog.LevelError, msg, args) }
func (s slogLogger) Warnf(msg string, args ...any)  { s.log(slog.LevelWarn, msg, args) }
func (s slogLogger) Infof(msg string, args ...any)  { s.log(slog.LevelInfo, msg, args) }
func (s slogLogger) Debugf(msg string, args ...any) { s.log(slog.LevelDebug, msg, args) }

func (s slogLogger) Sub(module string) waLog.Logger {
	return slogLogger{s.l.With("sub", module), s.min}
}
