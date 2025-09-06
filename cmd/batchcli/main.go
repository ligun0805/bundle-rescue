package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
  "github.com/ethereum/go-ethereum/rpc"

	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// RPC client used for eth_call stateOverrides in 7702 preflight.
var gStateOverrideRPC *rpc.Client
// newEthClientWithTimeout dials RPC with keep-alives and sane timeouts.
func newEthClientWithTimeout(rpcURL string) (*ethclient.Client, error) {
	transport := &http.Transport{
		MaxIdleConns:       100,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: false,
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	rpcClient, err := rpc.DialHTTPWithClient(rpcURL, httpClient)
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(rpcClient), nil
}


type appConfig struct {
	rpcURL         string
	safePrivateHex string
	inputPath      string
	outOKPath      string
	outBadPath     string
	rpcDelay       time.Duration
	rowDelay       time.Duration
	pairTimeout    time.Duration
	preflightAttempts int
	preflightAttemptTimeout time.Duration
  showPairLogs   bool
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func mustLoadConfig() appConfig {
	var cfg appConfig
	flag.StringVar(&cfg.inputPath, "input", getenv("BATCH_INPUT", ""), "Path to CSV with pairs: token,privateKey")
	flag.StringVar(&cfg.outOKPath, "out-ok", getenv("BATCH_OUT_OK", "ok_pairs.csv"), "Output CSV for promising pairs")
	flag.StringVar(&cfg.outBadPath, "out-bad", getenv("BATCH_OUT_BAD", "bad_pairs.csv"), "Output CSV for rejected pairs")
	flag.StringVar(&cfg.rpcURL, "rpc", getenv("RPC_URL", ""), "RPC endpoint URL")
	flag.StringVar(&cfg.safePrivateHex, "safe-pk", getenv("SAFE_PRIVATE_KEY", ""), "SAFE private key (hex) to receive tokens")
  flag.BoolVar(&cfg.showPairLogs, "pair-logs", false, "Print per-pair diagnostic logs to stdout")

	// Delay between RPC calls (helps avoid 429 / -32005). Default: 200 ms.
	delayEnv := getenv("BATCH_RPC_DELAY_MS", "200")
	delayMS := 200
	if v, err := strconv.Atoi(strings.TrimSpace(delayEnv)); err == nil && v >= 0 {
		delayMS = v
	}
	flag.IntVar(&delayMS, "rpc-delay-ms", delayMS, "Delay between RPC calls in milliseconds")

	// Explicit per-pair delay to avoid provider bursts across different wallets.
	rowDelayEnv := getenv("BATCH_ROW_DELAY_MS", "300")
	rowDelayMS := 300
	if v, err := strconv.Atoi(strings.TrimSpace(rowDelayEnv)); err == nil && v >= 0 {
		rowDelayMS = v
	}
	
	// Per-pair total timeout (caps all operations for one pair). Default: 15000 ms.
	pairTimeoutEnv := getenv("BATCH_PAIR_TIMEOUT_MS", "15000")
	pairTimeoutMS := 15000
	if v, err := strconv.Atoi(strings.TrimSpace(pairTimeoutEnv)); err == nil && v > 0 {
		pairTimeoutMS = v
	}
	flag.IntVar(&pairTimeoutMS, "pair-timeout-ms", pairTimeoutMS, "Max time budget for processing a single pair (ms)")

	// Preflight retry knobs (short attempt timeouts x attempts). Defaults: 3 x 4000 ms.
	pfAttemptsEnv := getenv("BATCH_PREFLIGHT_ATTEMPTS", "3")
	pfAttempts := 3
	if v, err := strconv.Atoi(strings.TrimSpace(pfAttemptsEnv)); err == nil && v > 0 {
		pfAttempts = v
	}
	flag.IntVar(&pfAttempts, "preflight-attempts", pfAttempts, "Number of preflight attempts on transient RPC errors")

	pfAttemptTOEnv := getenv("BATCH_PREFLIGHT_ATTEMPT_TIMEOUT_MS", "4000")
	pfAttemptTOMS := 4000
	if v, err := strconv.Atoi(strings.TrimSpace(pfAttemptTOEnv)); err == nil && v > 0 {
		pfAttemptTOMS = v
	}
	flag.IntVar(&pfAttemptTOMS, "preflight-attempt-timeout-ms", pfAttemptTOMS, "Timeout per preflight attempt (ms)")


	flag.Parse()

	if cfg.inputPath == "" {
		fmt.Fprintln(os.Stderr, "missing -input (or BATCH_INPUT) file with rows: token,privateKey")
		askExitAndQuit(2)
	}
	if cfg.rpcURL == "" {
		fmt.Fprintln(os.Stderr, "missing RPC: set -rpc or RPC_URL")
		askExitAndQuit(2)
	}
	if strings.TrimSpace(cfg.safePrivateHex) == "" {
		fmt.Fprintln(os.Stderr, "missing SAFE private key: set -safe-pk or SAFE_PRIVATE_KEY")
		askExitAndQuit(2)
	}
	cfg.rpcDelay = time.Duration(delayMS) * time.Millisecond
	cfg.rowDelay = time.Duration(rowDelayMS) * time.Millisecond
	cfg.pairTimeout = time.Duration(pairTimeoutMS) * time.Millisecond
	cfg.preflightAttempts = pfAttempts
	cfg.preflightAttemptTimeout = time.Duration(pfAttemptTOMS) * time.Millisecond
	return cfg
}

type pairRow struct {
	warn          string
	tokenHex      string
	privateHex    string
	fromAddress   common.Address
	tokenAddress  common.Address
	tokenSymbol   string
	tokenDecimals int
	balanceWei    *big.Int
	reason        string
}

func main() {
	cfg := mustLoadConfig()
	setRPCDelay(cfg.rpcDelay)
	setPairTimeout(cfg.pairTimeout)
	setPreflightRetryConfig(cfg.preflightAttempts, cfg.preflightAttemptTimeout)
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		askExitAndQuit(1)
	}
	fmt.Println("Done. OK =>", cfg.outOKPath, " BAD =>", cfg.outBadPath)
}

// askExitAndQuit prints a prompt and waits for Enter before exiting.
// This avoids instant window close on double-click runs (Windows).
func askExitAndQuit(code int) {
	fmt.Fprint(os.Stderr, "Exit now? Press Enter to close...")
	_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
	os.Exit(code)
}

func run(cfg appConfig) error {
	ec, err := newEthClientWithTimeout(cfg.rpcURL)
	if err != nil {
		return fmt.Errorf("dial rpc: %w", err)
	}
	defer ec.Close()

	// Best-effort RPC client for stateOverrides (7702 preflight).
	if rc, e := rpc.DialContext(context.Background(), cfg.rpcURL); e == nil {
		gStateOverrideRPC = rc
	}

	safePriv, err := hexToECDSA(cfg.safePrivateHex)
	if err != nil {
		return fmt.Errorf("SAFE key: %w", err)
	}
	safeAddress := gethcrypto.PubkeyToAddress(safePriv.PublicKey)

	data, err := os.ReadFile(cfg.inputPath)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}

	okW, badW, err := openOutputs(cfg.outOKPath, cfg.outBadPath)
	if err != nil {
		return fmt.Errorf("open outputs: %w", err)
	}
	defer okW.Flush()
	defer badW.Flush()

	// headers
	_ = okW.Write([]string{"token", "privateKey", "from", "symbol", "decimals", "balanceTokens"})
	_ = badW.Write([]string{"token", "privateKey", "from", "reason"})

	return processBytes(ec, safeAddress, data, okW, badW, cfg.rowDelay, cfg.showPairLogs)
}

