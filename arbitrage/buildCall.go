package arbitrage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os/exec"
	"rocketpoolArbitrage/arbitrage/contract"
	"rocketpoolArbitrage/rocketpoolContracts/minipoolDelegate"
	"rocketpoolArbitrage/rocketpoolContracts/rETH"
	uniswap "rocketpoolArbitrage/uniswapContracts"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"golang.org/x/sync/errgroup"

	"github.com/0xtrooper/flashbots_client"
)

const (
	BURN_CALL_MAX_GAS               = 200000 // roughly 140k to burn
	ARBITRAGE_UNISWAP_CALL_MAX_GAS  = 350000 // roughly uniswap 180k + burn 140k
	DISTRIBUTE_CALL_MAX_GAS         = 500000 // roughly 400k
	ARBITRAGE_PARASWAP_CALL_MAX_GAS = 750000 // in case there is a complicated path, reserve more gas
)

func BuildCallLocalReth(ctx context.Context, logger *slog.Logger, dataIn DataIn) (*flashbots_client.Bundle, *big.Int, *big.Int, error) {
	logger.With(slog.String("function", "BuildCallLocalReth"))

	baseGas, tipGas, err := getCurrentGasSettings(ctx, dataIn.Client, dataIn.Ratelimit)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to get current gas settings"), err)
	}

	baseGasBoosted := new(big.Int).Div(new(big.Int).Mul(baseGas, big.NewInt(150)), big.NewInt(100))

	if logger.Enabled(ctx, slog.LevelInfo) {
		baseGasFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(baseGas), new(big.Float).SetInt(big.NewInt(1e9))).Float64()
		tipGasFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(tipGas), new(big.Float).SetInt(big.NewInt(1e9))).Float64()
		baseGasBoostedFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(baseGasBoosted), new(big.Float).SetInt(big.NewInt(1e9))).Float64()

		fmt.Printf("Current gas settings: base fee per gas is %.4f gwei, tip is %.6f gwei.\n", baseGasFloat, tipGasFloat)
		fmt.Printf("Sending transaction with a base fee per gas of %.6f gwei for timely inclusion.\n\n", baseGasBoostedFloat)
	}

	nonce, err := getCurrentNonce(ctx, dataIn.Client, *dataIn.NodeAddress, dataIn.Ratelimit)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to get current nonce"), err)
	}

	txs, err := generateAndBuildDistributeCalls(
		dataIn.NetworkId,
		nonce,
		dataIn.MinipoolAddresses,
		baseGasBoosted,
		tipGas,
		logger,
		dataIn.Command,
		dataIn.NodeAddressPrivateKey,
	)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to generate distribute calls"), err)
	}

	rETHShare, err := CalcaulteDistributedBalance(ctx, logger, dataIn.Client, dataIn.MinipoolAddresses, dataIn.Ratelimit)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to calculate distributed balance"), err)
	}

	rEthContractAddress, err := GetREthContractAddress(dataIn.NetworkId)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to get rETH contract address"), err)
	}

	rethInstance, err := rETH.NewRETH(rEthContractAddress, dataIn.Client)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to create rETH instance"), err)
	}

	// always print the rETH burn overwite message, its recomended to simulate these transactions on tenderly
	fmt.Println(string(colorRed), "If you want to use tenderly to simulate the arbitrage, you need to overwrite the state for the final transaction:")
	fmt.Printf("    - Set the ETH balance of the rETH contract (%s) to %s\n", rEthContractAddress.Hex(), rETHShare.String())
	fmt.Println(string(colorReset))

	// calcaulte amount rETH to burn
	rethToBurn, err := ConvertWethToReth(ctx, rethInstance, rETHShare)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to convert rETH to WETH"), err)
	}
	if dataIn.Ratelimit > 0 {
		time.Sleep(time.Duration(dataIn.Ratelimit) * time.Millisecond)
	}

	logger.Debug("calculated rETH to burn", slog.String("rethToBurn", rethToBurn.String()))

	nextNonce := nonce + uint64(len(txs))
	rawBurnTx, err := generateBurnCall(rEthContractAddress, dataIn.NetworkId, nextNonce, rethToBurn, baseGasBoosted, tipGas)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to generate burn call"), err)
	}

	signedBurnTx, err := signTransaction(logger, dataIn.Command, dataIn.NodeAddressPrivateKey, rawBurnTx)
	if err != nil {
		return nil, nil, nil, errors.Join(errors.New("failed to sign burn tx"), err)
	}

	logger.Debug("signed burn tx", slog.String("txHash", signedBurnTx.Hash().Hex()))
	txs = append(txs, signedBurnTx)

	if dataIn.Ratelimit > 0 {
		time.Sleep(time.Duration(dataIn.Ratelimit*4) * time.Millisecond)
	}
	bundle := flashbots_client.NewBundleWithTransactions(txs)

	return bundle, rethToBurn, rETHShare, nil
}

