package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"rocketpoolArbitrage/arbitrage"
	"rocketpoolArbitrage/beaconchain"
	"rocketpoolArbitrage/rocketpoolContracts/rETH"
	uniswap "rocketpoolArbitrage/uniswapContracts"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

func main() {
	nodeAddress, eth1Url, eth2Url, err := parseInput()
	if err != nil {
		fmt.Println(err)
		return
	}

	data, err := beaconchain.GetBeaconchainData(nodeAddress, eth2Url)
	if err != nil {
		fmt.Println(err)
		return
	}

	ctx := context.Background()
	slog.SetLogLoggerLevel(slog.LevelError)
	logger := slog.Default()

	client, err := ethclient.DialContext(ctx, eth1Url)
	if err != nil {
		fmt.Println(err)
		return
	}

	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		fmt.Println(err)
		return
	}

	feePaidSingle := new(big.Int).Mul(gasPrice, big.NewInt(int64(arbitrage.DISTRIBUTE_CALL_MAX_GAS+arbitrage.ARBITRAGE_PARASWAP_CALL_MAX_GAS)))
	feePaidSingleFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(feePaidSingle), new(big.Float).SetInt(big.NewInt(1e18))).Float64()

	fmt.Println("Example profits:")
	halfPool, err := getExampleProfits(ctx, logger, client, 16000) // mETH
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("If you exit a 16 ETH minipool, the amount distributed is 16 ETH and the expected profit is %.6f ETHwith a fee of %.6f \n", halfPool, feePaidSingleFloat)

	leb8Pool, err := getExampleProfits(ctx, logger, client, 24000) // mETH
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("If you exit a 8 ETH minipool, the amount distributed is 24 ETH and the expected profit is %.6f ETH with a fee of %.6f\n", leb8Pool, feePaidSingleFloat)

	distributeA, err := getExampleProfits(ctx, logger, client, 200) // mETH
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("If you distribute a minipool with 0.2 ETH, the expected profit is %.6f ETH with a fee of %.6f\n", distributeA, feePaidSingleFloat)

	distributeB, err := getExampleProfits(ctx, logger, client, 100) // mETH
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Printf("If you distribute a minipool with 0.1 ETH, the expected profit is %.6f ETH with a fee of %.6f\n\n", distributeB, feePaidSingleFloat)

	count := 0
	withdrawnBalance := big.NewInt(0)
	for _, minipool := range data.Minipools {
		balance, err := client.BalanceAt(ctx, minipool.WithdrawalAddress, nil)
		if err != nil {
			fmt.Println(err)
			return
		}

		if balance.Cmp(big.NewInt(0)) > 0 {
			withdrawnBalance.Add(withdrawnBalance, balance)
			count++
		}
	}

	gasUsed := count*arbitrage.DISTRIBUTE_CALL_MAX_GAS + arbitrage.ARBITRAGE_PARASWAP_CALL_MAX_GAS
	feePaid := new(big.Int).Mul(gasPrice, big.NewInt(int64(gasUsed)))

	rethShare, allMinipoolsProfit, err := estimateProfit(ctx, logger, client, nodeAddress, data)
	if err != nil {
		fmt.Println(err)
		return
	}

	withdrawnBalanceFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(withdrawnBalance), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
	rethShareFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(rethShare), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
	allMinipoolsProfitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(allMinipoolsProfit), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
	feePaidFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(feePaid), new(big.Float).SetInt(big.NewInt(1e18))).Float64()
	fmt.Printf("You currently have %.4f ETH withdrawn.\nIf you distribute that, %.6f ETH will be send to rETH and the expected profit is %.6f ETH with a fee of %.6f\n",
		withdrawnBalanceFloat,
		rethShareFloat,
		allMinipoolsProfitFloat,
		feePaidFloat,
	)
}

