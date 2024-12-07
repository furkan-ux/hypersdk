// // // Copyright (C) 2025, Ava Labs, Inc. All rights reserved.
// // // See the file LICENSE for licensing terms.

package cmd

// todoca: make txns relayed but put Evmll.From into From field and not the relayer's address

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/hypersdk/api/indexer"
	jrpc "github.com/ava-labs/hypersdk/api/jsonrpc"
	wsrpc "github.com/ava-labs/hypersdk/api/ws"
	"github.com/ava-labs/hypersdk/auth"
	"github.com/ava-labs/hypersdk/chain"
	"github.com/ava-labs/hypersdk/codec"
	"github.com/ava-labs/hypersdk/examples/morpheusvm/actions"
	"github.com/ava-labs/hypersdk/examples/morpheusvm/storage"
	brpc "github.com/ava-labs/hypersdk/examples/morpheusvm/vm"
	"github.com/ava-labs/hypersdk/utils"
	"github.com/ava-labs/subnet-evm/core/types"
	"github.com/ava-labs/subnet-evm/params"
	eth_rpc "github.com/ava-labs/subnet-evm/rpc"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spf13/cobra"
)

type API struct {
	chainConfig *params.ChainConfig
	factory     chain.AuthFactory
	cli         *jrpc.JSONRPCClient
	bcli        *brpc.JSONRPCClient
	icli        *indexer.Client
	ws          *wsrpc.WebSocketClient
}

var (
	EvmRelayPort    int
	EvmRelayAddress string
)

func padder(address common.Address) codec.Address {
	codecAddr := codec.Address{}
	codecAddr[0] = auth.SECP256K1ID
	id := ids.ID{}
	zeroBytes := make([]byte, 32)
	copy(id[:], zeroBytes)
	copy(id[12:], address[:])
	copy(codecAddr[1:], id[:])
	return codecAddr
}

func (s *API) ChainId() *hexutil.Big {
	return (*hexutil.Big)(s.chainConfig.ChainID)
}

func (s *API) BlockNumber(ctx context.Context) (hexutil.Big, error) {
	_, number, _, err := s.cli.Accepted(ctx)
	utils.Outf("{{yellow}}BlockNumber: %d{{/}}\n", number)
	big := new(big.Int).SetUint64(number)
	return hexutil.Big(*big), err
}

func (s *API) GetBalance(ctx context.Context, address common.Address, blockNrOrHash eth_rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	bal, err := s.bcli.GetBalanceEVM(ctx, address)
	balance_big := new(big.Int).SetUint64(bal)
	// multiply by 10^10
	balance_big = balance_big.Mul(balance_big, big.NewInt(1000000000))
	utils.Outf("{{yellow}}GetBalance: %s %d{{/}}\n", address.Hex(), balance_big)
	return (*hexutil.Big)(balance_big), err
}

func (s *API) GetTransactionCount(ctx context.Context, address common.Address, blockNrOrHash eth_rpc.BlockNumberOrHash) (*hexutil.Uint64, error) {
	paddedAddress := padder(address)
	nonce, err := s.bcli.Nonce(ctx, paddedAddress)
	utils.Outf("{{yellow}}GetTransactionCount: %s %d{{/}}\n", address.Hex(), nonce)
	utils.Outf("{{blue}}GetTransactionCount storage: %s{{/}}\n", storage.ConvertAddress(paddedAddress))
	if err != nil {
		return nil, fmt.Errorf("failed to get nonce: %w", err)
	}
	return (*hexutil.Uint64)(&nonce), nil
}

func (s *API) GetBlockByNumber(ctx context.Context, number eth_rpc.BlockNumber, fullTx bool) (map[string]interface{}, error) {
	utils.Outf("{{yellow}}GetBlockByNumber: %d{{/}}\n", number.Int64())
	header := &types.Header{
		BaseFee: new(big.Int),
		Number:  big.NewInt(number.Int64()),
	}
	block := types.NewBlock(header, nil, nil, nil, nil)
	return RPCMarshalBlock(block, false, fullTx, s.chainConfig), nil
}

func (s *API) GetBlockByHash(ctx context.Context, hash common.Hash, fullTx bool) (map[string]interface{}, error) {
	header := &types.Header{
		BaseFee: new(big.Int),
		Number:  big.NewInt(1),
	}
	block := types.NewBlock(header, nil, nil, nil, nil)
	return RPCMarshalBlock(block, false, fullTx, s.chainConfig), nil
}

