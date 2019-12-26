package pier

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path"
	"sync"
	"time"

	cliContext "github.com/cosmos/cosmos-sdk/client/context"
	"github.com/ethereum/go-ethereum"
	ethCommon "github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/spf13/viper"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/tendermint/tendermint/libs/common"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/maticnetwork/heimdall/checkpoint"
	"github.com/maticnetwork/heimdall/contracts/rootchain"
	"github.com/maticnetwork/heimdall/helper"
	hmtypes "github.com/maticnetwork/heimdall/types"
)

// MaticCheckpointer to propose
type MaticCheckpointer struct {
	// Base service
	common.BaseService

	// storage client
	storageClient *leveldb.DB

	// ETH client
	MaticClient *ethclient.Client
	// ETH RPC client
	MaticRPCClient *rpc.Client
	// Mainchain client
	MainClient *ethclient.Client
	// Rootchain instance
	RootChainInstance *rootchain.Rootchain
	// header channel
	HeaderChannel chan *types.Header
	// cancel function for poll/subscription
	cancelSubscription context.CancelFunc
	// header listener subscription
	cancelHeaderProcess context.CancelFunc

	cliCtx cliContext.CLIContext
}

type ContractCheckpointState struct {
	start              uint64
	end                uint64
	currentHeaderBlock *big.Int
	err                error
}

func NewContractCheckpointState(_start uint64, _end uint64, _currentHeaderBlock *big.Int, _err error) ContractCheckpointState {
	return ContractCheckpointState{
		start:              _start,
		end:                _end,
		currentHeaderBlock: _currentHeaderBlock,
		err:                _err,
	}
}

type HeimdallCheckpoint struct {
	start uint64
	end   uint64
	found bool
}

func NewHeimdallCheckpoint(_start uint64, _end uint64, _found bool) HeimdallCheckpoint {
	return HeimdallCheckpoint{
		start: _start,
		end:   _end,
		found: _found,
	}
}

// NewMaticCheckpointer returns new service object
func NewMaticCheckpointer() *MaticCheckpointer {
	// create logger
	logger := log.NewTMLogger(log.NewSyncWriter(os.Stdout)).With("module", maticCheckpointer)

	// root chain instance
	rootchainInstance, err := helper.GetRootChainInstance()
	if err != nil {
		logger.Error("Error while getting root chain instance", "error", err)
		panic(err)
	}

	cliCtx := cliContext.NewCLIContext()
	cliCtx.Async = true

	// creating checkpointer object
	checkpointer := &MaticCheckpointer{
		storageClient:     getBridgeDBInstance(viper.GetString(bridgeDBFlag)),
		MaticClient:       helper.GetMaticClient(),
		MaticRPCClient:    helper.GetMaticRPCClient(),
		MainClient:        helper.GetMainClient(),
		RootChainInstance: rootchainInstance,
		HeaderChannel:     make(chan *types.Header),
		cliCtx:            cliCtx,
	}

	checkpointer.BaseService = *common.NewBaseService(logger, maticCheckpointer, checkpointer)
	return checkpointer
}

// startHeaderProcess starts header process when they get new header
func (checkpointer *MaticCheckpointer) startHeaderProcess(ctx context.Context) {
	for {
		select {
		case newHeader := <-checkpointer.HeaderChannel:
			checkpointer.sendRequest(newHeader)
		case <-ctx.Done():
			return
		}
	}
}

// OnStart starts new block subscription
func (checkpointer *MaticCheckpointer) OnStart() error {
	checkpointer.BaseService.OnStart() // Always call the overridden method.

	// create cancellable context
	ctx, cancelSubscription := context.WithCancel(context.Background())
	checkpointer.cancelSubscription = cancelSubscription

	// create cancellable context
	headerCtx, cancelHeaderProcess := context.WithCancel(context.Background())
	checkpointer.cancelHeaderProcess = cancelHeaderProcess

	// start header process
	go checkpointer.startHeaderProcess(headerCtx)

	// subscribe to new head
	subscription, err := checkpointer.MaticClient.SubscribeNewHead(ctx, checkpointer.HeaderChannel)
	if err != nil {
		// start go routine to poll for new header using client object
		go checkpointer.startPolling(ctx, helper.GetConfig().CheckpointerPollInterval)
	} else {
		// start go routine to listen new header using subscription
		go checkpointer.startSubscription(ctx, subscription)
	}

	// subscribed to new head
	checkpointer.Logger.Debug("Subscribed to new head")

	return nil
}

