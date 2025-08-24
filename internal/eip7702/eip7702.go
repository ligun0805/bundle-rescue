package eip7702

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	u256 "github.com/holiman/uint256"
)

// ABI of a minimal delegate with `sweepERC20(address[] tokens, address to)` and `sweepETH(address to)`.
// Keep it here to encode calldata without touching your contracts.
const rescueDelegateABI = `[
  {"type":"function","stateMutability":"nonpayable","name":"sweepERC20",
   "inputs":[{"name":"tokens","type":"address[]"},{"name":"to","type":"address"}],"outputs":[]},
  {"type":"function","stateMutability":"nonpayable","name":"sweepETH",
   "inputs":[{"name":"to","type":"address"}],"outputs":[]}
]`

// RelayResult describes one RPC attempt result.
type RelayResult struct {
	RelayURL      string
	Accepted      bool
	ResponseBody  string
	HTTPStatus    int
	RequestMethod string
}

// ExtraHeaders maps relay URL -> {Header:Value}. Useful for BLXR API keys etc.
type ExtraHeaders map[string]map[string]string

// BuildAuthorizations creates N sequential EIP-7702 authorizations [k..k+N-1].
// Each authorization delegates code of `delegateContract` for the `authorityEOA`.
func BuildAuthorizations(chainID *big.Int, authorityEOA common.Address, delegateContract common.Address,
	firstAuthNonce uint64, count int, authorityPrivKey *ecdsa.PrivateKey) ([]types.SetCodeAuthorization, error) {
	if count <= 0 {
		return nil, fmt.Errorf("count must be > 0")
	}
	auths := make([]types.SetCodeAuthorization, 0, count)
	for i := 0; i < count; i++ {
		auth := types.SetCodeAuthorization{
			ChainID: *u256.MustFromBig(chainID),
			Address: delegateContract, // pointer to code to execute
			Nonce:   firstAuthNonce + uint64(i),
			// V,R,S will be filled by SignSetCode
		}
		signed, err := types.SignSetCode(authorityPrivKey, auth)
		if err != nil {
			return nil, fmt.Errorf("SignSetCode failed at index %d: %w", i, err)
		}
		// Optional sanity: recover authority from signature
		if rec, err := signed.Authority(); err != nil || rec != authorityEOA {
			return nil, fmt.Errorf("authorization %d: authority mismatch (got %s, want %s, err=%v)",
				i, rec.Hex(), authorityEOA.Hex(), err)
		}
		auths = append(auths, signed)
	}
	return auths, nil
}

// EncodeCalldataSweepERC20 encodes delegate call:
//   sweepERC20(tokens[], recipient)
func EncodeCalldataSweepERC20(tokens []common.Address, recipient common.Address) ([]byte, error) {
	parsed, err := abi.JSON(bytes.NewReader([]byte(rescueDelegateABI)))
	if err != nil {
		return nil, err
	}
	return parsed.Pack("sweepERC20", tokens, recipient)
}



// BuildSetCodeTx builds an unsigned SetCodeTx (EIP-7702 tx-type 0x04).
// Execution will be performed in the context of authorityEOA, but gas is paid by sponsor (tx.From).
type BuildParams struct {
	ChainID            *big.Int
	SponsorNonce       uint64
	GasLimit           uint64
	MaxPriorityFeeWei  *big.Int
	MaxFeeWei          *big.Int
	AuthorityEOA       common.Address
	DelegateContract   common.Address
	Calldata           []byte
	Authorizations     []types.SetCodeAuthorization // at least one, preferably sequential nonces
	AccessList         types.AccessList             // optional
}

func BuildSetCodeTx(p BuildParams) (*types.Transaction, error) {
	if len(p.Authorizations) == 0 {
		return nil, fmt.Errorf("empty Authorizations")
	}
    // Diagnostic: warn if calldata accidentally contains duplicated 4-byte selector head.
    // This won't break execution (Solidity decoder игнорирует хвост), но поможет заметить сборку calldata дважды.
    if len(p.Calldata) > 4 {
        head := p.Calldata[:4]
        if idx := bytes.Index(p.Calldata[4:], head); idx >= 0 {
            fmt.Println("[warn] duplicated calldata head detected; length=", len(p.Calldata))
        }
    }	
	txdata := &types.SetCodeTx{
		ChainID:    u256.MustFromBig(p.ChainID),
		Nonce:      p.SponsorNonce,
		GasTipCap:  u256.MustFromBig(p.MaxPriorityFeeWei),
		GasFeeCap:  u256.MustFromBig(p.MaxFeeWei),
		Gas:        p.GasLimit,
		To:         p.AuthorityEOA, // Top-level call goes to the EOA; code is delegated to DelegateContract
		Value:      u256.NewInt(0),
		Data:       p.Calldata,     // Calldata for the delegate ABI
		AccessList: p.AccessList,   // usually empty
		AuthList:   p.Authorizations,
	}
	return types.NewTx(txdata), nil
}