// type feeHistoryResult struct {
// 	OldestBlock  *hexutil.Big     `json:"oldestBlock"`
// 	Reward       [][]*hexutil.Big `json:"reward,omitempty"`
// 	BaseFee      []*hexutil.Big   `json:"baseFeePerGas,omitempty"`
// 	GasUsedRatio []float64        `json:"gasUsedRatio"`
// }

func (s *API) FeeHistory(ctx context.Context, blockCount math.HexOrDecimal64, lastBlock eth_rpc.BlockNumber, rewardPercentiles []float64) (*feeHistoryResult, error) {
	_, number, _, err := s.cli.Accepted(ctx)
	if err != nil {
		return nil, err
	}
	rewards := make([][]*hexutil.Big, 0)
	baseFee := make([]*hexutil.Big, 0)
	gasUsedRatio := make([]float64, 0)

	baseFee = append(baseFee, (*hexutil.Big)(big.NewInt(0)))
	gasUsedRatio = append(gasUsedRatio, 0)
	rewards = append(rewards, make([]*hexutil.Big, len(rewardPercentiles)))
	for i := 0; i < len(rewardPercentiles); i++ {
		rewards[0][i] = (*hexutil.Big)(big.NewInt(0))
	}

	return &feeHistoryResult{
		OldestBlock:  (*hexutil.Big)(new(big.Int).SetUint64(number)),
		Reward:       rewards,
		BaseFee:      baseFee,
		GasUsedRatio: gasUsedRatio,
	}, nil
}

func (s *API) EstimateGas(ctx context.Context, args TransactionArgs, blockNrOrHash *eth_rpc.BlockNumberOrHash, overrides *StateOverride) (hexutil.Uint64, error) {
	lotsOfGas := uint64(25000000)
	paddedAddress := padder(*args.From)
	to := args.To
	if to == nil {
		args.To = &common.Address{}
	}
	// nonce, err := s.bcli.Nonce(ctx, paddedAddress)
	// if err != nil {
	// 	return 0, fmt.Errorf("failed to get nonce: %w", err)
	// }
	call := &actions.EvmCall{
		From:     *args.From,
		To:       args.To,
		GasLimit: lotsOfGas,
		Value:    uint64(0),
		Data:     []byte{},
	}
	if args.Value != nil {
		call.Value = args.Value.ToInt().Uint64()
	}
	if args.Data != nil {
		call.Data = ([]byte)(*args.Data)
	}
	// if args.Nonce != nil {
	// 	call.Nonce = uint64(*args.Nonce)
	// }
	simulated, err := s.cli.SimulateActions(ctx, []chain.Action{call}, paddedAddress)
	utils.Outf("{{yellow}}EstimateGas debug: %+v{{/}}\n", err)
	if err != nil {
		return 0, fmt.Errorf("failed to simulate action: %w", err)
	}
	evmCallOutputBytes := simulated[0].Output
	reader := codec.NewReader(evmCallOutputBytes, len(evmCallOutputBytes))
	evmCallResultTyped, err := brpc.OutputParser.Unmarshal(reader)
	if err != nil {
		return 0, fmt.Errorf("failed to unmarshal evm call result: %w", err)
	}
	evmCallResult := evmCallResultTyped.(*actions.EvmCallResult)
	utils.Outf("{{yellow}}EstimateGas: %+v{{/}}\n", evmCallResult.UsedGas)
	return hexutil.Uint64(evmCallResult.UsedGas), nil
}