func processBytes(ec *ethclient.Client, safeAddr common.Address, data []byte, okW, badW *csv.Writer, rowDelay time.Duration, showPairLogs bool) error {
	// Delimiter auto-detect on the first non-empty line
	delim := detectDelimiter(data)
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	reader.Comma = delim

	lineNo := 0
	for {
		row, e := reader.Read()
		if e != nil {
			if errors.Is(e, io.EOF) {
				break
			}
			return e
		}
		lineNo++
		if skipRow(row, lineNo) {
			continue
		}
		if len(row) < 2 {
			_ = badW.Write([]string{strings.Join(row, string([]rune{delim})), "", "", "not enough columns, expected token,privateKey"})
			// per-pair delay even on malformed row
			if rowDelay > 0 {
				time.Sleep(rowDelay)
			}
			continue
		}

		tokenHex, privateHex := strings.TrimSpace(row[0]), strings.TrimSpace(row[1])
		result := processOne(ec, safeAddr, tokenHex, privateHex, showPairLogs, lineNo)

		if result.reason != "" {
			// Attach collected "soft" warnings (decimals/symbol/balance) to reason for context.
			badReason := result.reason
			if strings.TrimSpace(result.warn) != "" {
				badReason = badReason + " | " + result.warn
			}
			_ = badW.Write([]string{tokenHex, privateHex, result.fromAddress.Hex(), badReason})
      pairLogf(showPairLogs, lineNo, tokenHex, result.fromAddress, "RESULT: BAD — %s", badReason)

			// per-pair delay before moving to next pair
			if rowDelay > 0 {
				time.Sleep(rowDelay)
			}
			continue
		}

		_ = okW.Write([]string{
			tokenHex,
			privateHex,
			result.fromAddress.Hex(),
			result.tokenSymbol,
			fmt.Sprintf("%d", result.tokenDecimals),
			formatTokensFromWei(result.balanceWei, result.tokenDecimals),
		})
    pairLogf(showPairLogs, lineNo, tokenHex, result.fromAddress, "RESULT: OK — symbol=%s decimals=%d balance=%s",
      result.tokenSymbol, result.tokenDecimals, formatTokensFromWei(result.balanceWei, result.tokenDecimals))

		// per-pair delay before next iteration
		if rowDelay > 0 {
			time.Sleep(rowDelay)
		}
	}

	return nil
}