func BuildCall(ctx context.Context, logger *slog.Logger, dataIn DataIn) (*flashbots_client.Bundle, *big.Int, error) {
	logger.With(slog.String("function", "BuildCall"))

	baseGas, tipGas, err := getCurrentGasSettings(ctx, dataIn.Client, dataIn.Ratelimit)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to get current gas settings"), err)
	}

	baseGasBoosted := new(big.Int).Div(new(big.Int).Mul(baseGas, big.NewInt(150)), big.NewInt(100))

	if logger.Enabled(ctx, slog.LevelInfo) {
		baseGasFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(baseGas), new(big.Float).SetInt(big.NewInt(1e9))).Float64()
		tipGasFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(tipGas), new(big.Float).SetInt(big.NewInt(1e9))).Float64()
		baseGasBoostedFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(baseGasBoosted), new(big.Float).SetInt(big.NewInt(1e9))).Float64()

		fmt.Printf("Current gas settings: base fee per gas is %.6f gwei, tip is %.6f gwei.\n", baseGasFloat, tipGasFloat)
		fmt.Printf("Sending transaction with a base fee per gas of %.6f gwei for timely inclusion.\n\n", baseGasBoostedFloat)
	}

	nonce, err := getCurrentNonce(ctx, dataIn.Client, *dataIn.NodeAddress, dataIn.Ratelimit)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to get current nonce"), err)
	}

	txs, err := generateAndBuildDistributeCalls(
		dataIn.NetworkId,
		nonce,
		dataIn.MinipoolAddresses,
		baseGasBoosted,
		tipGas,
		logger,
		dataIn.Command,
		dataIn.NodeAddressPrivateKey,
	)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to generate distribute calls"), err)
	}

	uniswapData, paraswapData, err := CalcualteArbitrageData(
		ctx,
		logger,
		dataIn.Client,
		dataIn.NodeAddress,
		dataIn.MinipoolAddresses,
		dataIn.NetworkId,
		dataIn.DryRun,
		dataIn.Ratelimit,
		dataIn.Protocol,
	)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to calculate arbitrage data"), err)
	}

	disributeFee := len(dataIn.MinipoolAddresses) * DISTRIBUTE_CALL_MAX_GAS

	if uniswapData != nil {
		uniswapData.expectedFee = disributeFee + ARBITRAGE_UNISWAP_CALL_MAX_GAS
		uniswapData.expectedProfitAfterFees = new(big.Int).Sub(uniswapData.expectedProfit, big.NewInt(int64(uniswapData.expectedFee)))
	} else {
		uniswapData = &UniswapArbitrage{
			expectedProfitAfterFees: big.NewInt(0),
		}
	}
	if paraswapData != nil {
		paraswapData.expectedFee = disributeFee + ARBITRAGE_PARASWAP_CALL_MAX_GAS
		paraswapData.expectedProfitAfterFees = new(big.Int).Sub(paraswapData.expectedProfit, big.NewInt(int64(paraswapData.expectedFee)))
	}

	uniswapIsBetter := uniswapData.expectedProfitAfterFees.Cmp(paraswapData.expectedProfitAfterFees) >= 0

	if logger.Enabled(ctx, slog.LevelInfo) {
		if dataIn.Protocol == UniswapProtocol || dataIn.Protocol == BestProtocol {
			amountWethFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(uniswapData.swapInAmountWeth), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			amountRethFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(uniswapData.swapOutAmountReth), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			profitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(uniswapData.expectedProfitAfterFees), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			secondaryRatio := amountWethFloat / amountRethFloat
			// update user about the secondary ratio
			fmt.Printf("Uniswap: Swapping %.6f WETH to %.6f rETH at a secondary ratio of %.5f with an expected profit of %.6f. (pool %s)\n",
				amountWethFloat,
				amountRethFloat,
				secondaryRatio,
				profitFloat,
				uniswapData.poolAddress.String(),
			)
		}
		if dataIn.Protocol == ParaswapProtocol || dataIn.Protocol == BestProtocol {
			amountWethFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(paraswapData.swapInAmountWeth), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			amountRethFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(paraswapData.swapOutAmountReth), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			profitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(paraswapData.expectedProfitAfterFees), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			secondaryRatio := amountWethFloat / amountRethFloat
			// update user about the secondary ratio
			fmt.Printf("Paraswap: Swapping %.6f WETH to %.6f rETH at a secondary ratio of %.5f with an expected profit of %.6f.\n",
				amountWethFloat,
				amountRethFloat,
				secondaryRatio,
				profitFloat,
			)
		}
		if dataIn.Protocol == BestProtocol {
			if uniswapIsBetter {
				fmt.Println("Uniswap is better, will use Uniswap.")
			} else {
				fmt.Println("Paraswap is better, will use Paraswap.")
			}
		}
		fmt.Println() // newline
	}

	arbitrageContractAddress, err := GetArbitrageContractAddress(dataIn.NetworkId)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to get arbitrage contract address"), err)
	}

	var expectedProfit *big.Int
	nextNonce := nonce + uint64(len(txs))
	logger.Debug("signed distribute txs", slog.Int("count", len(txs)))
	if dataIn.Protocol == UniswapProtocol || (dataIn.Protocol == BestProtocol && uniswapIsBetter) {
		expectedProfit = new(big.Int).Sub(uniswapData.expectedProfit, big.NewInt(int64(uniswapData.expectedFee)))

		var minProfit *big.Int
		if dataIn.CheckProfit {
			// add 95% of the profit to the min profit
			minProfit = new(big.Int).Div(new(big.Int).Mul(expectedProfit, big.NewInt(95)), big.NewInt(100))
		} else {
			if logger.Enabled(ctx, slog.LevelInfo) {
				fmt.Println("Ignoring distribute cost, will distribute regardless of profit.")
			}
			minProfit = big.NewInt(0)
		}

		if logger.Enabled(ctx, slog.LevelDebug) {
			minProfitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(minProfit), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			logger.Debug("calculated min profit", slog.Float64("minProfit", minProfitFloat))
		}

		rawArbitrageTx, err := generateArbitrageCall(
			dataIn.NetworkId,
			nextNonce,
			uniswapData,
			minProfit,
			baseGasBoosted,
			tipGas,
			arbitrageContractAddress,
			*dataIn.ReceiverAddress,
		)
		if err != nil {
			return nil, nil, errors.Join(errors.New("failed to generate arbitrage call"), err)
		}

		signedArbitrageTx, err := signTransaction(logger, dataIn.Command, dataIn.NodeAddressPrivateKey, rawArbitrageTx)
		if err != nil {
			return nil, nil, errors.Join(errors.New("failed to sign arbitrage tx"), err)
		}

		logger.Debug("signed arbitrage tx", slog.String("txHash", signedArbitrageTx.Hash().Hex()))
		txs = append(txs, signedArbitrageTx)
	} else if dataIn.Protocol == ParaswapProtocol || (dataIn.Protocol == BestProtocol && !uniswapIsBetter) {
		expectedProfit = new(big.Int).Sub(paraswapData.expectedProfit, big.NewInt(int64(paraswapData.expectedFee)))

		var minProfit *big.Int
		if dataIn.CheckProfit {
			// add 95% of the profit to the min profit
			minProfit = new(big.Int).Div(new(big.Int).Mul(expectedProfit, big.NewInt(95)), big.NewInt(100))
		} else {
			if logger.Enabled(ctx, slog.LevelInfo) {
				fmt.Println("Ignoring distribute cost, will distribute regardless of profit.")
			}
			minProfit = big.NewInt(0)
		}

		if logger.Enabled(ctx, slog.LevelDebug) {
			minProfitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(minProfit), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			logger.Debug("calculated min profit", slog.Float64("minProfit", minProfitFloat))
		}

		rawArbitrageTx, err := generateParaswapArbitrageCall(
			dataIn.NetworkId,
			nextNonce,
			paraswapData,
			minProfit,
			baseGasBoosted,
			tipGas,
			arbitrageContractAddress,
			*dataIn.ReceiverAddress,
		)
		if err != nil {
			return nil, nil, errors.Join(errors.New("failed to generate paraswap call"), err)
		}

		signedParaswapTx, err := signTransaction(logger, dataIn.Command, dataIn.NodeAddressPrivateKey, rawArbitrageTx)
		if err != nil {
			return nil, nil, errors.Join(errors.New("failed to sign paraswap tx"), err)
		}

		logger.Debug("signed arbitrage tx", slog.String("txHash", signedParaswapTx.Hash().Hex()))
		txs = append(txs, signedParaswapTx)
	} else {
		fmt.Println("Protocol picked: ", dataIn.Protocol)
		fmt.Println("Uniswap is better: ", uniswapIsBetter)
		return nil, nil, errors.New("invalid protocol")
	}

	if dataIn.Ratelimit > 0 {
		time.Sleep(time.Duration(dataIn.Ratelimit*4) * time.Millisecond)
	}
	bundle := flashbots_client.NewBundleWithTransactions(txs)

	return bundle, expectedProfit, nil
}