func (s *API) Call(ctx context.Context, args TransactionArgs, blockNrOrHash eth_rpc.BlockNumberOrHash, overrides *StateOverride, blockOverrides *BlockOverrides) (hexutil.Bytes, error) {
	utils.Outf("{{red}}Call: %+v{{/}}\n", args)

	lotsOfGas := uint64(25000000)

	// _, nonce, err := s.bcli.EvmAccount(ctx, evmAddr.Hex())
	// if err != nil {
	// 	return nil, err
	// }
	from := *args.From
	to := args.To
	if to == nil {
		to = &common.Address{}
	}
	call := &actions.EvmCall{
		From:     from,
		To:       to,
		GasLimit: lotsOfGas,
		Value:    uint64(0),
		Data:     []byte{},
	}
	if args.Value != nil {
		call.Value = args.Value.ToInt().Uint64()
	}
	if args.Data != nil {
		call.Data = ([]byte)(*args.Data)
	}
	// if args.Gas != nil {
	// 	call.GasLimit = uint64(*args.Gas)
	// }
	utils.Outf("{{blue}}Call: %+v %+v{{/}}\n", call)
	trace, err := s.cli.SimulateActions(ctx, []chain.Action{call}, padder(*args.From))
	utils.Outf("{{yellow}}Call: %+v{{/}}\n", err)
	if err != nil {
		return nil, fmt.Errorf("failed to simulate action: %w", err)
	}
	utils.Outf("{{yellow}}Call: %+v %+v{{/}}\n", args, trace[0].Output)
	evmCallResultTyped, err := brpc.OutputParser.Unmarshal(codec.NewReader(trace[0].Output, len(trace[0].Output)))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal evm call result: %w", err)
	}
	evmCallResult := evmCallResultTyped.(*actions.EvmCallResult)
	if !evmCallResult.Success {
		return nil, fmt.Errorf("transaction failed: %v", evmCallResult.ErrorCode)
	}
	return hexutil.Bytes(evmCallResult.Return), nil
}

func (s *API) SendRawTransaction(ctx context.Context, input hexutil.Bytes) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, fmt.Errorf("failed to unmarshal transaction: %w", err)
	}

	signer := types.LatestSignerForChainID(tx.ChainId())
	from, err := signer.Sender(tx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get sender: %w", err)
	}
	utils.Outf("{{yellow}}SendRawTransaction: %s %s{{/}}\n", from.Hex(), tx.Hash().Hex())

	to := tx.To()
	if to == nil {
		to = &common.Address{}
	}

	call := &actions.EvmCall{
		From:     from,
		To:       to,
		Value:    tx.Value().Uint64(),
		GasLimit: tx.Gas(),
		Data:     tx.Data(),
	}
	trace, err := s.cli.SimulateActions(ctx, []chain.Action{call}, padder(from))
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to simulate action: %w", err)
	}
	simulatedCallOut := trace[0].Output
	reader := codec.NewReader(simulatedCallOut, len(simulatedCallOut))
	evmCallResultTyped, err := brpc.OutputParser.Unmarshal(reader)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to unmarshal evm call result: %w", err)
	}
	evmCallResult := evmCallResultTyped.(*actions.EvmCallResult)
	if !evmCallResult.Success {
		return common.Hash{}, fmt.Errorf("transaction failed: %v", evmCallResult.ErrorCode)
	}
	utils.Outf("{{yellow}}SendRawTransaction Error code, success: %+v %+v{{/}}\n", evmCallResult.ErrorCode, evmCallResult.Success)
	call.Keys = trace[0].StateKeys

	parser, err := s.bcli.Parser(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get parser: %w", err)
	}
	_, sentTx, _, err := s.cli.GenerateTransaction(ctx, parser, []chain.Action{call}, s.factory)
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to generate transaction: %w", err)
	}

	if err := s.ws.RegisterTx(sentTx); err != nil {
		return common.Hash{}, fmt.Errorf("failed to register transaction: %w", err)
	}
	utils.Outf("{{yellow}}Sent transaction: %s{{/}}\n", common.Hash(sentTx.GetID()))
	callerNonce, err := s.bcli.Nonce(ctx, padder(from))
	if err != nil {
		return common.Hash{}, fmt.Errorf("failed to get nonce: %w", err)
	}
	callerNonce -= 1
	if call.To == nil {
		contractAddress := crypto.CreateAddress(from, callerNonce)
		utils.Outf("{{yellow}}Contract created: %s{{/}}\n", contractAddress)
	}
	return common.Hash(sentTx.GetID()), nil
}

func (s *API) NetworkID(ctx context.Context) (hexutil.Uint64, error) {
	utils.Outf("{{yellow}}NetworkID{{/}}\n")
	return hexutil.Uint64(s.chainConfig.ChainID.Uint64()), nil
}

