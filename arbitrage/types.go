package arbitrage

import (
	"crypto/ecdsa"
	"encoding/json"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/0xtrooper/flashbots_client"
)

type Protocol string

const (
	BestProtocol     Protocol = "best"
	UniswapProtocol  Protocol = "uniswap"
	ParaswapProtocol Protocol = "paraswap"
)

type DataIn struct {
	Command                         string
	LocalReth                       bool
	MinipoolAddresses               []common.Address
	NodeAddressPrivateKey           *ecdsa.PrivateKey
	NodeAddress                     *common.Address
	ReceiverAddress                 *common.Address
	Client                          *ethclient.Client
	FbClient                        *flashbots_client.FlashbotsClient
	RandomPrivateKey                bool
	SkipConfirmation                bool
	CheckProfit                     bool
	CheckProfitIgnoreDistributeCost bool
	DryRun                          bool
	Ratelimit                       int
	Protocol                        Protocol
	NetworkId                       uint64
	TipOverwrite                    *big.Int
}

type UniswapArbitrage struct {
	expectedProfit          *big.Int
	expectedProfitAfterFees *big.Int

	swapInAmountWeth  *big.Int
	swapOutAmountReth *big.Int
	sqrtPriceLimitX96 *big.Int

	expectedFee int
	poolAddress common.Address
}

func (ua *UniswapArbitrage) GetExpectedProfit() *big.Int {
	return new(big.Int).Set(ua.expectedProfit)
}

func (ua *UniswapArbitrage) GetSwapInAmountWeth() *big.Int {
	return new(big.Int).Set(ua.swapInAmountWeth)
}

type ParaswapArbitrage struct {
	expectedProfit          *big.Int
	expectedProfitAfterFees *big.Int
	swapInAmountWeth        *big.Int
	swapOutAmountReth       *big.Int
	expectedFee             int
	calldata                []byte
}

func (pa *ParaswapArbitrage) GetExpectedProfit() *big.Int {
	return new(big.Int).Set(pa.expectedProfit)
}

func (ua *ParaswapArbitrage) GetSwapInAmountWeth() *big.Int {
	return new(big.Int).Set(ua.swapInAmountWeth)
}

type OneInchTokenInfo struct {
	Address       string   `json:"address"`
	Symbol        string   `json:"symbol"`
	Name          string   `json:"name"`
	Decimals      int      `json:"decimals"`
	LogoURI       string   `json:"logoURI"`
	DomainVersion string   `json:"domainVersion,omitempty"`
	Eip2612       bool     `json:"eip2612,omitempty"`
	IsFoT         bool     `json:"isFoT,omitempty"`
	Tags          []string `json:"tags,omitempty"`
}

type OneInchSelectedProtocol struct {
	Name             string `json:"name"`
	Part             int    `json:"part"`
	FromTokenAddress string `json:"fromTokenAddress"`
	ToTokenAddress   string `json:"toTokenAddress"`
	Gas              int    `json:"gas,omitempty"`
}

type OneInchQuoteResponse struct {
	SrcToken  OneInchTokenInfo              `json:"srcToken"`
	DstToken  OneInchTokenInfo              `json:"dstToken"`
	DstAmount string                        `json:"dstAmount"`
	Protocols [][][]OneInchSelectedProtocol `json:"protocols"`
}

type OneInchErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"description"`
	Code        int    `json:"statusCode"`
	RequestId   string `json:"requestId"`
}

type ParaswapTransactionsBody struct {
	SrcToken     string          `json:"srcToken"`
	SrcDecimals  int             `json:"srcDecimals"`
	DestToken    string          `json:"destToken"`
	DestDecimals int             `json:"destDecimals"`
	SrcAmount    string          `json:"srcAmount"`
	DestAmount   string          `json:"destAmount"`
	PriceRoute   json.RawMessage `json:"priceRoute"`
	TxOrigin     string          `json:"txOrigin"`
	UserAddress  string          `json:"userAddress"`
}

type ParaswapPriceRoute struct {
	BlockNumber  int64  `json:"blockNumber"`
	Network      int    `json:"network"`
	SrcToken     string `json:"srcToken"`
	SrcDecimals  int    `json:"srcDecimals"`
	SrcAmount    string `json:"srcAmount"`
	DestToken    string `json:"destToken"`
	DestDecimals int    `json:"destDecimals"`
	DestAmount   string `json:"destAmount"`
	BestRoute    []struct {
		Percent int `json:"percent"`
		Swaps   []struct {
			SrcToken      string `json:"srcToken"`
			SrcDecimals   int    `json:"srcDecimals"`
			DestToken     string `json:"destToken"`
			DestDecimals  int    `json:"destDecimals"`
			SwapExchanges []struct {
				Exchange      string   `json:"exchange"`
				SrcAmount     string   `json:"srcAmount"`
				DestAmount    string   `json:"destAmount"`
				Percent       int      `json:"percent"`
				PoolAddresses []string `json:"poolAddresses"`
				Data          struct {
					Path []struct {
						TokenIn    string `json:"tokenIn"`
						TokenOut   string `json:"tokenOut"`
						Fee        string `json:"fee"`
						CurrentFee string `json:"currentFee"`
					} `json:"path"`
					GasUSD string `json:"gasUSD"`
				} `json:"data"`
			} `json:"swapExchanges"`
		} `json:"swaps"`
	} `json:"bestRoute"`
	GasCostUSD         string `json:"gasCostUSD"`
	GasCost            string `json:"gasCost"`
	Side               string `json:"side"`
	Version            string `json:"version"`
	ContractAddress    string `json:"contractAddress"`
	TokenTransferProxy string `json:"tokenTransferProxy"`
	ContractMethod     string `json:"contractMethod"`
	PartnerFee         int    `json:"partnerFee"`
	SrcUSD             string `json:"srcUSD"`
	DestUSD            string `json:"destUSD"`
	Partner            string `json:"partner"`
	MaxImpactReached   bool   `json:"maxImpactReached"`
	Hmac               string `json:"hmac"`
}

type ParaswapPriceResponse struct {
	PriceRoute ParaswapPriceRoute `json:"priceRoute"`
}

type ParaswapTransactionsResponse struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Value    string `json:"value"`
	Data     string `json:"data"`
	GasPrice string `json:"gasPrice"`
}

type ParaswapErrorResponse struct {
	Error string `json:"error"`
}