func CalcaulteDistributedBalance(ctx context.Context, logger *slog.Logger, client *ethclient.Client, minipoolAddresses []common.Address, ratelimit int) (*big.Int, error) {
	totalNodeShare := new(big.Int)
	totalDistributeAmount := new(big.Int)
	for _, minipoolAddress := range minipoolAddresses {
		balance, err := client.BalanceAt(ctx, minipoolAddress, nil)
		if err != nil {
			return nil, errors.Join(errors.New("failed to get minipool balance"), err)
		}

		logger.Debug("fetched minipool balance",
			slog.String("minipool", minipoolAddress.Hex()),
			slog.String("balance", balance.String()),
		)

		minipoolInstance, err := minipoolDelegate.NewMinipoolDelegate(minipoolAddress, client)
		if err != nil {
			return nil, err
		}

		refundBalance, err := GetMinipoolRefundBalance(ctx, minipoolInstance)
		if err != nil {
			return nil, errors.Join(errors.New("failed to get refund balance"), err)
		}
		if ratelimit > 0 {
			time.Sleep(time.Duration(ratelimit) * time.Millisecond)
		}

		distributeAmount := new(big.Int).Sub(balance, refundBalance)

		logger.Debug("fetched refund balance",
			slog.String("minipool", minipoolAddress.Hex()),
			slog.String("refundBalance", refundBalance.String()),
			slog.String("distributeAmount", distributeAmount.String()),
		)

		rETHShare, err := CalculateMinipoolUserShare(ctx, minipoolInstance, distributeAmount)
		if err != nil {
			return nil, errors.Join(errors.New("failed to calculate rETH share"), err)
		}
		if ratelimit > 0 {
			time.Sleep(time.Duration(ratelimit) * time.Millisecond)
		}

		nodeShare := new(big.Int).Sub(balance, rETHShare)

		totalDistributeAmount = new(big.Int).Add(totalDistributeAmount, rETHShare)
		totalNodeShare = new(big.Int).Add(totalNodeShare, nodeShare)
	}

	totalNodeShareFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(totalNodeShare), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
	rETHShareFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(totalDistributeAmount), new(big.Float).SetInt(big.NewInt(1e18))).Float64()

	if logger.Enabled(ctx, slog.LevelInfo) {
		fmt.Printf("Calculated distribution amounts: %.6f ETH sent to NO, %.6f ETH sent to rETH contract.\n\n", totalNodeShareFloat, rETHShareFloat)
	}

	return totalDistributeAmount, nil
}

