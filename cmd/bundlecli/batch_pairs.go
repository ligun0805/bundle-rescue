package main

// Batch processing of pairs (token,privateKey,from) via EIP-7702 only.
// Reads CSV from PAIRS_CSV env var and executes each pair non-interactively.
// Logs per-pair decisions and relay responses into logs/bundlecli_batch_<timestamp>.log.

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"encoding/csv"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"

	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
	eip7702 "github.com/ligun0805/bundle-rescue/internal/eip7702"
)

// delegateABI keeps only the functions we actually use to avoid bloat.
const delegateABI = `[
  {"type":"function","stateMutability":"nonpayable","name":"sweepToken",
   "inputs":[{"name":"token","type":"address"},{"name":"recipient","type":"address"}],"outputs":[]},
  {"type":"function","stateMutability":"nonpayable","name":"sellToETH_V2",
   "inputs":[
     {"name":"tokenIn","type":"address"},
     {"name":"amountIn","type":"uint256"},
     {"name":"amountOutMinETH","type":"uint256"},
     {"name":"recipient","type":"address"},
     {"name":"deadline","type":"uint256"}
   ],"outputs":[]}
]`

// runBatchPairsFromCSV processes ok.csv-like files in a non-interactive way.
// Each row: token,privateKey,from[,reason]
func runBatchPairsFromCSV(
	ctx context.Context,
	ec *ethclient.Client,
	cfg EnvConfig,
	chainID *big.Int,
	sponsorAddr common.Address,
	csvPath string,
) error {
	csvPath = strings.TrimSpace(csvPath)
	if csvPath == "" {
		return errors.New("empty CSV path")
	}
	f, err := os.Open(csvPath)
	if err != nil {
		return fmt.Errorf("open CSV: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(bufio.NewReader(f))
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return fmt.Errorf("parse CSV: %w", err)
	}
	if len(rows) == 0 {
		return errors.New("CSV is empty")
	}

	// Prepare logging
	_ = os.MkdirAll("logs", 0o755)
	logPath := filepath.Join("logs", fmt.Sprintf("bundlecli_batch_%s.log", time.Now().Format("20060102_150405")))
	lf, err := os.Create(logPath)
	if err != nil {
	return fmt.Errorf("create log: %w", err)
	}
	defer lf.Close()
	logw := bufio.NewWriter(lf)
	defer logw.Flush()
	fmt.Fprintf(logw, "# batch started at %s\n", time.Now().Format(time.RFC3339))

	// Separate rpc client for PreflightTransfer7702
	httpClient := &http.Client{ Timeout: 30 * time.Second, Transport: &http.Transport{ MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second } }
	rc, err := rpc.DialHTTPWithClient(cfg.RPC, httpClient)
	if err != nil {
		return err
	}
	defer rc.Close()

	parsedABI, err := abi.JSON(strings.NewReader(delegateABI))
	if err != nil {
		return fmt.Errorf("delegate ABI parse: %w", err)
	}
	if strings.TrimSpace(cfg.DelegateHex) == "" || !common.IsHexAddress(cfg.DelegateHex) {
		return fmt.Errorf("bad DELEGATE_ADDRESS in .env")
	}
	delegateAddr := common.HexToAddress(cfg.DelegateHex)
	relays := splitCSV(cfg.RelaysCSV)

	// Skip header if present
	start := 0
	if len(rows) > 0 {
		h := strings.ToLower(strings.TrimSpace(rows[0][0]))
		if strings.Contains(h, "token") || strings.Contains(h, "address") {
			start = 1
		}
	}

	for i := start; i < len(rows); i++ {
		row := rows[i]
		if len(row) < 3 { continue }
		tokenHex := strings.TrimSpace(row[0])
		fromPKHex := strings.TrimSpace(row[1])
		fromHex := strings.TrimSpace(row[2])

		if !common.IsHexAddress(tokenHex) || !common.IsHexAddress(fromHex) || len(fromPKHex) < 16 {
			fmt.Fprintf(logw, "[row %d] skip: malformed values\n", i+1)
			continue
		}
		token := common.HexToAddress(tokenHex)
		from := common.HexToAddress(fromHex)

		// Check privkey -> from
		fromPK, err := crypto.HexToECDSA(strings.TrimPrefix(fromPKHex, "0x"))
		if err != nil || crypto.PubkeyToAddress(fromPK.PublicKey) != from {
			fmt.Fprintf(logw, "[row %d] error: bad private key for %s\n", i+1, from.Hex())
			continue
		}

		// Balance
		bal, err := fetchTokenBalance(ctx, ec, token, from)
		if err != nil {
			fmt.Fprintf(logw, "[row %d] %s balanceOf error: %v\n", i+1, token.Hex(), err)
			continue
		}
		if bal == nil || bal.Sign() == 0 {
			fmt.Fprintf(logw, "[row %d] %s balance=0 - skip\n", i+1, token.Hex())
			continue
		}

		// Decide route
		ok, why, _ := core.PreflightTransfer7702(ctx, ec, rc, token, from, sponsorAddr, bal)
		route := "sell-v2"
		if ok { route = "transfer" }
		fmt.Fprintf(logw, "[row %d] plan: %s (%s)\n", i+1, route, why)

		var calldata []byte
		switch route {
		case "transfer":
			calldata, err = parsedABI.Pack("sweepToken", token, sponsorAddr)
		default:
			amountOutMin := big.NewInt(0) // accept any, private tx mitigates MEV
			deadline := big.NewInt(time.Now().Add(20*time.Minute).Unix())
			calldata, err = parsedABI.Pack("sellToETH_V2", token, bal, amountOutMin, sponsorAddr, deadline)
		}
		if err != nil {
			fmt.Fprintf(logw, "[row %d] abi pack failed: %v\n", i+1, err)
			continue
		}

		// 7702 authorizations
		authNonce, _ := ec.NonceAt(ctx, from, nil)
		auths, err := eip7702.BuildAuthorizations(chainID, from, delegateAddr, authNonce, 1, fromPK)
		if err != nil {
			fmt.Fprintf(logw, "[row %d] build auth failed: %v\n", i+1, err)
			continue
		}

		// Sponsor fees & nonce
		sponsorNonce, err := eip7702.EstimateSponsorNonce(ctx, ec, sponsorAddr)
		if err != nil {
			fmt.Fprintf(logw, "[row %d] sponsor nonce error: %v\n", i+1, err)
			continue
		}
		var tipWei *big.Int
		if cfg.TipGwei > 0 {
			tipWei = new(big.Int).Mul(big.NewInt(cfg.TipGwei), big.NewInt(1_000_000_000))
		}
		tip, cap, err := eip7702.PrepareFees(ctx, ec, tipWei)
		if err != nil {
			fmt.Fprintf(logw, "[row %d] fee prep error: %v\n", i+1, err)
			continue
		}
		gasLimit := uint64(500_000) // transfer~90k, v2~220-300k => 500k запас

		// Build & sign setcode tx
		unsigned, err := eip7702.BuildSetCodeTx(eip7702.BuildParams{
			ChainID:           chainID,
			SponsorNonce:      sponsorNonce,
			GasLimit:          gasLimit,
			MaxPriorityFeeWei: tip,
			MaxFeeWei:         cap,
			AuthorityEOA:      from,
			DelegateContract:  delegateAddr,
			Calldata:          calldata,
			Authorizations:    auths,
		})
		if err != nil {
			fmt.Fprintf(logw, "[row %d] build setcode tx failed: %v\n", i+1, err)
			continue
		}
		safePK, err := crypto.HexToECDSA(strings.TrimPrefix(cfg.SafePK, "0x"))
		if err != nil {
			fmt.Fprintf(logw, "[row %d] safe key parse failed: %v\n", i+1, err)
			continue
		}
		signed, err := eip7702.SignSetCodeTx(chainID, safePK, unsigned)
		if err != nil {
			fmt.Fprintf(logw, "[row %d] sign failed: %v\n", i+1, err)
			continue
		}

		// Send private
		raw, err := signed.MarshalBinary()
		if err != nil {
			fmt.Fprintf(logw, "[row %d] rlp failed: %v\n", i+1, err)
			continue
		}
		var authSigner *ecdsa.PrivateKey
		if strings.TrimSpace(cfg.AuthPK) != "" {
			if k, e := crypto.HexToECDSA(strings.TrimPrefix(cfg.AuthPK, "0x")); e == nil {
				authSigner = k
			}
		}
		results := eip7702.SendPrivate(ctx, "0x"+common.Bytes2Hex(raw), relays, nil, authSigner)
		accepted := false
		for _, rr := range results {
			fmt.Fprintf(logw, "[row %d] relay=%s status=%d accepted=%v err=%s\n", i+1, rr.RelayURL, rr.Status, rr.Accepted, rr.Error)
			if rr.Accepted { accepted = true }
		}
		if !accepted {
			fmt.Fprintf(logw, "[row %d] no relay accepted\n", i+1)
		}
	}

	fmt.Fprintf(logw, "# batch finished at %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("Batch log written to %s\n", logPath)
	return nil
}