// SignSetCodeTx signs the tx with the sponsor key (payer of gas).
func SignSetCodeTx(chainID *big.Int, sponsorPriv *ecdsa.PrivateKey, tx *types.Transaction) (*types.Transaction, error) {
	signer := types.NewPragueSigner(chainID) // supports SetCodeTx (0x04)
	return types.SignTx(tx, signer, sponsorPriv)
}

// PrepareFees fills fees if not provided: tip=tipWei, cap=max(baseFee*2+tip, tip*2).
func PrepareFees(ctx context.Context, ec *ethclient.Client, tipWei *big.Int) (tip, cap *big.Int, err error) {
	h, err := ec.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if tipWei == nil || tipWei.Sign() == 0 {
		// 2 gwei default tip if not provided
		tipWei = big.NewInt(0).Mul(big.NewInt(2), big.NewInt(1_000_000_000))
	}
	base := new(big.Int).Set(h.BaseFee)                 // base fee
	cap = new(big.Int).Add(new(big.Int).Mul(base, big.NewInt(2)), tipWei) // 2x baseFee + tip
	if t2 := new(big.Int).Mul(tipWei, big.NewInt(2)); t2.Cmp(cap) > 0 {
		cap = t2
	}
	return new(big.Int).Set(tipWei), cap, nil
}

// EstimateSponsorNonce and EstimateGas helper (keeps network usage minimal).
func EstimateSponsorNonce(ctx context.Context, ec *ethclient.Client, sponsor common.Address) (uint64, error) {
	return ec.NonceAt(ctx, sponsor, nil)
}

func EstimateGas(ctx context.Context, ec *ethclient.Client, fromSponsor common.Address, toEOA common.Address, calldata []byte) (uint64, error) {
	// We can't pass AuthList via eth_estimateGas, but a conservative fixed limit works well for sweep.
	// If you still want to estimate, do it against your own node with Prague support.
	const defaultLimit = 180_000
	_ = fromSponsor
	_ = toEOA
	_ = calldata
	return defaultLimit, nil
}

// SendPrivate tries common variants in this order:
// 1) eth_sendPrivateTransaction { "tx": "0x..." }
// 2) eth_sendPrivateRawTransaction "0x..."
// 3) eth_sendRawTransaction "0x..." (beaver treats as private)
func SendPrivate(ctx context.Context, rawTxHex string, relays []string, headers ExtraHeaders, authSigner *ecdsa.PrivateKey) []RelayResult {
	results := make([]RelayResult, 0, len(relays)*3)
	for _, url := range relays {
		// Per-relay method preference
		methods := []string{"eth_sendPrivateTransaction", "eth_sendPrivateRawTransaction", "eth_sendRawTransaction"}
		if strings.Contains(url, "blxrbdn.com") {
			methods = append([]string{"blxr_private_tx"}, methods...)
		}
		for _, m := range methods {
			// Build params for the given method
			var params any
			switch m {
			case "eth_sendPrivateTransaction":
				params = []any{map[string]any{"tx": rawTxHex}}
			case "eth_sendPrivateRawTransaction", "eth_sendRawTransaction":
				params = []any{rawTxHex}
			case "blxr_private_tx":
				params = []any{map[string]any{"transaction": strings.TrimPrefix(rawTxHex, "0x")}}
			default:
				params = []any{rawTxHex}
			}
			reqBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": m, "params": params}
			b, _ := json.Marshal(reqBody)
			hdr := map[string]string{"Content-Type": "application/json"}
			if headers != nil {
				if h, ok := headers[url]; ok && h != nil {
					for k, v := range h {
						hdr[k] = v
					}
				}
			}
			// Flashbots-style header where required
			if authSigner != nil && (strings.Contains(url, "flashbots.net") || strings.Contains(url, "payload.de") || strings.Contains(url, "buildernet")) {
				if sig := makeFlashbotsHeader(authSigner, b); sig != "" {
					hdr["X-Flashbots-Signature"] = sig
					hdr["x-auction-signature"] = sig
				}
			}
			code, body, err := doHTTP(ctx, url, b, hdr)
			ok := (err == nil && code >= 200 && code < 300)
			if !ok && code == 405 {
				// Some endpoints reject unknown method with 405; continue to next method.
			}
			results = append(results, RelayResult{
				RelayURL:      url,
				Accepted:      ok,
				ResponseBody:  body,
				HTTPStatus:    code,
				RequestMethod: m,
			})
			if ok {
				break // stop trying other methods for this relay
			}
		}
	}
	return results
}