func detectDelimiter(data []byte) rune {
	lines := strings.Split(string(data), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if strings.Contains(l, ";") && !strings.Contains(l, ",") {
			return ';'
		}
		break
	}
	return ','
}

func skipRow(row []string, lineNo int) bool {
	if len(row) == 0 {
		return true
	}
	if len(row) == 1 && strings.TrimSpace(row[0]) == "" {
		return true
	}
	if lineNo == 1 {
		head := strings.ToLower(strings.Join(row, ","))
		if strings.Contains(head, "token") && strings.Contains(head, "priv") {
			return true
		}
	}
	return false
}

func openOutputs(okPath, badPath string) (*csv.Writer, *csv.Writer, error) {
	okF, err := os.Create(okPath)
	if err != nil {
		return nil, nil, err
	}
	badF, err := os.Create(badPath)
	if err != nil {
		_ = okF.Close()
		return nil, nil, err
	}
	return csv.NewWriter(okF), csv.NewWriter(badF), nil
}

func processOne(ec *ethclient.Client, safeAddr common.Address, tokenHex, privateHex string, showPairLogs bool, lineNo int) pairRow {
	out := pairRow{tokenHex: tokenHex, privateHex: privateHex}
	if !common.IsHexAddress(tokenHex) {
		out.reason = "invalid token address"
		return out
	}
	out.tokenAddress = common.HexToAddress(tokenHex)

	prv, err := hexToECDSA(privateHex)
	if err != nil {
		out.reason = "invalid private key"
		return out
	}
	out.fromAddress = gethcrypto.PubkeyToAddress(prv.PublicKey)
  pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "START")

	ctx, cancel := context.WithTimeout(context.Background(), getPairTimeout())
	defer cancel()
	var warnParts []string

	// decimals(): on failure assume 18 (do not reject)
	dec, derr := fetchTokenDecimals(ctx, ec, out.tokenAddress)
	if derr != nil {
		// Keep going with 18, but remember why decimals failed for a better reason text later.
		warnParts = append(warnParts, "decimals() failed: "+classifyCallError(ctx, ec, out.tokenAddress, derr))
		out.tokenDecimals = 18
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "decimals(): FAIL — %s", classifyCallError(ctx, ec, out.tokenAddress, derr))
	} else {
		out.tokenDecimals = dec
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "decimals(): %d", dec)
	}

	// symbol(): best-effort
	if sym, e := fetchTokenSymbol(ctx, ec, out.tokenAddress); e == nil && sym != "" {
		out.tokenSymbol = sym
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "symbol(): %s", sym)
	} else if e != nil {
		warnParts = append(warnParts, "symbol() failed: "+classifyCallError(ctx, ec, out.tokenAddress, e))
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "symbol(): FAIL — %s", classifyCallError(ctx, ec, out.tokenAddress, e))
	}

	// balanceOf(): if failed — fallback to preflight(1)
	bal, berr := fetchTokenBalance(ctx, ec, out.tokenAddress, out.fromAddress)
	if berr != nil {
		warnParts = append(warnParts, "balanceOf() failed: "+classifyCallError(ctx, ec, out.tokenAddress, berr))
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "balanceOf(): FAIL — %s", classifyCallError(ctx, ec, out.tokenAddress, berr))
	}
	out.balanceWei = bal

	// If balance successfully fetched and equals zero — stop further checks and mark BAD.
	// This avoids running restrictions/preflight for addresses that simply hold no tokens.
	if berr == nil && (bal == nil || bal.Sign() <= 0) {
		out.reason = "no token balance"
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "balanceOf(): 0 — stop, no preflight")
		if len(warnParts) > 0 {
			out.warn = strings.Join(warnParts, "; ")
		}
		return out
	}

	// If balance call failed (transport/other RPC error) — keep old fallback:
	// do a tiny preflight (1 wei) to see if the route is theoretically transferable.
	if berr != nil {
		pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): fallback 1 wei (balance unknown)")
    if reason := checkTransferViability(ctx, ec, out.tokenAddress, out.fromAddress, safeAddr, big.NewInt(1)); reason != "" {
			out.reason = reason
      pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): FAIL — %s", reason)
			if len(warnParts) > 0 {
				out.warn = strings.Join(warnParts, "; ")
			}
			return out
		}
		pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): OK")
    // transferable but balance is unknown — keep OK (balance will print as 0).
		return out
	}

	// Non-zero balance: regular strict preflight
	pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): start, amountWei=%s", bal.String())
  if reason := checkTransferViability(ctx, ec, out.tokenAddress, out.fromAddress, safeAddr, bal); reason != "" {
		out.reason = reason
    pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): FAIL — %s", reason)
		if len(warnParts) > 0 {
			out.warn = strings.Join(warnParts, "; ")
		}
		return out
	}
  pairLogf(showPairLogs, lineNo, tokenHex, out.fromAddress, "preflight(): OK")

	if len(warnParts) > 0 {
		out.warn = strings.Join(warnParts, "; ")
	}
	return out
}