// OnStop stops all necessary go routines
func (checkpointer *MaticCheckpointer) OnStop() {
	checkpointer.BaseService.OnStop() // Always call the overridden method.

	// close bridge db instance
	closeBridgeDBInstance()

	// cancel subscription if any
	checkpointer.cancelSubscription()

	// cancel header process
	checkpointer.cancelHeaderProcess()
}

func (checkpointer *MaticCheckpointer) startPolling(ctx context.Context, pollInterval int) {
	// How often to fire the passed in function in second
	interval := time.Duration(pollInterval) * time.Millisecond

	// Setup the ticket and the channel to signal
	// the ending of the interval
	ticker := time.NewTicker(interval)

	// start listening
	for {
		select {
		case <-ticker.C:
			if isProposer() {
				header, err := checkpointer.MaticClient.HeaderByNumber(ctx, nil)
				if err == nil && header != nil {
					// send data to channel
					checkpointer.HeaderChannel <- header
				} else if err != nil {
					checkpointer.Logger.Error("Unable to fetch header by number from matic", "Error", err)
				}
			}
		case <-ctx.Done():
			ticker.Stop()
			return
		}
	}
}

func (checkpointer *MaticCheckpointer) startSubscription(ctx context.Context, subscription ethereum.Subscription) {
	for {
		select {
		case err := <-subscription.Err():
			// stop service
			checkpointer.Logger.Error("Error while subscribing new blocks", "error", err)
			checkpointer.Stop()

			// cancel subscription
			checkpointer.cancelSubscription()
			return
		case <-ctx.Done():
			return
		}
	}
}

func (c *MaticCheckpointer) sendRequest(newHeader *types.Header) {
	c.Logger.Debug("New block detected", "blockNumber", newHeader.Number)

	// get state
	var checkpointStateOnChain *ContractCheckpointState
	var checkpointStateInBuffer *HeimdallCheckpoint
	var checkpointStateOnHeimdall *HeimdallCheckpoint

	var wg sync.WaitGroup
	wg.Add(3)
	c.Logger.Info("Collecting checkpoint status from different sources")
	// fetch checkpoint from contract
	go func() {
		defer wg.Done()
		checkpointStateOnChain, _ = c.nextExpectedCheckpoint(newHeader.Number.Uint64())
	}()

	// fetch checkpoint from buffer
	go func() {
		defer wg.Done()
		checkpointStateInBuffer, _ = c.fetchBufferedCheckpoint()
	}()

	// fetch checkpoint last confirmed on heimdall
	go func() {
		defer wg.Done()
		checkpointStateOnHeimdall, _ = c.fetchCommittedCheckpoint()
	}()

	// wait for state collection
	wg.Wait()

	err := c.determineAction(*checkpointStateOnChain, *checkpointStateInBuffer, *checkpointStateOnHeimdall)
	if err != nil {
		c.Logger.Error("Error determining next action", "error", err)
		return
	}
}

func (c *MaticCheckpointer) determineAction(
	probableNextCheckpoint ContractCheckpointState,
	bufferedCheckpont HeimdallCheckpoint,
	latestCommittedCheckpoint HeimdallCheckpoint) (err error) {
	// ACK needs to be sent
	if lastHeimdallCheckpoint.end+1 == lastContractCheckpoint.start {
		c.Logger.Debug("Detected mainchain checkpoint,sending ACK", "HeimdallEnd", lastHeimdallCheckpoint.end, "ContractStart", lastHeimdallCheckpoint.start)
		headerNumber := lastContractCheckpoint.currentHeaderBlock.Sub(lastContractCheckpoint.currentHeaderBlock, big.NewInt(int64(helper.GetConfig().ChildBlockInterval)))
		msg := checkpoint.NewMsgCheckpointAck(headerNumber.Uint64(), uint64(time.Now().Unix()))
		txBytes, err := helper.CreateTxBytes(msg)
		if err != nil {
			c.Logger.Error("Error while creating tx bytes", "error", err)
			return
		}
		// send tendermint request
		_, err = helper.SendTendermintRequest(c.cliCtx, txBytes)
		if err != nil {
			c.Logger.Error("Error while sending request to Tendermint", "error", err)
			return
		}
		return
	}
	start := lastContractCheckpoint.start
	end := lastContractCheckpoint.end

}

