package arbitrage

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"rocketpoolArbitrage/rocketpoolContracts/storage"
	"strings"
	"time"
	"unicode"

	"log/slog"

	"github.com/0xtrooper/flashbots_client"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func ExecuteDistribute(ctx context.Context, logger *slog.Logger, dataIn *DataIn) error {
	logger.With(slog.String("function", "Simulate"))

	err := VerifyInputData(ctx, logger, dataIn)
	if err != nil {
		return errors.Join(errors.New("failed to verify input data"), err)
	}

	logger.Debug("verified input data")

	// get node withdraw address
	isWithdrawalAddress := false
	isNodeAddress := false
	if dataIn.ReceiverAddress == nil {
		withdrawalAddress, err := getWithdrawalAddress(ctx, dataIn.Client, dataIn.NetworkId, *dataIn.NodeAddress, dataIn.Ratelimit)
		if err != nil {
			return errors.Join(errors.New("failed to get withdrawal address"), err)
		}

		logger.Debug("receiver address not set, updated to withdraw address", slog.String("receiverAddress", withdrawalAddress.Hex()))
		dataIn.ReceiverAddress = &withdrawalAddress
		if withdrawalAddress.Cmp(*dataIn.NodeAddress) == 0 {
			isNodeAddress = true
		} else {
			isWithdrawalAddress = true
		}
	}

	// update user on receiver address
	if logger.Enabled(ctx, slog.LevelInfo) {
		fmt.Print("Any profit will be sent to ")
		fmt.Print(colorOrange, (*dataIn.ReceiverAddress).Hex(), colorReset)
		if isWithdrawalAddress {
			fmt.Println(". This should be your withdrawal address.")
		} else if isNodeAddress {
			fmt.Println(". This should be your node address.")
		} else {
			fmt.Println(". Set with the --receiver flag.")
		}
	}

	// if using a random private key, update the fee refund recipient to the receiver address
	if dataIn.RandomPrivateKey && dataIn.NetworkId != 17000 {
		err = dataIn.FbClient.UpdateFeeRefundRecipient(*dataIn.ReceiverAddress)
		if err != nil {
			return errors.Join(errors.New("failed to update flashbots fee refund recipient"), err)
		}
		if logger.Enabled(ctx, slog.LevelInfo) {
			fmt.Print("Updated flashbots fee refund recipient to ")
			fmt.Print(colorOrange, (*dataIn.ReceiverAddress).Hex(), colorReset)
			fmt.Println(".")
		}
	}

	// build bundle
	var bundle *flashbots_client.Bundle
	var rethToBurn, rETHShare, expectedProfit *big.Int
	if dataIn.LocalReth {
		bundle, rethToBurn, rETHShare, err = BuildCallLocalReth(ctx, logger, *dataIn)
		if err != nil {
			return errors.Join(errors.New("failed to build call"), err)
		}
	} else {
		bundle, expectedProfit, err = BuildCall(ctx, logger, *dataIn)
		if err != nil {
			return errors.Join(errors.New("failed to build call"), err)
		}
	}

	logger.Debug("created flashbots client")
	success, bundleHash, arbTxHash, err := simulateBundle(logger, dataIn, bundle)
	if err != nil {
		// handle known revert reasons, user was updated in the simulateBundle function
		if strings.EqualFold(err.Error(), "Paraswap failed") || strings.EqualFold(err.Error(), "Insufficient ETH balance for exchange") {
			return nil
		}
		return errors.Join(errors.New("failed to simulate bundle"), err)
	}

	maxBundleFees, maxArbitrageFees := evalGasPrices(bundle)

	// print update based on user selection
	if logger.Enabled(ctx, slog.LevelInfo) {
		if dataIn.LocalReth {
			rEthBurnedFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(rethToBurn), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			ethReceivedFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(rETHShare), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			expectedFeeFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(maxBundleFees), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			fmt.Print("Simulated bundle (")
			if success {
				fmt.Print(string(colorGreen), "success", string(colorReset))
			} else {
				fmt.Print(string(colorRed), "failed", string(colorReset))
			}
			fmt.Println("):")
			fmt.Printf("    Expected to burn %.6f rETH for %.6f ETH, with a tx fee of %.6f\n", rEthBurnedFloat, ethReceivedFloat, expectedFeeFloat)
		} else {
			maxBundleFeesFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(maxBundleFees), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			maxArbitrageFeesFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(maxArbitrageFees), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			expectedProfitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(expectedProfit), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
			fmt.Print("Simulated bundle (")
			if success {
				fmt.Print(string(colorGreen), "success", string(colorReset))
			} else {
				fmt.Print(string(colorRed), "failed", string(colorReset))
			}
			fmt.Println("):")
			fmt.Printf("    Expected profit after fees: %.6f, with a tx fee of %.6f\n", expectedProfitFloat-maxBundleFeesFloat, maxBundleFeesFloat)
			fmt.Printf("    Expected profit after arbitrage fees: %.6f, with a tx fee of %.6f (interesting if you want to distribute regardless)\n\n", expectedProfitFloat-maxArbitrageFeesFloat, maxArbitrageFeesFloat)
		}
	}

	// print txs:
	// - this will always be printed if the user is using local rETH to allow confirming the burn
	// - if dry-run is set, this will be printed regardless of the user's choice and the txs will not be sent
	if dataIn.DryRun || dataIn.LocalReth {
		if dataIn.DryRun {
			fmt.Println("Dry run. Would have sent the following bundle:")
		}

		txs := bundle.Transactions()
		for i, tx := range txs {
			baseGwei, _ := new(big.Float).Quo(new(big.Float).SetInt(tx.GasFeeCap()), new(big.Float).SetInt(big.NewInt(1e9))).Float64()
			tipGwei, _ := new(big.Float).Quo(new(big.Float).SetInt(tx.GasTipCap()), new(big.Float).SetInt(big.NewInt(1e9))).Float64()

			fmt.Printf("Transaction %d:\n", i+1)
			fmt.Printf("    From: %s\n", dataIn.NodeAddress.Hex())
			fmt.Printf("    To: %s\n", tx.To().Hex())
			fmt.Printf("    Value: %s\n", tx.Value().String())
			fmt.Printf("    Gas Limit: %d\n", tx.Gas())
			fmt.Printf("    Base Fee: %s (%.4f Gwei)\n", tx.GasFeeCap().String(), baseGwei)
			fmt.Printf("    Priority Fee: %s (%.6f Gwei)\n", tx.GasTipCap().String(), tipGwei)
			fmt.Printf("    Nonce: %d\n", tx.Nonce())
			fmt.Printf("    Data: %s\n", hex.EncodeToString(tx.Data()))
		}

		// only return if dry-run is set
		if dataIn.DryRun {
			return nil
		}
	}

	// end the attempt if the simulation failed - after printing the dryrun if it was requested!
	if !success {
		return errors.New("bundle simulation failed")
	}

	// this checks if a bundle makes sense to make arbitrage profits
	if dataIn.CheckProfit && !dataIn.CheckProfitIgnoreDistributeCost && expectedProfit.Cmp(maxBundleFees) < 0 {
		return errors.New("expected profit is less than max bundle fees")
	}

	// this checks if a bundle makes sense if the user wants to distribute
	// aka. distribute gas needs to be paid one way or another
	if dataIn.CheckProfit && dataIn.CheckProfitIgnoreDistributeCost && expectedProfit.Cmp(maxArbitrageFees) < 0 {
		return errors.New("expected profit is less than max arbitrage fees")
	}

	// ask for user confirmation
	if !dataIn.SkipConfirmation && !waitForUserConfirmation(dataIn.LocalReth) {
		return errors.New("user did not confirm to proceed")
	}

	// add more builders to improve chance to be included
	bundle.UseAllBuilders(dataIn.NetworkId)

	// set target block number
	blockNumber, err := dataIn.Client.BlockNumber(ctx)
	if err != nil {
		return errors.Join(errors.New("failed to get block number"), err)
	}
	bundle.SetTargetBlockNumber(blockNumber + 1)

	fmt.Printf("\nSent bundle with hash: %s. Waiting for up to one minute to see if the transaction is included...\n\n", bundleHash)

	timeoutContext, cancel := context.WithTimeout(ctx, time.Second*70)
	successfullyIncluded, err := dataIn.FbClient.SendNBundleAndWait(timeoutContext, bundle, 4)
	cancel()
	if err != nil {
		return errors.Join(errors.New("failed to wait for bundle inclusion"), err)
	}

	if !successfullyIncluded {
		fmt.Println(string(colorRed), "Error: Bundle was not included in the mempool.", string(colorReset))
		fmt.Println("This can happen at times of high activity. Please try again later.")
		fmt.Println("If the issue keeps happening, consider raising a github issue.")
		return nil
	}

	// print successful inclusion and tx link
	var txType string
	if dataIn.LocalReth {
		txType = "Burn"
	} else {
		txType = "Arbitrage"
	}

	explorer := ""
	if dataIn.NetworkId == 1 {
		explorer = "https://etherscan.io/tx/"
	} else if dataIn.NetworkId == 17000 {
		explorer = "https://explorer.holesky.io/tx/"
	}
	if len(bundle.Transactions()) == 2 {
		fmt.Printf("Distributed minipool! %s tx: %s%s\n\n", txType, explorer, arbTxHash.Hex())
	} else {
		fmt.Printf("Distributed minipools! %s tx: %s%s\n\n", txType, explorer, arbTxHash.Hex())
	}

	return nil
}

