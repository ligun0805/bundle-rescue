package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

type appConfig struct {
	rpcURL         string
	safePrivateHex string
	inputPath      string
	outOKPath      string
	outBadPath     string
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
	flag.Parse()

	if cfg.inputPath == "" {
		fmt.Fprintln(os.Stderr, "missing -input (or BATCH_INPUT) file with rows: token,privateKey")
		os.Exit(2)
	}
	if cfg.rpcURL == "" {
		fmt.Fprintln(os.Stderr, "missing RPC: set -rpc or RPC_URL")
		os.Exit(2)
	}
	if strings.TrimSpace(cfg.safePrivateHex) == "" {
		fmt.Fprintln(os.Stderr, "missing SAFE private key: set -safe-pk or SAFE_PRIVATE_KEY")
		os.Exit(2)
	}
	return cfg
}

type pairRow struct {
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
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
	fmt.Println("Done. OK =>", cfg.outOKPath, " BAD =>", cfg.outBadPath)
}

func run(cfg appConfig) error {
	ec, err := ethclient.Dial(cfg.rpcURL)
	if err != nil {
		return fmt.Errorf("dial rpc: %w", err)
	}
	defer ec.Close()

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

	return processBytes(ec, safeAddress, data, okW, badW)
}

func processBytes(ec *ethclient.Client, safeAddr common.Address, data []byte, okW, badW *csv.Writer) error {
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
			continue
		}
		tokenHex, privateHex := strings.TrimSpace(row[0]), strings.TrimSpace(row[1])
		result := processOne(ec, safeAddr, tokenHex, privateHex)
		if result.reason != "" {
			_ = badW.Write([]string{tokenHex, privateHex, result.fromAddress.Hex(), result.reason})
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

func processOne(ec *ethclient.Client, safeAddr common.Address, tokenHex, privateHex string) pairRow {
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

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	dec, derr := fetchTokenDecimals(ctx, ec, out.tokenAddress)
	if derr != nil {
		out.reason = "decimals() call failed"
		return out
	}
	out.tokenDecimals = dec
	if sym, e := fetchTokenSymbol(ctx, ec, out.tokenAddress); e == nil {
		out.tokenSymbol = sym
	}

	bal, berr := fetchTokenBalance(ctx, ec, out.tokenAddress, out.fromAddress)
	if berr != nil {
		out.reason = "balanceOf() call failed"
		return out
	}
	out.balanceWei = bal
	if bal == nil || bal.Sign() <= 0 {
		out.reason = "zero token balance"
		return out
	}

	if reason := checkTransferViability(ctx, ec, out.tokenAddress, out.fromAddress, safeAddr, bal); reason != "" {
		out.reason = reason
		return out
	}
	return out
}

func checkTransferViability(ctx context.Context, ec *ethclient.Client, token, from, to common.Address, amount *big.Int) string {
	restr, err := core.CheckRestrictions(ctx, ec, token, from, to)
	if err == nil && restr.Blocked() {
		return "blocked: " + restr.Summary()
	}
	ok, why, _ := core.PreflightTransfer(ctx, ec, token, from, to, amount)
	if !ok {
		if why == "" {
			why = "preflight failed"
		}
		return why
	}
	return ""
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
	res, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
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
	res, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
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
	out, err := ec.CallContract(ctx, ethereum.CallMsg{To: &token, Data: data}, nil)
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