func (s *API) GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]interface{}, error) {
	utils.Outf("{{yellow}}GetTransactionReceipt: %s{{/}}\n", hash)

	txresponse, success, err := s.icli.GetTx(ctx, ids.ID(hash))
	utils.Outf("{{yellow}}GetTransactionReceipt: %+v %+v{{/}}\n", txresponse, success)
	if err != nil {
		utils.Outf("{{red}}Error GetTransactionReceipt: %v %v{{/}}\n", hash, err)
		return nil, fmt.Errorf("failed to get transaction receipt: %w", err)
	}
	if !success {
		utils.Outf("{{yellow}}GetTransactionReceipt: %v not found{{/}}\n", hash)
		return nil, fmt.Errorf("transaction not found: %w", err)
	}
	var (
		successUint     hexutil.Uint
		contractAddress common.Address
	)
	if success {
		successUint = hexutil.Uint(1)
	}

	evmCallResultTyped, err := brpc.OutputParser.Unmarshal(codec.NewReader(txresponse.Outputs[0], len(txresponse.Outputs[0])))
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal evm call result: %w", err)
	}
	evmCallResult := evmCallResultTyped.(*actions.EvmCallResult)
	contractAddress = evmCallResult.ContractAddress

	blockHash := common.Hash{}
	_, blockNumber, timestamp, err := s.cli.Accepted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get accepted block: %w", err)
	}
	blockNumberUint := new(big.Int).SetUint64(blockNumber)
	fields := map[string]interface{}{
		"blockHash":         blockHash,
		"blockNumber":       (*hexutil.Big)(blockNumberUint),
		"transactionHash":   hash,
		"transactionIndex":  hexutil.Uint(0),
		"from":              common.Address{0x44},
		"to":                common.Address{0x32},
		"gasUsed":           hexutil.Uint64(txresponse.Fee),
		"cumulativeGasUsed": hexutil.Uint64(txresponse.Fee),
		"contractAddress":   contractAddress,
		"logs":              []interface{}{},
		"logsBloom":         types.Bloom{},
		"status":            successUint,
		"blockTime":         timestamp,
		"type":              hexutil.Uint(0x2),
		"effectiveGasPrice": (*hexutil.Big)(big.NewInt(0)),
	}
	utils.Outf("{{yellow}}GetTransactionReceipt: %+v{{/}}\n", fields)
	return fields, nil
}

func (s *API) GetTransactionByHash(ctx context.Context, hash common.Hash) (*RPCTransaction, error) {
	utils.Outf("{{yellow}}GetTransactionByHash: %s{{/}}\n", hash)
	txresponse, success, err := s.icli.GetTx(ctx, ids.ID(hash))
	if err != nil {
		utils.Outf("{{red}}GetTransactionByHash: %v %v{{/}}\n", hash, err)
		return nil, err
	}
	if !success {
		utils.Outf("{{yellow}}GetTransactionByHash: %v not found{{/}}\n", hash)
		return nil, nil
	}
	blockHash := common.Hash{}
	to := common.Address{}
	_, blockNumber, _, err := s.cli.Accepted(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get accepted block: %w", err)
	}
	fields := &RPCTransaction{
		BlockHash:        &blockHash,
		BlockNumber:      (*hexutil.Big)(big.NewInt(int64(blockNumber))),
		From:             common.Address{0x44},
		Gas:              hexutil.Uint64(txresponse.Fee),
		GasPrice:         (*hexutil.Big)(big.NewInt(0)),
		GasFeeCap:        (*hexutil.Big)(big.NewInt(0)),
		GasTipCap:        (*hexutil.Big)(big.NewInt(0)),
		Hash:             hash,
		Input:            []byte{},
		To:               &to,
		TransactionIndex: new(hexutil.Uint64),
		Value:            (*hexutil.Big)(big.NewInt(0)),
		Type:             0x2,
		Accesses:         nil,
		ChainID:          s.ChainId(),
		V:                (*hexutil.Big)(big.NewInt(0)),
		R:                (*hexutil.Big)(big.NewInt(0)),
		S:                (*hexutil.Big)(big.NewInt(0)),
		YParity:          new(hexutil.Uint64),
	}
	utils.Outf("{{yellow}}GetTransactionByHash: %+v{{/}}\n", fields)
	return fields, nil
}

func (s *API) GetCode(ctx context.Context, address common.Address, blockNrOrHash eth_rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	code, err := s.bcli.EvmGetCode(ctx, address)
	utils.Outf("{{yellow}}Contract address: %s code: %s{{/}}\n", address.Hex(), hexutil.Bytes(code).String())
	return code, err
}

