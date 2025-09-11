package flashbots

type FlashbotsNetwork string

var FlashbotsUrlPerNetwork = map[uint64]string{
	1:        "https://relay.flashbots.net",
	5:        "https://relay-goerli.flashbots.net",
	11155111: "https://relay-sepolia.flashbots.net",
	17000:    "https://relay-holesky.flashbots.net",
}

const (
	JsonRpcParseError     = -32700
	JsonRpcInvalidRequest = -32600
	JsonRpcMethodNotFound = -32601
	JsonRpcInvalidParams  = -32602
	JsonRpcInternalError  = -32603
)

var AllBuilders = []string{
	"flashbots",
	"f1b.io",
	"rsync",
	"beaverbuild.org",
	"builder0x69",
	"Titan",
	"EigenPhi",
	"boba-builder",
	"Gambit Labs",
	"payload",
	"Loki",
	"BuildAI",
	"JetBuilder",
	"tbuilder",
	"penguinbuild",
	"bobthebuilder",
	"BTCS",
	"bloXroute",
	"Blockbeelder",
}
