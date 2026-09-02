package events

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestJSONLWriterReaderRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewJSONLWriter(&buf)
	samples := sampleEvents()
	for _, ev := range samples {
		if err := w.Write(ev); err != nil {
			t.Fatalf("Write %s: %v", ev.Kind(), err)
		}
	}
	if w.Written() != uint64(len(samples)) {
		t.Errorf("Written = %d, want %d", w.Written(), len(samples))
	}
	if w.Err() != nil {
		t.Errorf("Err = %v", w.Err())
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(samples) {
		t.Fatalf("%d lines, want %d", len(lines), len(samples))
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, `{"kind":"`+string(samples[i].Kind())+`"`) {
			t.Errorf("line %d: %s", i+1, l)
		}
	}

	got, err := ReadAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, samples) {
		t.Errorf("ReadAll differs from what was written")
		for i := range samples {
			if i < len(got) && !reflect.DeepEqual(got[i], samples[i]) {
				t.Errorf("  %d: got %#v\n     want %#v", i, got[i], samples[i])
			}
		}
	}
}

func TestJSONLReaderSkipsBlankLinesAndReportsBadLine(t *testing.T) {
	in := "\n" +
		`{"kind":"run_paused","at":"2026-09-02T12:01:03.5Z","run_id":"r","reason":"a"}` + "\n" +
		"   \n" +
		`this is not json` + "\n" +
		`{"kind":"run_resumed","at":"2026-09-02T12:01:04Z","run_id":"r"}` + "\n"
	r := NewJSONLReader(strings.NewReader(in))

	ev, err := r.Next()
	if err != nil || ev.Kind() != KindRunPaused {
		t.Fatalf("first: %v %v", ev, err)
	}
	if r.Line() != 2 {
		t.Errorf("Line() = %d, want 2", r.Line())
	}
	_, err = r.Next()
	if err == nil || !strings.Contains(err.Error(), "line 4") || !errors.Is(err, ErrNotEvent) {
		t.Fatalf("bad line error: %v", err)
	}
	// The reader can continue past the bad line.
	ev, err = r.Next()
	if err != nil || ev.Kind() != KindRunResumed {
		t.Fatalf("after bad line: %v %v", ev, err)
	}
	if _, err = r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("at end: %v, want io.EOF", err)
	}
	if _, err = r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after end: %v, want io.EOF again", err)
	}

	// ReadAll returns what it read before the error, plus the error.
	got, err := ReadAll(strings.NewReader(in))
	if err == nil || len(got) != 1 {
		t.Errorf("ReadAll = %d events, %v", len(got), err)
	}
	empty, err := ReadAll(strings.NewReader("\n\n"))
	if err != nil || len(empty) != 0 {
		t.Errorf("ReadAll(empty) = %v, %v", empty, err)
	}
}

func TestJSONLReaderHandlesLongLines(t *testing.T) {
	// A GraphChanged snapshot far larger than bufio.Scanner's default 64 KiB.
	g := GraphChanged{Meta: sampleMeta()}
	for i := 0; i < 2000; i++ {
		b := sampleBead()
		b.Description = strings.Repeat("x", 100)
		g.Beads = append(g.Beads, b)
	}
	var buf bytes.Buffer
	if err := NewJSONLWriter(&buf).Write(g); err != nil {
		t.Fatal(err)
	}
	if buf.Len() < 200*1024 {
		t.Fatalf("test line too short to exercise the buffer: %d bytes", buf.Len())
	}
	got, err := ReadAll(&buf)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadAll: %d, %v", len(got), err)
	}
	if !reflect.DeepEqual(got[0], g) {
		t.Error("long GraphChanged did not round trip")
	}
}