func CalcualteArbitrageData(
	ctx context.Context,
	logger *slog.Logger,
	client *ethclient.Client,
	senderAddress *common.Address,
	minipoolAddresses []common.Address,
	networkId uint64,
	dryRun bool,
	ratelimit int,
	protocol Protocol,
) (*UniswapArbitrage, *ParaswapArbitrage, error) {
	rEthContractAddress, err := GetREthContractAddress(networkId)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to get rETH contract address"), err)
	}

	rethInstance, err := rETH.NewRETH(rEthContractAddress, client)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to create rETH instance"), err)
	}

	rETHShare, err := CalcaulteDistributedBalance(ctx, logger, client, minipoolAddresses, ratelimit)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to calculate distributed balance"), err)
	}

	if rETHShare.Cmp(big.NewInt(1e9)) <= 0 {
		return nil, nil, errors.New("rETH share is too low, make sure the minipools have any ETH to distribute")
	}

	if dryRun {
		fmt.Println(string(colorRed), "If you want to use tenderly to simulate the arbitrage, you need to overwrite the state for the final transaction:")
		fmt.Printf("    - Set the ETH balance of the rETH contract (%s) to %s\n", rEthContractAddress.Hex(), rETHShare.String())
		fmt.Println(string(colorReset))
	}

	// calcaulte amount rETH to burn
	rethToBurn, err := ConvertWethToReth(ctx, rethInstance, rETHShare)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to convert rETH to WETH"), err)
	}
	if ratelimit > 0 {
		time.Sleep(time.Duration(ratelimit) * time.Millisecond)
	}

	logger.Debug("calculated rETH to burn", slog.String("rethToBurn", rethToBurn.String()))

	// fetch paraswap data
	dataParaswap, err := fetchParaswapData(ctx, logger, rethToBurn, senderAddress, rEthContractAddress, networkId)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to fetch paraswap data"), err)
	}

	dataParaswap.expectedProfit = new(big.Int).Sub(rETHShare, dataParaswap.swapInAmountWeth)

	primaryRatio := new(big.Float).Quo(new(big.Float).SetInt(rETHShare), new(big.Float).SetInt(rethToBurn))

	// get best pool to swap rETH
	poolAddress, uniswapReturnAmountWeth, sqrtPriceLimitX96, err := uniswap.GetBestPoolWithdrawArb(ctx, logger, networkId, client, rethToBurn, primaryRatio, ratelimit)
	if err != nil {
		if errors.Is(err, uniswap.ErrPriceLimitExceeded) {
			if protocol == ParaswapProtocol {
				return nil, dataParaswap, nil
			} else {
				return nil, nil, errors.New("uniswap liquidity exceeded. Try with a smaller amount or use \"--protocol paraswap\"")
			}
		} else {
			return nil, nil, errors.Join(errors.New("failed to get best pool"), err)
		}
	}

	if logger.Enabled(ctx, slog.LevelInfo) {
		rethToBurnFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(rethToBurn), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		ethUnlockedFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(rETHShare), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		primaryRatio := ethUnlockedFloat / rethToBurnFloat

		// update user about the primary ratio
		fmt.Printf("Calculated rETH to burn: Burning %.6f rETH for %.6f ETH at a primary ratio of %.5f.\n\n",
			rethToBurnFloat,
			ethUnlockedFloat,
			primaryRatio,
		)
	}

	uniswapReturnAmountWeth = new(big.Int).Sub(uniswapReturnAmountWeth, big.NewInt(5))
	dataUniswap := &UniswapArbitrage{
		poolAddress:       poolAddress,
		swapInAmountWeth:  uniswapReturnAmountWeth,
		swapOutAmountReth: rethToBurn,
		sqrtPriceLimitX96: sqrtPriceLimitX96,
		expectedProfit:    new(big.Int).Sub(rETHShare, uniswapReturnAmountWeth),
	}

	return dataUniswap, dataParaswap, nil
}

