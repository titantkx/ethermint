package common

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"math/big"

	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/titantkx/ethermint/x/evm/statedb"

	"github.com/titantkx/ethermint/types"
)

const UnknownMethodCallGas uint64 = 3000

type Contexter interface {
	Ctx() sdk.Context
}

// Operation is a type that defines if the precompile call
// produced an addition or subtraction of an account's balance
type Operation int8

const (
	Sub Operation = iota
	Add
)

type balanceChangeEntry struct {
	Account common.Address
	Amount  *big.Int
	Op      Operation
}

func NewBalanceChangeEntry(acc common.Address, amt *big.Int, op Operation) balanceChangeEntry { //nolint:revive
	return balanceChangeEntry{acc, amt, op}
}

// snapshot contains all state and events previous to the precompile call
// This is needed to allow us to revert the changes
// during the EVM execution
type snapshot struct {
	MultiStore sdk.CacheMultiStore
	Events     sdk.Events
}

type PrecompileExecutor interface {
	RequiredGas(input []byte, method *abi.Method) uint64

	Execute(
		ctx sdk.Context,
		evm *vm.EVM,
		stateDB vm.StateDB,
		method *abi.Method,
		caller common.Address,
		callingContract vm.ContractRef,
		args []interface{},
		value *big.Int,
		readOnly bool,
		isFromDelegateCall bool,
	) (ret []byte, err error)
}

var _ vm.PrecompiledContract = &Precompile{}

type Precompile struct {
	abi.ABI
	address        common.Address
	journalEntries []balanceChangeEntry

	executor PrecompileExecutor
}

func NewPrecompile(
	abi abi.ABI,
	address common.Address,
	executor PrecompileExecutor,
) *Precompile {
	return &Precompile{
		ABI:      abi,
		address:  address,
		executor: executor,
	}
}

// RequiredGas calculates the base minimum required gas for a transaction or a query.
func (p Precompile) RequiredGas(input []byte) uint64 {
	if len(input) < 4 {
		return p.executor.RequiredGas(input, nil)
	}

	methodID := input[:4]
	method, err := p.MethodById(methodID)
	if err != nil {
		return UnknownMethodCallGas
	}

	return p.executor.RequiredGas(input, method)
}

func (p Precompile) GetABI() abi.ABI {
	return p.ABI
}

// Prepare runs the initial setup required to run a transaction or a query.
// It returns the sdk Context, EVM stateDB, ABI method, initial gas and calling arguments.
func (p Precompile) Prepare(
	evm *vm.EVM,
	contract *vm.Contract,
) (ctx sdk.Context,
	stateDB *statedb.StateDB,
	s snapshot, //nolint:revive
	method *abi.Method,
	initialGas storetypes.Gas,
	initialGasLimit storetypes.Gas,
	args []interface{},
	err error,
) {
	stateDB, ok := evm.StateDB.(*statedb.StateDB)
	if !ok {
		//nolint
		return sdk.Context{}, nil, s, nil, uint64(0), uint64(0), nil, fmt.Errorf(ErrNotRunInEvm)
	}

	// get the stateDB cache ctx
	ctx, err = stateDB.GetCacheContext()
	if err != nil {
		return sdk.Context{}, nil, s, nil, uint64(0), uint64(0), nil, err
	}

	initialGasLimit = ctx.GasMeter().Limit()

	// take a snapshot of the current state before any changes
	// to be able to revert the changes
	s.MultiStore = stateDB.MultiStoreSnapshot()
	s.Events = ctx.EventManager().Events()

	// commit the current changes in the cache ctx
	// to get the updated state for the precompile call
	if err := stateDB.CommitWithCacheCtx(); err != nil {
		return sdk.Context{}, nil, s, nil, uint64(0), initialGasLimit, nil, err
	}

	method, err = p.getMethod(contract)
	if err != nil {
		return sdk.Context{}, nil, s, nil, uint64(0), initialGasLimit, nil, err
	}

	// if the method type is `function` continue looking for arguments
	if method.Type == abi.Function {
		argsBz := contract.Input[4:]
		args, err = method.Inputs.Unpack(argsBz)
		if err != nil {
			return sdk.Context{}, nil, s, nil, uint64(0), initialGasLimit, nil, err
		}
	}

	initialGas = ctx.GasMeter().GasConsumed()

	defer HandleGasError(ctx, contract, initialGas, initialGasLimit, &err)()

	// set the default SDK gas configuration to track gas usage
	// we are changing the gas meter type, so it panics gracefully when out of gas
	ctx = ctx.WithGasMeter(storetypes.NewGasMeter(contract.Gas)).
		WithKVGasConfig(storetypes.KVGasConfig()).
		WithTransientKVGasConfig(storetypes.TransientGasConfig())

	// we need to consume the gas that was already used by the EVM
	ctx.GasMeter().ConsumeGas(initialGas, "creating a new gas meter")

	return ctx, stateDB, s, method, initialGas, initialGasLimit, args, nil
}