func TestJSONLReaderPropagatesReadErrors(t *testing.T) {
	boom := errors.New("boom")
	r := NewJSONLReader(io.MultiReader(strings.NewReader(`{"kind":"warning","message":"a"}`+"\n"), failingReader{boom}))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, boom) {
		t.Errorf("read error not propagated: %v", err)
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

func TestJSONLWriterStickyError(t *testing.T) {
	boom := errors.New("disk full")
	fw := &failingWriter{err: boom, after: 1}
	w := NewJSONLWriter(fw)
	ev := RunPaused{Meta: sampleMeta(), Reason: "a"}
	if err := w.Write(ev); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := w.Write(ev); !errors.Is(err, boom) {
		t.Fatalf("second write: %v, want boom", err)
	}
	if err := w.Write(ev); !errors.Is(err, boom) {
		t.Fatalf("third write: %v, want sticky boom", err)
	}
	if fw.calls != 2 {
		t.Errorf("underlying writer called %d times, want 2 (sticky error must not retry)", fw.calls)
	}
	if !errors.Is(w.Err(), boom) || w.Written() != 1 {
		t.Errorf("Err=%v Written=%d", w.Err(), w.Written())
	}
	// A marshal failure is reported but is not sticky.
	w2 := NewJSONLWriter(io.Discard)
	if err := w2.Write(nil); !errors.Is(err, ErrNotEvent) {
		t.Errorf("Write(nil) = %v", err)
	}
	if err := w2.Write(ev); err != nil {
		t.Errorf("write after marshal failure: %v", err)
	}
}

type failingWriter struct {
	err   error
	after int
	calls int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls++
	if f.calls > f.after {
		return 0, f.err
	}
	return len(p), nil
}

func TestJSONLWriterWritesThroughToFileImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test cleanup
	w := NewJSONLWriter(f)
	if err := w.Write(RunStopped{Meta: sampleMeta(), Reason: "SIGINT"}); err != nil {
		t.Fatal(err)
	}
	// Read the file back through a separate descriptor without closing or
	// syncing the writer's: the bytes must already be there.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), `{"kind":"run_stopped"`) || !strings.HasSuffix(string(got), "\n") {
		t.Errorf("file content: %q", got)
	}
}

func TestJSONLWriterServe(t *testing.T) {
	t.Run("bus close ends serve with nil", func(t *testing.T) {
		bus := NewBus()
		var buf syncBuffer
		w := NewJSONLWriter(&buf)
		errc := make(chan error, 1)
		ready := make(chan struct{})
		go func() {
			close(ready)
			errc <- w.Serve(context.Background(), bus, 64)
		}()
		<-ready
		waitFor(t, func() bool { return bus.Stats().Subscribers == 1 })
		samples := sampleEvents()
		for _, ev := range samples {
			bus.Publish(ev)
		}
		bus.Close()
		select {
		case err := <-errc:
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after bus.Close")
		}
		got, err := ReadAll(strings.NewReader(buf.String()))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, samples) {
			t.Errorf("Serve wrote %d events, want %d identical", len(got), len(samples))
		}
	})

	t.Run("context cancel ends serve with ctx error", func(t *testing.T) {
		bus := NewBus()
		defer bus.Close()
		w := NewJSONLWriter(io.Discard)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- w.Serve(ctx, bus, 4) }()
		waitFor(t, func() bool { return bus.Stats().Subscribers == 1 })
		cancel()
		select {
		case err := <-errc:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Serve: %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after cancel")
		}
		waitFor(t, func() bool { return bus.Stats().Subscribers == 0 })
	})

	t.Run("write failure ends serve with the error", func(t *testing.T) {
		bus := NewBus()
		defer bus.Close()
		boom := errors.New("boom")
		w := NewJSONLWriter(&failingWriter{err: boom})
		errc := make(chan error, 1)
		go func() { errc <- w.Serve(context.Background(), bus, 4) }()
		waitFor(t, func() bool { return bus.Stats().Subscribers == 1 })
		bus.Publish(RunPaused{Meta: sampleMeta(), Reason: "x"})
		select {
		case err := <-errc:
			if !errors.Is(err, boom) {
				t.Fatalf("Serve: %v, want boom", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Serve did not return after write failure")
		}
		waitFor(t, func() bool { return bus.Stats().Subscribers == 0 })
	})
}
