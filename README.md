# Bundle Rescue (CLI + Fyne GUI)

Rescue ERC-20 tokens from compromised EOAs **without exposing fund tx** to public mempool:
- Bundle **fund (safe→from)** + **transfer (from→to)** via private relays.
- Retries across N next blocks, exponential tip bump, parallel relays.
- Pre-simulation (`eth_callBundle`). Optional **simulate-only** mode.
- CLI batch via CSV/JSON/STDIN; GUI supports **import/export CSV/JSON**.
- **Derives `from` address from `fromPk`** automatically.

## Build
```bash
go mod tidy
go build -o bundlecli ./cmd/bundlecli
go build -o bundlegui ./cmd/bundlegui
```

## CLI Examples
```bash
# Single pair (derive from fromPk)
./bundlecli -rpc $RPC_URL -chain 1   -relays https://relay.flashbots.net   -authPk $FLASHBOTS_AUTH_PK -safePk $SAFE_PRIVATE_KEY   -token 0xToken -fromPk 0xFromPK -to 0xTo -amountWei 1000000

# Batch from CSV
./bundlecli -rpc $RPC_URL -chain 1   -relays https://relay.flashbots.net   -authPk $FLASHBOTS_AUTH_PK -safePk $SAFE_PRIVATE_KEY   -csv pairs.csv -blocks 8 -tipGwei 2 -tipMul 1.25 -baseMul 2 -bufferPct 5

# Simulate only
./bundlecli ... -simulate
```

## GUI
```
./bundlegui
```
Fill globals (RPC/Chain/Relays/Auth PK/Safe PK), add pairs (Token, From PK, To, Amount), or **import CSV/JSON**. Toggle **Simulate only** to just run `eth_callBundle` with no send. Invalid private key is highlighted, and derived `from` is previewed in real time.

## .env
See `.env.example`. CLI/GUI read `.env` and `.env.local`. Keep secrets out of VCS.


## Env-only mode
- **CLI**: pass `-envOnly` to force reading RPC/Chain/Relays/AuthPK/SafePK only from `.env` and ignore flags.
- **GUI**: toggle **Use .env globals (lock fields)** to hide/lock the global inputs and use values from `.env`.


## Token-denominated amounts
- You can specify **amounts in tokens** (e.g., `1.5`) instead of `amountWei`:
  - **CLI CSV/JSON with header**: use columns/keys `amount` and optional `decimals` (if omitted, the app fetches `decimals()` from the token contract; fallback = 18).
  - **GUI**: field **Amount (tokens)** and optional **Decimals** (auto if empty). The app converts to base units precisely (no float rounding).
  - Legacy CSV without header still expects `amountWei` for backward compatibility.


## Toggle Tokens/Wei in GUI
Use the radio toggle **Show amount: Tokens / Wei** (toolbar above the table) to switch the 4th column between token-denominated amount and base units (wei).


## Batch controls (GUI)
- **Sim+Send ALL** — проходит пары по очереди; поведение на ошибку задаёт переключатель **On error: Stop on first failure / Continue on errors**.
- **Simulate ALL** — только симуляция для всех пар (без отправки); тот же переключатель ошибок.
- Вкладка **Sim Details** показывает сырые ответы `eth_callBundle` по каждому релее.
- **Export Telemetry JSON** — выгружает логи/детали/телеметрию симуляций в JSON (включая конфигурацию и список пар).