func checkTransferViability(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int) string {
	restr, err := core.CheckRestrictions(ctx, ec, token, from, to)
	if err == nil && restr.Blocked() {
		return "blocked: " + restr.Summary()
	}
	// Preflight with short attempt timeouts and limited retries against transient RPC failures.
	if reason := preflightWithRetry7702(ctx, ec, token, from, to, amount, getPreflightAttempts(), getPreflightAttemptTimeout()); reason != "" {
		// Optional-return fallback (SafeERC20 semantics):
		// If the failure looks like ABI/empty-output/boolean-decode issue, try raw eth_call and treat empty return as success.
		if isOptionalReturnCandidate(reason) {
			ok, detail := optionalReturnTransferCall(ctx, ec, token, from, to, amount)
			if ok {
				return ""
			}
			if strings.TrimSpace(detail) != "" {
				return detail
			}
		}
		return reason
	}
	return ""
}

// 7702-aware preflight with retries: simulates transfer() with stateOverrides (EOA has code).
// Falls back gracefully if gStateOverrideRPC is nil (core.PreflightTransfer7702 делает это сам).
func preflightWithRetry7702(
	ctx context.Context,
	ec *ethclient.Client,
	token, from, to common.Address,
	amount *big.Int,
	attempts int,
	attemptTimeout time.Duration,
) string {
	if attempts < 1 {
		attempts = 1
	}
	backoff := 300 * time.Millisecond
	for i := 1; i <= attempts; i++ {
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		ok, why, err := core.PreflightTransfer7702(attemptCtx, ec, gStateOverrideRPC, token, from, to, amount)
		cancel()

		if err != nil {
			// Retry only on transient transport-level problems; contract-level results should not retry.
			if isTransientNetworkError(err) && i < attempts {
				time.Sleep(backoff)
				if backoff < 2*time.Second {
					backoff *= 2
				}
				continue
			}
			return fmt.Sprintf("%s: %v", classifyRPCError(err), err)
		}
		if !ok {
			if strings.TrimSpace(why) == "" {
				return "not transferable: preflight 7702 failed"
			}
			return why // e.g., "blocked in 7702 context" / "no v2 pair ..." / "route=router"
		}
		return "" // success
	}
	return fmt.Sprintf("rpc_timeout: preflight 7702 attempts exhausted (attempts=%d)", attempts)
}

