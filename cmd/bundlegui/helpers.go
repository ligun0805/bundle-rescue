package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

type pairRow struct {
	Token, From, FromPK, To   string
	AmountWei, AmountTokens   string
	Decimals                  int
	BalanceWei, BalanceTokens string
}

func short(s string) string {
	if len(s) <= 16 { return s }
	return s[:10] + "…" + s[len(s)-5:]
}
func mustBig(s string) *big.Int {
	s = strings.TrimSpace(s)
	if s == "" { return big.NewInt(0) }
	if strings.HasPrefix(s,"0x") || strings.HasPrefix(s,"0X") {
		v:=new(big.Int); v.SetString(s[2:],16); return v
	}
	v:=new(big.Int); if _,ok := v.SetString(s,10); ok { return v }
	return big.NewInt(0)
}
func atoi(s string, d int) int { if v,err := strconv.Atoi(strings.TrimSpace(s)); err==nil { return v }; return d }
func atoi64(s string, d int64) int64 { if v,err := strconv.ParseInt(strings.TrimSpace(s),10,64); err==nil { return v }; return d }
func atof(s string, d float64) float64 { if v,err := strconv.ParseFloat(strings.TrimSpace(s),64); err==nil { return v }; return d }

func deriveAddrFromPK(hexPk string) (string, error) {
	h := strings.TrimPrefix(strings.TrimSpace(hexPk), "0x")
	priv, err := crypto.HexToECDSA(h)
	if err != nil { return "", err }
	addr := crypto.PubkeyToAddress(priv.PublicKey)
	return addr.Hex(), nil
}

// ERC-20 helpers
func fetchTokenDecimals(ec *ethclient.Client, token common.Address) (int, error) {
	data := common.FromHex("0x313ce567") // decimals()
	res, err := ec.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil { return 0, err }
	if len(res)==0 { return 18, nil }
	return int(res[len(res)-1]), nil
}
func fetchTokenBalance(ec *ethclient.Client, token common.Address, owner common.Address) (*big.Int, error) {
	data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(owner.Bytes(),32)...)
	res, err := ec.CallContract(context.Background(), ethereum.CallMsg{To: &token, Data: data}, nil)
	if err != nil { return nil, err }
	if len(res)==0 { return big.NewInt(0), nil }
	return new(big.Int).SetBytes(res), nil
}

func toWeiFromTokens(amount string, decimals int) (*big.Int, error) {
	amount = strings.TrimSpace(amount)
	if amount == "" { return nil, fmt.Errorf("empty amount") }
	if decimals < 0 { decimals = 18 }
	parts := strings.SplitN(amount, ".", 2)
	intPart := parts[0]
	fracPart := ""
	if len(parts) == 2 { fracPart = parts[1] }
	if len(fracPart) > decimals { return nil, fmt.Errorf("too many fractional digits for %d decimals", decimals) }
	fracPart = fracPart + strings.Repeat("0", decimals-len(fracPart))
	clean := strings.TrimLeft(intPart+fracPart, "0")
	if clean == "" { return big.NewInt(0), nil }
	v, ok := new(big.Int).SetString(clean, 10)
	if !ok { return nil, fmt.Errorf("bad amount") }
	return v, nil
}
func formatTokensFromWei(v *big.Int, decimals int) string {
	if v==nil { return "0" }
	if decimals<=0 { return v.String() }
	s := new(big.Int).Abs(v).String()
	neg := v.Sign() < 0
	if len(s) <= decimals {
		frac := strings.Repeat("0", decimals-len(s)) + s
		out := "0." + strings.TrimRight(frac, "0")
		if out == "0." { out = "0" }
		if neg { return "-" + out }
		return out
	}
	intPart := s[:len(s)-decimals]
	frac := strings.TrimRight(s[len(s)-decimals:], "0")
	out := intPart
	if frac != "" { out = intPart + "." + frac }
	if neg { return "-" + out }
	return out
}

// import/export
func parseCSVAll(rd io.Reader) ([]pairRow, error) {
	r := csv.NewReader(rd); r.FieldsPerRecord = -1
	rows, err := r.ReadAll(); if err != nil { return nil, err }
	if len(rows)==0 { return nil, nil }
	head := rows[0]
	lower := strings.ToLower(strings.Join(head, ","))
	var out []pairRow
	if strings.Contains(lower, "token") {
		idx := map[string]int{}; for i,h := range head { idx[strings.ToLower(strings.TrimSpace(h))]=i }
		get := func(row []string, k string) string { if j,ok := idx[k]; ok && j < len(row) { return strings.TrimSpace(row[j]) }; return "" }
		for i:=1; i<len(rows); i++ {
			row := rows[i]; if len(row)==0 { continue }
			p := pairRow{
				Token: get(row,"token"), From: get(row,"from"), FromPK:get(row,"frompk"), To:get(row,"to"),
				AmountWei:get(row,"amountwei"), AmountTokens:get(row,"amount"), Decimals:-1,
			}
			if d := get(row,"decimals"); d!="" { if n,err := strconv.Atoi(d); err==nil { p.Decimals = n } }
			if p.Token=="" && p.FromPK=="" && p.To=="" { continue }
			out = append(out, p)
		}
	} else {
		for _,row := range rows {
			if len(row)==0 { continue }
			if len(row)!=4 { continue }
			out = append(out, pairRow{ Token: strings.TrimSpace(row[0]), FromPK: strings.TrimSpace(row[1]), To: strings.TrimSpace(row[2]), AmountWei: strings.TrimSpace(row[3]), Decimals:-1 })
		}
	}
	return out, nil
}
func parseJSONAll(rd io.Reader) ([]pairRow, error) {
	b, err := io.ReadAll(rd); if err != nil { return nil, err }
	var arr []map[string]string
	if err := json.Unmarshal(b, &arr); err != nil { return nil, err }
	var out []pairRow
	for _, m := range arr {
		p := pairRow{ Token:m["token"], From:m["from"], FromPK:m["fromPk"], To:m["to"], AmountWei:m["amountWei"], AmountTokens:m["amount"], Decimals:-1 }
		if d := strings.TrimSpace(m["decimals"]); d!="" { if n,err := strconv.Atoi(d); err==nil { p.Decimals = n } }
		if p.Token=="" && p.FromPK=="" && p.To=="" { continue }
		out = append(out, p)
	}
	return out, nil
}
