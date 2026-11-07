// Package lnd reads from LND for Jet through LND's REST API, using only the
// standard library. This package only reads: Reader has no method that
// changes anything, and the client is meant to hold LND's read-only macaroon.
package lnd

import (
	"context"
	"time"

	"github.com/drneski/lightning-jet/v2/internal/contracts"
)

// Info is the node's identity and sync state.
type Info struct {
	IdentityPubkey string
	Alias          string
	Version        string
	BlockHeight    uint32
	SyncedToChain  bool
	SyncedToGraph  bool
}

// Channel is one open channel as LND lists it. Amounts are whole sats, as LND
// reports them.
type Channel struct {
	ChanID       contracts.ChanID
	RemotePubkey string
	ChannelPoint string
	Active       bool
	Private      bool
	CapacitySat  int64
	LocalSat     int64
	RemoteSat    int64
}

// Edge is a channel as the graph knows it: both endpoints and their policies.
// A policy is nil until it has been gossiped.
type Edge struct {
	ChanID      contracts.ChanID
	CapacitySat int64
	Node1Pub    string
	Node2Pub    string
	Node1Policy *RoutingPolicy
	Node2Policy *RoutingPolicy
}

// RoutingPolicy is one side's policy for a channel.
type RoutingPolicy struct {
	TimeLockDelta    uint32
	MinHTLCMsat      int64
	MaxHTLCMsat      int64
	FeeBaseMsat      int64
	FeeRateMilliMsat int64
	Disabled         bool
	LastUpdate       time.Time
}

// Forward is one completed forward.
type Forward struct {
	Time       time.Time
	ChanIDIn   contracts.ChanID
	ChanIDOut  contracts.ChanID
	AmtInMsat  int64
	AmtOutMsat int64
	FeeMsat    int64
}

// Reader is everything Jet reads from LND. Every call takes a context, and the
// implementation must respect its deadline.
type Reader interface {
	GetInfo(ctx context.Context) (Info, error)
	ListChannels(ctx context.Context) ([]Channel, error)
	GetChanInfo(ctx context.Context, id contracts.ChanID) (Edge, error)
	// ForwardingHistory returns every forward in [start, end), across as many
	// pages as LND needs.
	ForwardingHistory(ctx context.Context, start, end time.Time) ([]Forward, error)
	// GraphNode returns a node's alias and public channels as the graph knows
	// them.
	GraphNode(ctx context.Context, pubkey string) (GraphNode, error)
	// Payments returns the payments we sent since a time, including those
	// that failed or are still in flight, with every attempt, across as many
	// pages as LND needs.
	Payments(ctx context.Context, since time.Time) ([]Payment, error)
}

// Payment is one payment we sent, with each attempt LND made to deliver it.
type Payment struct {
	CreatedAt time.Time
	Status    string // SUCCEEDED, FAILED or IN_FLIGHT
	Attempts  []Attempt
}

// Attempt is one route LND tried for a payment. FailureSourceIndex is the
// position on the route of the node that reported the failure, 0 being us.
type Attempt struct {
	At                 time.Time
	Status             string // SUCCEEDED, FAILED or IN_FLIGHT
	TotalAmtMsat       int64
	TotalFeesMsat      int64
	Hops               []Hop
	FailureCode        string
	FailureSourceIndex int
}

// Hop is one channel of a route and the node at its far end.
type Hop struct {
	ChanID contracts.ChanID
	PubKey string
}

// GraphNode is a node as gossip describes it. The alias is whatever the node
// announced: untrusted text, up to 32 bytes.
type GraphNode struct {
	Alias    string
	Channels []Edge
}
