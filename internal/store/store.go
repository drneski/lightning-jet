// Package store keeps Jet's records in SQLite. Records are append-only: the
// store has no update or delete, and a correction is a new record.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"

	_ "modernc.org/sqlite" // the pure-Go SQLite driver, registered as "sqlite"
)

// ErrNotFound is returned when no record matches.
var ErrNotFound = errors.New("store: not found")

// Store is Jet's database.
type Store struct {
	db *sql.DB
}

// migrations are applied in order, once each. Never edit one that has
// shipped; add a new one.
var migrations = []string{
	`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
	CREATE TABLE snapshots (
		id TEXT PRIMARY KEY,
		taken_at INTEGER NOT NULL,
		node_pubkey TEXT NOT NULL,
		content_sha256 TEXT NOT NULL,
		record TEXT NOT NULL
	);
	CREATE INDEX snapshots_taken_at ON snapshots (taken_at);
	CREATE TABLE estimates (
		id TEXT PRIMARY KEY,
		snapshot_id TEXT NOT NULL REFERENCES snapshots (id),
		chan_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		computed_at INTEGER NOT NULL,
		record TEXT NOT NULL
	);
	CREATE INDEX estimates_snapshot ON estimates (snapshot_id);
	CREATE TABLE intents (
		id TEXT PRIMARY KEY,
		snapshot_id TEXT NOT NULL REFERENCES snapshots (id),
		chan_id TEXT NOT NULL,
		action TEXT NOT NULL,
		mode TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		record TEXT NOT NULL
	);
	CREATE INDEX intents_chan ON intents (chan_id, created_at);
	CREATE TABLE abstentions (
		id TEXT PRIMARY KEY,
		snapshot_id TEXT NOT NULL REFERENCES snapshots (id),
		chan_id TEXT NOT NULL,
		reason TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		record TEXT NOT NULL
	);
	CREATE INDEX abstentions_chan ON abstentions (chan_id, created_at);
	CREATE TABLE decisions (
		id TEXT PRIMARY KEY,
		intent_id TEXT NOT NULL UNIQUE REFERENCES intents (id),
		verdict TEXT NOT NULL,
		mode TEXT NOT NULL,
		decided_at INTEGER NOT NULL,
		record TEXT NOT NULL
	);
	CREATE TABLE outcomes (
		id TEXT PRIMARY KEY,
		intent_id TEXT NOT NULL REFERENCES intents (id),
		state TEXT NOT NULL,
		at INTEGER NOT NULL,
		record TEXT NOT NULL
	);
	CREATE INDEX outcomes_intent ON outcomes (intent_id, at);`,

	`CREATE TABLE htlc_failures (
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		kind TEXT NOT NULL,
		event_type TEXT NOT NULL,
		incoming_chan_id TEXT NOT NULL,
		outgoing_chan_id TEXT NOT NULL,
		incoming_htlc_id INTEGER NOT NULL,
		outgoing_htlc_id INTEGER NOT NULL,
		incoming_amt_msat INTEGER NOT NULL,
		outgoing_amt_msat INTEGER NOT NULL,
		incoming_timelock INTEGER NOT NULL,
		outgoing_timelock INTEGER NOT NULL,
		wire_failure TEXT NOT NULL,
		failure_detail TEXT NOT NULL,
		failure_string TEXT NOT NULL,
		recorded_at INTEGER NOT NULL
	);
	CREATE INDEX htlc_failures_outgoing ON htlc_failures (outgoing_chan_id, at);
	CREATE INDEX htlc_failures_at ON htlc_failures (at);
	CREATE TABLE channel_events (
		id INTEGER PRIMARY KEY,
		at INTEGER NOT NULL,
		type TEXT NOT NULL,
		channel_point TEXT NOT NULL,
		chan_id TEXT
	);
	CREATE INDEX channel_events_point ON channel_events (channel_point, at);
	CREATE TABLE stream_events (
		id INTEGER PRIMARY KEY,
		stream TEXT NOT NULL,
		event TEXT NOT NULL,
		at INTEGER NOT NULL,
		detail TEXT NOT NULL
	);
	CREATE INDEX stream_events_stream ON stream_events (stream, at);`,
}

