package types

import (
	"encoding/json"
	"errors"

	"github.com/maticnetwork/heimdall/bor/types"
	hmTypes "github.com/maticnetwork/heimdall/types"
)

// GenesisState is the checkpoint state that must be provided at genesis.
type GenesisState struct {
	Params Params `json:"params" yaml:"params"`

	BufferedCheckpoint *hmTypes.CheckpointBlockHeader  `json:"buffered_checkpoint" yaml:"buffered_checkpoint"`
	LastNoACK          uint64                          `json:"last_no_ack" yaml:"last_no_ack"`
	AckCount           uint64                          `json:"ack_count" yaml:"ack_count"`
	Headers            []hmTypes.CheckpointBlockHeader `json:"headers" yaml:"headers"`
}

// NewGenesisState creates a new genesis state.
func NewGenesisState(
	params Params,
	bufferedCheckpoint *hmTypes.CheckpointBlockHeader,
	lastNoACK uint64,
	ackCount uint64,
	headers []hmTypes.CheckpointBlockHeader,
) GenesisState {
	return GenesisState{
		Params:             params,
		BufferedCheckpoint: bufferedCheckpoint,
		LastNoACK:          lastNoACK,
		AckCount:           ackCount,
		Headers:            headers,
	}
}

// DefaultGenesisState returns a default genesis state
func DefaultGenesisState() GenesisState {
	return GenesisState{
		Params: DefaultParams(),
	}
}

// ValidateGenesis performs basic validation of bor genesis data returning an
// error for any failed validation criteria.
func ValidateGenesis(data GenesisState) error {
	if err := data.Params.Validate(); err != nil {
		return err
	}

	if len(data.Headers) != 0 {
		if int(data.AckCount) != len(data.Headers) {
			return errors.New("Incorrect state in state-dump , Please Check")
		}
	}

	return nil
}

// GetGenesisStateFromAppState returns staking GenesisState given raw application genesis state
func GetGenesisStateFromAppState(appState map[string]json.RawMessage) GenesisState {
	var genesisState GenesisState
	if appState[ModuleName] != nil {
		types.ModuleCdc.MustUnmarshalJSON(appState[ModuleName], &genesisState)
	}
	return genesisState
}
