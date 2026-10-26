package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Observations are what LND's event streams report as they happen and won't
// report again: failed HTLCs and channels going up and down. Like decision
// records they are append-only. Their rows are plain columns, not JSON.

// HTLCFailure is one failed HTLC as LND reported it. There is no payment hash:
// LND's HTLC stream doesn't carry one.
type HTLCFailure struct {
	At               time.Time
	Kind             string // link_fail or forward_fail
	EventType        string // FORWARD, SEND or RECEIVE
	IncomingChanID   contracts.ChanID
	OutgoingChanID   contracts.ChanID
	IncomingHTLCID   uint64
	OutgoingHTLCID   uint64
	IncomingAmtMsat  contracts.MilliSat
	OutgoingAmtMsat  contracts.MilliSat
	IncomingTimelock uint32
	OutgoingTimelock uint32
	WireFailure      string
	FailureDetail    string
	FailureString    string
	RecordedAt       time.Time
}

// SaveHTLCFailure records a failed HTLC.
func (s *Store) SaveHTLCFailure(ctx context.Context, f HTLCFailure) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO htlc_failures
		(at, kind, event_type, incoming_chan_id, outgoing_chan_id, incoming_htlc_id, outgoing_htlc_id,
		 incoming_amt_msat, outgoing_amt_msat, incoming_timelock, outgoing_timelock,
		 wire_failure, failure_detail, failure_string, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ms(f.At), f.Kind, f.EventType, f.IncomingChanID.String(), f.OutgoingChanID.String(),
		int64(f.IncomingHTLCID), int64(f.OutgoingHTLCID), int64(f.IncomingAmtMsat), int64(f.OutgoingAmtMsat),
		f.IncomingTimelock, f.OutgoingTimelock, f.WireFailure, f.FailureDetail, f.FailureString, ms(f.RecordedAt))
	return err
}

// ChannelEvent is one channel event as LND reported it, stamped when it was
// received, since LND's channel events carry no time.
type ChannelEvent struct {
	At           time.Time
	Type         string
	ChannelPoint string
	ChanID       contracts.ChanID
}

// SaveChannelEvent records a channel event.
func (s *Store) SaveChannelEvent(ctx context.Context, e ChannelEvent) error {
	var chanID any
	if e.ChanID != 0 {
		chanID = e.ChanID.String()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO channel_events (at, type, channel_point, chan_id) VALUES (?, ?, ?, ?)`,
		ms(e.At), e.Type, e.ChannelPoint, chanID)
	return err
}

// Stream connection events. Together they say exactly when a stream was
// being recorded, so a gap is known rather than silent.
const (
	StreamConnected    = "connected"
	StreamDisconnected = "disconnected"
)

// RecordStreamEvent records that a stream connected or disconnected.
func (s *Store) RecordStreamEvent(ctx context.Context, stream, event string, at time.Time, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO stream_events (stream, event, at, detail) VALUES (?, ?, ?, ?)`,
		stream, event, ms(at), detail)
	return err
}

// StreamEvent is one recorded connect or disconnect.
type StreamEvent struct {
	Event  string
	At     time.Time
	Detail string
}

// StreamEvents returns a stream's connection events since a time, oldest
// first.
func (s *Store) StreamEvents(ctx context.Context, stream string, since time.Time) ([]StreamEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event, at, detail FROM stream_events WHERE stream = ? AND at >= ? ORDER BY at, id`, stream, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamEvent
	for rows.Next() {
		var e StreamEvent
		var at int64
		if err := rows.Scan(&e.Event, &at, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastStreamEvent returns a stream's most recent connection event.
func (s *Store) LastStreamEvent(ctx context.Context, stream string) (event string, at time.Time, err error) {
	var atMs int64
	err = s.db.QueryRowContext(ctx, `SELECT event, at FROM stream_events WHERE stream = ? ORDER BY at DESC, id DESC LIMIT 1`, stream).Scan(&event, &atMs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, ErrNotFound
	}
	return event, time.UnixMilli(atMs).UTC(), err
}

// Coverage returns how much of [since, now] a stream was recorded for. A
// connection still open counts up to now.
func (s *Store) Coverage(ctx context.Context, stream string, since, now time.Time) (time.Duration, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event, at FROM stream_events WHERE stream = ? ORDER BY at, id`, stream)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var covered time.Duration
	var openAt time.Time
	open := false
	add := func(from, to time.Time) {
		if from.Before(since) {
			from = since
		}
		if to.After(now) {
			to = now
		}
		if to.After(from) {
			covered += to.Sub(from)
		}
	}
	for rows.Next() {
		var event string
		var atMs int64
		if err := rows.Scan(&event, &atMs); err != nil {
			return 0, err
		}
		at := time.UnixMilli(atMs).UTC()
		switch event {
		case StreamConnected:
			if open {
				add(openAt, at) // a connection that ended without a record
			}
			open, openAt = true, at
		case StreamDisconnected:
			if open {
				add(openAt, at)
			}
			open = false
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if open {
		add(openAt, now)
	}
	return covered, nil
}

// BalanceFailures is a channel's HTLCs that failed for lack of balance.
type BalanceFailures struct {
	Count      int
	AmountMsat contracts.MilliSat
}

// BalanceFailuresSince totals, per outgoing channel, the forwards that failed
// because the channel lacked balance. It counts attempts, not payments: a
// sender's retries and probes count again.
func (s *Store) BalanceFailuresSince(ctx context.Context, since time.Time) (map[contracts.ChanID]BalanceFailures, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT outgoing_chan_id, COUNT(*), SUM(outgoing_amt_msat) FROM htlc_failures
		WHERE kind = 'link_fail' AND failure_detail = 'INSUFFICIENT_BALANCE' AND at >= ?
		GROUP BY outgoing_chan_id`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[contracts.ChanID]BalanceFailures{}
	for rows.Next() {
		var ch string
		var n int
		var amt int64
		if err := rows.Scan(&ch, &n, &amt); err != nil {
			return nil, err
		}
		id, err := contracts.ParseChanID(ch)
		if err != nil {
			return nil, err
		}
		out[id] = BalanceFailures{Count: n, AmountMsat: contracts.MilliSat(amt)}
	}
	return out, rows.Err()
}

// SetMeta stores operational state, such as when a stream was last seen
// alive. Meta is mutable; records are not.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Meta returns a meta value, or ErrNotFound.
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}