func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request body
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			fmt.Println("Error reading request body:", err)
			return
		}
		// Restore the request body to its original state
		r.Body = io.NopCloser(bytes.NewBuffer(body))

		// Log information about the request including the body
		fmt.Printf("Received %s request for %s from %s\n", r.Method, r.URL.Path, r.RemoteAddr)
		fmt.Println("Request body:", string(body))

		// Call the next handler in the chain
		next.ServeHTTP(w, r)
	})
}

func (s *API) GasPrice(ctx context.Context) (*hexutil.Big, error) {
	utils.Outf("{{yellow}}GasPrice{{/}}\n")
	return (*hexutil.Big)(big.NewInt(1)), nil
}

func (s *API) SuggestGasPrice(ctx context.Context) (*hexutil.Big, error) {
	utils.Outf("{{yellow}}SuggestGasPrice{{/}}\n")
	return (*hexutil.Big)(big.NewInt(1)), nil
}

func (s *API) SuggestGasTipCap(ctx context.Context) (*hexutil.Big, error) {
	utils.Outf("{{yellow}}SuggestGasTipCap{{/}}\n")
	return (*hexutil.Big)(big.NewInt(0)), nil
}

func (s *API) MaxPriorityFeePerGas(ctx context.Context) (*hexutil.Big, error) {
	utils.Outf("{{yellow}}MaxPriorityFeePerGas{{/}}\n")
	return (*hexutil.Big)(big.NewInt(0)), nil
}

// func (s *API) PendingNonceAt(ctx context.Context, address common.Address) (hexutil.Uint64, error) {
// 	utils.Outf("{{yellow}}PendingNonceAt: %s{{/}}\n", address.Hex())
// 	utils.Outf("{{yellow}}PendingNonceAt storage: %s{{/}}\n", storage.ConvertAddress(padder(address)))
// 	nonce, err := s.bcli.Nonce(ctx, padder(address))
// 	if err != nil {
// 		return 0, fmt.Errorf("failed to get nonce: %w", err)
// 	}
// 	return hexutil.Uint64(nonce), nil
// }

var evmRelayCmd = &cobra.Command{
	Use: "evm-relay",
	RunE: func(*cobra.Command, []string) error {
		_, _, factory, cli, bcli, ws, err := handler.DefaultActor()
		if err != nil {
			return err
		}
		_, chainURIs, err := handler.h.GetDefaultChain(true)
		if err != nil {
			return fmt.Errorf("failed to get network: %w", err)
		}
		icli := indexer.NewClient(chainURIs[0])

		noDeadline := time.Duration(0)
		server := eth_rpc.NewServer(noDeadline)
		config := params.SubnetEVMDefaultChainConfig
		api := &API{
			chainConfig: params.SubnetEVMDefaultChainConfig,
			factory:     factory,
			cli:         cli,
			bcli:        bcli,
			icli:        icli,
			ws:          ws,
		}
		err = server.RegisterName("eth", api)
		if err != nil {
			return err
		}
		chainID := config.ChainID.Uint64()
		err = server.RegisterName("net", NewNetAPI(chainID))
		if err != nil {
			return err
		}

		addr := net.JoinHostPort("localhost", fmt.Sprintf("%d", EvmRelayPort))
		http.Handle("/evm-relay/rpc", LogRequests(server))

		timeout := 30 * time.Second
		sever := &http.Server{
			Addr:              addr,
			ReadHeaderTimeout: timeout,
		}

		utils.Outf(
			"{{yellow}}EVMRelay{{/}}\n",
		)
		go func() {
			utils.Outf("{{green}}Listening on %s{{/}}\n", addr)
			err := sever.ListenAndServe()
			if err != nil && err != http.ErrServerClosed {
				panic(err)
			}
		}()

		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
		<-interrupt

		if err := sever.Shutdown(context.Background()); err != nil {
			return err
		}

		return nil
	},
}

func MakeEVMCmd() *cobra.Command {
	evmRelayCmd.Flags().IntVar(&EvmRelayPort, "port", 11000, "port to listen on")
	evmRelayCmd.Flags().StringVar(&EvmRelayAddress, "address", "", "address to relay from")
	return evmRelayCmd
}