// preflightWithRetry runs preflight with multiple short attempts to survive transient RPC issues.
// Returns empty string if transferable, otherwise a descriptive reason.
func preflightWithRetry(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int, attempts int, attemptTimeout time.Duration) string {
	if attempts < 1 {
		attempts = 1
	}
	backoff := 300 * time.Millisecond
	for i := 1; i <= attempts; i++ {
		// Bound each attempt with a sub-timeout; do not exceed the parent context.
		attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
		ok, why, err := core.PreflightTransfer(attemptCtx, ec, token, from, to, amount)
		cancel()

		if err != nil {
			// Retry only on transient transport-level problems; contract-level errors should bubble up.
			if isTransientNetworkError(err) && i < attempts {
				time.Sleep(backoff)
				if backoff < 2*time.Second {
					backoff *= 2
				}
				continue
			}
			cls := classifyRPCError(err)
			return fmt.Sprintf("%s: %v", cls, err)
		}
		if !ok {
			if strings.TrimSpace(why) == "" {
				return "not transferable: preflight failed"
			}
			return "not transferable: " + why
		}
		// Success
		return ""
	}
	return fmt.Sprintf("rpc_timeout: preflight attempts exhausted (attempts=%d)", attempts)
}

// isTransientNetworkError detects short-lived provider/transport failures worth retrying.
func isTransientNetworkError(err error) bool {
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "context deadline exceeded") { return true }
	if strings.Contains(s, "client.timeout exceeded") { return true }
	if strings.Contains(s, "i/o timeout") { return true }
	if strings.Contains(s, "tls handshake timeout") { return true }
	if strings.Contains(s, "eof") { return true }
	if strings.Contains(s, "connection reset") { return true }
	if strings.Contains(s, "502") || strings.Contains(s, "503") || strings.Contains(s, "504") { return true }
	return false
}


// --- local helpers (copied, minimal, no refactor) ---