// Open opens or creates the database at path, creating its directory with
// owner-only permissions, and brings the schema up to date.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: creating directory: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database for reading, as jet status, explain
// and health do. It never creates, migrates or writes anything, so it works for
// a user who may read the files but not write them, such as an operator in the
// service's group.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	q.Set("mode", "ro")
	if _, err := os.Stat(path + "-wal"); errors.Is(err, fs.ErrNotExist) {
		// No write-ahead log means no jet run has the database open, and
		// the last one to close it folded the log in. SQLite would still
		// create a log to read through, which a reader without write access
		// can't, so read the file as it is. A jet run starting meanwhile
		// writes to a new log, which this read doesn't see.
		q.Set("immutable", "1")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", path, err)
	}
	version, err := schemaVersion(ctx, db)
	switch {
	case err != nil:
	case version < len(migrations):
		err = fmt.Errorf("store: database schema %d is older than this Jet (%d); jet run upgrades it", version, len(migrations))
	case version > len(migrations):
		err = fmt.Errorf("store: database schema %d is newer than this Jet (%d)", version, len(migrations))
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// schemaVersion is the number of migrations applied, 0 for a new database.
func schemaVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var version int
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil && !isMissingTable(err) && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("store: reading schema version: %w", err)
	}
	return version, nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	version, err := schemaVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema %d is newer than this Jet (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('schema_version', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		len(migrations)); err != nil {
		return err
	}
	return tx.Commit()
}

func isMissingTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// Cycle is everything one decision cycle produced. SaveCycle writes it in one
// transaction, so a cycle is recorded whole or not at all.
type Cycle struct {
	Snapshot    *contracts.Snapshot
	Estimates   []*contracts.Estimate
	Intents     []*contracts.Intent
	Abstentions []*contracts.Abstention
	Decisions   []*contracts.Decision
	Outcomes    []*contracts.Outcome
}

// SaveCycle validates every record and writes them all, or none.
func (s *Store) SaveCycle(ctx context.Context, c Cycle) error {
	if c.Snapshot == nil {
		return errors.New("store: a cycle needs its snapshot")
	}
	var errs []error
	errs = append(errs, c.Snapshot.Validate())
	for _, r := range c.Estimates {
		errs = append(errs, r.Validate())
	}
	for _, r := range c.Intents {
		errs = append(errs, r.Validate())
	}
	for _, r := range c.Abstentions {
		errs = append(errs, r.Validate())
	}
	for _, r := range c.Decisions {
		errs = append(errs, r.Validate())
	}
	for _, r := range c.Outcomes {
		errs = append(errs, r.Validate())
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("store: refusing an invalid cycle: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert := func(query string, record any, args ...any) error {
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, query, append(args, string(b))...)
		return err
	}
	snap := c.Snapshot
	if err := insert(`INSERT INTO snapshots (id, taken_at, node_pubkey, content_sha256, record) VALUES (?, ?, ?, ?, ?)`,
		snap, snap.ID, ms(snap.TakenAt), snap.NodePubkey, snap.ContentSHA256); err != nil {
		return fmt.Errorf("store: snapshot: %w", err)
	}
	for _, r := range c.Estimates {
		if err := insert(`INSERT INTO estimates (id, snapshot_id, chan_id, kind, computed_at, record) VALUES (?, ?, ?, ?, ?, ?)`,
			r, r.ID, r.SnapshotID, r.ChanID.String(), r.Kind, ms(r.ComputedAt)); err != nil {
			return fmt.Errorf("store: estimate: %w", err)
		}
	}
	for _, r := range c.Intents {
		if err := insert(`INSERT INTO intents (id, snapshot_id, chan_id, action, mode, created_at, record) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r, r.ID, r.SnapshotID, r.Target.ChanID.String(), string(r.Action), string(r.Mode), ms(r.CreatedAt)); err != nil {
			return fmt.Errorf("store: intent: %w", err)
		}
	}
	for _, r := range c.Abstentions {
		if err := insert(`INSERT INTO abstentions (id, snapshot_id, chan_id, reason, created_at, record) VALUES (?, ?, ?, ?, ?, ?)`,
			r, r.ID, r.SnapshotID, r.ChanID.String(), string(r.Reason), ms(r.CreatedAt)); err != nil {
			return fmt.Errorf("store: abstention: %w", err)
		}
	}
	for _, r := range c.Decisions {
		if err := insert(`INSERT INTO decisions (id, intent_id, verdict, mode, decided_at, record) VALUES (?, ?, ?, ?, ?, ?)`,
			r, r.ID, r.IntentID, string(r.Verdict), string(r.Mode), ms(r.DecidedAt)); err != nil {
			return fmt.Errorf("store: decision: %w", err)
		}
	}
	for _, r := range c.Outcomes {
		if err := insert(`INSERT INTO outcomes (id, intent_id, state, at, record) VALUES (?, ?, ?, ?, ?)`,
			r, r.ID, r.IntentID, string(r.State), ms(r.At)); err != nil {
			return fmt.Errorf("store: outcome: %w", err)
		}
	}
	return tx.Commit()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// ApprovedFeeChanges returns when fee changes to a channel were approved in a
// mode since a time, newest first. Shadow and live approvals are separate
// ledgers, so shadow mode meets the limits a live run would, without touching
// live history.
func (s *Store) ApprovedFeeChanges(ctx context.Context, chanID contracts.ChanID, mode contracts.Mode, since time.Time) ([]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT d.decided_at FROM decisions d JOIN intents i ON i.id = d.intent_id
		WHERE i.chan_id = ? AND i.action = ? AND d.verdict = ? AND d.mode = ? AND d.decided_at >= ?
		ORDER BY d.decided_at DESC`,
		chanID.String(), string(contracts.ActionFeeUpdate), string(contracts.Approved), string(mode), ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []time.Time
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return nil, err
		}
		out = append(out, time.UnixMilli(at).UTC())
	}
	return out, rows.Err()
}

