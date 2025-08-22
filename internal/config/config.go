package config

import (
	"os"
	"strings"
	"strconv"
)

// Settings keeps all configuration options.
// Naming mirrors existing env keys to avoid touching other code.
type Settings struct {
	RPCURL                      string
	ChainID                     string // keep as string to match current usage in CLI/GUI
	Relays                      []string
	BloxrouteRelay              string
	FlashbotsAuthPKHex          string
	SafePrivateKeyHex           string
	Blocks                      int
	TipGwei                     int64
	TipMul                      float64
	BasefeeMul                  int64
	BufferPct                   int64
	Builders                    []string
	MinTimestamp                int64
	MaxTimestamp                int64
	BeaverAllowBuildernetRefunds bool
	BeaverRefundRecipient       string
	NetcheckBlocks              int
}

// Load reads settings from environment supporting both UPPER_CASE and lower_case keys.
func Load() Settings {
	get := func(keys []string, def string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" { return v }
		}
		return def
	}
	getInt := func(keys []string, def int) int {
		s := get(keys, "")
		if s == "" { return def }
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil { return n }
		return def
	}
	getInt64 := func(keys []string, def int64) int64 {
		s := get(keys, "")
		if s == "" { return def }
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil { return n }
		return def
	}
	getFloat := func(keys []string, def float64) float64 {
		s := get(keys, "")
		if s == "" { return def }
		if n, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil { return n }
		return def
	}
	getBool := func(keys []string, def bool) bool {
		s := strings.ToLower(get(keys, ""))
		if s == "" { return def }
		return s == "1" || s == "true" || s == "yes" || s == "on"
	}
	splitCSV := func(s string) []string {
		parts := strings.Split(s, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" { out = append(out, p) }
		}
		return out
	}

	st := Settings{}
	st.RPCURL     = get([]string{"rpc_url", "RPC_URL"}, "https://eth.llamarpc.com")
	st.ChainID    = get([]string{"chain_id", "CHAIN_ID"}, "")
	relaysCSV     := get([]string{"relays", "RELAYS"}, "https://relay.flashbots.net")
	st.Relays     = splitCSV(relaysCSV)
	st.BloxrouteRelay = get([]string{"bloxroute_relay", "BLOXROUTE_RELAY"}, "https://api.blxrbdn.com")
	st.FlashbotsAuthPKHex = get([]string{"flashbots_auth_pk", "FLASHBOTS_AUTH_PK"}, "")
	st.SafePrivateKeyHex  = get([]string{"safe_private_key", "SAFE_PRIVATE_KEY"}, "")

	st.Blocks     = getInt([]string{"blocks", "BLOCKS"}, 6)
	st.TipGwei    = getInt64([]string{"tip_gwei", "TIP_GWEI"}, 3)
	st.TipMul     = getFloat([]string{"tip_mul", "TIP_MUL"}, 1.25)
	st.BasefeeMul = getInt64([]string{"basefee_mul", "BASE_MUL"}, 2)
	st.BufferPct  = getInt64([]string{"buffer_pct", "BUFFER_PCT"}, 5)

	st.Builders   = splitCSV(get([]string{"builders", "BUILDERS"}, ""))
	st.MinTimestamp = getInt64([]string{"min_timestamp", "MIN_TIMESTAMP"}, 0)
	st.MaxTimestamp = getInt64([]string{"max_timestamp", "MAX_TIMESTAMP"}, 0)
	st.BeaverAllowBuildernetRefunds = getBool([]string{"beaver_allow_buildernet_refunds", "BEAVER_ALLOW_BUILDERNET_REFUNDS"}, true)
	st.BeaverRefundRecipient = get([]string{"beaver_refund_recipient", "BEAVER_REFUND_RECIPIENT"}, "")
	st.NetcheckBlocks = getInt([]string{"netcheck_blocks", "NETCHECK_BLOCKS"}, 100)

	return st
}