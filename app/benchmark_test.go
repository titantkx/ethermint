package app

import (
	"encoding/json"
	"io"
	"testing"

	dbm "github.com/cometbft/cometbft-db"
	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/libs/log"
	"github.com/cosmos/cosmos-sdk/baseapp"
	simtestutil "github.com/cosmos/cosmos-sdk/testutil/sims"
	"github.com/evmos/ethermint/encoding"
)

func BenchmarkEthermintApp_ExportAppStateAndValidators(b *testing.B) {
	db := dbm.NewMemDB()
	app := NewEthermintApp(
		log.NewTMLogger(io.Discard), db, nil, true, map[int64]bool{},
		DefaultNodeHome, 0,
		encoding.MakeConfig(ModuleBasics),
		simtestutil.EmptyAppOptions{},
		baseapp.SetChainID("ethermint_9000-1"),
	)

	genesisState := NewTestGenesisState(app.AppCodec())
	stateBytes, err := json.MarshalIndent(genesisState, "", "  ")
	if err != nil {
		b.Fatal(err)
	}

	// Initialize the chain
	app.InitChain(
		abci.RequestInitChain{
			ChainId:       "ethermint_9000-1",
			Validators:    []abci.ValidatorUpdate{},
			AppStateBytes: stateBytes,
		},
	)
	app.Commit()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// Making a new app object with the db, so that initchain hasn't been called
		app2 := NewEthermintApp(
			log.NewTMLogger(log.NewSyncWriter(io.Discard)), db, nil, true, map[int64]bool{}, DefaultNodeHome, 0,
			encoding.MakeConfig(ModuleBasics),
			simtestutil.EmptyAppOptions{},
		)
		if _, err := app2.ExportAppStateAndValidators(false, []string{}, []string{}); err != nil {
			b.Fatal(err)
		}
	}
}