func hexToECDSA(s string) (*ecdsa.PrivateKey, error) {
	h := strings.TrimSpace(strings.TrimPrefix(s, "0x"))
	if h == "" {
		return nil, errors.New("empty hex")
	}
	return gethcrypto.HexToECDSA(h)
}

func fetchTokenDecimals(ctx context.Context, ec *ethclient.Client, token common.Address) (int, error) {
	data := common.FromHex("0x313ce567") // decimals()
	throttle()
	res, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
	if err != nil {
		return 0, err
	}
	if len(res) == 0 {
		return 18, nil
	}
	// ABI uint8 encoded as 32 bytes (big-endian)
	if len(res) < 32 {
		return int(new(big.Int).SetBytes(res).Int64()), nil
	}
	return int(new(big.Int).SetBytes(res[len(res)-32:]).Int64()), nil
}

func fetchTokenBalance(ctx context.Context, ec *ethclient.Client, token, owner common.Address) (*big.Int, error) {
	data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(owner.Bytes(), 32)...)
	throttle()
	res, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return big.NewInt(0), nil
	}
	return new(big.Int).SetBytes(res), nil
}

func fetchTokenSymbol(ctx context.Context, ec *ethclient.Client, token common.Address) (string, error) {
	data := common.FromHex("0x95d89b41") // symbol()
	throttle()
	out, err := callContractWithRetry(ctx, ec, ethereum.CallMsg{To: &token, Data: data})
	if err != nil || len(out) == 0 {
		return "", err
	}
	// Try dynamic string: offset @32, length @64, bytes after
	if len(out) >= 64 {
		l := new(big.Int).SetBytes(out[32:64]).Int64()
		if l > 0 && 64+int(l) <= len(out) {
			return string(out[64 : 64+int(l)]), nil
		}
	}
	// Fallback: bytes32 right-padded with zeros
	return strings.TrimRight(string(out), "\x00"), nil
}

// --- RPC concurrency gate (limits parallel eth_call to protect the RPC) ---
var rpcConcurrencyGate chan struct{}

func init() {
	n := 16
	if v := strings.TrimSpace(os.Getenv("BATCH_RPC_MAX_CONCURRENCY")); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 && i <= 256 {
			n = i
		}
	}
	rpcConcurrencyGate = make(chan struct{}, n)
}

// --- tiny RPC throttle/retry (batch-local) ---
var gRPCDelay time.Duration

func setRPCDelay(d time.Duration) { gRPCDelay = d }

// throttle sleeps between RPC calls to avoid rate limiting.
func throttle() {
	if gRPCDelay > 0 {
		time.Sleep(gRPCDelay)
	}
}

// callContractWithRetry wraps eth_call with small exponential backoff.
func callContractWithRetry(ctx context.Context, ec *ethclient.Client, msg ethereum.CallMsg) ([]byte, error) {
	// Concurrency limiter
	rpcConcurrencyGate <- struct{}{}
	defer func() { <-rpcConcurrencyGate }()
  
	const maxAttempts = 3
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ret, err := ec.CallContract(ctx, msg, nil)
		if err == nil {
			return ret, nil
		}
		lastErr = err
		if attempt < maxAttempts {
			time.Sleep(backoff)
			if strings.Contains(err.Error(), "Too Many Requests") || strings.Contains(err.Error(), "-32005") {
				backoff *= 2
			}
		}
	}
	return nil, lastErr
}

// classifyRPCError returns a coarse class for RPC transport errors.
func classifyRPCError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "context deadline exceeded") {
		return "rpc_timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "rpc_timeout"
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") || strings.Contains(s, "eof") {
		return "rpc_unavailable"
	}
	if strings.Contains(s, "too many requests") || strings.Contains(s, "-32005") {
		return "rpc_rate_limited"
	}
	return "rpc_error"
}


