package bundlecore

import (
	"crypto/ecdsa"
	"errors"
	"math/big"
	"strings"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Parse hex ECDSA private key (with / without 0x).
func hexToECDSAPriv(s string) (*ecdsa.PrivateKey, error) {
	h := strings.TrimSpace(strings.TrimPrefix(s, "0x"))
	if len(h) == 0 {
		return nil, errors.New("empty private key")
	}
	return gethcrypto.HexToECDSA(h)
}

func gweiToWei(g int64) *big.Int {
	x := new(big.Int).SetInt64(g)
	return x.Mul(x, big.NewInt(1_000_000_000))
}

func mulBig(a *big.Int, m int64) *big.Int {
	if a == nil {
		return big.NewInt(0)
	}
	return new(big.Int).Mul(a, big.NewInt(m))
}

func addBig(a, b *big.Int) *big.Int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return new(big.Int).Add(a, b)
}

// Human-readable helpers (ETH/gwei).
func fmtETH(x *big.Int) string {
	if x == nil {
		return "0"
	}
	r := new(big.Rat).SetFrac(new(big.Int).Set(x), big.NewInt(1_000_000_000_000_000_000))
	return r.FloatString(6)
}

func fmtGwei(x *big.Int) string {
	if x == nil {
		return "0"
	}
	r := new(big.Rat).SetFrac(new(big.Int).Set(x), big.NewInt(1_000_000_000))
	return r.FloatString(2)
}