// Evaluation is the latest decision about one channel, assembled from its
// records: the intent or abstention, and everything it rests on.
type Evaluation struct {
	Snapshot   *contracts.Snapshot
	Estimates  []*contracts.Estimate
	Intent     *contracts.Intent
	Abstention *contracts.Abstention
	Decision   *contracts.Decision
	Outcomes   []*contracts.Outcome
}

// LatestEvaluation returns the most recent intent or abstention for a channel,
// with its snapshot, estimates, decision and outcomes.
func (s *Store) LatestEvaluation(ctx context.Context, chanID contracts.ChanID) (*Evaluation, error) {
	var kind, record string
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, record FROM (
			SELECT 'intent' AS kind, record, created_at, id FROM intents WHERE chan_id = ?
			UNION ALL
			SELECT 'abstention' AS kind, record, created_at, id FROM abstentions WHERE chan_id = ?
		) ORDER BY created_at DESC, id DESC LIMIT 1`, chanID.String(), chanID.String()).Scan(&kind, &record)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	ev := &Evaluation{}
	var snapshotID string
	var estimateIDs []string
	if kind == "intent" {
		ev.Intent = &contracts.Intent{}
		if err := json.Unmarshal([]byte(record), ev.Intent); err != nil {
			return nil, err
		}
		snapshotID, estimateIDs = ev.Intent.SnapshotID, ev.Intent.Rationale.EstimateIDs
	} else {
		ev.Abstention = &contracts.Abstention{}
		if err := json.Unmarshal([]byte(record), ev.Abstention); err != nil {
			return nil, err
		}
		snapshotID, estimateIDs = ev.Abstention.SnapshotID, ev.Abstention.EstimateIDs
	}
	if ev.Snapshot, err = getRecord[contracts.Snapshot](ctx, s.db, `SELECT record FROM snapshots WHERE id = ?`, snapshotID); err != nil {
		return nil, fmt.Errorf("store: snapshot %s: %w", snapshotID, err)
	}
	for _, id := range estimateIDs {
		e, err := getRecord[contracts.Estimate](ctx, s.db, `SELECT record FROM estimates WHERE id = ?`, id)
		if err != nil {
			return nil, fmt.Errorf("store: estimate %s: %w", id, err)
		}
		ev.Estimates = append(ev.Estimates, e)
	}
	if ev.Intent != nil {
		d, err := getRecord[contracts.Decision](ctx, s.db, `SELECT record FROM decisions WHERE intent_id = ?`, ev.Intent.ID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		ev.Decision = d
		rows, err := s.db.QueryContext(ctx, `SELECT record FROM outcomes WHERE intent_id = ? ORDER BY at, id`, ev.Intent.ID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var rec string
			if err := rows.Scan(&rec); err != nil {
				return nil, err
			}
			o := &contracts.Outcome{}
			if err := json.Unmarshal([]byte(rec), o); err != nil {
				return nil, err
			}
			ev.Outcomes = append(ev.Outcomes, o)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return ev, nil
}

func getRecord[T any](ctx context.Context, db *sql.DB, query string, arg any) (*T, error) {
	var rec string
	err := db.QueryRowContext(ctx, query, arg).Scan(&rec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := new(T)
	if err := json.Unmarshal([]byte(rec), v); err != nil {
		return nil, err
	}
	return v, nil
}

// Status summarizes the most recent cycle.
type Status struct {
	SnapshotID  string
	TakenAt     time.Time
	Channels    int
	Abstentions map[contracts.AbstentionReason]int
	Approved    int
	Rejected    int
	// PerChannel is each channel's result in that cycle, in channel order.
	PerChannel []ChannelStatus
}

// ChannelStatus is one channel's result in a cycle: an abstention's reason,
// or a proposal and its verdict.
type ChannelStatus struct {
	ChanID       contracts.ChanID
	LocalMsat    contracts.MilliSat
	CapacityMsat contracts.MilliSat
	Abstained    contracts.AbstentionReason
	Proposal     *contracts.FeeParameters
	Verdict      contracts.Verdict
}

// LatestStatus returns a summary of the most recent recorded cycle.
func (s *Store) LatestStatus(ctx context.Context) (*Status, error) {
	snap, err := getRecord[contracts.Snapshot](ctx, s.db, `SELECT record FROM snapshots ORDER BY taken_at DESC, id DESC LIMIT ?`, 1)
	if err != nil {
		return nil, err
	}
	st := &Status{SnapshotID: snap.ID, TakenAt: snap.TakenAt, Channels: len(snap.Channels), Abstentions: map[contracts.AbstentionReason]int{}}
	rows, err := s.db.QueryContext(ctx, `SELECT reason, COUNT(*) FROM abstentions WHERE snapshot_id = ? GROUP BY reason`, snap.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var n int
		if err := rows.Scan(&reason, &n); err != nil {
			return nil, err
		}
		st.Abstentions[contracts.AbstentionReason(reason)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(d.verdict = 'approved'), 0), COALESCE(SUM(d.verdict = 'rejected'), 0)
		FROM decisions d JOIN intents i ON i.id = d.intent_id WHERE i.snapshot_id = ?`, snap.ID).Scan(&st.Approved, &st.Rejected)
	if err != nil {
		return nil, err
	}

	results := map[contracts.ChanID]*ChannelStatus{}
	for _, c := range snap.Channels {
		cs := &ChannelStatus{ChanID: c.ChanID, LocalMsat: c.LocalMsat, CapacityMsat: c.CapacityMsat}
		results[c.ChanID] = cs
		st.PerChannel = append(st.PerChannel, *cs)
	}
	abst, err := s.db.QueryContext(ctx, `SELECT chan_id, reason FROM abstentions WHERE snapshot_id = ?`, snap.ID)
	if err != nil {
		return nil, err
	}
	defer abst.Close()
	for abst.Next() {
		var ch, reason string
		if err := abst.Scan(&ch, &reason); err != nil {
			return nil, err
		}
		if id, err := contracts.ParseChanID(ch); err == nil && results[id] != nil {
			results[id].Abstained = contracts.AbstentionReason(reason)
		}
	}
	if err := abst.Err(); err != nil {
		return nil, err
	}
	props, err := s.db.QueryContext(ctx, `
		SELECT i.chan_id, i.record, d.verdict FROM intents i JOIN decisions d ON d.intent_id = i.id WHERE i.snapshot_id = ?`, snap.ID)
	if err != nil {
		return nil, err
	}
	defer props.Close()
	for props.Next() {
		var ch, rec, verdict string
		if err := props.Scan(&ch, &rec, &verdict); err != nil {
			return nil, err
		}
		id, err := contracts.ParseChanID(ch)
		if err != nil || results[id] == nil {
			continue
		}
		var in contracts.Intent
		if err := json.Unmarshal([]byte(rec), &in); err != nil {
			return nil, err
		}
		results[id].Proposal, results[id].Verdict = in.Parameters.Fee, contracts.Verdict(verdict)
	}
	if err := props.Err(); err != nil {
		return nil, err
	}
	for i := range st.PerChannel {
		st.PerChannel[i] = *results[st.PerChannel[i].ChanID]
	}
	return st, nil
}
