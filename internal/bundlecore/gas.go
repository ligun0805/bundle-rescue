package bundlecore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/ethereum/go-ethereum/ethclient"
)

// Latest base fee and head number.
func latestBaseFee(ctx context.Context, ec *ethclient.Client) (*big.Int, *big.Int, error) {
	h, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if h.BaseFee == nil {
		return nil, h.Number, errors.New("no baseFee (pre-1559?)")
	}
	return new(big.Int).Set(h.BaseFee), new(big.Int).Set(h.Number), nil
}

// Next base fee via eth_feeHistory(1, "pending").
func nextBaseFeeViaFeeHistory(ctx context.Context, rpcURL string) (*big.Int, error) {
	type feeHistResp struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			BaseFeePerGas []string `json:"baseFeePerGas"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	body, _ := json.Marshal(rpcReq{Jsonrpc: "2.0", Method: "eth_feeHistory", Params: []any{"0x1", "pending", []int{50}}, ID: 1})
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out feeHistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, errors.New(out.Error.Message)
	}
	if len(out.Result.BaseFeePerGas) < 2 {
		return nil, errors.New("feeHistory: short baseFee array")
	}
	bf, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result.BaseFeePerGas[len(out.Result.BaseFeePerGas)-1], "0x"), 16)
	if !ok {
		return nil, errors.New("feeHistory: parse baseFee")
	}
	return bf, nil
}

// RewardStats aggregates min/avg/max for given percentiles.
type RewardStats struct {
	Min *big.Int
	Avg *big.Int
	Max *big.Int
}

// FeeHistoryStats returns min/avg/max over last N blocks for given percentiles.
func FeeHistoryStats(ctx context.Context, rpcURL string, blocks int, percentiles []int) (map[int]RewardStats, error) {
	if blocks <= 0 {
		blocks = 100
	}
	if len(percentiles) == 0 {
		percentiles = []int{50, 95, 99}
	}

	type feeHistResp struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Reward [][]string `json:"reward"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}

	body, _ := json.Marshal(rpcReq{
		Jsonrpc: "2.0", Method: "eth_feeHistory",
		Params: []any{fmt.Sprintf("0x%x", blocks), "pending", percentiles}, ID: 1,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out feeHistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, errors.New(out.Error.Message)
	}
	if len(out.Result.Reward) == 0 {
		return nil, errors.New("feeHistory: empty reward")
	}

	res := make(map[int]RewardStats, len(percentiles))
	cols := len(out.Result.Reward[0])
	for i, p := range percentiles {
		_ = i
		res[p] = RewardStats{Min: nil, Avg: big.NewInt(0), Max: big.NewInt(0)}
		if i >= cols {
			break
		}
	}
	for _, row := range out.Result.Reward {
		for j := 0; j < len(percentiles) && j < len(row); j++ {
			p := percentiles[j]
			v, ok := new(big.Int).SetString(strings.TrimPrefix(row[j], "0x"), 16)
			if !ok {
				continue
			}
			st := res[p]
			if st.Min == nil || v.Cmp(st.Min) < 0 {
				st.Min = new(big.Int).Set(v)
			}
			if v.Cmp(st.Max) > 0 {
				st.Max = new(big.Int).Set(v)
			}
			st.Avg.Add(st.Avg, v)
			res[p] = st
		}
	}
	for p, st := range res {
		st.Avg = st.Avg.Div(st.Avg, big.NewInt(int64(blocks)))
		if st.Min == nil {
			st.Min = big.NewInt(0)
		}
		res[p] = st
	}
	return res, nil
}

// TipFromFeeHistory returns MAX reward[percentile] over last N blocks.
func TipFromFeeHistory(ctx context.Context, rpcURL string, blocks int, percentile int) (*big.Int, error) {
	if blocks <= 0 {
		blocks = 100
	}
	if percentile <= 0 || percentile > 99 {
		percentile = 99
	}
	type feeHistResp struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			Reward [][]string `json:"reward"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	body, _ := json.Marshal(rpcReq{
		Jsonrpc: "2.0", Method: "eth_feeHistory",
		Params: []any{fmt.Sprintf("0x%x", blocks), "pending", []int{percentile}}, ID: 1,
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var out feeHistResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, errors.New(out.Error.Message)
	}

	max := big.NewInt(0)
	for _, row := range out.Result.Reward {
		if len(row) == 0 {
			continue
		}
		v, ok := new(big.Int).SetString(strings.TrimPrefix(row[0], "0x"), 16)
		if !ok {
			continue
		}
		if v.Cmp(max) > 0 {
			max = v
		}
	}
	if max.Sign() == 0 {
		return nil, errors.New("feeHistory: empty reward")
	}
	return max, nil
}

// Fallback priority fee via eth_maxPriorityFeePerGas.
func suggestPriorityViaRPC(ctx context.Context, rpcURL string) *big.Int {
	type respT struct {
		Jsonrpc string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  string `json:"result"`
		Error   *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	body, _ := json.Marshal(rpcReq{Jsonrpc: "2.0", Method: "eth_maxPriorityFeePerGas", Params: []any{}, ID: 1})
	req, _ := http.NewRequestWithContext(ctx, "POST", rpcURL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out respT
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	if out.Error != nil || out.Result == "" {
		return nil
	}
	val, ok := new(big.Int).SetString(strings.TrimPrefix(out.Result, "0x"), 16)
	if !ok {
		return nil
	}
	return val
}