// Determines the next checkpoint based on on-chain contract state
// expects the lastest block on bor chain as an argument
// returns the next checkpoint basec on average and max checkpoint size permitted
func (c *MaticCheckpointer) nextExpectedCheckpoint(latestChildBlock uint64) (*ContractCheckpointState, error) {
	currentCheckpointHead, err := c.fetchCheckpointFromContract()
	if err != nil {
		c.Logger.Error("Error while fetching current header block object from rootchain", "error", err)
		return nil, err
	}

	// find next start/end
	var start, end uint64
	start = currentCheckpointHead.EndBlock
	// add 1 if start > 0
	if start > 0 {
		start = start + 1
	}
	// get diff
	diff := latestChildBlock - start + 1
	// process if diff > 0 (positive)
	if diff > 0 {
		expectedDiff := diff - diff%helper.GetConfig().AvgCheckpointLength
		if expectedDiff > 0 {
			expectedDiff = expectedDiff - 1
		}
		// cap with max checkpoint length
		if expectedDiff > helper.GetConfig().MaxCheckpointLength-1 {
			expectedDiff = helper.GetConfig().MaxCheckpointLength - 1
		}
		// get end result
		end = expectedDiff + start
	}

	// check if we need to force push a new checkpoint due to BP's not producing blocks
	start, end, isUpdated := c.isForcePushNeeded(start, end, diff, latestChildBlock, int64(currentCheckpointHead.TimeStamp))
	if isUpdated {
		c.Logger.Info("Need to force push checkpoint", "start", start, "end", end)
	}

	return &(NewContractCheckpointState(start, end, nil, nil)), nil
}

//
//
// Data fetchers
//

// fetch checkpoint present in buffer from heimdall
func (c *MaticCheckpointer) fetchBufferedCheckpoint() (*HeimdallCheckpoint, error) {
	c.Logger.Info("Fetching checkpoint in buffer")

	_checkpoint, err := c.fetchCheckpoint(GetHeimdallServerEndpoint(BufferedCheckpointURL))
	if err != nil {
		return nil, err
	}

	bufferedCheckpoint := NewHeimdallCheckpoint(_checkpoint.StartBlock, _checkpoint.EndBlock)
	return bufferedCheckpoint, nil
}

// fetches latest committed checkpoint from heimdall
func (c *MaticCheckpointer) fetchCommittedCheckpoint() (*HeimdallCheckpoint, error) {
	c.Logger.Info("Fetching last committed checkpoint")

	_checkpoint, err := c.fetchCheckpoint(GetHeimdallServerEndpoint(LatestCheckpointURL))
	if err != nil {
		return nil, err
	}

	return NewHeimdallCheckpoint(_checkpoint.StartBlock, _checkpoint.EndBlock), nil
}

// fetches latest committed checkpoint from heimdall
func (c *MaticCheckpointer) fetchCheckpointFromContract() (hmtypes.CheckpointBlockHeader, error) {
	c.Logger.Info("Fetching last committed checkpoint on basechain")

	// fetch current header block from mainchain contract
	_currentHeaderBlock, err := c.contractConnector.CurrentHeaderBlock()
	if err != nil {
		c.Logger.Error("Error while fetching current header block number from rootchain", "error", err)
		return nil, err
	}
	// current header block
	currentHeaderBlockNumber := big.NewInt(0).SetUint64(_currentHeaderBlock)

	// get header info
	// currentHeaderBlock = currentHeaderBlock.Sub(currentHeaderBlock, helper.GetConfig().ChildBlockInterval)
	root, currentStart, currentEnd, lastCheckpointTime, err := c.contractConnector.GetHeaderInfo(currentHeaderBlockNumber.Uint64())
	if err != nil {
		c.Logger.Error("Error while fetching current header block object from rootchain", "error", err)
		return hmtypes.CheckpointBlockHeader{}, err
	}

	return hmtypes.CheckpointBlockHeader{
		RootHash:   root,
		StartBlock: currentStart,
		EndBlock:   currentEnd,
		TimeStamp:  lastCheckpointTime,
	}, nil
}