func getCurrentGasSettings(ctx context.Context, client *ethclient.Client, ratelimit int) (baseGas *big.Int, tipGas *big.Int, err error) {
	// Get gas price
	timeoutCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	var eg errgroup.Group
	eg.Go(func() error {
		var err error
		baseGas, err = client.SuggestGasPrice(timeoutCtx)
		if err != nil {
			return errors.Join(errors.New("failed to get suggested gas price"), err)
		} else {
			return nil
		}
	})

	eg.Go(func() error {
		var err error
		tipGas, err = client.SuggestGasTipCap(timeoutCtx)
		if err != nil {
			return errors.Join(errors.New("failed to get gas tip cap"), err)
		} else {
			return nil
		}
	})

	if ratelimit > 0 {
		time.Sleep(time.Duration(2*ratelimit) * time.Millisecond)
	}

	return baseGas, tipGas, eg.Wait()
}

func getCurrentNonce(ctx context.Context, client *ethclient.Client, address common.Address, ratelimit int) (uint64, error) {
	nonce, err := client.PendingNonceAt(ctx, address)
	if err != nil {
		return 0, errors.Join(errors.New("failed to get current nonce"), err)
	}

	if ratelimit > 0 {
		time.Sleep(time.Duration(ratelimit) * time.Millisecond)
	}

	return nonce, nil
}

func generateAndBuildDistributeCalls(
	networkId, nonce uint64,
	minipoolAddresses []common.Address,
	baseGas, tipGas *big.Int,
	logger *slog.Logger,
	apiCommand string,
	privateKey *ecdsa.PrivateKey,
) ([]*types.Transaction, error) {
	var txs []*types.Transaction

	for i, minipoolAddress := range minipoolAddresses {
		rawTx, err := generateDistributeCall(networkId, nonce+uint64(i), minipoolAddress, baseGas, tipGas)
		if err != nil {
			return nil, errors.Join(errors.New("failed to generate distribute call"), err)
		}

		signedTx, err := signTransaction(logger, apiCommand, privateKey, rawTx)
		if err != nil {
			return nil, errors.Join(errors.New("failed to sign distribute tx"), err)
		}

		txs = append(txs, signedTx)
	}

	return txs, nil
}

