package health

import (
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestNotifySendsToTheSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := Notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "READY=1" {
		t.Errorf("received %q, %v", buf[:n], err)
	}
}

func TestNotifyOutsideSystemdDoesNothing(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := Notify("READY=1"); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestWatchdogInterval(t *testing.T) {
	t.Setenv("WATCHDOG_USEC", "600000000") // 10 minutes
	t.Setenv("WATCHDOG_PID", "")
	if got := WatchdogInterval(); got != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", got)
	}
	t.Setenv("WATCHDOG_PID", strconv.Itoa(1)) // someone else's
	if got := WatchdogInterval(); got != 0 {
		t.Errorf("another process's watchdog gave %v", got)
	}
	t.Setenv("WATCHDOG_USEC", "")
	if got := WatchdogInterval(); got != 0 {
		t.Errorf("no watchdog gave %v", got)
	}
}

func TestJudge(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	fresh := map[string]time.Time{"htlc": now.Add(-time.Minute), "channels": now.Add(-30 * time.Second)}
	for _, c := range Judge(now, now.Add(-40*time.Minute), time.Hour, fresh) {
		if !c.OK {
			t.Errorf("healthy node: %+v", c)
		}
	}
	checks := Judge(now, now.Add(-3*time.Hour), time.Hour, map[string]time.Time{"htlc": now.Add(-10 * time.Minute)})
	want := map[string]bool{"cycle": false, "htlc stream": false, "channels stream": false}
	for _, c := range checks {
		if c.OK != want[c.Name] {
			t.Errorf("stale node: %+v", c)
		}
	}
	if c := Judge(now, time.Time{}, time.Hour, fresh)[0]; c.OK || c.Detail != "no cycle has completed yet" {
		t.Errorf("no cycle: %+v", c)
	}
}
