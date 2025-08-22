package bundlecore

import (
	"crypto/ecdsa"
	"encoding/hex"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"  
	"github.com/ethereum/go-ethereum/core/types"
	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Build EIP-1559 transaction.
func buildDynamicTx(chain *big.Int, nonce uint64, to *common.Address, value *big.Int, gasLimit uint64, tip, feeCap *big.Int, data []byte) *types.Transaction {
	df := &types.DynamicFeeTx{
		ChainID:   chain,
		Nonce:     nonce,
		Gas:       gasLimit,
		GasTipCap: new(big.Int).Set(tip),
		GasFeeCap: new(big.Int).Set(feeCap),
		To:        to,
		Value:     new(big.Int).Set(value),
		Data:      data,
	}
	return types.NewTx(df)
}

// Sign transaction with latest signer for given chain ID.
func signTx(tx *types.Transaction, chain *big.Int, prv *ecdsa.PrivateKey) (*types.Transaction, error) {
	signer := types.LatestSignerForChainID(chain)
	return types.SignTx(tx, signer, prv)
}

// Hex-encode transaction.
func txAsHex(tx *types.Transaction) string {
	b, _ := tx.MarshalBinary()
	return "0x" + hex.EncodeToString(b)
}

// NewTransactorFromHex builds *bind.TransactOpts from hex key and chain ID.
func NewTransactorFromHex(pkHex string, chainID *big.Int) (*bind.TransactOpts, error) {
	prv, err := gethcrypto.HexToECDSA(strings.TrimPrefix(pkHex, "0x"))
	if err != nil {
		return nil, err
	}
	return bind.NewKeyedTransactorWithChainID(prv, chainID)
}
