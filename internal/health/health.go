// Package health tells a supervisor whether Jet is alive. Under systemd it
// speaks the notify protocol over $NOTIFY_SOCKET (READY, WATCHDOG, STOPPING),
// and it judges the heartbeats Jet keeps in its store, which jet health reads.
package health

import (
	"net"
	"os"
	"strconv"
	"time"
)

// Notify sends a state such as "READY=1" to systemd. Outside systemd, or when
// the unit doesn't ask for notifications, it does nothing.
func Notify(state string) error {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return nil
	}
	if sock[0] == '@' {
		sock = "\x00" + sock[1:] // an abstract socket
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sock, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(state))
	return err
}

// WatchdogInterval is how often to ping systemd's watchdog: half its timeout,
// or zero when the unit has no watchdog.
func WatchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0 // the watchdog is for another process
	}
	return time.Duration(usec) * time.Microsecond / 2
}

// Meta keys for the heartbeats, in Unix milliseconds.
const (
	KeyCycleOK   = "cycle.last_ok_at"
	KeyCycleFail = "cycle.last_failure_at"
	KeyCycleErr  = "cycle.last_failure"
	KeyInterval  = "cycle.interval_ms"
)

// Check is one health check's result.
type Check struct {
	Name   string
	OK     bool
	Detail string
}

// Judge decides whether heartbeats are fresh. A cycle is due every interval;
// one missed with some slack is unhealthy. A stream marks itself alive every
// minute while connected.
func Judge(now, lastCycle time.Time, interval time.Duration, streams map[string]time.Time) []Check {
	var out []Check
	due := 2*interval + 5*time.Minute
	switch {
	case lastCycle.IsZero():
		out = append(out, Check{"cycle", false, "no cycle has completed yet"})
	case now.Sub(lastCycle) > due:
		out = append(out, Check{"cycle", false, "the last completed cycle was " + ago(now, lastCycle) + "; one is due every " + interval.String()})
	default:
		out = append(out, Check{"cycle", true, "the last cycle completed " + ago(now, lastCycle)})
	}
	for _, name := range []string{"htlc", "channels"} {
		at, ok := streams[name]
		switch {
		case !ok:
			out = append(out, Check{name + " stream", false, "never recorded"})
		case now.Sub(at) > 3*time.Minute:
			out = append(out, Check{name + " stream", false, "last seen alive " + ago(now, at)})
		default:
			out = append(out, Check{name + " stream", true, "connected, seen alive " + ago(now, at)})
		}
	}
	return out
}

func ago(now, t time.Time) string {
	return now.Sub(t).Round(time.Second).String() + " ago"
}