func evalGasPrices(bundle *flashbots_client.Bundle) (bundleGasPrice, arbitrageGasPrice *big.Int) {
	bundleGasPrice = bundle.MaximumGasFeePaid()

	txs := bundle.Transactions()
	arbTx := txs[len(txs)-1]

	return bundleGasPrice, arbTx.Cost()
}

func waitForUserConfirmation(isUsingLocalReth bool) bool {
	if isUsingLocalReth {
		fmt.Println(string(colorRed), "\nSince you're using your own rETH, this transaction is NOT time-sensitive.")
		fmt.Println("Feel free to review and confirm the transactions above at your own pace. For instance by using Tenderly.")
		fmt.Println("For detailed instructions on how to simulate these transactions, visit:")
		fmt.Println("https://github.com/0xtrooper/RocketpoolExitArbitrage?tab=readme#how-to-simulate")
		fmt.Println(string(colorReset))
	}

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Do you want to proceed? (y/n): ")
	response, err := reader.ReadString('\n')
	if err != nil {
		log.Fatal(err)
	}
	response = strings.TrimSpace(response)
	switch strings.ToLower(response) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	default:
		fmt.Println("Invalid input. Please type 'y' or 'n'.")
		return waitForUserConfirmation(isUsingLocalReth)
	}
}