func parseInput() (common.Address, string, string, error) {
	nodeAddressFlag := flag.String("nodeAddress", "", "Node address")
	rpcEth1Flag := flag.String("rpc-eth1", "http://localhost:8545", "Ethereum RPC endpoint for all on-chain calls. (default: http://localhost:8545)")
	rpcEth1PortFlag := flag.String("rpc-eth1-port", "8545", "If using localhost but on a non-default port, override the port here.")
	rpcEth2Flag := flag.String("rpc-eth2", "http://localhost:5052", "Ethereum Beacon Node RPC endpoint for all on-chain calls. (default: http://localhost:5052)")
	rpcEth2PortFlag := flag.String("rpc-eth2-port", "5052", "If using localhost but on a non-default port, override the port here.")

	flag.Parse()

	var nodeAddress common.Address
	if *nodeAddressFlag != "" {
		if !common.IsHexAddress(*nodeAddressFlag) {
			return common.Address{}, "", "", errors.New("invalid node address")
		}

		nodeAddress = common.HexToAddress(*nodeAddressFlag)
	}

	var eth1Url string
	if *rpcEth1PortFlag == "http://localhost:8545" {
		if *rpcEth1PortFlag != "8545" {
			eth1Url = "http://localhost:" + *rpcEth1PortFlag
		} else {
			eth1Url = *rpcEth1Flag
		}
	} else {
		// user should set the full url in the --rpc flag, check that they didn't set --rpcPort
		if *rpcEth1PortFlag != "8545" {
			return common.Address{}, "", "", errors.New("only use --rpc-eth1-port without setting --rpc")
		}

		eth1Url = *rpcEth1Flag
	}

	var eth2Url string
	if *rpcEth2Flag == "http://localhost:5052" {
		if *rpcEth2PortFlag != "5052" {
			eth2Url = "http://localhost:" + *rpcEth2PortFlag
		} else {
			eth2Url = *rpcEth2Flag
		}
	} else {
		// user should set the full url in the --rpc flag, check that they didn't set --rpcPort
		if *rpcEth2PortFlag != "5052" {
			return common.Address{}, "", "", errors.New("only use --rpc-eth2-port without setting --rpc")
		}

		eth2Url = *rpcEth2Flag
	}

	eth2Url = strings.TrimRight(eth2Url, "/")

	return nodeAddress, eth1Url, eth2Url, nil
}

func estimateProfit(ctx context.Context, logger *slog.Logger, client *ethclient.Client, nodeAddress common.Address, data *beaconchain.Data) (*big.Int, *big.Int, error) {
	networkID, err := client.NetworkID(ctx)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to get network ID"), err)
	}
	time.Sleep(500 * time.Millisecond)

	addresses := make([]common.Address, len(data.Minipools))
	for i, minipool := range data.Minipools {
		addresses[i] = minipool.WithdrawalAddress
	}

	uniswapData, paraswapData, err := arbitrage.CalculateArbitrageData(
		ctx,
		logger,
		client,
		&nodeAddress,
		addresses,
		networkID.Uint64(),
		false,
		500,
		arbitrage.BestProtocol,
	)
	if err != nil {
		return nil, nil, errors.Join(errors.New("failed to calculate arbitrage data"), err)
	}
	time.Sleep(500 * time.Millisecond)

	uniswapExpectedProfit := uniswapData.GetExpectedProfit()
	paraswapExpectedProfit := paraswapData.GetExpectedProfit()

	if uniswapExpectedProfit.Cmp(paraswapExpectedProfit) >= 0 {
		return uniswapData.GetSwapInAmountWeth(), uniswapExpectedProfit, nil
	} else {
		return paraswapData.GetSwapInAmountWeth(), paraswapExpectedProfit, nil
	}
}

func getExampleProfits(ctx context.Context, logger *slog.Logger, client *ethclient.Client, amountToDistribute int) (float64, error) {
	networkID, err := client.NetworkID(ctx)
	if err != nil {
		return 0, errors.Join(errors.New("failed to get network ID"), err)
	}
	time.Sleep(500 * time.Millisecond)

	rETHAddress, err := arbitrage.GetREthContractAddress(networkID.Uint64())
	if err != nil {
		return 0, errors.Join(errors.New("failed to get rETH contract address"), err)
	}

	rethInstance, err := rETH.NewRETH(rETHAddress, client)
	if err != nil {
		return 0, err
	}
	time.Sleep(500 * time.Millisecond)

	rETHShare := new(big.Int).Mul(big.NewInt(int64(amountToDistribute)), big.NewInt(1e15))
	rethToBurn, err := arbitrage.ConvertWethToReth(ctx, rethInstance, rETHShare)
	if err != nil {
		return 0, errors.Join(errors.New("failed to convert rETH to WETH"), err)
	}

	// get best pool to swap rETH
	ratio := new(big.Float).SetFloat64(1.99)
	_, uniswapReturnAmountWeth, _, err := uniswap.GetBestPoolWithdrawArb(ctx, logger, networkID.Uint64(), client, rethToBurn, ratio, 500)
	if err != nil {
		return 0, errors.Join(errors.New("failed to get best pool"), err)
	}

	profit := new(big.Int).Sub(rETHShare, uniswapReturnAmountWeth)
	profitFloat, _ := new(big.Float).Quo(new(big.Float).SetInt(profit), new(big.Float).SetInt(big.NewInt(1e18))).Float64()

	return profitFloat, nil
}
