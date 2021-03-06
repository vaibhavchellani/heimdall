package pier

import (
	"bytes"
	"container/list"
	"context"
	"encoding/hex"
	"math/big"
	"strconv"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	cliContext "github.com/cosmos/cosmos-sdk/client/context"
	"github.com/cosmos/cosmos-sdk/codec"
	ethereum "github.com/maticnetwork/bor"
	"github.com/maticnetwork/bor/accounts/abi"
	ethCommon "github.com/maticnetwork/bor/common"
	"github.com/maticnetwork/bor/core/types"
	"github.com/spf13/viper"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/tendermint/tendermint/libs/common"
	httpClient "github.com/tendermint/tendermint/rpc/client"

	checkpointTypes "github.com/maticnetwork/heimdall/checkpoint/types"
	clerkTypes "github.com/maticnetwork/heimdall/clerk/types"
	"github.com/maticnetwork/heimdall/contracts/rootchain"
	"github.com/maticnetwork/heimdall/contracts/stakinginfo"
	"github.com/maticnetwork/heimdall/contracts/statesender"
	"github.com/maticnetwork/heimdall/helper"
	stakingTypes "github.com/maticnetwork/heimdall/staking/types"
	topupTypes "github.com/maticnetwork/heimdall/topup/types"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

const (
	headerEvent      = "NewHeaderBlock"
	stakeInitEvent   = "Staked"
	unstakeInitEvent = "UnstakeInit"
	signerChange     = "SignerChange"

	lastBlockKey = "last-block" // storage key
)

// LightHeader represent light header for in-memory queue
type LightHeader struct {
	Number *big.Int `json:"number"           gencodec:"required"`
	Time   uint64   `json:"timestamp"        gencodec:"required"`
}

// Syncer syncs validators and checkpoints
type Syncer struct {
	// Base service
	common.BaseService

	// storage client
	storageClient *leveldb.DB

	// contract caller
	contractConnector helper.ContractCaller

	// ABIs
	abis []*abi.ABI

	// header channel
	HeaderChannel chan *types.Header
	// cancel function for poll/subscription
	cancelSubscription context.CancelFunc

	// header listener subscription
	cancelHeaderProcess context.CancelFunc

	// cli context
	cliCtx cliContext.CLIContext

	// queue connector
	queueConnector *QueueConnector

	// http client to subscribe to
	httpClient *httpClient.HTTP

	// queue
	headerQueue *list.List

	// confirmation time
	txConfirmationTime uint64
}

// NewSyncer returns new service object for syncing events
func NewSyncer(cdc *codec.Codec, queueConnector *QueueConnector, httpClient *httpClient.HTTP) *Syncer {
	// create logger
	logger := Logger.With("module", ChainSyncer)

	contractCaller, err := helper.NewContractCaller()
	if err != nil {
		logger.Error("Error while getting root chain instance", "error", err)
		panic(err)
	}

	abis := []*abi.ABI{
		&contractCaller.RootChainABI,
		&contractCaller.StateSenderABI,
		&contractCaller.StakingInfoABI,
	}

	cliCtx := cliContext.NewCLIContext().WithCodec(cdc)
	cliCtx.BroadcastMode = client.BroadcastSync
	cliCtx.TrustNode = true

	// creating syncer object
	syncer := &Syncer{
		storageClient: getBridgeDBInstance(viper.GetString(BridgeDBFlag)),

		cliCtx:            cliCtx,
		queueConnector:    queueConnector,
		httpClient:        httpClient,
		contractConnector: contractCaller,

		abis:          abis,
		HeaderChannel: make(chan *types.Header),

		headerQueue:        list.New(),
		txConfirmationTime: uint64(helper.GetConfig().TxConfirmationTime.Seconds()),
	}

	syncer.BaseService = *common.NewBaseService(logger, ChainSyncer, syncer)
	return syncer
}

// startHeaderProcess starts header process when they get new header
func (syncer *Syncer) startHeaderProcess(ctx context.Context) {
	for {
		select {
		case newHeader := <-syncer.HeaderChannel:
			syncer.processHeader(newHeader)
		case <-ctx.Done():
			return
		}
	}
}

// OnStart starts new block subscription
func (syncer *Syncer) OnStart() error {
	syncer.BaseService.OnStart() // Always call the overridden method.

	// create cancellable context
	ctx, cancelSubscription := context.WithCancel(context.Background())
	syncer.cancelSubscription = cancelSubscription

	// create cancellable context
	headerCtx, cancelHeaderProcess := context.WithCancel(context.Background())
	syncer.cancelHeaderProcess = cancelHeaderProcess

	// start header process
	go syncer.startHeaderProcess(headerCtx)

	// subscribe to new head
	subscription, err := syncer.contractConnector.MainChainClient.SubscribeNewHead(ctx, syncer.HeaderChannel)
	if err != nil {
		// start go routine to poll for new header using client object
		go syncer.startPolling(ctx, helper.GetConfig().SyncerPollInterval)
	} else {
		// start go routine to listen new header using subscription
		go syncer.startSubscription(ctx, subscription)
	}

	// subscribed to new head
	syncer.Logger.Debug("Subscribed to new head")

	return nil
}

// OnStop stops all necessary go routines
func (syncer *Syncer) OnStop() {
	syncer.BaseService.OnStop() // Always call the overridden method.

	// close db
	closeBridgeDBInstance()

	// cancel subscription if any
	syncer.cancelSubscription()

	// cancel header process
	syncer.cancelHeaderProcess()
}

// startPolling starts polling
func (syncer *Syncer) startPolling(ctx context.Context, pollInterval time.Duration) {
	// How often to fire the passed in function in second
	interval := pollInterval

	// Setup the ticket and the channel to signal
	// the ending of the interval
	ticker := time.NewTicker(interval)

	// start listening
	for {
		select {
		case <-ticker.C:
			header, err := syncer.contractConnector.MainChainClient.HeaderByNumber(ctx, nil)
			if err == nil && header != nil {
				// send data to channel
				syncer.HeaderChannel <- header
			}
		case <-ctx.Done():
			ticker.Stop()
			return
		}
	}
}

func (syncer *Syncer) startSubscription(ctx context.Context, subscription ethereum.Subscription) {
	for {
		select {
		case err := <-subscription.Err():
			// stop service
			syncer.Logger.Error("Error while subscribing new blocks", "error", err)
			syncer.Stop()

			// cancel subscription
			syncer.cancelSubscription()
			return
		case <-ctx.Done():
			return
		}
	}
}

func (syncer *Syncer) processHeader(newHeader *types.Header) {
	syncer.Logger.Debug("New block detected", "blockNumber", newHeader.Number)

	// adding into queue
	syncer.headerQueue.PushBack(&LightHeader{
		Number: newHeader.Number,
		Time:   newHeader.Time,
	})

	// current time
	currentTime := uint64(time.Now().UTC().Unix())

	var start *big.Int
	var end *big.Int

	// check start and end header
	for syncer.headerQueue.Len() > 0 {
		e := syncer.headerQueue.Front() // First element
		h := e.Value.(*LightHeader)
		if h.Time+syncer.txConfirmationTime > currentTime {
			break
		}

		if start == nil {
			start = h.Number
		}
		end = h.Number

		syncer.headerQueue.Remove(e) // Dequeue
	}

	if start == nil {
		return
	}

	// default fromBlock
	fromBlock := start
	// get last block from storage
	hasLastBlock, _ := syncer.storageClient.Has([]byte(lastBlockKey), nil)
	if hasLastBlock {
		lastBlockBytes, err := syncer.storageClient.Get([]byte(lastBlockKey), nil)
		if err != nil {
			syncer.Logger.Info("Error while fetching last block bytes from storage", "error", err)
			return
		}

		syncer.Logger.Debug("Got last block from bridge storage", "lastBlock", string(lastBlockBytes))
		if result, err := strconv.ParseUint(string(lastBlockBytes), 10, 64); err == nil {
			if result > fromBlock.Uint64() {
				fromBlock = big.NewInt(0).SetUint64(result)
			}

			fromBlock = big.NewInt(0).SetUint64(result + 1)
		}
	}

	// to block
	toBlock := end

	// debug log
	syncer.Logger.Info("Processing header", "fromBlock", fromBlock, "toBlock", toBlock)

	// set last block to storage
	syncer.storageClient.Put([]byte(lastBlockKey), []byte(toBlock.String()), nil)

	// log
	syncer.Logger.Info("Querying event logs", "fromBlock", fromBlock, "toBlock", toBlock)

	// draft a query
	query := ethereum.FilterQuery{
		FromBlock: fromBlock,
		ToBlock:   toBlock,
		Addresses: []ethCommon.Address{
			helper.GetRootChainAddress(),
			helper.GetStakingInfoAddress(),
			helper.GetStateSenderAddress(),
		},
	}

	// get all logs
	logs, err := syncer.contractConnector.MainChainClient.FilterLogs(context.Background(), query)
	if err != nil {
		syncer.Logger.Error("Error while filtering logs from syncer", "error", err)
		return
	} else if len(logs) > 0 {
		syncer.Logger.Debug("New logs found", "numberOfLogs", len(logs))
	}

	// log
	for _, vLog := range logs {
		topic := vLog.Topics[0].Bytes()
		for _, abiObject := range syncer.abis {
			selectedEvent := helper.EventByID(abiObject, topic)
			if selectedEvent != nil {
				syncer.Logger.Debug("selectedEvent ", " event name -", selectedEvent.Name)
				switch selectedEvent.Name {
				case "NewHeaderBlock":
					syncer.processCheckpointEvent(selectedEvent.Name, abiObject, &vLog)
				// TODO remove post new bridge design
				// case "Staked":
				// 	syncer.processStakedEvent(selectedEvent.Name, abiObject, &vLog)
				case "UnstakeInit":
					syncer.processUnstakeInitEvent(selectedEvent.Name, abiObject, &vLog)
				case "StakeUpdate":
					syncer.processStakeUpdateEvent(selectedEvent.Name, abiObject, &vLog)
				case "SignerChange":
					syncer.processSignerChangeEvent(selectedEvent.Name, abiObject, &vLog)
				case "ReStaked":
					syncer.processReStakedEvent(selectedEvent.Name, abiObject, &vLog)
				case "Jailed":
					syncer.processJailedEvent(selectedEvent.Name, abiObject, &vLog)
				case "StateSynced":
					syncer.processStateSyncedEvent(selectedEvent.Name, abiObject, &vLog)
				case "TopUpFee":
					syncer.processTopupFeeEvent(selectedEvent.Name, abiObject, &vLog)
					// case "Withdraw":
					// 	syncer.processWithdrawEvent(selectedEvent.Name, abiObject, &vLog)
				}
				break
			}
		}
	}
}

func (syncer *Syncer) processCheckpointEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(rootchain.RootchainNewHeaderBlock)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Info(
			"⬜ New event found",
			"event", eventName,
			"start", event.Start,
			"end", event.End,
			"reward", event.Reward,
			"root", "0x"+hex.EncodeToString(event.Root[:]),
			"proposer", event.Proposer.Hex(),
			"headerNumber", event.HeaderBlockId,
		)

		// create msg checkpoint ack message
		msg := checkpointTypes.NewMsgCheckpointAck(helper.GetFromAddress(syncer.cliCtx), event.HeaderBlockId.Uint64(), hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()), uint64(vLog.Index))
		syncer.queueConnector.BroadcastToHeimdall(msg)
	}
}