//
// Utilities
//

// fetches checkpoint from given URL
func (c *MaticCheckpointer) fetchCheckpoint(url string) (checkpoint hmtypes.CheckpointBlockHeader, err error) {
	response, err := FetchFromAPI(c.cliCtx, url)
	if err != nil {
		return checkpoint, err
	}

	if err := json.Unmarshal(response.Result, &checkpoint); err != nil {
		c.Logger.Error("Error unmarshalling checkpoint", "error", err)
		return checkpoint, err
	}

	return checkpoint, nil
}

// FetchFromAPI fetches data from any URL
func FetchFromAPI(cliCtx cliContext.CLIContext, URL string) (result rest.ResponseWithHeight, err error) {
	resp, err := http.Get(URL)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()

	// response
	if resp.StatusCode == 200 {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return result, err
		}
		// unmarshall data from buffer
		// var proposers []hmtypes.Validator
		var response rest.ResponseWithHeight
		if err := cliCtx.Codec.UnmarshalJSON(body, &response); err != nil {
			return result, err
		}
		return response, nil
	}

	return result, fmt.Errorf("Error while fetching data from url: %v, status: %v", URL, resp.StatusCode)
}

// GetHeimdallServerEndpoint returns heimdall server endpoint
func GetHeimdallServerEndpoint(endpoint string) string {
	u, _ := url.Parse(helper.GetConfig().HeimdallServerURL)
	u.Path = path.Join(u.Path, endpoint)
	return u.String()
}

func (c *MaticCheckpointer) isForcePushNeeded(start, end, diff, latestChildBlock uint64, lastCheckpointTime int64) (newStart, newEnd uint64, updated bool) {
	isUpdated := false
	// Handle when block producers go down
	if end == 0 || end == start || (0 < diff && diff < helper.GetConfig().AvgCheckpointLength) {
		c.Logger.Debug("Fetching last header block to calculate time")
		currentTime := time.Now().UTC().Unix()
		defaultForcePushInterval := helper.GetConfig().MaxCheckpointLength * 2 // in seconds
		if currentTime-lastCheckpointTime > int64(defaultForcePushInterval) {
			end = latestChildBlock
			c.Logger.Info("Force push checkpoint",
				"currentTime", currentTime,
				"lastCheckpointTime", lastCheckpointTime,
				"defaultForcePushInterval", defaultForcePushInterval,
				"start", start,
				"end", end,
			)
			isUpdated = true
		}
	}
	return start, end, isUpdated
}

func (c *MaticCheckpointer) CreateAndSendCheckpointToHeimdall(start, end uint64) {
	// Get root hash
	root, err := checkpoint.GetHeaders(start, end)
	if err != nil {
		return
	}

	c.Logger.Info("New checkpoint header created", "start", start, "end", end, "root", ethCommon.BytesToHash(root))

	// TODO submit checkcoint
	txBytes, err := helper.CreateTxBytes(
		checkpoint.NewMsgCheckpointBlock(
			ethCommon.BytesToAddress(helper.GetAddress()),
			start,
			end,
			ethCommon.BytesToHash(root),
			uint64(time.Now().Unix()),
		),
	)

	if err != nil {
		c.Logger.Error("Error while creating tx bytes", "error", err)
		return
	}

	resp, err := helper.SendTendermintRequest(c.cliCtx, txBytes)
	if err != nil {
		c.Logger.Error("Error while sending request to Tendermint", "error", err)
		return
	}

	c.Logger.Info("Checkpoint sent successfully", "hash", resp.Hash.String(), "start", start, "end", end, "root", hex.EncodeToString(root))
}