// classifyCallError returns a short, user-facing explanation for common eth_call failures.
// It does NOT change control flow; used only to produce richer "reason" strings.
func classifyCallError(ctx context.Context, ec *ethclient.Client, target common.Address, err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	// 1) Provider throttling
	if strings.Contains(s, "Too Many Requests") || strings.Contains(s, "-32005") {
		return "[RATE_LIMIT] provider throttled the request"
	}
	// 2) Execution reverted (try to show revert reason)
	if strings.Contains(s, "execution reverted") {
		if idx := strings.Index(s, ":"); idx >= 0 && idx+1 < len(s) {
			reason := strings.TrimSpace(s[idx+1:])
			if reason != "" {
				return "[REVERT] " + reason
			}
		}
		return "[REVERT] execution reverted"
	}
	// 3) Not a contract (EOA / selfdestructed)
	if code, e := ec.CodeAt(ctx, target, nil); e == nil && len(code) == 0 {
		return "[NOT_CONTRACT] no bytecode at address"
	}
	// 4) ABI / return quirks
	if strings.Contains(s, "unsupported") || strings.Contains(s, "abi") {
		return "[UNSUPPORTED] ABI/return type mismatch"
	}
	// 5) Fallback
	if strings.Contains(strings.ToLower(s), "invalid opcode") || strings.Contains(strings.ToLower(s), "0xfe") {
		// Explicitly label VM invalid to distinguish from "revert".
		return "[INVALID] invalid opcode during execution"
	}
	return "[RPC] " + s
}

// --- global knobs similar to RPC delay (no refactor through signatures) ---
var gPairTimeout time.Duration
var gPreflightAttempts int
var gPreflightAttemptTimeout time.Duration

func setPairTimeout(d time.Duration) { gPairTimeout = d }
func getPairTimeout() time.Duration {
	if gPairTimeout > 0 {
		return gPairTimeout
	}
	return 8 * time.Second
}

func setPreflightRetryConfig(attempts int, attemptTimeout time.Duration) {
	if attempts < 1 { attempts = 1 }
	gPreflightAttempts = attempts
	gPreflightAttemptTimeout = attemptTimeout
}
func getPreflightAttempts() int { if gPreflightAttempts < 1 { return 1 }; return gPreflightAttempts }
func getPreflightAttemptTimeout() time.Duration { if gPreflightAttemptTimeout <= 0 { return 4 * time.Second }; return gPreflightAttemptTimeout }


func formatTokensFromWei(x *big.Int, decimals int) string {
	if x == nil {
		return "0"
	}
	if decimals <= 0 {
		return x.String()
	}
	base := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	var intPart big.Int
	intPart.Quo(x, base)
	var frac big.Int
	frac.Rem(x, base)
	// scale fractional to 6 digits
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(6), nil)
	fracScaled := new(big.Int).Mul(&frac, scale)
	fracScaled.Quo(fracScaled, base)
	fs := fracScaled.String()
	if len(fs) < 6 {
		fs = strings.Repeat("0", 6-len(fs)) + fs
	}
	fs = strings.TrimRight(fs, "0")
	if fs == "" {
		return intPart.String()
	}
	return intPart.String() + "." + fs
}

// ---- Optional-return preflight (SafeERC20 semantics) -------------------------------------------

// isOptionalReturnCandidate decides whether we should try SafeERC20-style fallback:
// heuristics: messages that look like ABI decode/empty output/boolean issues or generic "preflight failed".
func isOptionalReturnCandidate(reason string) bool {
	s := strings.ToLower(reason)
	if strings.Contains(s, "abi") || strings.Contains(s, "unmarshal") || strings.Contains(s, "decode") {
		return true
	}
	if strings.Contains(s, "empty") || strings.Contains(s, "no return") {
		return true
	}
	if strings.Contains(s, "preflight failed") {
		return true
	}
	// do NOT fallback on explicit VM errors (revert/invalid)
	if strings.Contains(s, "revert") || strings.Contains(s, "invalid opcode") {
		return false
	}
	return false
}