// HandleGasError handles the out of gas panic by resetting the gas meter and returning an error.
// This is used in order to avoid panics and to allow for the EVM to continue cleanup if the tx or query run out of gas.
func HandleGasError(ctx sdk.Context, contract *vm.Contract, initialGas storetypes.Gas, initialGasLimit storetypes.Gas, err *error) func() {
	return func() {
		if r := recover(); r != nil {
			switch r.(type) {
			case sdk.ErrorOutOfGas:
				// update contract gas
				usedGas := ctx.GasMeter().GasConsumed() - initialGas
				_ = contract.UseGas(usedGas)

				*err = vm.ErrOutOfGas
				// use InfiniteGasMeter with previous Gas limit.
				ctx = ctx.WithGasMeter(types.NewInfiniteGasMeterWithLimit(initialGasLimit))
				ctx.GasMeter().ConsumeGas(usedGas+initialGas, "change back to InfiniteGasMeter")
			default:
				panic(r)
			}
		}
	}
}

func (p Precompile) Run(
	evm *vm.EVM,
	contract *vm.Contract,
	sender common.Address,
	callingContract vm.ContractRef,
	input []byte, //nolint:revive
	value *big.Int,
	readOnly bool,
	isFromDelegateCall bool,
) (bz []byte, err error) {
	ctx, stateDB, snapshot, method, initialGas, initialGasLimit, args, err := p.Prepare(evm, contract)
	if err != nil {
		return nil, err
	}

	// This handles any out of gas errors that may occur during the execution of a precompile tx or query.
	// It avoids panics and returns the out of gas error so the EVM can continue gracefully.
	defer HandleGasError(ctx, contract, initialGas, initialGasLimit, &err)()

	// execute the precompile contract
	bz, err = p.executor.Execute(ctx, evm, stateDB, method, sender, callingContract, args, value, readOnly, isFromDelegateCall)
	if err != nil {
		return nil, err
	}

	cost := ctx.GasMeter().GasConsumed() - initialGas

	if !contract.UseGas(cost) {
		return nil, vm.ErrOutOfGas
	}

	if err := p.AddJournalEntries(stateDB, snapshot); err != nil {
		return nil, err
	}

	// @todo maybe we need to use InfiniteGasMeter with previous Gas limit here

	return bz, nil
}

// AddJournalEntries adds the balanceChange (if corresponds)
// and precompileCall entries on the stateDB journal
// This allows to revert the call changes within an evm tx
func (p Precompile) AddJournalEntries(stateDB *statedb.StateDB, s snapshot) error {
	for _, entry := range p.journalEntries {
		switch entry.Op {
		case Sub:
			// add the corresponding balance change to the journal
			stateDB.SubBalance(entry.Account, entry.Amount)
		case Add:
			// add the corresponding balance change to the journal
			stateDB.AddBalance(entry.Account, entry.Amount)
		}
	}

	if err := stateDB.AddPrecompileFn(p.Address(), s.MultiStore, s.Events); err != nil {
		return err
	}
	return nil
}

