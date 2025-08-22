package main

import (
	"math/big"
	"strings"
)

func formatGwei(v *big.Int) string {
	if v==nil { return "0" }
	r := new(big.Rat).SetFrac(v, big.NewInt(1_000_000_000))
	return r.FloatString(2)
}

func formatEther(v *big.Int) string {
	if v==nil { return "0" }
	s := new(big.Rat).SetFrac(v, big.NewInt(1_000_000_000_000_000_000))
	return s.FloatString(6)
}

func parseETH(s string) *big.Int {
	s = strings.TrimSpace(s)
	if s == "" { return big.NewInt(0) }
	neg := false
	if s[0] == '+' || s[0] == '-' {
		if s[0] == '-' { neg = true }
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := new(big.Int)
	if parts[0] == "" { intPart.SetInt64(0) } else { intPart.SetString(parts[0], 10) }
	wei := new(big.Int).Mul(intPart, big.NewInt(1_000_000_000_000_000_000))
	if len(parts) == 2 && parts[1] != "" {
		frac := parts[1]
		if len(frac) > 18 { frac = frac[:18] }
		frac = frac + strings.Repeat("0", 18-len(frac))
		fracInt := new(big.Int); fracInt.SetString(frac, 10)
		wei.Add(wei, fracInt)
	}
	if neg { wei.Neg(wei) }
	return wei
}