func (syncer *Syncer) processStakedEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoStaked)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validator", event.Signer,
			"ID", event.ValidatorId,
			"activatonEpoch", event.ActivationEpoch,
			"amount", event.Amount,
		)

		// compare user to get address
		if isEventSender(syncer.cliCtx, event.ValidatorId.Uint64()) {
			pubkey := helper.GetPubKey()
			msg := stakingTypes.NewMsgValidatorJoin(
				hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
				event.ValidatorId.Uint64(),
				hmTypes.NewPubKey(pubkey[:]),
				hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()),
				uint64(vLog.Index),
			)

			// process staked
			syncer.queueConnector.BroadcastToHeimdall(msg)
		}
	}
}

func (syncer *Syncer) processUnstakeInitEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoUnstakeInit)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validator", event.User,
			"validatorID", event.ValidatorId,
			"deactivatonEpoch", event.DeactivationEpoch,
			"amount", event.Amount,
		)

		// msg validator exit
		if isEventSender(syncer.cliCtx, event.ValidatorId.Uint64()) {
			msg := stakingTypes.NewMsgValidatorExit(
				hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
				event.ValidatorId.Uint64(),
				hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()),
				uint64(vLog.Index),
			)

			// broadcast heimdall
			syncer.queueConnector.BroadcastToHeimdall(msg)
		}
	}
}

