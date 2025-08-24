package bundlecore

import "encoding/json"

type rpcReq struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

type rpcResp struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

// ===== bloXroute bundle simulation =====
type BlxrSimulateBundleParams struct {
    Transaction       []string `json:"transaction"`          // raw txs without "0x"
    BlockNumber       string   `json:"block_number"`         // hex block number
    StateBlockNumber  string   `json:"state_block_number,omitempty"` // e.g. "latest"
    Timestamp         *int64   `json:"timestamp,omitempty"`
    BlockchainNetwork string   `json:"blockchain_network,omitempty"` // "Mainnet" or "BSC-Mainnet"
}

type BlxrSimulateTxResult struct {
    GasUsed uint64 `json:"gasUsed"`
    TxHash  string `json:"txHash"`
    Value   string `json:"value,omitempty"`
    Error   string `json:"error,omitempty"`
    Revert  string `json:"revert,omitempty"`
}

type BlxrSimulateBundleResult struct {
    BundleGasPrice   string                 `json:"bundleGasPrice"`
    BundleHash       string                 `json:"bundleHash"`
    CoinbaseDiff     string                 `json:"coinbaseDiff"`
    EthSentToCoinbase string                `json:"ethSentToCoinbase"`
    GasFees          string                 `json:"gasFees"`
    Results          []BlxrSimulateTxResult `json:"results"`
    StateBlockNumber uint64                 `json:"stateBlockNumber"`
    TotalGasUsed     uint64                 `json:"totalGasUsed"`
}

type JSONRPCError struct {
    Code    int    `json:"code"`
    Message string `json:"message"`
}

type BlxrSimulateBundleResponse struct {
    JSONRPC string                    `json:"jsonrpc"`
    ID      any                       `json:"id"`
    Result  *BlxrSimulateBundleResult `json:"result,omitempty"`
    Error   *JSONRPCError             `json:"error,omitempty"`
}