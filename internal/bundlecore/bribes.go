package bundlecore

import (
	"bytes"
	"math"
	"math/big"
	"sort"

	"context"

	"github.com/ethereum/go-ethereum/ethclient"
)

// BribeSummary holds simple stats for coinbase bribes.
type BribeSummary struct {
	Count int
	Sum   *big.Int
	Max   *big.Int
	P50   *big.Int
	P95   *big.Int
	P99   *big.Int
}

// ScanCoinbaseBribes scans last N blocks for common bribe patterns:
//   1) contract creation with init-code containing {0x41,0xff} (COINBASE; SELFDESTRUCT)
//   2) direct ETH transfers to coinbase
func ScanCoinbaseBribes(ctx context.Context, ec *ethclient.Client, blocks int) ([]*big.Int, error) {
	if blocks <= 0 {
		blocks = 100
	}
	head, err := ec.HeaderByNumber(ctx, nil)
	if err != nil || head == nil || head.Number == nil {
		return nil, err
	}
	var out []*big.Int
	for i := 0; i < blocks; i++ {
		n := new(big.Int).Sub(head.Number, big.NewInt(int64(i)))
		if n.Sign() <= 0 {
			break
		}
		b, err := ec.BlockByNumber(ctx, n)
		if err != nil || b == nil {
			continue
		}
		cb := b.Header().Coinbase
		for _, tx := range b.Transactions() {
			// creation with 0x41ff pattern
			if tx.To() == nil {
				data := tx.Data()
				if len(data) >= 2 && bytes.Contains(data, []byte{0x41, 0xff}) {
					if tx.Value() != nil && tx.Value().Sign() > 0 {
						out = append(out, new(big.Int).Set(tx.Value()))
					}
					continue
				}
			}
			// direct transfer to coinbase
			if tx.To() != nil && *tx.To() == cb && tx.Value() != nil && tx.Value().Sign() > 0 {
				out = append(out, new(big.Int).Set(tx.Value()))
			}
		}
	}
	return out, nil
}

func bribeQuantile(vals []*big.Int, q float64) *big.Int {
	if len(vals) == 0 {
		return big.NewInt(0)
	}
	cp := make([]*big.Int, len(vals))
	for i, v := range vals {
		cp[i] = new(big.Int).Set(v)
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i].Cmp(cp[j]) < 0 })
	idx := int(math.Ceil(q*float64(len(cp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return new(big.Int).Set(cp[idx])
}

// SummarizeBribes aggregates stats over bribe values.
func SummarizeBribes(vals []*big.Int) BribeSummary {
	s := BribeSummary{Count: len(vals), Sum: big.NewInt(0), Max: big.NewInt(0), P50: big.NewInt(0), P95: big.NewInt(0), P99: big.NewInt(0)}
	for _, v := range vals {
		s.Sum.Add(s.Sum, v)
		if v.Cmp(s.Max) > 0 {
			s.Max = new(big.Int).Set(v)
		}
	}
	if len(vals) > 0 {
		s.P50 = bribeQuantile(vals, 0.50)
		s.P95 = bribeQuantile(vals, 0.95)
		s.P99 = bribeQuantile(vals, 0.99)
	}
	return s
}