func generateDistributeCall(chainId, nonce uint64, minipoolAddress common.Address, baseGas, tipGas *big.Int) (*types.Transaction, error) {
	minipoolAbi, err := abi.JSON(strings.NewReader(minipoolDelegate.MinipoolDelegateABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get minipool ABI"), err)
	}

	// Pack the function call with parameters
	callData, err := minipoolAbi.Pack("distributeBalance", false)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	dynTx := &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(chainId),
		Nonce:     nonce,
		GasFeeCap: baseGas,
		GasTipCap: tipGas,
		To:        &minipoolAddress,
		Value:     big.NewInt(0),
		Gas:       DISTRIBUTE_CALL_MAX_GAS,
		Data:      callData,
	}

	return types.NewTx(dynTx), nil
}

func generateArbitrageCall(chainId, nonce uint64, uniswapData *UniswapArbitrage, minProfit, baseGas, tipGas *big.Int, arbitrageContractAddress, receiver common.Address) (*types.Transaction, error) {
	arbitrageAbi, err := abi.JSON(strings.NewReader(contract.ContractABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get arbitrage ABI"), err)
	}

	callData, err := arbitrageAbi.Pack(
		"arb",
		uniswapData.poolAddress,
		uniswapData.sqrtPriceLimitX96,
		uniswapData.swapInAmountWeth,
		minProfit,
		receiver,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	dynTx := &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(chainId),
		Nonce:     nonce,
		GasFeeCap: baseGas,
		GasTipCap: tipGas,
		To:        &arbitrageContractAddress,
		Value:     big.NewInt(0),
		Gas:       ARBITRAGE_UNISWAP_CALL_MAX_GAS,
		Data:      callData,
	}

	return types.NewTx(dynTx), nil
}

func generateParaswapArbitrageCall(
	chainId, nonce uint64,
	paraswapData *ParaswapArbitrage,
	minProfit, baseGas, tipGas *big.Int,
	arbitrageContractAddress,
	receiver common.Address,
) (*types.Transaction, error) {
	arbitrageAbi, err := abi.JSON(strings.NewReader(contract.ContractABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get arbitrage ABI"), err)
	}

	// fmt.Println("Paraswap calldata:")
	// fmt.Println("    amount: ", paraswapData.swapInAmountWeth.String())
	// fmt.Println("    calldata: ", hex.EncodeToString(paraswapData.calldata))
	// fmt.Println("	 minProfit: ", minProfit.String())
	// fmt.Println("	 receiver: ", receiver.String())

	callData, err := arbitrageAbi.Pack("arbParaswap", paraswapData.swapInAmountWeth, paraswapData.calldata, minProfit, receiver)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	dynTx := &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(chainId),
		Nonce:     nonce,
		GasFeeCap: baseGas,
		GasTipCap: tipGas,
		To:        &arbitrageContractAddress,
		Value:     big.NewInt(0),
		Gas:       ARBITRAGE_PARASWAP_CALL_MAX_GAS,
		Data:      callData,
	}

	return types.NewTx(dynTx), nil
}

func generateBurnCall(rethContractAddress common.Address, chainId, nonce uint64, rethToBurn, baseGas, tipGas *big.Int) (*types.Transaction, error) {
	rEthAbi, err := abi.JSON(strings.NewReader(rETH.RETHABI))
	if err != nil {
		return nil, errors.Join(errors.New("failed to get arbitrage ABI"), err)
	}

	callData, err := rEthAbi.Pack("burn", rethToBurn)
	if err != nil {
		return nil, fmt.Errorf("failed to pack function data: %v", err)
	}

	dynTx := &types.DynamicFeeTx{
		ChainID:   new(big.Int).SetUint64(chainId),
		Nonce:     nonce,
		GasFeeCap: baseGas,
		GasTipCap: tipGas,
		To:        &rethContractAddress,
		Value:     big.NewInt(0),
		Gas:       BURN_CALL_MAX_GAS,
		Data:      callData,
	}

	return types.NewTx(dynTx), nil
}

func signTransaction(logger *slog.Logger, apiCommand string, privateKey *ecdsa.PrivateKey, tx *types.Transaction) (signedTx *types.Transaction, err error) {
	if privateKey != nil {
		signedTx, err = types.SignTx(tx, types.LatestSignerForChainID(tx.ChainId()), privateKey)
		if err != nil {
			return nil, errors.Join(errors.New("failed to sign tx using private key"), err)
		}
	} else if apiCommand != "" {
		signedTx, err = useSmartnodeDaemon(logger, apiCommand, tx)
		if err != nil {
			return nil, errors.Join(errors.New("failed to sign tx using smartnode daemon"), err)
		}
	} else {
		return nil, errors.New("no signing method provided")
	}
	return signedTx, nil
}

func useSmartnodeDaemon(logger *slog.Logger, apiCommand string, tx *types.Transaction) (*types.Transaction, error) {
	serializedTx, err := tx.MarshalBinary()
	if err != nil {
		return nil, errors.Join(errors.New("failed to serialize tx"), err)
	}

	args := strings.Fields(apiCommand) // split on whitespace, not commas
	args = append(args, "api", "node", "sign", hex.EncodeToString(serializedTx))

	cmd := exec.Command(args[0], args[1:]...)

	logger.Debug("signing tx", slog.String("cmd", strings.Join(args, " ")))
	stdout, err := cmd.Output()

	if err != nil {
		return nil, errors.Join(errors.New("failed to sign tx"), err)
	}

	var result struct {
		Status     string `json:"status"`
		Error      string `json:"error"`
		SignedData string `json:"signedData"`
	}

	if err := json.Unmarshal(stdout, &result); err != nil {
		return nil, errors.Join(errors.New("failed to parse JSON response"), err)
	}

	if result.Status != "success" {
		return nil, fmt.Errorf("failed to sign tx: %s", result.Error)
	}

	parsedData, err := hex.DecodeString(result.SignedData[2:])
	if err != nil {
		return nil, errors.Join(errors.New("failed to decode signed tx"), err)
	}

	signedTx := types.Transaction{}
	if err := signedTx.UnmarshalBinary(parsedData); err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal signed tx"), err)
	}

	return &signedTx, nil
}