// makeFlashbotsHeader signs the JSON body per Flashbots requirement.
func makeFlashbotsHeader(priv *ecdsa.PrivateKey, body []byte) string {
    // Flashbots requires EIP-191 over the HEX string of keccak256(body), not raw body.
    // Ref: docs "Authentication" Go example.
    hashedHex := crypto.Keccak256Hash(body).Hex()            // "0x..." ASCII
    digest := accounts.TextHash([]byte(hashedHex))           // EIP-191
    sig, err := crypto.Sign(digest, priv)                    // 65-byte sig with v in {0,1}
    if err != nil { return "" }
    addr := crypto.PubkeyToAddress(priv.PublicKey)
    return fmt.Sprintf("%s:%s", addr.Hex(), hexutil.Encode(sig))
}

func doHTTP(ctx context.Context, url string, body []byte, headers map[string]string) (status int, respBody string, err error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	httpClient := &http.Client{Timeout: 8 * time.Second}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw), nil
}

// High-level helper: build, sign and send one sponsored 7702 sweep transaction.
type RescueRequest struct {
	// Chain/network
	ChainID *big.Int
	// Actor keys & addresses
	AuthorityPrivKey *ecdsa.PrivateKey // EOA to be rescued (also known to attacker)
	AuthorityAddress common.Address
	SponsorPrivKey   *ecdsa.PrivateKey // pays gas
	SponsorAddress   common.Address
	// Delegate & action
	DelegateContract common.Address
	Recipient        common.Address
	TokenList        []common.Address
	// Auth nonces
	FirstAuthNonce uint64 // authority's current 7702 nonce (get from explorer or maintain internally)
	AuthCount      int    // number of sequential authorizations to include (e.g. 3-5)
	// Fees / gas
	TipWei *big.Int // optional; if nil will default to 2 gwei
	// Relays
	RelayURLs []string
	ExtraHeaders ExtraHeaders
	AuthSignerPriv *ecdsa.PrivateKey
	EnableSimulation bool
}

type RescueResponse struct {
	TxHash        common.Hash
	RawTxHex      string
	RelayAttempts []RelayResult
}

