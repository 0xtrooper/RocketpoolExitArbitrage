package uniswap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"rocketpoolArbitrage/uniswapContracts/helper"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	QuoterAddressStr = "0x61ffe014ba17989e743c5f6cb21bf9697530b21e"
	rETHAddressStr   = "0xae78736Cd615f374D3085123A210448E74Fc6393"
	WETHAddressStr   = "0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"

	PoolA = "0x553e9C493678d8606d6a5ba284643dB2110Df823" // 0.01% fee
	PoolB = "0xa4e0faA58465A2D369aa21B3e42d43374c6F9613" // 0.05% fee
)

var (
	ErrPriceLimitExceeded = errors.New("price limit exceeded")

	UniswapQ96 = new(big.Float).SetInt(new(big.Int).Lsh(big.NewInt(1), 96))
)

func GetBestPoolWithdrawArb(ctx context.Context, logger *slog.Logger, client *ethclient.Client, amount *big.Int, primaryRatio *big.Float) (common.Address, *big.Int, *big.Int, error) {
	// withdraw swaps => zeroForOne = false
	return GetBestPool(ctx, logger, client, false, amount, primaryRatio)
}

func GetBestPool(ctx context.Context, logger *slog.Logger, client *ethclient.Client, zeroForOne bool, amount *big.Int, primaryRatio *big.Float) (common.Address, *big.Int, *big.Int, error) {
	// afaik there is no good uniswap deployment on holesky
	networkID, err := client.NetworkID(ctx)
	if err != nil {
		return common.Address{}, nil, nil, errors.Join(errors.New("failed to verify client connection"), err)
	}
	if networkID.Uint64() != 1 {
		return common.Address{}, nil, nil, errors.New("only mainnet is supported for uniswap arbitrage")
	}

	var limit *big.Int
	if primaryRatio != nil {
		sqrt := new(big.Float).Sqrt(primaryRatio)        // calculate sqrt
		resFloat := new(big.Float).Mul(sqrt, UniswapQ96) // multiply by 2^96
		limit, _ = resFloat.Int(nil)
	} else {
		limit = big.NewInt(0)
	}

	poolAAmountIn, err := getExactOutput(ctx, client, zeroForOne, amount, big.NewInt(100), limit)
	if err != nil {
		return common.Address{}, nil, nil, errors.Join(errors.New("failed to get pool A"), err)
	}

	if logger != nil && logger.Enabled(ctx, slog.LevelDebug) {
		poolAAmountFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(poolAAmountIn), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		logger.Debug("0.01 percent pool", slog.String("address", PoolB), slog.Float64("Amount In", poolAAmountFloat))
	}

	poolBAmountIn, err := getExactOutput(ctx, client, zeroForOne, amount, big.NewInt(500), limit)
	if err != nil {
		// the pool B has a rather low liquidity - 200k USD per side as of 10.01.2025
		// if we get an error this is probably cause by the low liquidity
		// therefor we only log it as a warning and default back to pool A
		logger.Debug("failed to get uniswap estimation - possible too low liquidty", slog.String("pool", PoolB))
		return common.HexToAddress(PoolA), poolAAmountIn, limit, nil
	}

	if logger != nil && logger.Enabled(ctx, slog.LevelDebug) {
		poolBAmountInFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(poolBAmountIn), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		logger.Debug("0.05 percent pool", slog.String("address", PoolA), slog.Float64("Amount In", poolBAmountInFloat))
	}

	// return pool with lowest amount in
	if poolAAmountIn.Cmp(poolBAmountIn) < 0 {
		return common.HexToAddress(PoolA), poolAAmountIn, limit, nil
	} else {
		return common.HexToAddress(PoolB), poolBAmountIn, limit, nil
	}
}

func getExactOutput(ctx context.Context, client *ethclient.Client, zeroForOne bool, amount, fee, limit *big.Int) (*big.Int, error) {
	quoterABI, err := abi.JSON(strings.NewReader(helper.HelperABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get Quoter ABI"), err)
	}

	var params helper.IQuoterV2QuoteExactOutputSingleParams
	if zeroForOne {
		params = helper.IQuoterV2QuoteExactOutputSingleParams{
			TokenIn:           common.HexToAddress(rETHAddressStr),
			TokenOut:          common.HexToAddress(WETHAddressStr),
			Amount:            amount,
			Fee:               fee,
			SqrtPriceLimitX96: big.NewInt(0), // No price limit
		}
	} else {
		params = helper.IQuoterV2QuoteExactOutputSingleParams{
			TokenIn:           common.HexToAddress(WETHAddressStr),
			TokenOut:          common.HexToAddress(rETHAddressStr),
			Amount:            amount,
			Fee:               fee,
			SqrtPriceLimitX96: big.NewInt(0), // No price limit
		}
	}

	// Pack the function call with parameters
	callData, err := quoterABI.Pack("quoteExactOutputSingle", params)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	quoterAddress := common.HexToAddress(QuoterAddressStr)
	msg := ethereum.CallMsg{
		To:   &quoterAddress,
		Data: callData,
	}

	// Perform the static call
	output, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make static call: %v", err)
	}

	var result struct {
		AmountIn                *big.Int
		SqrtPriceX96After       *big.Int
		InitializedTicksCrossed uint32
		GasEstimate             *big.Int
	}
	err = quoterABI.UnpackIntoInterface(&result, "quoteExactOutputSingle", output)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack output: %v", err)
	}

	if result.SqrtPriceX96After.Cmp(limit) > 0 {
		return nil, ErrPriceLimitExceeded
	}

	return result.AmountIn, nil
}

func getExactInput(ctx context.Context, client *ethclient.Client, zeroForOne bool, amount, fee, limit *big.Int) (*big.Int, error) {
	quoterABI, err := abi.JSON(strings.NewReader(helper.HelperABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get Quoter ABI"), err)
	}

	var params helper.IQuoterV2QuoteExactInputSingleParams
	if zeroForOne {
		params = helper.IQuoterV2QuoteExactInputSingleParams{
			TokenIn:           common.HexToAddress(rETHAddressStr),
			TokenOut:          common.HexToAddress(WETHAddressStr),
			AmountIn:          amount,
			Fee:               fee,
			SqrtPriceLimitX96: big.NewInt(0), // No price limit
		}
	} else {
		params = helper.IQuoterV2QuoteExactInputSingleParams{
			TokenIn:           common.HexToAddress(WETHAddressStr),
			TokenOut:          common.HexToAddress(rETHAddressStr),
			AmountIn:          amount,
			Fee:               fee,
			SqrtPriceLimitX96: big.NewInt(0), // No price limit
		}
	}

	// Pack the function call with parameters
	callData, err := quoterABI.Pack("quoteExactInputSingle", params)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	quoterAddress := common.HexToAddress(QuoterAddressStr)
	msg := ethereum.CallMsg{
		To:   &quoterAddress,
		Data: callData,
	}

	// Perform the static call
	output, err := client.CallContract(ctx, msg, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make static call: %v", err)
	}

	var result struct {
		AmountOut               *big.Int
		SqrtPriceX96After       *big.Int
		InitializedTicksCrossed uint32
		GasEstimate             *big.Int
	}
	err = quoterABI.UnpackIntoInterface(&result, "quoteExactInputSingle", output)
	if err != nil {
		return nil, fmt.Errorf("failed to unpack output: %v", err)
	}

	if result.SqrtPriceX96After.Cmp(limit) > 0 {
		return nil, ErrPriceLimitExceeded
	}

	return result.AmountOut, nil
}