func (syncer *Syncer) processStakeUpdateEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoStakeUpdate)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validatorID", event.ValidatorId,
			"newAmount", event.NewAmount,
		)

		// msg validator exit
		if isEventSender(syncer.cliCtx, event.ValidatorId.Uint64()) {
			msg := stakingTypes.NewMsgStakeUpdate(
				hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
				event.ValidatorId.Uint64(),
				hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()),
				uint64(vLog.Index),
			)

			// broadcast heimdall
			syncer.queueConnector.BroadcastToHeimdall(msg)
		}
	}
}

func (syncer *Syncer) processSignerChangeEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoSignerChange)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validatorID", event.ValidatorId,
			"newSigner", event.NewSigner.Hex(),
			"oldSigner", event.OldSigner.Hex(),
		)

		// signer change
		if bytes.Compare(event.NewSigner.Bytes(), helper.GetAddress()) == 0 {
			pubkey := helper.GetPubKey()
			msg := stakingTypes.NewMsgSignerUpdate(
				hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
				event.ValidatorId.Uint64(),
				hmTypes.NewPubKey(pubkey[:]),
				hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()),
				uint64(vLog.Index),
			)

			// process signer update
			syncer.queueConnector.BroadcastToHeimdall(msg)
		}
	}
}

func (syncer *Syncer) processReStakedEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoReStaked)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validatorId", event.ValidatorId,
			"amount", event.Amount,
		)

		// // msg validator exit
		// msg := staking.NewMsgValidatorExit(
		// 	hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
		// 	event.ValidatorId.Uint64(),
		// 	vLog.TxHash,
		// )

		// // broadcast heimdall
		// syncer.queueConnector.BroadcastToHeimdall(msg)
	}
}