func getWithdrawalAddress(ctx context.Context, client *ethclient.Client, networkId uint64, nodeAddress common.Address, ratelimit int) (common.Address, error) {
	rocketpoolStorageAddress, err := GetRocketpoolStorageAddress(networkId)
	if err != nil {
		return common.Address{}, errors.Join(errors.New("failed to get rocketpool storage address"), err)
	}

	storageInterface, err := storage.NewStorage(rocketpoolStorageAddress, client)
	if err != nil {
		return common.Address{}, errors.Join(errors.New("failed to create storage contract instance"), err)
	}

	timoutCtx, cancel := context.WithTimeout(ctx, time.Second*5)
	defer cancel()

	opts := &bind.CallOpts{
		Pending: false,
		Context: timoutCtx,
	}

	address, err := storageInterface.GetNodeWithdrawalAddress(opts, nodeAddress)
	if err != nil {
		return common.Address{}, errors.Join(errors.New("failed to get node withdrawal address"), err)
	}
	if ratelimit > 0 {
		time.Sleep(time.Duration(ratelimit) * time.Millisecond)
	}

	return address, nil
}

func simulateBundle(logger *slog.Logger, dataIn *DataIn, bundle *flashbots_client.Bundle) (bool, common.Hash, common.Hash, error) {
	simulationStateBlock, err := dataIn.Client.BlockNumber(context.Background())
	if err != nil {
		return false, common.Hash{}, common.Hash{}, errors.Join(errors.New("failed to get block number"), err)
	}

	res, success, err := dataIn.FbClient.SimulateBundle(bundle, simulationStateBlock)
	// If there was an error and it's the special "header not found" error, try with previous block
	if err != nil && strings.Contains(err.Error(), "header not found") {
		logger.Debug("header not found, retrying", slog.Uint64("previous block", simulationStateBlock-1))
		res, success, err = dataIn.FbClient.SimulateBundle(bundle, simulationStateBlock-1)
	}
	if err != nil {
		return false, common.Hash{}, common.Hash{}, err
	}

	for index, tx := range res.Results {
		if tx.Error != "" {
			parsedMsg := sanitizeString(tx.RevertReason)
			// handle known revert reasons
			if strings.EqualFold(parsedMsg, "Paraswap failed") || strings.EqualFold(parsedMsg, "Insufficient ETH balance for exchange") {
				fmt.Printf("\nError simulating transaction: %s\n", parsedMsg)
				fmt.Println("This issue is often caused by significant price movements or high MEV bot activity.")
				fmt.Println("Please try again shortly.")

				return false, common.Hash{}, common.Hash{}, errors.New(parsedMsg)
			}

			logger.Warn("tx failed",
				slog.Int("index", index),
				slog.String("error", tx.Error),
				slog.String("revertReason", hex.EncodeToString([]byte(tx.RevertReason))),
				slog.String("parsedRevertReason", parsedMsg),
			)

			success = false
		}
	}

	return success, res.BundleHash, res.Results[len(res.Results)-1].TxHash, nil
}

// best effort to sanitize revert reasons
// the revert reason gets messed up upstream
// to avoid a big refactor, this attempts a best-effort sanitization
func sanitizeString(s string) string {
	var b strings.Builder
	for _, r := range s {
		// Only include valid ASCII characters
		if r >= 32 && r <= 126 {
			b.WriteRune(r)
		}
	}

	// Find the first two letter or digit sequence
	sanitized := b.String()
	runes := []rune(sanitized)
	for i := 0; i < len(runes); i++ {
		if unicode.IsLetter(runes[i]) || unicode.IsDigit(runes[i]) {
			count := 1
			// Count consecutive letters/digits starting at i.
			for j := i + 1; j < len(runes); j++ {
				if unicode.IsLetter(runes[j]) || unicode.IsDigit(runes[j]) {
					count++
				} else {
					break
				}
			}
			if count >= 2 {
				return strings.TrimSpace(string(runes[i:]))
			}
		}
	}
	return sanitized
}
