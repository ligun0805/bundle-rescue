package bundlecore

import (
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

type Params struct {
	RPC         string
	ChainID     *big.Int
	Relays      []string
	AuthPrivHex string
	Logf        func(string, ...any)
	OnSimResult func(relay, raw string, ok bool, err string)

	// Flashbots / builders options
	Builders        []string // only for Flashbots Relay
	ReplacementUUID string   // Titan/RSYNC/Payload-compatible
	MinTimestamp    int64
	MaxTimestamp    int64
	BeaverAllowBuilderNetRefunds *bool
	BeaverRefundRecipientHex     string

	// Transfer details
	Token     common.Address
	From      common.Address
	To        common.Address
	AmountWei *big.Int

	// Keys
	SafePKHex string
	FromPKHex string

	// Strategy & tuning
	Blocks       int
	TipGweiBase  int64
	TipMul       float64
	BaseMul      int64
	BufferPct    int64
	SimulateOnly bool
	SkipIfPaused bool
	Verbose      bool

	// Tip selection mode
	TipMode       string // "fixed" (default) or "feehist"
	TipWindow     int    // last N blocks for eth_feeHistory.reward
	TipPercentile int    // 1..99 (usually 99)

	// Optional coinbase bribe
	BribeWei      *big.Int
	BribeGasLimit uint64

	// Per-relay extra headers
	ExtraHeaders map[string]map[string]string
}

type Result struct {
	Included bool
	Reason   string
}

func (p *Params) logf(format string, a ...any) {
	if p.Logf != nil {
		p.Logf(format, a...)
	}
}

func (p *Params) headerFor(u string) map[string]string {
	if p.ExtraHeaders == nil {
		return nil
	}
	if h, ok := p.ExtraHeaders[u]; ok {
		return h
	}
	u2 := strings.TrimPrefix(u, "mev:")
	if h, ok := p.ExtraHeaders[u2]; ok {
		return h
	}
	return nil
}