func (syncer *Syncer) processJailedEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(stakinginfo.StakinginfoJailed)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"validatorID", event.ValidatorId,
			"exitEpoch", event.ExitEpoch,
		)

		// // msg validator exit
		// msg := staking.NewMsgValidatorExit(
		// 	hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
		// 	event.ValidatorId.Uint64(),
		// 	vLog.TxHash,
		// )

		// // broadcast heimdall
		// syncer.queueConnector.BroadcastToHeimdall(msg)
	}
}

//
// Process withdraw event
//

// func (syncer *Syncer) processWithdrawEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
// 	event := new(depositmanager.DepositmanagerDeposit)
// 	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
// 		logEventParseError(syncer.Logger, eventName, err)
// 	} else {
// 		syncer.Logger.Debug(
// 			"New event found",
// 			"event", eventName,
// 			"user", event.User,
// 			"depositCount", event.DepositCount,
// 			"token", event.Token.String(),
// 		)

// 		// TODO dispatch to heimdall
// 	}
// }

//
// Process state synced event
//

func (syncer *Syncer) processStateSyncedEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {
	event := new(statesender.StatesenderStateSynced)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Debug(
			"⬜ New event found",
			"event", eventName,
			"id", event.Id,
			"contract", event.ContractAddress,
			"data", hex.EncodeToString(event.Data),
			"borChainId", helper.GetConfig().BorChainID,
		)

		// create clerk event record
		msg := clerkTypes.NewMsgEventRecord(
			hmTypes.BytesToHeimdallAddress(helper.GetAddress()),
			hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()),
			uint64(vLog.Index),
			event.Id.Uint64(),
			helper.GetConfig().BorChainID,
		)

		// broadcast to heimdall
		syncer.queueConnector.BroadcastToHeimdall(msg)
	}
}

// processTopupFeeEvent
func (syncer *Syncer) processTopupFeeEvent(eventName string, abiObject *abi.ABI, vLog *types.Log) {

	event := new(stakinginfo.StakinginfoTopUpFee)
	if err := helper.UnpackLog(abiObject, event, eventName, vLog); err != nil {
		logEventParseError(syncer.Logger, eventName, err)
	} else {
		syncer.Logger.Info(
			"New event found",
			"event", eventName,
			"validatorId", event.ValidatorId,
			"Fee", event.Fee,
		)

		// create msg checkpoint ack message
		msg := topupTypes.NewMsgTopup(helper.GetFromAddress(syncer.cliCtx), event.ValidatorId.Uint64(), hmTypes.BytesToHeimdallHash(vLog.TxHash.Bytes()), uint64(vLog.Index))
		syncer.queueConnector.BroadcastToHeimdall(msg)
	}
}