// SetBalanceChangeEntries sets the balanceChange entries
// as the journalEntries field of the precompile.
// These entries will be added to the stateDB's journal
// when calling the AddJournalEntries function
func (p *Precompile) SetBalanceChangeEntries(entries ...balanceChangeEntry) {
	p.journalEntries = entries
}

func (p Precompile) Address() common.Address {
	return p.address
}

func (p *Precompile) SetAddress(addr common.Address) {
	p.address = addr
}

func (p Precompile) getMethod(contract *vm.Contract) (method *abi.Method, err error) {
	// NOTE: This is a special case where the calling transaction does not specify a function name.
	// In this case we default to a `fallback` or `receive` function on the contract.
	isEmptyCallData := len(contract.Input) == 0
	isShortCallData := len(contract.Input) > 0 && len(contract.Input) < 4
	isStandardCallData := len(contract.Input) >= 4

	switch {
	// Case 1: Calldata is empty
	case isEmptyCallData:
		method, err = p.emptyCallData(contract)

	// Case 2: calldata is non-empty but less than 4 bytes needed for a method
	case isShortCallData:
		method, err = p.methodIDCallData()

	// Case 3: calldata is non-empty and contains the minimum 4 bytes needed for a method
	case isStandardCallData:
		method, err = p.standardCallData(contract)
	}

	return method, err
}

// emptyCallData is a helper function that returns the method to be called when the calldata is empty.
func (p Precompile) emptyCallData(contract *vm.Contract) (method *abi.Method, err error) {
	switch {
	// Case 1.1: Send call or transfer tx - 'receive' is called if present and value is transferred
	case contract.Value().Sign() > 0 && p.HasReceive():
		return &p.Receive, nil
	// Case 1.2: Either 'receive' is not present, or no value is transferred - call 'fallback' if present
	case p.HasFallback():
		return &p.Fallback, nil
	// Case 1.3: Neither 'receive' nor 'fallback' are present - return error
	default:
		return nil, vm.ErrExecutionReverted
	}
}

// methodIDCallData is a helper function that returns the method to be called when the calldata is less than 4 bytes.
func (p Precompile) methodIDCallData() (method *abi.Method, err error) {
	// Case 2.2: calldata contains less than 4 bytes needed for a method and 'fallback' is not present - return error
	if !p.HasFallback() {
		return nil, vm.ErrExecutionReverted
	}
	// Case 2.1: calldata contains less than 4 bytes needed for a method - 'fallback' is called if present
	return &p.Fallback, nil
}

// standardCallData is a helper function that returns the method to be called when the calldata is 4 bytes or more.
func (p Precompile) standardCallData(contract *vm.Contract) (method *abi.Method, err error) {
	methodID := contract.Input[:4]
	// NOTE: this function iterates over the method map and returns
	// the method with the given ID
	method, err = p.MethodById(methodID)

	// Case 3.1 calldata contains a non-existing method ID, and `fallback` is not present - return error
	if err != nil && !p.HasFallback() {
		return nil, err
	}

	// Case 3.2: calldata contains a non-existing method ID - 'fallback' is called if present
	if err != nil && p.HasFallback() {
		return &p.Fallback, nil
	}

	return method, nil
}

func MustGetABI(f embed.FS, filename string) abi.ABI {
	abiBz, err := f.ReadFile(filename)
	if err != nil {
		panic(err)
	}

	newAbi, err := abi.JSON(bytes.NewReader(abiBz))
	if err != nil {
		panic(err)
	}
	return newAbi
}

func ValidateArgsLength(args []interface{}, length int) error {
	if len(args) != length {
		return fmt.Errorf("expected %d arguments but got %d", length, len(args))
	}

	return nil
}

func ValidateNonPayable(value *big.Int) error {
	if value != nil && value.Sign() != 0 {
		return errors.New("sending funds to a non-payable function")
	}

	return nil
}
