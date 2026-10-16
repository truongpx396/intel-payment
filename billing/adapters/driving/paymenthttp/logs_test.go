package paymenthttp_test

import (
	"bytes"
	"log/slog"
	"sync"
)

// logSink is an operator's log the tests can read.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *logSink) logger() *slog.Logger { return slog.New(slog.NewTextHandler(l, nil)) }
