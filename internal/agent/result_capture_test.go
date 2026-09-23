package agent

import (
	"errors"
	"io"
	"testing"
)

type shortResultWriter struct{}

func (shortResultWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestIncompleteFullLogWriteFailsResultCapture(t *testing.T) {
	log := &liveJobLog{full: shortResultWriter{}}
	n, err := log.Write([]byte("required log record"))
	if n != len("required log record")-1 || !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, errResultCapture) {
		t.Fatalf("short full-log write must fail result capture: n=%d err=%v", n, err)
	}
	if log.String() != "" {
		t.Fatal("incomplete full log was presented as successfully captured output")
	}
	if !errors.Is(log.Err(), errResultCapture) || !errors.Is(log.Err(), io.ErrShortWrite) {
		t.Fatal("full-log failure must remain observable even if the process also fails")
	}
}