func fetchParaswapData(
	ctx context.Context,
	logger *slog.Logger,
	amount *big.Int,
	senderAddress *common.Address,
	rEthContractAddress common.Address,
	networkId uint64,
) (*ParaswapArbitrage, error) {
	WETHContractAddress, err := GetWETHContractAddress(networkId)
	if err != nil {
		return nil, errors.Join(errors.New("failed to get WETH contract address"), err)
	}

	arbitrageContractAddress, err := GetArbitrageContractAddress(networkId)
	if err != nil {
		return nil, errors.Join(errors.New("failed to get arbitrage contract address"), err)
	}

	ParaswapV6_2Address, err := GetParaswapV6_2Address(networkId)
	if err != nil {
		return nil, errors.Join(errors.New("failed to get paraswap v6.2 address"), err)
	}

	urlPrices := fmt.Sprintf("https://api.paraswap.io/prices?network=%d&version=6.2&slippage=50&srcToken=%s&srcDecimals=18&destToken=%s&destDecimals=18&amount=%s&side=BUY&userAddress=%s",
		networkId,
		WETHContractAddress.Hex(),
		rEthContractAddress.Hex(),
		amount.String(),
		arbitrageContractAddress.Hex(),
	)

	reqPrices, err := http.NewRequest("GET", urlPrices, nil)
	if err != nil {
		return nil, errors.Join(errors.New("failed to create request"), err)
	}

	reqPrices.Header.Add("Content-Type", "application/json")
	reqPrices.Header.Add("Accept", "application/json")

	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	respPrices, err := client.Do(reqPrices)
	if err != nil {
		return nil, errors.Join(errors.New("failed to send request"), err)
	}
	defer respPrices.Body.Close()

	if respPrices.StatusCode < http.StatusOK || respPrices.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected status code from prices API: %d", respPrices.StatusCode)
	}

	bodyPrices, err := io.ReadAll(respPrices.Body)
	if err != nil {
		return nil, errors.Join(errors.New("failed to read response body"), err)
	}

	var errResp ParaswapErrorResponse
	err = json.Unmarshal(bodyPrices, &errResp)
	if err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal error response body"), err)
	}

	if errResp.Error != "" {
		return nil, fmt.Errorf("paraswap API error: %s", errResp.Error)
	}

	var prices ParaswapPriceResponse
	err = json.Unmarshal(bodyPrices, &prices)
	if err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal response body"), err)
	}

	if logger.Enabled(ctx, slog.LevelDebug) {
		amountIn, _ := new(big.Int).SetString(prices.PriceRoute.SrcAmount, 10)
		amountOut, _ := new(big.Int).SetString(prices.PriceRoute.DestAmount, 10)

		amountInFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(amountIn), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		amountOutFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(amountOut), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
		ratio := amountInFloat / amountOutFloat

		logger.Debug("fetched paraswap prices, building transacton next",
			slog.Float64("amountIn", amountInFloat),
			slog.Float64("amountOut", amountOutFloat),
			slog.Float64("ratio", ratio),
		)
	}

	// Unmarshal bodyPrices to extract the inner "priceRoute"
	var rawResponse map[string]json.RawMessage
	err = json.Unmarshal(bodyPrices, &rawResponse)
	if err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal bodyPrices"), err)
	}

	// Extract the "priceRoute" field
	innerPriceRoute, ok := rawResponse["priceRoute"]
	if !ok {
		return nil, errors.New("priceRoute field not found in bodyPrices")
	}

	urlTransaction := fmt.Sprintf(
		"https://api.paraswap.io/transactions/%d?onlyParams=false&ignoreChecks=true&ignoreGasEstimate=true",
		networkId,
	)

	transactionsBody := ParaswapTransactionsBody{
		SrcToken:     prices.PriceRoute.SrcToken,
		SrcDecimals:  18,
		DestToken:    prices.PriceRoute.DestToken,
		DestDecimals: 18,
		SrcAmount:    prices.PriceRoute.SrcAmount,
		DestAmount:   prices.PriceRoute.DestAmount,
		PriceRoute:   innerPriceRoute,
		TxOrigin:     senderAddress.String(),
		UserAddress:  arbitrageContractAddress.Hex(),
	}

	transactionsBodyJSON, err := json.Marshal(transactionsBody)
	if err != nil {
		return nil, errors.Join(errors.New("failed to marshal transaction body"), err)
	}

	reqTransaction, err := http.NewRequest("POST", urlTransaction, bytes.NewBuffer(transactionsBodyJSON))
	if err != nil {
		return nil, errors.Join(errors.New("failed to create request"), err)
	}

	reqTransaction.Header.Add("Content-Type", "application/json")
	reqTransaction.Header.Add("Accept", "application/json")

	respTransaction, err := client.Do(reqTransaction)
	if err != nil {
		return nil, errors.Join(errors.New("failed to send request"), err)
	}
	defer respTransaction.Body.Close()

	if respTransaction.StatusCode < http.StatusOK || respTransaction.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("unexpected status code from transactions API: %d", respTransaction.StatusCode)
	}

	bodyTransaction, err := io.ReadAll(respTransaction.Body)
	if err != nil {
		return nil, errors.Join(errors.New("failed to read response body"), err)
	}

	err = json.Unmarshal(bodyTransaction, &errResp)
	if err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal error response body"), err)
	}

	if errResp.Error != "" {
		return nil, fmt.Errorf("paraswap API error: %s", errResp.Error)
	}

	var transactionsResponse ParaswapTransactionsResponse
	err = json.Unmarshal(bodyTransaction, &transactionsResponse)
	if err != nil {
		return nil, errors.Join(errors.New("failed to unmarshal response body"), err)
	}

	logger.Debug("fetched paraswap transaction data")

	if ParaswapV6_2Address.Cmp(common.HexToAddress(transactionsResponse.To)) != 0 {
		return nil, fmt.Errorf("invalid paraswap router address: %s, expected: %s", transactionsResponse.To, ParaswapV6_2Address.Hex())
	}

	if arbitrageContractAddress.Cmp(common.HexToAddress(transactionsResponse.From)) != 0 {
		return nil, fmt.Errorf("invalid paraswap from address: %s, expected: %s (arbitrage contract)", transactionsResponse.From, arbitrageContractAddress.Hex())
	}

	if transactionsResponse.Value != "0" {
		return nil, fmt.Errorf("invalid paraswap value: %s, expected: 0", transactionsResponse.Value)
	}

	swapInAmount, _ := new(big.Int).SetString(prices.PriceRoute.SrcAmount, 10)
	swapOutAmount, _ := new(big.Int).SetString(prices.PriceRoute.DestAmount, 10)
	dataParaswap := &ParaswapArbitrage{
		swapInAmountWeth:  swapInAmount,
		swapOutAmountReth: swapOutAmount,
		calldata:          common.FromHex(transactionsResponse.Data),
	}

	return dataParaswap, nil
}
