package bundlecore

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

const minimalNonEmptyCode = "0x00"

func PreflightTransfer7702(
	ctx context.Context,
	ec *ethclient.Client,
	rc *rpc.Client,
	token common.Address,
	fromEOA common.Address,
	recipient common.Address,
	amount *big.Int,
) (bool, string, error) {
	if amount == nil || amount.Sign() == 0 {
		return false, "no balance", nil
	}
	if rc == nil {
		return PreflightTransfer(ctx, ec, token, fromEOA, recipient, amount)
	}

	okDirect, _, err := simulateTransferWithOverride(ctx, rc, token, fromEOA, recipient, amount)
	if err != nil {
		return false, "preflight error: " + err.Error(), nil
	}
	if okDirect {
		return true, "route=direct", nil
	}

	weth := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	pair := getV2Pair(ctx, ec, token, weth)
	if pair == (common.Address{}) {
		return false, "no v2 pair for router path", nil
	}
	okToPair, _, err2 := simulateTransferWithOverride(ctx, rc, token, fromEOA, pair, amount)
	if err2 != nil {
		return false, "preflight error: " + err2.Error(), nil
	}
	if okToPair {
		return true, "route=router", nil
	}
	return false, "blocked in 7702 context", nil
}

func simulateTransferWithOverride(
	ctx context.Context,
	rc *rpc.Client,
	token, fromEOA, to common.Address,
	amount *big.Int,
) (bool, string, error) {
	data := make([]byte, 0, 4+32+32)
	data = append(data, 0xa9, 0x05, 0x9c, 0xbb)
	data = append(data, common.LeftPadBytes(to.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)

	callObj := map[string]interface{}{
		"from": fromEOA,
		"to":   token,
		"data": "0x" + hex.EncodeToString(data),
	}
	override := map[string]map[string]string{
		strings.ToLower(fromEOA.Hex()): {"code": minimalNonEmptyCode},
	}

	var res string
	if err := rc.CallContext(ctx, &res, "eth_call", callObj, "latest", override); err != nil {
		return false, "", nil
	}
	if res == "" || res == "0x" {
		return true, "", nil
	}
	b, err := hex.DecodeString(res[2:])
	if err != nil {
		return false, "", errors.New("bad eth_call result")
	}
	if len(b) >= 32 && b[len(b)-1] == 1 {
		return true, "", nil
	}
	return false, "", nil
}

func getV2Pair(ctx context.Context, ec *ethclient.Client, token, weth common.Address) common.Address {
	selector := []byte{0xe6, 0xa4, 0x39, 0x05}
	data := make([]byte, 0, 4+32+32)
	data = append(data, selector...)
	data = append(data, common.LeftPadBytes(token.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(weth.Bytes(), 32)...)

	factory := common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f")
	out, err := ec.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: data}, nil)
	if err != nil || len(out) < 32 {
		return common.Address{}
	}
	return common.BytesToAddress(out[12:32])
}