// ExecuteRescue builds sweepERC20 calldata, multiple authorizations, signs and sends privately.
func ExecuteRescue(ctx context.Context, ec *ethclient.Client, req RescueRequest) (*RescueResponse, error) {
	if req.AuthCount <= 0 {
		req.AuthCount = 2
	}
	// 1) Fees and sponsor nonce
	tip, cap, err := PrepareFees(ctx, ec, req.TipWei)
	if err != nil {
		return nil, err
	}
	sponsorNonce, err := EstimateSponsorNonce(ctx, ec, req.SponsorAddress)
	if err != nil {
		return nil, err
	}
	// 2) Calldata
	calldata, err := EncodeCalldataSweepERC20(req.TokenList, req.Recipient)
	if err != nil {
		return nil, err
	}
	// 3) Authorizations [k..k+N-1]
	auths, err := BuildAuthorizations(req.ChainID, req.AuthorityAddress, req.DelegateContract, req.FirstAuthNonce, req.AuthCount, req.AuthorityPrivKey)
	if err != nil {
		return nil, err
	}
	// 4) Gas limit (fixed safe default)
	gasLimit, err := EstimateGas(ctx, ec, req.SponsorAddress, req.AuthorityAddress, calldata)
	if err != nil {
		return nil, err
	}
	// 5) Build + sign
	unsigned, err := BuildSetCodeTx(BuildParams{
		ChainID:          req.ChainID,
		SponsorNonce:     sponsorNonce,
		GasLimit:         gasLimit,
		MaxPriorityFeeWei: tip,
		MaxFeeWei:        cap,
		AuthorityEOA:     req.AuthorityAddress,
		DelegateContract: req.DelegateContract,
		Calldata:         calldata,
		Authorizations:   auths,
	})
	if err != nil {
		return nil, err
	}
	signed, err := SignSetCodeTx(req.ChainID, req.SponsorPrivKey, unsigned)
	if err != nil {
		return nil, err
	}
	// 6) Encode & send to relays
	raw, err := signed.MarshalBinary()
	if err != nil {
		return nil, err
	}
	rawHex := "0x" + hex.EncodeToString(raw)
	// (optional) simulate via Flashbots eth_callBundle at head+1 using the same raw tx
	if req.EnableSimulation {
		head, _ := ec.BlockNumber(ctx)
		blockHex := fmt.Sprintf("0x%x", head+1)
		relay := pickFlashbotsRelay(req.RelayURLs)
		ok, reason, _, _, simErr := simulateFlashbotsCallBundle(ctx, relay, req.ExtraHeaders, req.AuthSignerPriv, rawHex, blockHex)
		if simErr != nil {
			return nil, fmt.Errorf("simulation http error: %v", simErr)
		}
		if !ok {
			return nil, fmt.Errorf("simulation reverted: %s", reason)
		}
	}
	
	attempts := SendPrivate(ctx, rawHex, req.RelayURLs, req.ExtraHeaders, req.AuthSignerPriv)
	return &RescueResponse{
		TxHash:        signed.Hash(),
		RawTxHex:      rawHex,
		RelayAttempts: attempts,
	}, nil
}

 
// pickFlashbotsRelay chooses a Flashbots-like relay or falls back to the first relay.
func pickFlashbotsRelay(relays []string) string {
	for _, u := range relays {
		if strings.Contains(u, "flashbots") {
			return u
		}
	}
	if len(relays) > 0 { return relays[0] }
	return "https://relay.flashbots.net"
}

// simulateFlashbotsCallBundle performs eth_callBundle for a single raw tx.
// Returns (ok, reason, body, status, err).
func simulateFlashbotsCallBundle(ctx context.Context, relay string, headers ExtraHeaders, authSigner *ecdsa.PrivateKey, rawTxHex string, blockHex string) (bool, string, string, int, error) {
	params := map[string]any{
		"txs":              []string{rawTxHex},
		"blockNumber":      blockHex,
		"stateBlockNumber": "latest",
	}
	reqBody := map[string]any{"jsonrpc":"2.0","id":1,"method":"eth_callBundle","params":[]any{params}}
	b, _ := json.Marshal(reqBody)
	hdr := map[string]string{"Content-Type":"application/json"}
	if headers != nil {
		if h, ok := headers[relay]; ok && h != nil {
			for k, v := range h { hdr[k] = v }
		}
	}
	// Flashbots authentication
	if authSigner != nil {
		if sig := makeFlashbotsHeader(authSigner, b); sig != "" {
			hdr["X-Flashbots-Signature"] = sig
			hdr["x-auction-signature"] = sig
		}
	}
    code, body, err := doHTTP(ctx, relay, b, hdr)
    if err != nil { return false, "http error", body, code, err }
	// Parse JSON for top-level error or per-tx error
	var resp struct {
		Error  *struct{ Code int `json:"code"`; Message string `json:"message"` } `json:"error"`
		Result *struct {
			Results []struct {
				Error string `json:"error"`
			} `json:"results"`
		} `json:"result"`
	}
	_ = json.Unmarshal([]byte(body), &resp)
	if resp.Error != nil {
		return false, resp.Error.Message, body, code, nil
	}
	if resp.Result != nil {
		for _, r := range resp.Result.Results {
			if r.Error != "" {
				return false, r.Error, body, code, nil
			}
		}
	}
	return true, "", body, code, nil
}

// Utility to parse hex keys safely in callers if needed.
func MustLoadKey(hexKey string) *ecdsa.PrivateKey {
	k, err := crypto.HexToECDSA(strip0x(hexKey))
	if err != nil {
		panic(err)
	}
	return k
}
func strip0x(s string) string {
	if len(s) >= 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
		return s[2:]
	}
	return s
}
