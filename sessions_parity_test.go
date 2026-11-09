package main

import (
	"strings"
	"testing"
	"time"
)

func TestSessionIDAndInteractiveStdin(t *testing.T) {
	app, _, _ := testApp(t)
	s, e := app.sessions.start(app.config.Get(), CommandInput{
		Cmd:     "read value; printf 'ANSWER=%s' \"$value\"",
		Workdir: app.config.Get().WorkRoot, YieldTimeMS: 250,
	})
	if e != nil {
		t.Fatal(e)
	}
	out := s.wait(250)
	if out.Finished || out.SessionID <= 0 {
		t.Fatalf("expected live numeric session %#v", out)
	}
	if e = s.write("demo\n"); e != nil {
		t.Fatal(e)
	}
	out = s.wait(3000)
	if !out.Finished || !strings.Contains(out.Output, "ANSWER=demo") {
		t.Fatalf("interactive output failed %#v", out)
	}
	if out.WallTimeSeconds <= 0 {
		t.Fatal("missing elapsed time")
	}
	if got, e := app.sessions.get(s.ID); e != nil || got != s {
		t.Fatal("numeric session lookup broken")
	}
}

func TestSessionGuardAndTimeout(t *testing.T) {
	app, _, _ := testApp(t)
	if _, e := app.sessions.start(app.config.Get(), CommandInput{Cmd: "echo hi", TTY: true}); e == nil {
		t.Fatal("TTY true must be rejected for plain pipe transport")
	}
	if _, e := app.sessions.start(app.config.Get(), CommandInput{Cmd: "echo hi", Shell: "fish"}); e == nil {
		t.Fatal("unsupported shell must be rejected")
	}
	s, e := app.sessions.start(app.config.Get(), CommandInput{Cmd: "sleep 3", TimeoutSeconds: 1})
	if e != nil {
		t.Fatal(e)
	}
	defer s.stop()
	out := s.wait(3000)
	if !out.Finished {
		t.Fatal("timed-out shell not stopped")
	}
	if time.Since(s.started) > 5*time.Second {
		t.Fatal("shell timeout exceeded")
	}
}