// optionalReturnTransferCall performs raw eth_call of transfer(address,uint256) from `from`.
// Rules:
//   - If call errors with revert/invalid → NOT transferable (return false, reason).
//   - If returns empty data → treat as SUCCESS (true, "").
//   - If returns >=32 bytes → decode last 32 bytes as bool; true → SUCCESS, false → NOT transferable.
//   - Any transport-level RPC error → return false with that error text (let caller report it).
func optionalReturnTransferCall(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int) (bool, string) {
	if amount == nil {
		amount = big.NewInt(0)
	}
	data := buildTransferCalldata(to, amount)
	throttle()
	ret, err := ec.CallContract(ctx, ethereum.CallMsg{
		From: from,
		To:   &token,
		// Give enough gas headroom; eth_call doesn't spend it, but some tokens require a minimum cap.
		Gas:  250000,
		Data: data,
	}, nil)
	if err != nil {
		low := strings.ToLower(err.Error())
		if strings.Contains(low, "execution reverted") {
			reason := extractRevertReason(low)
			if reason == "" {
				reason = "execution reverted"
			}
			return false, "not transferable: " + reason
		}
		if strings.Contains(low, "invalid opcode") || strings.Contains(low, "0xfe") {
			return false, "not transferable: invalid opcode: INVALID"
		}
		// transport or other RPC error
		cls := classifyRPCError(err)
		return false, fmt.Sprintf("%s: %v", cls, err)
	}
	// Optional return handling
	if len(ret) == 0 {
		return true, ""
	}
	present, val := decodeOptionalBool(ret)
	if present {
		if val {
			return true, ""
		}
		return false, "not transferable: token returned false"
	}
	// Unknown payload: be conservative — assume success if not zeroed?
	// For safety: treat non-empty, non-decodable as SUCCESS only if it is 32-byte non-zero.
	if len(ret) >= 32 && bytes.Compare(ret[len(ret)-32:], make([]byte, 32)) != 0 {
		return true, ""
	}
	return false, "not transferable: unexpected return payload"
}

// buildTransferCalldata returns ABI-encoded calldata for transfer(address,uint256).
func buildTransferCalldata(to common.Address, amount *big.Int) []byte {
	// methodID of transfer(address,uint256) = 0xa9059cbb
	out := make([]byte, 0, 4+32+32)
	out = append(out, 0xa9, 0x05, 0x9c, 0xbb)
	out = append(out, common.LeftPadBytes(to.Bytes(), 32)...)
	out = append(out, common.LeftPadBytes(amount.Bytes(), 32)...)
	return out
}

// decodeOptionalBool tries to parse the last 32 bytes as bool.
// Returns (present=true, value) if it looks like a canonical ABI-encoded bool; else (present=false, false).
func decodeOptionalBool(ret []byte) (bool, bool) {
	if len(ret) < 32 {
		return false, false
	}
	word := ret[len(ret)-32:]
	// canonical bool is 0x...00 or 0x...01
	allZero := true
	for _, b := range word[:31] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if !allZero {
		return false, false
	}
	v := word[31]
	if v == 0 {
		return true, false
	}
	if v == 1 {
		return true, true
	}
	// non-canonical but non-zero ⇒ treat as true to be permissive
	return true, true
}

// extractRevertReason pulls short revert reason from a lowercase error string "execution reverted: <reason>".
func extractRevertReason(lowerErr string) string {
	const p = "execution reverted"
	i := strings.Index(lowerErr, p)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(lowerErr[i+len(p):])
	rest = strings.TrimPrefix(rest, ":")
	return strings.TrimSpace(rest)
}

// pairLogf prints a single diagnostic line for a pair when enabled.
// The format is: "[pair N] token=<addr> from=<addr> | message"
func pairLogf(enabled bool, lineNo int, tokenHex string, from common.Address, format string, args ...any) {
	if !enabled {
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[pair %d] token=%s from=%s | %s\n", lineNo, tokenHex, from.Hex(), msg)
}