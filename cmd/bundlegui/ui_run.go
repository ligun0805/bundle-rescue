package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	core "github.com/ligun0805/bundle-rescue/internal/bundlecore"
)

// runAll iterates over the queue and simulates/sends each pair.
func runAll(a fyne.App, simOnly bool, rpc, chain, relays, auth, safe, blocksS, tipS, tipMulS, baseMulS, bufferS string) {
	defer func() {
		if r := recover(); r != nil {
			appendLogLine(a, fmt.Sprintf("[panic] %v", r))
		}
	}()
	if len(pairs)==0 { appendLogLine(a, "no pairs"); return }
	ec, err := ethclient.Dial(rpc); if err!=nil { appendLogLine(a, fmt.Sprintf("dial err: %v", err)); return }
	runCtx, runCancel = context.WithCancel(context.Background())
	ctx := runCtx
	total := len(pairs)
	ensureLogWindow(a).Show()
	if logProg != nil { logProg.Min = 0; logProg.Max = float64(total); logProg.SetValue(0) }
	if logProgLbl != nil { logProgLbl.SetText(fmt.Sprintf("0/%d", total)) }
	for i, pr := range pairs {
		select { case <-ctx.Done(): appendLogLine(a, "STOP pressed — cancelling"); return; default: }
		appendLogLine(a, fmt.Sprintf("=== %s ALL: pair %d/%d ===", map[bool]string{true:"Simulate", false:"Run"}[simOnly], i+1, len(pairs)))
		p := core.Params{
			RPC: rpc, ChainID: mustBig(chain), Relays: strings.Split(relays, ","), AuthPrivHex: auth,
			Token: common.HexToAddress(pr.Token), From: common.HexToAddress(pr.From), To: common.HexToAddress(pr.To),
			AmountWei: mustBig(pr.AmountWei), SafePKHex: safe, FromPKHex: pr.FromPK,
			Blocks: atoi(blocksS, 6), TipGweiBase: atoi64(tipS, 3), TipMul: atof(tipMulS, 1.25), BaseMul: atoi64(baseMulS, 2), BufferPct: atoi64(bufferS, 5),
			SimulateOnly: simOnly, SkipIfPaused: true,
			Logf: func(f string, a2 ...any){ appendLogLine(a, fmt.Sprintf(f, a2...)) },
			OnSimResult: func(relay, raw string, ok bool, err string){
				telAdd(TelemetryItem{ Time: time.Now().UTC().Format(time.RFC3339), Action:"eth_callBundle", PairIndex:i, Relay: relay, OK: ok, Error: err, Raw: raw })
				if simOnly { statsSimulated++ }
			},
		}
		out, err := core.Run(ctx, ec, p)
		if err != nil { appendLogLine(a, "error: "+err.Error()) } else {
			appendLogLine(a, "result: " + out.Reason)
			if out.Included { statsRescued++ }
		}
		if logProg != nil { logProg.SetValue(float64(i+1)) }
		if logProgLbl != nil { logProgLbl.SetText(fmt.Sprintf("%d/%d", i+1, total)) }
	}
	appendLogLine(a, "ALL: completed")
}
