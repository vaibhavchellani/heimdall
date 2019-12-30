package helper

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	cosmosContext "github.com/cosmos/cosmos-sdk/client/context"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/pkg/errors"
	httpClient "github.com/tendermint/tendermint/rpc/client"
	ctypes "github.com/tendermint/tendermint/rpc/core/types"
	tmTypes "github.com/tendermint/tendermint/types"
)

const (
	// CommitTimeout commit timeout
	CommitTimeout = 2 * time.Minute
)

// GetNodeStatus returns node status
func GetNodeStatus(cliCtx cosmosContext.CLIContext) (*ctypes.ResultStatus, error) {
	node, err := cliCtx.GetNode()
	if err != nil {
		return nil, err
	}

	return node.Status()
}

// QueryTxsByEvents performs a search for transactions for a given set of tags via
// Tendermint RPC. It returns a slice of Info object containing txs and metadata.
// An error is returned if the query fails.
func QueryTxsByEvents(cliCtx cosmosContext.CLIContext, tags []string, page, limit int) (*sdk.SearchTxsResult, error) {
	if len(tags) == 0 {
		return nil, errors.New("must declare at least one tag to search")
	}

	if page <= 0 {
		return nil, errors.New("page must greater than 0")
	}

	if limit <= 0 {
		return nil, errors.New("limit must greater than 0")
	}

	// XXX: implement ANY
	query := strings.Join(tags, " AND ")

	node, err := cliCtx.GetNode()
	if err != nil {
		return nil, err
	}

	prove := !cliCtx.TrustNode
	resTxs, err := node.TxSearch(query, prove, page, limit)
	if err != nil {
		return nil, err
	}

	if prove {
		for _, tx := range resTxs.Txs {
			err := ValidateTxResult(cliCtx, tx)
			if err != nil {
				return nil, err
			}
		}
	}

	resBlocks, err := getBlocksForTxResults(cliCtx, resTxs.Txs)
	if err != nil {
		return nil, err
	}

	txs, err := formatTxResults(resTxs.Txs, resBlocks)
	if err != nil {
		return nil, err
	}

	result := sdk.NewSearchTxsResult(resTxs.TotalCount, len(txs), page, limit, txs)

	return &result, nil
}

// formatTxResults parses the indexed txs into a slice of TxResponse objects.
func formatTxResults(resTxs []*ctypes.ResultTx, resBlocks map[int64]*ctypes.ResultBlock) ([]sdk.TxResponse, error) {
	var err error
	out := make([]sdk.TxResponse, len(resTxs))
	for i := range resTxs {
		out[i], err = formatTxResult(resTxs[i], resBlocks[resTxs[i].Height])
		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

// ValidateTxResult performs transaction verification.
func ValidateTxResult(cliCtx cosmosContext.CLIContext, resTx *ctypes.ResultTx) error {
	if !cliCtx.TrustNode {
		check, err := cliCtx.Verify(resTx.Height)
		if err != nil {
			return err
		}
		err = resTx.Proof.Validate(check.Header.DataHash)
		if err != nil {
			return err
		}
	}
	return nil
}

func getBlocksForTxResults(cliCtx cosmosContext.CLIContext, resTxs []*ctypes.ResultTx) (map[int64]*ctypes.ResultBlock, error) {
	node, err := cliCtx.GetNode()
	if err != nil {
		return nil, err
	}

	resBlocks := make(map[int64]*ctypes.ResultBlock)

	for _, resTx := range resTxs {
		if _, ok := resBlocks[resTx.Height]; !ok {
			resBlock, err := node.Block(&resTx.Height)
			if err != nil {
				return nil, err
			}

			resBlocks[resTx.Height] = resBlock
		}
	}

	return resBlocks, nil
}

func formatTxResult(resTx *ctypes.ResultTx, resBlock *ctypes.ResultBlock) (sdk.TxResponse, error) {
	tx, err := parseTx(resTx.Tx)
	if err != nil {
		return sdk.TxResponse{}, err
	}

	return sdk.NewResponseResultTx(resTx, tx, resBlock.Block.Time.Format(time.RFC3339)), nil
}

func parseTx(txBytes []byte) (sdk.Tx, error) {
	decoder := GetTxDecoder()
	return decoder(txBytes)
}

// QueryTx query tx from node
func QueryTx(cliCtx cosmosContext.CLIContext, hashHexStr string) (sdk.TxResponse, error) {
	hash, err := hex.DecodeString(hashHexStr)
	if err != nil {
		return sdk.TxResponse{}, err
	}

	node, err := cliCtx.GetNode()
	if err != nil {
		return sdk.TxResponse{}, err
	}

	resTx, err := node.Tx(hash, !cliCtx.TrustNode)
	if err != nil {
		return sdk.TxResponse{}, err
	}

	if !cliCtx.TrustNode {
		if err = ValidateTxResult(cliCtx, resTx); err != nil {
			return sdk.TxResponse{}, err
		}
	}

	resBlocks, err := getBlocksForTxResults(cliCtx, []*ctypes.ResultTx{resTx})
	if err != nil {
		return sdk.TxResponse{}, err
	}

	out, err := formatTxResult(resTx, resBlocks[resTx.Height])
	if err != nil {
		return out, err
	}

	return out, nil
}

// QueryTxWithProof query tx with proof from node
func QueryTxWithProof(cliCtx cosmosContext.CLIContext, hash []byte) (*ctypes.ResultTx, error) {
	node, err := cliCtx.GetNode()
	if err != nil {
		return nil, err
	}

	return node.Tx(hash, true)
}

// GetBlock returns a block
func GetBlock(cliCtx cosmosContext.CLIContext, height int64) (*ctypes.ResultBlock, error) {
	node, err := cliCtx.GetNode()
	if err != nil {
		return nil, err
	}

	return node.Block(&height)
}

// GetBlockWithClient get block through per height
func GetBlockWithClient(client *httpClient.HTTP, height int64) (*tmTypes.Block, error) {
	c, cancel := context.WithTimeout(context.Background(), CommitTimeout)
	defer cancel()

	// get block using client
	block, err := client.Block(&height)
	if err == nil && block != nil {
		return block.Block, nil
	}

	// subscriber
	subscriber := fmt.Sprintf("new-block-%v", height)

	// query for event
	query := tmTypes.QueryForEvent(tmTypes.EventNewBlock).String()

	// register for the next event of this type
	eventCh, err := client.Subscribe(c, subscriber, query)
	if err != nil {
		return nil, errors.Wrap(err, "failed to subscribe")
	}

	// unsubscribe query
	defer client.Unsubscribe(c, subscriber, query)

	select {
	case event := <-eventCh:
		eventData := event.Data.(tmTypes.TMEventData)
		switch t := eventData.(type) {
		case tmTypes.EventDataNewBlock:
			return t.Block, nil
		default:
			return nil, errors.New("timed out waiting for event")
		}
	case <-c.Done():
		return nil, errors.New("timed out waiting for event")
	}
}